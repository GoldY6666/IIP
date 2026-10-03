package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yu/iip/internal/db"
	"github.com/yu/iip/internal/task"
)

// Tasks 是 task.Store 的 PostgreSQL 实现。
//
// 与 Lifecycle / Reviews 分开成独立类型：队列的写入模式（SKIP LOCKED 领取、
// 条件入队、超时回收）与事件处置完全不同，混在一起会让两个职责互相牵连。
type Tasks struct {
	queries *db.Queries
}

// 编译期断言：必须满足 task 包定义的队列契约。
var _ task.Store = (*Tasks)(nil)

// maxClaimBatch 是单次领取的硬上限，防止超出 int32 范围的配置值溢出成负数
// （LIMIT 负数 = 一条都领不到，且不报错）。
const maxClaimBatch = 1 << 20

// NewTasks 构造任务队列仓储。conn 取 db.DBTX（可接连接池或事务）。
func NewTasks(conn db.DBTX) *Tasks {
	return &Tasks{queries: db.New(conn)}
}

// Enqueue 入队一个任务。
func (r *Tasks) Enqueue(ctx context.Context, kind task.Kind, payload json.RawMessage) (task.Task, error) {
	if kind == "" {
		return task.Task{}, task.ErrEmptyKind
	}
	if len(payload) == 0 {
		return task.Task{}, task.ErrEmptyPayload
	}

	row, err := r.queries.EnqueueTask(ctx, db.EnqueueTaskParams{
		Kind:    string(kind),
		Payload: payload,
	})
	if err != nil {
		return task.Task{}, fmt.Errorf("repository: 任务入队失败: %w", err)
	}
	return toTask(&row), nil
}

