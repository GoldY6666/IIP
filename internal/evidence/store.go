package evidence

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// 本文件定义证据的**持久化契约**与领域视图（不带 pgtype 泄漏）。
//
// 与 task 包同构：契约定义在领域层（消费方），实现放 internal/repository。
// 这样领域规则（预算怎么算、类型怎么合并）可零 SQL、零连接地完整测试。

// StoredEvidence 是一条已落库的证据（领域视图）。
//
// 刻意与 evidence.Evidence 分成两个类型：
// 前者是**采集器吐出的待写数据**（confidence 是 *float64，
// 因为「未评估」与「0」必须能区分），后者是**已落库的数据**
// （时间已按 PG 精度截断、metadata 已反序列化）。
// 合成一个类型会导致「刚采集完还没落库」和「从库里读出来」两种状态
// 用同一组字段表达，而它们的空值语义不同 —— 那类 bug 极难查。
type StoredEvidence struct {
	ID         int64
	IncidentID int64
	Type       Type
	Source     Source
	Query      string
	Value      string
	Timestamp  time.Time

	// Confidence 是 0–1；**Valid 为 false 表示未评估**（DB 里是 NULL）。
	//
	// 用值 + 标志而非 *float64：后者让每个调用点都要判 nil，
	// 而漏判一次就是「把未评估显示成 0%」或 panic。
	Confidence float64
	Valid      bool

	Metadata map[string]string
	// CreatedAt 是**落库时刻**（不是证据时刻）。
	//
	// 两者不可混用：Timestamp 是「证据指向的那个瞬间」，
	// CreatedAt 是「我们什么时候把它写进来的」。
	// 补采一条三小时前的日志时，两者会差三小时。
	CreatedAt time.Time
}

// Store 是证据的持久化契约。**定义在消费方**（本包），实现放 internal/repository。
type Store interface {
	// Save 落库一批证据，返回落库后的记录（含自增 id 与 created_at）。
	//
	// **整批在一个事务里**：一批证据要么都写进去，要么都不写。
	// 半批落库的失败态最难处理——页面上会看到「3 条 Prometheus 证据 +
	// 0 条 Loki 证据」，而没有任何错误提示表明 Loki 那批丢了。
	Save(ctx context.Context, incidentID int64, list []Evidence) ([]StoredEvidence, error)

	// ListByIncident 返回该事件的证据，可选按 type 过滤，
	// 并给出**未被截断前**的真计数。
	//
	// 契约要点（与 M3 的 AlertCount 同源教训）：
	// 返回的 List 最多 limit 条，而 Total 是真值。
	// **调用方绝不能用 len(List) 当总数** —— 手册 §4.2 要求
	// 「只保留了 50 条」必须被如实标成 truncated，而不是说成「一共 50 条」。
	ListByIncident(ctx context.Context, incidentID int64, typ Type, limit int) (EvidenceList, error)

	// CountByType 返回该事件各类型的证据条数，
	// **包含 0 条的类型**（类型全集来自 SortedTypes）。
	CountByType(ctx context.Context, incidentID int64) (map[Type]int, error)
}

// EvidenceList 是一次证据查询的结果。
type EvidenceList struct {
	// Items 是实际返回的证据，最多 limit 条。
	Items []StoredEvidence

	// Total 是该事件（同样过滤条件下）的证据**真计数**。
	//
	// 必需而非优化：M3 的详情页曾用 len(alerts) 当 AlertCount，
	// 而 alerts 已截断到 200 → 250 条的事件显示「200 条关联告警」。
	Total int

	// Truncated 报告是否因超限被截断。
	//
	// 必须能被 API 读到（手册 §4.2）：把「只保留了 50 条」说成「一共 50 条」，
	// 用户会以为没有更多证据。
	Truncated bool
}

// 仓储层错误。
var (
	// ErrInvalidIncidentID 表示事件 id 非法（非正整数）。
	//
	// 单独一个哨兵而不是让上层看 0：incident_id=0 会让查询返回空结果，
	// 表现成「这个事件没有证据」—— 与「事件不存在」或「还没采过」
	// 三种情况在页面上完全一样，无法区分。
	ErrInvalidIncidentID = fmt.Errorf("incident_id 必须为正整数")

	// ErrInvalidType 表示证据类型不在合法集合内。
	ErrInvalidType = fmt.Errorf("证据类型非法")
)

// ValidIncidentID 校验事件 id。
func ValidIncidentID(id int64) error {
	if id <= 0 {
		return fmt.Errorf("%w，当前为 %d", ErrInvalidIncidentID, id)
	}
	return nil
}

// ValidFilterType 校验可选的类型过滤值：空串表示不过滤。
func ValidFilterType(t Type) error {
	if t == "" {
		return nil
	}
	if !t.IsValid() {
		return fmt.Errorf("%w: %q（合法值：%v）", ErrInvalidType, t, SortedTypes())
	}
	return nil
}

// MarshalMetadata 把 metadata 序列化成 JSONB。
//
// 返回空 JSON 对象而非 nil：evidence 表的 metadata 列有 DEFAULT '{}'，
// 但显式写 '{}' 能让「无 metadata」与「metadata 是 NULL」在库上可区分 ——
// 后者意味着写入路径有问题。
func MarshalMetadata(md map[string]string) (json.RawMessage, error) {
	if len(md) == 0 {
		return json.RawMessage(`{}`), nil
	}
	b, err := json.Marshal(md)
	if err != nil {
		return nil, fmt.Errorf("序列化证据 metadata 失败: %w", err)
	}
	return b, nil
}

// UnmarshalMetadata 解析 JSONB metadata。
//
// 空/非法 JSONB 归一化成空 map 而非报错：metadata 是**补充信息**，
// 解析失败不该让整条证据读不出来（那会让一次格式变更
// 把整个证据列表变成 500）。非法内容记在返回的 map 里由调用方决定是否展示。
func UnmarshalMetadata(raw []byte) map[string]string {
	if len(raw) == 0 {
		return map[string]string{}
	}
	var md map[string]string
	if err := json.Unmarshal(raw, &md); err != nil {
		return map[string]string{"_metadata_parse_error": err.Error()}
	}
	if md == nil {
		// JSON 的 null 会被解成 nil map —— 后续往里写会 panic。
		return map[string]string{}
	}
	return md
}

// FillTypeCounts 把「库里实际有的类型」补成「全部类型都有值」。
//
// 库里没有的类型补 0：前端 Tab 角标必须显示 0 而不是让整个 Tab 消失 ——
// 「没采到」与「压根没查过」在页面上必须长得不一样。
func FillTypeCounts(counts map[Type]int) map[Type]int {
	out := make(map[Type]int, len(validTypes))
	for _, t := range SortedTypes() {
		out[t] = counts[t] // 缺失时零值 0
	}
	return out
}

// SortedTypeNames 返回全部合法类型的字符串形式，按字典序。
func SortedTypeNames() []string {
	types := SortedTypes()
	out := make([]string, 0, len(types))
	for _, t := range types {
		out = append(out, string(t))
	}
	sort.Strings(out)
	return out
}
