// Package task 承载 M4 的任务队列领域逻辑：状态、领取契约与重试策略。
//
// 本包的领域层（退避计算、能否领取、该不该判死信）一律**零 SQL**，
// 与 M3 的 incident 包同构：这些是纯业务规则，脱离数据库即可完整测试。
//
// 规格来源：docs/IIP-M4详细开发手册.md §4.1。
package task

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Kind 是任务类型。M4 只有一个采集任务，但用字符串枚举而非单一类型，
// 是为了让「新增任务类型」不必改动领取与重试的代码路径。
type Kind string

const (
	// KindCollectEvidence 是 M4 的证据采集任务（手册 §4.1）。
	KindCollectEvidence Kind = "collect_evidence"
)

// Status 是任务状态，取值与 0005 的 chk_tasks_status 一一对应。
type Status string

const (
	// StatusPending 待领取（含退避中与被回收的）。
	StatusPending Status = "pending"
	// StatusInFlight 已被某个 worker 领取、执行中。
	StatusInFlight Status = "in_flight"
	// StatusDone 成功完成。
	StatusDone Status = "done"
	// StatusDead 超过最大尝试次数，进死信。
	//
	// 死信**保留行而不删除**：删掉就看不出「曾经试过、试了几次、
	// 为什么失败」，而排查采集缺失时这恰恰是唯一的线索。
	StatusDead Status = "dead"
)

// 队列参数（手册 §4.1 的默认配置）。
const (
	// DefaultBatch 是单次轮询最多领取的任务数。
	//
	// 不宜过大：一轮里领太多会让 drain 阶段要等更久（worker 退出时
	// 要等所有 in-flight 完成，上限 drainTimeout）。
	DefaultBatch = 10

	// DefaultMaxAttempts 是超过即判死信的尝试次数（计划规定最大 3 次）。
	DefaultMaxAttempts = 3

	// DefaultStaleAfter 是 in_flight 多久算「持有者已消失」并可被回收。
	DefaultStaleAfter = 5 * time.Minute

	// BackoffBase 是第一次失败后的退避时长。
	BackoffBase = 5 * time.Second

	// BackoffMax 是退避封顶（手册 §4.1 决策 3）。
	//
	// 封顶的理由：第 10 次失败后若不封顶，5s * 2^9 ≈ 42 分钟才可见，
	// 而那时人早忘了这回事。封到 5 分钟，失败率高的任务仍能被稳定重试，
	// 又不会因为等待而彻底失联。
	BackoffMax = 5 * time.Minute
)

// 错误类型。API/日志层据此区分「业务失败」与「队列本身出问题」。
var (
	// ErrTaskNotFound 表示按 id 找不到任务，或任务已不处于 in_flight
	//（已被超时回收并被别人领走）。
	ErrTaskNotFound = errors.New("任务不存在或已不在执行中")

	// ErrEmptyKind 表示入队时 kind 为空。
	ErrEmptyKind = errors.New("任务类型不能为空")

	// ErrEmptyPayload 表示入队时 payload 为空。
	//
	// 不给默认值：payload 空意味着执行时无事可做，
	// 与其领回来再失败，不如入队时就拒。
	ErrEmptyPayload = errors.New("任务参数不能为空")
)

// CollectEvidencePayload 是 KindCollectEvidence 的参数。
type CollectEvidencePayload struct {
	// IncidentID 是要采集证据的事件主键。
	IncidentID int64 `json:"incident_id"`

	// WindowStart 是本次采集窗口的**起点游标**，即「上次采到哪儿了」。
	//
	// **零值表示首次采集**：此时窗口起点由 worker 按事件开始时间推导
	// （见 evidence.ResolveWindow）。
	//
	// 为什么要游标而不是每次都重采整个窗口：新告警合入已有事件会触发
	// 新一轮采集，若每次都从事故起点重采，同一批 RED 指标会以完全相同的
	// (source, query, timestamp) 再落库一份，页面上出现两份一模一样的证据。
	// 带游标后每轮只采「上次之后」的新增区间。
	//
	// 用零值而非 `*time.Time`：JSON 里 null 与缺字段都会解成 nil，
	// 而「首次」与「上次采到 1970 年」在业务上不可能同时成立，
	// 多一个指针只是多一个可写错的状态。
	WindowStart time.Time `json:"window_start,omitempty"`
}

// Task 是一个任务行（领域视图，不带 pgtype 泄漏）。
type Task struct {
	ID          int64
	Kind        Kind
	Payload     json.RawMessage
	Status      Status
	Attempts    int
	AvailableAt time.Time
	ClaimedAt   *time.Time
	ClaimedBy   string
	LastError   string
}

// IncidentID 解析出采集任务的目标事件。
//
// 只对 KindCollectEvidence 有意义；其余 kind 返回错误而不是 0 ——
// 返回 0 会让调用方去查incident 0，然后拿到一个「不存在」的错误，
// 掩盖真正的问题（拿错了 kind 的任务）。
func (t *Task) IncidentID() (int64, error) {
	if t.Kind != KindCollectEvidence {
		return 0, fmt.Errorf("任务类型 %q 不支持解析 incident_id", t.Kind)
	}
	var p CollectEvidencePayload
	if err := json.Unmarshal(t.Payload, &p); err != nil {
		return 0, fmt.Errorf("解析任务 %d 的参数失败: %w", t.ID, err)
	}
	if p.IncidentID <= 0 {
		return 0, fmt.Errorf("任务 %d 的 incident_id 非法: %d", t.ID, p.IncidentID)
	}
	return p.IncidentID, nil
}