// EnqueueIfAbsent 仅当该事件没有未完成的采集任务时才入队。
//
// 「已存在」返回 (零值 Task, nil) 而不是错误：调用方（事件创建路径）只想知道
// 「现在有没有在排队」，把幂等命中当成错误会让上层凭空多出一类错误处理分支，
// 而实际上它和「成功入队」一样都是正常结果。
func (r *Tasks) EnqueueIfAbsent(ctx context.Context, kind task.Kind, payload json.RawMessage) (task.Task, error) {
	if kind == "" {
		return task.Task{}, task.ErrEmptyKind
	}
	if len(payload) == 0 {
		return task.Task{}, task.ErrEmptyPayload
	}
	// incident_id 是该查询的幂等键，必须能从 payload 里取到；
	// 取不到说明调用方传了与 kind 不匹配的 payload，此时静默插一条
	// 「无法判重」的任务比直接报错更糟 —— 它会在下一次入队时不被识别。
	incidentID, err := incidentIDFromPayload(payload)
	if err != nil {
		return task.Task{}, fmt.Errorf("repository: 条件入队失败: %w", err)
	}

	row, err := r.queries.EnqueueTaskIfAbsent(ctx, db.EnqueueTaskIfAbsentParams{
		Kind:       string(kind),
		Payload:    payload,
		IncidentID: strconv.FormatInt(incidentID, 10),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// WHERE NOT EXISTS 不成立 → 一个字都没插 → RETURNING 无行。
		// 这是「已在队列中」的正常信号，不是失败。
		return task.Task{}, nil
	}
	if err != nil {
		return task.Task{}, fmt.Errorf("repository: 条件入队失败: %w", err)
	}
	return toTask(&row), nil
}

// Claim 领取一批可执行任务并标记为 in_flight。
//
// 返回空切片表示「当前没有可领的任务」，不是错误：轮询 worker 的常态就是
// 空转，把 ErrNoRows 往上抛只会在日志里刷出无意义的错误。
func (r *Tasks) Claim(ctx context.Context, batch int, claimedBy string) ([]task.Task, error) {
	if batch <= 0 {
		return nil, fmt.Errorf("repository: 领取批量必须为正整数，当前为 %d", batch)
	}
	// 显式夹紧到 int32 上限而不是让调用方保证：batch 来自配置，
	// 而配置是可以被环境变量改成任意值的。让int32(1<<40) 静默溢出成
	// 负数会让 LIMIT 变成「负数行」→ 一个任务都领不到，且不报错。
	if batch > maxClaimBatch {
		batch = maxClaimBatch
	}
	if claimedBy == "" {
		// 空标识会让「这批任务被谁领走过」在库上无从查证 —— 而那正是
		// 排查卡住任务时唯一的线索（手册§4.1 决策 5）。
		return nil, errors.New("repository: 领取者标识不能为空")
	}

	rows, err := r.queries.ClaimTasks(ctx, db.ClaimTasksParams{
		ClaimedBy: textOrNull(claimedBy),
		Batch:     int32(batch), // 已夹紧到 maxClaimBatch，转int32 安全
	})
	if err != nil {
		return nil, fmt.Errorf("repository: 领取任务失败: %w", err)
	}

	out := make([]task.Task, 0, len(rows))
	for i := range rows {
		out = append(out, toTask(&rows[i]))
	}
	return out, nil
}

// Complete 标记成功。
//
// 受影响 0 行时返回 task.ErrTaskNotFound：说明这行已被超时回收并被别人领走，
// 此时本次采集的结果**不该被当作权威**（可能有另一个 worker 也在采同一个事件），
// 调用方需要知道「我的完成没生效」而不是被静默吞掉。
func (r *Tasks) Complete(ctx context.Context, id int64) error {
	n, err := r.queries.CompleteTask(ctx, id)
	if err != nil {
		return fmt.Errorf("repository: 标记任务成功失败: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("repository: 标记任务 %d 成功: %w", id, task.ErrTaskNotFound)
	}
	return nil
}

// Fail 记录一次失败并决定重试或判死信。
//
// delay 由调用方按指数退避算好（封顶是业务决策，不该埋在 SQL 里），
// 而 available_at 的实际落点由数据库 NOW() 计算 —— 退避与「是否可见」
// 必须用同一个时钟源，否则会出现「刚退避完就能领」或「迟迟不可领」。
func (r *Tasks) Fail(ctx context.Context, id int64, delay time.Duration, maxAttempts int, lastErr string) (task.Task, error) {
	if delay < 0 {
		return task.Task{}, fmt.Errorf("repository: 退避时长不能为负，当前为 %s", delay)
	}
	if maxAttempts <= 0 {
		return task.Task{}, fmt.Errorf("repository: 最大尝试次数必须为正整数，当前为 %d", maxAttempts)
	}

	row, err := r.queries.FailTask(ctx, db.FailTaskParams{
		MaxAttempts:  int32(maxAttempts),
		DelaySeconds: delay.Seconds(),
		LastError:    textOrNull(lastErr),
		ID:           id,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return task.Task{}, fmt.Errorf("repository: 标记任务 %d 失败: %w", id, task.ErrTaskNotFound)
	}
	if err != nil {
		return task.Task{}, fmt.Errorf("repository: 标记任务 %d 失败: %w", id, err)
	}
	return toTask(&row), nil
}

// ReclaimStale 回收持有者已消失的 in_flight 任务，返回回收条数。
func (r *Tasks) ReclaimStale(ctx context.Context, staleAfter time.Duration) (int64, error) {
	if staleAfter <= 0 {
		return 0, fmt.Errorf("repository: 超时阈值必须为正数，当前为 %s", staleAfter)
	}

	n, err := r.queries.ReclaimStaleTasks(ctx, staleAfter.Seconds())
	if err != nil {
		return 0, fmt.Errorf("repository: 回收超时任务失败: %w", err)
	}
	return n, nil
}

// CountByStatus 返回各状态的任务数，供 worker 心跳与端到端断言使用。
//
// 不是 task.Store 的方法：它是**观测**接口而非队列操作，
// 塞进Store 会让所有实现方（含测试替身）都被迫实现一个它们用不上的方法。
func (r *Tasks) CountByStatus(ctx context.Context) (map[task.Status]int, error) {
	row, err := r.queries.CountTasksByStatus(ctx)
	if err != nil {
		return nil, fmt.Errorf("repository: 统计任务数失败: %w", err)
	}
	// 四个键**无条件**写入，包括计数为 0 的：
	// 调用方查「dead 有几条」时不该先判断键是否存在——缺键与 0 在
	// 「有没有死信」这个问题上含义完全不同。
	return map[task.Status]int{
		task.StatusPending:  int(row.Pending),
		task.StatusInFlight: int(row.InFlight),
		task.StatusDone:     int(row.Done),
		task.StatusDead:     int(row.Dead),
	}, nil
}

// incidentIDFromPayload 从采集任务 payload 中取 incident_id。
func incidentIDFromPayload(payload json.RawMessage) (int64, error) {
	var p task.CollectEvidencePayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return 0, fmt.Errorf("解析任务参数失败: %w", err)
	}
	if p.IncidentID <= 0 {
		return 0, fmt.Errorf("incident_id 非法: %d", p.IncidentID)
	}
	return p.IncidentID, nil
}

// toTask 把 DB 行转成领域对象，**不泄漏 pgtype**。
//
// 取指针：db.Task 有 11 个字段、248 字节，按值传参会被 gocritic 的
// hugeParam 判为「每层栈都拷一份」，而这里没有任何理由要拷贝。
func toTask(row *db.Task) task.Task {
	return task.Task{
		ID:          row.ID,
		Kind:        task.Kind(row.Kind),
		Payload:     row.Payload,
		Status:      task.Status(row.Status),
		Attempts:    int(row.Attempts),
		AvailableAt: row.AvailableAt.Time,
		ClaimedAt:   timestamptzPtr(row.ClaimedAt),
		ClaimedBy:   row.ClaimedBy.String,
		LastError:   row.LastError.String,
	}
}

// timestamptzPtr 把可空时间列转成 *time.Time；NULL 即 nil。
func timestamptzPtr(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time
	return &v
}
