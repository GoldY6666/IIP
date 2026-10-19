package repository

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/yu/iip/internal/db"
	"github.com/yu/iip/internal/task"
)

// EnqueueEvidence 为事件入队一个证据采集任务（correlation.EvidenceEnqueuer 的实现）。
//
// 幂等由 EnqueueIfAbsent 保证：同事件已有 pending/in_flight 任务时不重复插。
// 因此关联路径可以对同一批告警的多次合入连续调用，无需先查再判。
//
// 游标在这里读而不是由调用方传：调用方（关联服务）不该知道窗口的存在，
// 而「上次采到哪儿」是队列自己的状态。把它做成参数会诱使调用方
// 从别处（evidence 表的 max(timestamp)）取一个错的游标。
//
// 拿不到游标不是错误：走首次采集的窗口推导，由 worker 完成时写回。
func (r *Tasks) EnqueueEvidence(ctx context.Context, incidentID int64) error {
	if incidentID <= 0 {
		return fmt.Errorf("repository: incident_id 必须为正整数，当前为 %d", incidentID)
	}

	cursor, err := r.LastWindowEnd(ctx, incidentID)
	if err != nil {
		// 游标读失败**不阻断入队**：拿零值入队的后果是最多重采一次
		// （产生重复证据），而不入队的后果是这次合入完全没有证据，
		// 且要等到下一次告警合入才会有人想起这件事。
		cursor = time.Time{}
	}

	payload, err := task.NewCollectEvidencePayload(incidentID, cursor)
	if err != nil {
		return fmt.Errorf("repository: 构造采集任务参数失败: %w", err)
	}
	if _, err := r.EnqueueIfAbsent(ctx, task.KindCollectEvidence, payload); err != nil {
		return fmt.Errorf("repository: 证据采集任务入队失败: %w", err)
	}
	return nil
}

// LastWindowEnd 返回该事件最近一次成功采集到的窗口终点。
//
// 从未采过时返回零值 + nil（不是错误）：「还没采过」是完全正常的状态，
// 首次采集的调用方要据此走窗口推导，返回错误会让它无从区分
// 「首次」与「查询失败」。
func (r *Tasks) LastWindowEnd(ctx context.Context, incidentID int64) (time.Time, error) {
	if incidentID <= 0 {
		return time.Time{}, fmt.Errorf("repository: incident_id 必须为正整数，当前为 %d", incidentID)
	}

	row, err := r.queries.LastCollectWindowEnd(ctx, strconv.FormatInt(incidentID, 10))
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("repository: 读取事件 %d 的采集游标失败: %w", incidentID, err)
	}
	if !row.Valid {
		return time.Time{}, nil
	}
	return row.Time.UTC(), nil
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
