package api

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/yu/iip/internal/evidence"
)

// CodeEvidenceTypeInvalid 是证据类型过滤值非法的错误码。
const CodeEvidenceTypeInvalid = "evidence_type_invalid"

// EvidenceLister 是证据查询能力。
//
// 定义在消费方（本包），与 IncidentLister 同一手法：
// handler 只依赖它真正用到的那两个方法，单测注入替身即可，不必连库。
type EvidenceLister interface {
	ListByIncident(ctx context.Context, incidentID int64,
		typ evidence.Type, limit int) (evidence.EvidenceList, error)
	CountByType(ctx context.Context, incidentID int64) (map[evidence.Type]int, error)
}

// evidenceBody 是响应里的一条证据。
//
// **不复用 evidence.StoredEvidence 直接序列化**：那个结构体是仓储层视图，
// 带 Valid 标志与 map 类型，直接吐出去会让 JSON 形状依赖内部实现
// （比如 Confidence 该不该出现、Metadata 空 map 序列化成 {} 还是 null）。
// 这里显式定义响应形状，与内部结构解耦 —— 改内部不改契约。
type evidenceBody struct {
	ID        int64  `json:"id"`
	Type      string `json:"type"`
	Source    string `json:"source"`
	Query     string `json:"query"`
	Value     string `json:"value"`
	Timestamp string `json:"timestamp"`

	// Confidence 是 -1 起步的哨兵：Valid 为 false 时序列化成 null。
	//
	// **不能省成 0** —— 「未评估」与「置信度 0」在页面上必须能区分：
	// 前者是「采集器没有这个判断能力」（OPEN-M4-1），
	// 后者是「这条证据明确没有支撑力」。合成 0 就是把「不知道」说成「没用」。
	Confidence *float64 `json:"confidence"`

	Metadata map[string]string `json:"metadata"`

	// CollectedAt 是**落库时刻**（≠ Timestamp 的证据时刻）。
	//
	// 单独回传的理由：补采一条三小时前的日志时两者差三小时，
	// 而「什么时候采到的」正是判断证据够不够新的依据。
	// 合成一个字段会让运维以为「这条证据是三小时前发生的」。
	CollectedAt string `json:"collected_at"`
}

type evidenceListResponse struct {
	Evidence []evidenceBody `json:"evidence"`
	// Count 是**本次返回的条数**（= len(evidence)），不是总数。
	//
	// 刻意与 Total 分开命名：Count 回答「这次给了你几条」，
	// Total 回答「一共有几条」。沿用 M3 的 AlertCount 教训 ——
	// 两者混成一个字段就必然出现「250 条显示成 200 条」。
	Count int `json:"count"`
	// Total 是**未被截断前**的真计数（SQL count(*)）。
	Total int `json:"total"`
	// Truncated 报告是否超限被截断（手册 §4.2）。
	//
	// 必须回传：把「只保留了 50 条」说成「一共 50 条」，
	// 用户会以为没有更多证据。
	Truncated bool `json:"truncated"`
	// Limit 是本次生效的上限，让前端能显示「仅显示最近 50 条」。
	Limit int `json:"limit"`
	// ByType 是各类型的条数（含 0 条的类型），供前端 Tab 角标。
	ByType map[string]int `json:"by_type"`
	// Type 是本次生效的类型过滤（空串 = 不过滤）。
	Type string `json:"type,omitempty"`
}

