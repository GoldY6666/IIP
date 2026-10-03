package incident

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Snapshot 是事件在某个时刻的状态切片（M3 读侧最小集）。
//
// 名字叫 Snapshot 而不是 Incident：这是**某个时刻**的取值，一旦离开事务就可能过期；
// 叫 Incident 会让人误以为可以拿它去写库。
type Snapshot struct {
	ID         int64
	Title      string
	Severity   string
	Status     Status
	Service    string
	StartedAt  time.Time
	ResolvedAt *time.Time // nil 表示未解决
	UpdatedAt  time.Time
}

// Entry 是一条时间线留痕。Metadata 承载结构化细节（状态流转固定带 from / to）。
type Entry struct {
	IncidentID  int64
	EventType   EventType
	Description string
	Actor       string            // 为空表示系统自动写入（落库为 NULL）
	Metadata    map[string]string // 为空时落库为 '{}'
	Timestamp   time.Time
}

// Summary 是列表页一行的形状。
//
// 比 Snapshot 多了两个**聚合**字段：关联告警数与待确认数。它们都在关联边上
// （手册 §4.4：incidents 表没有这两列，也没有「待确认计数」），只能 JOIN 聚合出来。
type Summary struct {
	ID               int64
	Title            string
	Severity         string
	Status           Status
	Service          string
	StartedAt        time.Time
	ResolvedAt       *time.Time
	UpdatedAt        time.Time
	AlertCount       int
	NeedsReviewCount int
}

// ListFilter 是列表的过滤与分页条件。
//
// Statuses / Severities 为空切片表示**不过滤**：由服务层展开成全部合法取值，
// SQL 因此只有一种执行形态（不写「NULL 则不过滤」的分支）。
type ListFilter struct {
	Statuses   []Status
	Severities []string
	Service    string
	From       *time.Time // 起始时间（含）
	To         *time.Time // 结束时间（含）
	Limit      int
	Offset     int
}

// ListResult 是列表的返回体。
//
// HasMore 由 limit+1 探测得出（手册 §4.3）：不查 COUNT，也不返回可能失真的 total。
// 调用方需要条数时用 len(Items)。
type ListResult struct {
	Items   []Summary
	HasMore bool
}

// 分页参数的界限（手册 §4.3）。
const (
	DefaultListLimit  = 100
	MaxListLimit      = 1000
	MaxTimelineRows   = 200 // 详情内联 timeline 的上限
	MaxIncidentAlerts = 200 // 详情内联关联告警的上限
)

// Detail 是详情页的聚合体（手册 §4.3）。
//
// TruncatedAlerts / TruncatedTimeline 必须显式回给前端：手册要求被截断时
// 前端要提示「仅显示最近 N 条」，把截断悄悄吞掉会让用户以为看到的就是全部。
type Detail struct {
	Summary
	Alerts            []AlertLink
	Timeline          []Entry
	TruncatedAlerts   bool
	TruncatedTimeline bool
}

// ResolveEffect 描述一次状态流转对 resolved_at 列的作用（手册 §4.1 决策 D1）。
//
// 三个取值互斥，由 ResolveAtEffect 计算：
//
//	SetResolvedNow → 写 NOW()          （进入 resolved）
//	ClearResolved  → 置 NULL           （从 resolved 重开）
//	两者皆 false   → 该列保持不动      （含进入 ignored）
type ResolveEffect struct {
	SetResolvedNow bool
	ClearResolved  bool
}

// ResolveAtEffect 计算 resolved_at 的副作用。
//
// 进入 ignored **不写** resolved_at 是有意为之：误报关闭不是一次真实恢复，
// 写进去会把误报计入 MTTR，把指标做脏。代价是「忽略时刻」只能从时间线取
// （手册已明确接受这一代价）。
func ResolveAtEffect(from, to Status) ResolveEffect {
	switch {
	case to == StatusResolved:
		return ResolveEffect{SetResolvedNow: true}
	case from == StatusResolved:
		// 只可能是 resolved → open（重开）：终态之间不可互转，已由状态机拦住。
		return ResolveEffect{ClearResolved: true}
	default:
		return ResolveEffect{}
	}
}

// ErrIncidentNotFound 表示按主键找不到事件（API 层映射为 404）。
var ErrIncidentNotFound = errors.New("事件不存在")

// ErrEmptyNote 表示备注内容为空（API 层映射为 400）。
//
// 空备注必须被拒：一条 description 为空的时间线在页面上是一行空白，
// 除了让人怀疑系统坏了之外没有任何价值。
var ErrEmptyNote = errors.New("备注内容不能为空")

