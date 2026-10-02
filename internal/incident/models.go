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
}

// UpdateStatusArg 是 UpdateStatus 的入参。
type UpdateStatusArg struct {
	ID     int64
	From   Status
	To     Status
	Effect ResolveEffect
}
