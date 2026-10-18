// 单任务执行与失败处置（从 processor.go 拆出，OPEN-M6-1）。
//
// 单文件 ≤ 300 行（docs/IIP-M1详细开发手册.md §4）。processOne 是「一个任务
// 从领到到落库」的完整流程，fail 是它的失败出口 —— 两者放在一起，
// 处理链路不必在三个文件之间跳。
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/yu/iip/internal/evidence"
	"github.com/yu/iip/internal/metrics"
	"github.com/yu/iip/internal/rca"
	"github.com/yu/iip/internal/task"
)

// processOne 执行单个采集任务，并把结果写回队列状态。
func (p *Processor) processOne(ctx context.Context, t *task.Task) error {
	if t.Kind != task.KindCollectEvidence {
		// 不认识的 kind 是**编程错误**（装配时写了新类型但没写处理器），
		// 不是业务失败。直接判失败让任务进死信，而不是静默跳过 ——
		// 静默跳过会让任务永远停在 in_flight 直到超时回收，循环往复。
		return p.fail(ctx, t, fmt.Errorf("不支持的任务类型 %q（本处理器只认 %q）",
			t.Kind, task.KindCollectEvidence))
	}

	incidentID, err := t.IncidentID()
	if err != nil {
		// 参数非法重试也没用，但让它进死信而不是无限重试：
		// 死信行会记下 last_error，排查时能看到「这个任务的参数坏了」。
		return p.fail(ctx, t, err)
	}
	cursor, err := t.Cursor()
	if err != nil {
		return p.fail(ctx, t, err)
	}

	target, err := p.incidents.Target(ctx, incidentID)
	if err != nil {
		return p.fail(ctx, t, fmt.Errorf("读取事件 %d 的采集锚点失败: %w", incidentID, err))
	}

	win, err := evidence.ResolveWindow(evidence.WindowRequest{
		IncidentStart: target.StartedAt,
		Cursor:        cursor,
		Now:           p.nowUTC(),
		Width:         p.cfg.WindowWidth,
	})
	if err != nil {
		return p.fail(ctx, t, fmt.Errorf("推导事件 %d 的采集窗口失败: %w", incidentID, err))
	}
	if err := win.Validate(); err != nil {
		return p.fail(ctx, t, fmt.Errorf("事件 %d 的采集窗口不可用: %w", incidentID, err))
	}

	collected, results := evidence.CollectAll(ctx, p.registry, evidence.Input{
		IncidentID: incidentID,
		Service:    target.Service,
		Window:     win,
	})
	p.observeCollectorDurations(results)
	p.logCollectorResults(incidentID, results)

	// 「全部采集器都失败」才判任务失败（手册 §4.3）。
	// 「采到 0 条」是**成功**：「这段时间没有异常」是完全正常的结论，
	// 判它失败会让任务重试三次再进死信，而重试一百次结果也一样。
	if evidence.AllFailed(results) {
		return p.fail(ctx, t, errors.New("全部采集器均失败"))
	}

	applied := evidence.Apply([][]evidence.Evidence{collected}, p.cfg.MaxPerIncident)

	// 置信度在**截断之后**算，不在之前。
	//
	// 顺序反过来会有两个错：一是覆盖度把已被砍掉的类型也数进去，
	// 页面显示 50 条证据、覆盖度却写 4/6，人怎么算都对不上；
	// 二是 Assign 会给随后被截断掉的证据白算一遍。
	//
	// 「为什么在这里调而不让各采集器各调一次」：覆盖度要跨采集器
	// 才能算，而每个采集器只知道自己这一类。详见
	// docs/S1-置信度判据设计.md §3.3。
	st := rca.Assign(applied.Evidence)

	kept := 0
	if len(applied.Evidence) > 0 {
		stored, saveErr := p.evidence.Save(ctx, incidentID, applied.Evidence)
		if saveErr != nil {
			return p.fail(ctx, t, fmt.Errorf("证据落库失败: %w", saveErr))
		}
		kept = len(stored)
	}

	// 游标在**落库之后**才推进：落库失败会让游标停在上次成功处，
	// 下轮重采同一区间 —— 宁可重复一次，也不能跳过没存下来的区间。
	if err := p.tasks.Complete(ctx, t.ID, win.End); err != nil {
		// 任务已被超时回收并被别人领走。本次采集的证据已经落库，
		// 但**游标没写进去** —— 下一轮会重采同一区间，证据重复。
		// 这比返回错误更让人困惑，故单独告警并说明后果。
		p.logger.Warn("标记任务成功失败，本轮证据已落库但游标未推进",
			slog.Int64("task_id", t.ID),
			slog.Int64("incident_id", incidentID),
			slog.Int("kept", kept),
			slog.String("error", err.Error()))
	}

	p.appendEvidenceEntry(ctx, incidentID, applied, kept, win, results)
	p.logHypotheses(incidentID, applied.Evidence)
	p.logger.Info("证据采集完成",
		slog.Int64("task_id", t.ID),
		slog.Int64("incident_id", incidentID),
		slog.Int("collected", applied.TotalCollected),
		slog.Int("kept", kept),
		slog.Bool("truncated", applied.Truncated),
		slog.String("confidence", st.String()),
		slog.String("window", win.Start.UTC().Format(time.RFC3339)+"/"+win.End.UTC().Format(time.RFC3339)))
	return nil
}

// fail 记录一次失败并按指数退避安排重试。
func (p *Processor) fail(ctx context.Context, t *task.Task, cause error) error {
	delay := task.BackoffDelay(t.Attempts, task.BackoffBase, task.BackoffMax)
	updated, err := p.tasks.Fail(ctx, t.ID, delay, p.cfg.MaxAttempts, cause.Error())
	if err != nil {
		// 连「标记失败」都失败（多半是这行已被超时回收）。
		// 返回原始原因：调用方看到的应该是「采集失败」这个真问题，
		// 而不是「标记失败失败」这个派生问题。
		//
		// 这里**不**计 attempt_failed：标记失败意味着这次失败没被记录下来，
		// 把它算进重试计数会让「失败次数」与任务行的 attempts 对不上，
		// 而那个不一致正是排查重试风暴时唯一可信的对照。
		return fmt.Errorf("%w（且标记失败也失败: %v）", cause, err)
	}
	// 进死信的那次也算 attempt_failed：它确实执行失败了一次，
	// 「最后一次失败」与「前两次失败」对容量规划同样有意义。
	// 「是否进死信」由日志与 task 状态表达，不需要在指标上再分一档。
	p.metrics.TaskRetry.WithLabelValues(metrics.RetryAttemptFailed).Inc()
	p.logger.Warn("采集任务失败，已安排重试或进死信",
		slog.Int64("task_id", t.ID),
		slog.Int("attempts", updated.Attempts),
		slog.String("status", string(updated.Status)),
		slog.Duration("retry_after", delay),
		slog.String("error", cause.Error()))
	if updated.Status == task.StatusDead {
		// 死信单独 INFO 一条：它不是「还在重试」的中间态，
		// 而是**终态** —— 这个事件不会有证据了，运维需要知道。
		p.logger.Info("采集任务已进入死信，该事件将没有自动证据",
			slog.Int64("task_id", t.ID),
			slog.Int("attempts", updated.Attempts),
			slog.String("last_error", updated.LastError))
	}
	return cause
}