// NewCollectEvidencePayload 构造采集任务的 payload。
// NewCollectEvidencePayload 构造采集任务的 payload。windowStart 为零值表示首次采集。
func NewCollectEvidencePayload(incidentID int64, windowStart time.Time) ([]byte, error) {
	if incidentID <= 0 {
		return nil, fmt.Errorf("incident_id 必须为正整数，当前为 %d", incidentID)
	}
	b, err := json.Marshal(CollectEvidencePayload{
		IncidentID:  incidentID,
		WindowStart: windowStart.UTC(),
	})
	if err != nil {
		return nil, fmt.Errorf("序列化任务参数失败: %w", err)
	}
	return b, nil
}

// Cursor 返回该任务带的窗口起点游标；零值表示首次采集。
//
// 与 IncidentID 分开成两个方法而不是让调用方自己解 JSON：
// payload 的形状是本包的实现细节，暴露 json.RawMessage 给执行层
// 意味着「以后加字段要记得同步改所有调用点」——而漏改的那个会静默
// 拿不到游标，从「只采增量」退回「每次全量重采」。
func (t *Task) Cursor() (time.Time, error) {
	if t.Kind != KindCollectEvidence {
		return time.Time{}, nil
	}
	var p CollectEvidencePayload
	if err := json.Unmarshal(t.Payload, &p); err != nil {
		return time.Time{}, fmt.Errorf("解析任务 %d 的采集游标失败: %w", t.ID, err)
	}
	return p.WindowStart.UTC(), nil
}

// IsTerminal 报告任务是否已到终态（不再会被领取）。
func (t *Task) IsTerminal() bool {
	return t.Status == StatusDone || t.Status == StatusDead
}

// Store 是队列的持久化契约。**定义在消费方**（本包），
// 实现放 internal/repository —— 与 M2/M3 的模式一致。
type Store interface {
	// Enqueue 入队一个任务。返回落库后的任务。
	Enqueue(ctx context.Context, kind Kind, payload json.RawMessage) (Task, error)

	// EnqueueIfAbsent 仅当该事件没有未完成的采集任务时才入队。
	// 已存在未完成任务时返回 (Task{}, nil) 且err == nil ——
	// 「已经在队列里」不是错误，是幂等命中。
	EnqueueIfAbsent(ctx context.Context, kind Kind, payload json.RawMessage) (Task, error)

	// Claim 领取一批可执行任务并标记为 in_flight。
	Claim(ctx context.Context, batch int, claimedBy string) ([]Task, error)

	// Complete 标记成功，并把本任务实际采到的窗口终点写成下一轮的游标。
	//
	// windowEnd 传零值表示「本次没拿到窗口」，此时**不写游标**：
	// 写一个零值会让下一轮从 1970 年开始，等于每次重采全部历史。
	//
	// 即使一条证据都没采到也要传真实窗口终点 —— 否则游标不推进，
	// 同一个空窗口会被下一轮重新采一遍（手册 §4.2 修订段）。
	//
	// 受影响行数为 0 时返回 ErrTaskNotFound
	// —— 说明这行已被超时回收并被别人领走，本次结果不该视为权威。
	Complete(ctx context.Context, id int64, windowEnd time.Time) error

	// Fail 记录一次失败并决定重试或判死信，返回更新后的任务。
	Fail(ctx context.Context, id int64, delay time.Duration, maxAttempts int, lastErr string) (Task, error)

	// ReclaimStale 回收持有者已消失的 in_flight 任务，返回回收条数。
	ReclaimStale(ctx context.Context, staleAfter time.Duration) (int64, error)

	// LastWindowEnd 返回该事件最近一次**成功采集**到的窗口终点。
	//
	// 零值表示「从未采过」，调用方据此走首次采集的窗口推导。
	// 只认 status='done' 的行：pending/in_flight 还没采完，
	// dead 是采失败了 —— 拿它们的游标会让下一轮跳过还没采过的区间。
	LastWindowEnd(ctx context.Context, incidentID int64) (time.Time, error)
}

// BackoffDelay 返回第 attempts 次失败后的退避时长。
//
// 指数退避且**封顶** BackoffMax（手册 §4.1 决策 3）。
// attempts 从 1 起（首次失败即 attempts=1）。
//
// 纯函数：这是最容易写错的算术，且它决定一个坏任务多久才被重试一次，
// 单元测试比集成测试更容易钉死边界（尤其 attempts=0 与超大attempts）。
//
// 参数名用 ceiling 而非 max：max 是 Go 1.21 起的**内建函数**，
// 遮蔽它会让本包内任何想用内建 max 的代码编译失败，属埋雷。
func BackoffDelay(attempts int, base, ceiling time.Duration) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	if base <= 0 {
		base = BackoffBase
	}
	if ceiling <= 0 {
		ceiling = BackoffMax
	}

	d := base
	for i := 1; i < attempts; i++ {
		d *= 2
		// 提前到顶比逐次翻倍再截断更省事，且避免整数溢出：
		// attempts 很大时 2^60 的 time.Duration 会溢出成负数，
		// 让 available_at 落到过去 —— 那样任务会被立刻反复领取。
		if d >= ceiling {
			return ceiling
		}
	}
	if d > ceiling {
		return ceiling
	}
	return d
}

// ShouldDeadLetter 报告第 attempts 次失败后是否应进死信。
//
// 判定用「>=」而非「>」：最大 3 次意味着**第 3 次失败就进死信**，
// 也就是只重试 2 次。若写成 >，会实际重试 3 次，与计划规定的
// 「最大 3 次」不符（多试一次，语义上是 4 次尝试）。
func ShouldDeadLetter(attempts, maxAttempts int) bool {
	if maxAttempts <= 0 {
		maxAttempts = DefaultMaxAttempts
	}
	return attempts >= maxAttempts
}