// EvidenceList 返回 GET /api/v1/incidents/{incidentID}/evidence 的 handler。
//
// 支持的 query 参数：
//   - type：按证据类型过滤（metric / log / trace / deployment /
//     config_change / historical）；非法值返回 400 而不是静默返回空列表——
//     静默空列表会被前端渲染成「该类型没有证据」，把参数拼写错误伪装成事实。
//   - limit：返回条数上限，默认 DefaultMaxPerIncident（50），硬上限 1000。
func EvidenceList(lister EvidenceLister, logger *slog.Logger) http.HandlerFunc {
	if logger == nil {
		logger = slog.Default()
	}
	return func(w http.ResponseWriter, r *http.Request) {
		incidentID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
		if err != nil || incidentID <= 0 {
			writeErrorCode(w, logger, http.StatusBadRequest, CodeInvalidBody, "事件 id 必须是正整数")
			return
		}

		q := r.URL.Query()
		typ := evidence.Type(q.Get("type"))
		// 不写成 `if err := ...`：那会遮蔽上面 ParseInt 的 err，
		// 而遮蔽后的 err 在此块内已无用途 —— 遮蔽只会让人读错作用域。
		if vErr := evidence.ValidFilterType(typ); vErr != nil {
			writeErrorCode(w, logger, http.StatusBadRequest, CodeEvidenceTypeInvalid, vErr.Error())
			return
		}
		limit, ok := parseEvidenceLimit(w, q.Get("limit"), logger)
		if !ok {
			return
		}

		res, err := lister.ListByIncident(r.Context(), incidentID, typ, limit)
		if err != nil {
			logger.Error("查询事件证据失败",
				slog.String("error", err.Error()),
				slog.Int64("incident_id", incidentID))
			writeErrorCode(w, logger, http.StatusServiceUnavailable,
				CodeStorageUnavail, msgStorageUnavailable)
			return
		}

		// by_type 是**辅助信息**而非主数据：它失败不该让整个请求 503。
		//
		// 反过来想就清楚了 —— 主列表都拿到了，为一个角标计数把整页变成错误页，
		// 是把「锦上添花」当成了「必需」。此时页面照常显示证据，只少一个角标。
		//
		// 补齐缺失类型放在**本层**而不是仓储层：这是响应形状的要求
		//（前端 Tab 角标要显示 0 而不是让 Tab 消失），
		// 不是查询语义。放在仓储层的话，任何绕过本handler 的调用方
		// 都要自己记得补一遍 —— 那正是「同一份补零逻辑散落多处」的起点。
		byType := make(map[string]int, len(evidence.SortedTypes()))
		counts, cErr := lister.CountByType(r.Context(), incidentID)
		switch {
		case cErr != nil:
			logger.Warn("统计各类型证据条数失败，响应里 by_type 将为空",
				slog.String("error", cErr.Error()),
				slog.Int64("incident_id", incidentID))
			// 失败时给**全 0**而不是空对象：空对象会让前端把
			// 「统计失败」与「真的是 0」渲染成同一个样子。
			// 至少 Tab 角标显示 0，而主列表是可信的。
			for _, t := range evidence.SortedTypes() {
				byType[string(t)] = 0
			}
		default:
			for t, n := range evidence.FillTypeCounts(counts) {
				byType[string(t)] = n
			}
		}

		writeJSON(w, logger, http.StatusOK, evidenceListResponse{
			Evidence:  toEvidenceBodies(res.Items),
			Count:     len(res.Items),
			Total:     res.Total,
			Truncated: res.Truncated,
			Limit:     limit,
			ByType:    byType,
			Type:      string(typ),
		})
	}
}

// maxEvidenceLimit 是单次查询返回条数的硬上限。
//
// 与仓储层的 maxListLimit 刻意是**两个独立常量**：
// 仓储层那道是防「配置写成天文数字」的最后一道闸，
// API 这道是防「一个 HTTP 请求把整张证据表拉进内存」。
// 合成一个会让其中一层改动意外影响另一层的契约。
const maxEvidenceLimit = 500

// parseEvidenceLimit 解析 limit 参数。
func parseEvidenceLimit(w http.ResponseWriter, raw string, logger *slog.Logger) (int, bool) {
	if raw == "" {
		return evidence.DefaultMaxPerIncident, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		writeErrorCode(w, logger, http.StatusBadRequest, CodeInvalidBody,
			"limit 必须是整数，当前为 "+raw)
		return 0, false
	}
	if n <= 0 {
		// 0 与负数都拒：LIMIT 0 会返回空列表且无任何提示，
		// 而调用方以为自己要了「0 条以上」——那是个不存在的语义。
		writeErrorCode(w, logger, http.StatusBadRequest, CodeInvalidBody,
			"limit 必须为正整数，当前为 "+raw)
		return 0, false
	}
	if n > maxEvidenceLimit {
		n = maxEvidenceLimit
	}
	return n, true
}

// toEvidenceBodies 把仓储视图转成响应体。
func toEvidenceBodies(items []evidence.StoredEvidence) []evidenceBody {
	out := make([]evidenceBody, 0, len(items))
	// 按索引取而非值拷贝：StoredEvidence 152 字节（含 map 与 time.Time），
	// 一次列表 50 条就是 7.6 KB 的纯拷贝。
	for i := range items {
		e := &items[i]
		b := evidenceBody{
			ID:        e.ID,
			Type:      string(e.Type),
			Source:    e.Source,
			Query:     e.Query,
			Value:     e.Value,
			Metadata:  nonNilMetadata(e.Metadata),
			Timestamp: formatEvidenceTime(e.Timestamp),
		}
		if !e.Timestamp.IsZero() {
			b.CollectedAt = formatEvidenceTime(e.CreatedAt)
		}
		if e.Valid {
			// 拷贝成新指针：直接取 &e.Confidence 会把整个循环变量
			// 的字段地址暴露出去（Go 1.22 前是经典别名 bug）。
			c := e.Confidence
			b.Confidence = &c
		}
		out = append(out, b)
	}
	return out
}

// nonNilMetadata 保证 metadata 序列化成对象而非 null。
//
// Go 的 nil map 序列化成 JSON null，而前端拿到 null 时
// `Object.keys(e.metadata)` 直接抛错 —— 整条证据渲染失败，
// 连带同卡片上的其他字段一起消失。
//
// 归一化放在**响应层**而不是依赖仓储层：nil 可能来自任何实现
// （测试替身、未来换存储、JSON 反序列化），只在仓储层兜一次
// 意味着每条新路径都得记得再兜一次。
func nonNilMetadata(md map[string]string) map[string]string {
	if md == nil {
		return map[string]string{}
	}
	return md
}

// formatEvidenceTime 统一时间格式。
//
// 用 RFC3339 而不是 time.Time 默认序列化：Go 默认输出 RFC3339Nano，
// 同一秒内的两条证据会出现长度不同的时间串，前端做字符串排序会错。
func formatEvidenceTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