// ConflictError 表示乐观并发冲突：期望的当前状态已经不是库里的当前状态。
//
// 带上 Expected 而非只给一句「请刷新后重试」：排查时可以立刻看出
// 「前端拿着哪个旧状态在提交」，这对定位并发 bug 是关键信息。
type ConflictError struct {
	ID       int64
	Expected Status
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("事件 %d 的状态已被并发修改（提交时依据的旧状态为 %s），请刷新后重试", e.ID, e.Expected)
}

// Code 返回供 API 响应体使用的错误码。
func (e *ConflictError) Code() string { return "status_conflict" }

// Store 是事件生命周期对持久层的全部需求，由 internal/repository 实现。
//
// 放在领域包而不是仓储包，是为了让服务层只依赖抽象：状态流转的测试可以拿
// 内存实现跑，不必起数据库。
type Store interface {
	// GetByID 读取事件当前状态；不存在时返回 ErrIncidentNotFound。
	GetByID(ctx context.Context, id int64) (Snapshot, error)

	// UpdateStatus 在「当前状态仍为 arg.From」的前提下更新状态，
	// 返回是否命中（false = 状态已被并发改走，一个并发冲突信号）。
	UpdateStatus(ctx context.Context, arg UpdateStatusArg) (bool, error)

	// AppendTimelineEntry 写入一条时间线留痕。
	AppendTimelineEntry(ctx context.Context, e Entry) error

	// List 按过滤条件取事件列表。调用方已把 Limit 规范化（含 +1 探测余量）。
	List(ctx context.Context, f ListFilter) ([]Summary, error)

	// Timeline 取事件的时间线（按时间倒序），limit 由调用方给出。
	Timeline(ctx context.Context, incidentID int64, limit int) ([]Entry, error)

	// Alerts 取事件的关联告警（含 score / needs_review），limit 由调用方给出。
	Alerts(ctx context.Context, incidentID int64, limit int) ([]AlertLink, error)

	// --- 以下为 REVIEW 队列（T3.4）所需，全部可在事务内执行 ---

	// ListReviews 取待确认队列的一页（limit 已含 +1 探测余量）。
	ListReviews(ctx context.Context, limit, offset int) ([]ReviewEdge, error)

	// GetReviewEdge 取一条**仍在待确认**的边；不存在或已处理时返回 ErrReviewEdgeNotFound。
	// 返回整条边而非一个布尔：reject 需要其中的 fingerprint 去写抑制行。
	GetReviewEdge(ctx context.Context, incidentID, alertID int64) (ReviewEdge, error)

	// ConfirmReviewEdge 清掉待确认标记并保留关联边，返回是否命中。
	ConfirmReviewEdge(ctx context.Context, incidentID, alertID int64) (bool, error)

	// DeleteAlertEdge 删除关联边，返回是否命中。
	DeleteAlertEdge(ctx context.Context, incidentID, alertID int64) (bool, error)

	// CountAlertEdges 统计该告警还剩多少条关联边。
	CountAlertEdges(ctx context.Context, alertID int64) (int, error)

	// ClearAlertIncident 把告警的 incident_id 置 NULL。
	ClearAlertIncident(ctx context.Context, alertID int64) error

	// AddSuppression 写一条抑制行（重复写入静默跳过）。
	AddSuppression(ctx context.Context, fingerprint string, incidentID int64, actor, reason string) error
}

// ReviewEdge 是待人工确认队列里的一条：哪条告警被关联到了哪个事件。
//
// 用 AlertLink 承载告警侧字段，避免再定义一套几乎相同的结构。
type ReviewEdge struct {
	IncidentID int64
	Alert      AlertLink
}

// ErrReviewEdgeNotFound 表示这条待确认边不存在或已被处理过（API 层映射为 404）。
//
// 「不存在」与「已处理」对调用方是同一个结果：都不该再操作一次。
// 区别在于日志，由服务层按需区分。
var ErrReviewEdgeNotFound = errors.New("待确认的关联边不存在或已处理")

// ErrInvalidReviewDecision 表示复核动作不是 confirm / reject。
var ErrInvalidReviewDecision = errors.New("复核动作必须是 confirm 或 reject")

// AlertLink 是详情页一条关联告警（手册 §4.4）。
//
// Score 为 nil 表示该告警是 NEW 档（无关联对象），前端应显示「—」而不是 0。
type AlertLink struct {
	AlertID          int64
	Fingerprint      string
	Severity         string
	Labels           map[string]string
	StartsAt         time.Time
	EndsAt           *time.Time
	CorrelationScore *float64
	NeedsReview      bool
	CorrelatedAt     time.Time
}

// UpdateStatusArg 是 UpdateStatus 的入参。
type UpdateStatusArg struct {
	ID     int64
	From   Status
	To     Status
	Effect ResolveEffect
}
