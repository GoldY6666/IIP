// Package worker 承载 M4 的证据采集执行层：把队列里的任务变成落库的证据。
//
// 与 task（队列语义）、evidence（证据与采集器语义）分开的理由：
// 那两个包是**纯领域**（零 SQL、零网络、可脱离数据库完整测试），
// 而本包是编排层 —— 它同时要知道队列、采集器注册表、证据仓储与事件快照，
// 任何一处出错的表现都不同于前两者（「任务重试」而不是「编译不过」）。
//
// 规格来源：docs/IIP-M4详细开发手册.md §4.3（逐采集器独立调用）、
// §4.4（时间语义）、§4.5（降级）、§七（必测边界）。
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/yu/iip/internal/evidence"
	"github.com/yu/iip/internal/incident"
	"github.com/yu/iip/internal/task"
)

// IncidentReader 是执行采集所需的**最小**事件读取能力。
//
// 只要 service（采集器的 label 匹配键）与 started_at（窗口推导的锚点），
// 刻意不要整个 incident.Store：执行层不需要改状态、不需要读时间线，
// 依赖越窄，单测里的替身越简单，也越不容易在执行层混进写操作。
type IncidentReader interface {
	// Target 返回采集所需的事件字段。
	//
	// 事件不存在时返回 error（调用方据此判失败重试），
	// 而不是一个零值 Target —— 零值会让 service 为空串、
	// started_at 为零，进而推导出「拿不到锚点」的退化窗口，
	// 把「事件不存在」伪装成「采集不到数据」。
	Target(ctx context.Context, incidentID int64) (Target, error)
}

// Target 是一个事件的采集锚点。
type Target struct {
	// Service 是服务名，采集器用它做 label 匹配。**允许为空**。
	Service string
	// StartedAt 是事件开始时间，窗口推导的锚点。
	StartedAt time.Time
}

// TimelineWriter 是写一条时间线留痕的能力（可选）。
type TimelineWriter interface {
	AppendTimelineEntry(ctx context.Context, e incident.Entry) error
}

// Config 是执行器的运行参数。
type Config struct {
	// Batch 是单轮最多处理几条任务。
	Batch int

	// StaleAfter 是 in_flight 多久算超时可回收。
	StaleAfter time.Duration

	// MaxAttempts 是超过即判死信的尝试次数。
	MaxAttempts int

	// MaxPerIncident 是单事件证据上限（<= 0 时用 evidence.DefaultMaxPerIncident）。
	MaxPerIncident int

	// WindowWidth 是采集窗口宽度上限（<= 0 时用 evidence.DefaultWindow）。
	WindowWidth time.Duration

	// ClaimedBy 是本实例标识，落在 tasks.claimed_by 上。
	//
	// 排查时能看出「这批任务被谁领走过」，比只有时间戳有用得多。
	// 空值时由 NewProcessor 用 hostname-pid 兜底。
	ClaimedBy string
}

// 补齐零值，避免调用方漏配导致的静默错误。
func (c Config) withDefaults() Config {
	if c.Batch <= 0 {
		c.Batch = task.DefaultBatch
	}
	if c.StaleAfter <= 0 {
		c.StaleAfter = task.DefaultStaleAfter
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = task.DefaultMaxAttempts
	}
	if c.MaxPerIncident <= 0 {
		c.MaxPerIncident = evidence.DefaultMaxPerIncident
	}
	if c.WindowWidth <= 0 {
		c.WindowWidth = evidence.DefaultWindow
	}
	return c
}

// Processor 执行一轮采集任务。
//
// **无状态且并发安全**：所有可变状态都在方法内部。
// 同一实例会被多个 worker goroutine 同时调用。
type Processor struct {
	tasks     task.Store
	evidence  evidence.Store
	incidents IncidentReader
	registry  *evidence.Registry
	timeline  TimelineWriter
	logger    *slog.Logger
	cfg       Config

	// now 是可注入时钟；nil 时取 time.Now().UTC()。
	now func() time.Time
}

// NewProcessor 构造执行器。logger 为 nil 时退化为 slog.Default()。
//
// timeline 为 nil 时不写留痕（证据照样落库）：留痕是给人读的补充，
// 不该成为证据能否落库的前提。
func NewProcessor(
	tasks task.Store,
	evStore evidence.Store,
	incidents IncidentReader,
	registry *evidence.Registry,
	timeline TimelineWriter,
	cfg Config,
	logger *slog.Logger,
) *Processor {
	if logger == nil {
		logger = slog.Default()
	}
	return &Processor{
		tasks:     tasks,
		evidence:  evStore,
		incidents: incidents,
		registry:  registry,
		timeline:  timeline,
		logger:    logger,
		cfg:       cfg.withDefaults(),
	}
}

// RunOnce 执行一轮：回收超时任务 → 领取一批 → 逐条执行。
//
// 返回本轮**处理的任务数**与遇到的第一个错误。错误只用于日志与健康观测，
// 绝不向上冒泡终止 worker —— 一个任务失败不该让整个进程退出。
//
// 先回收后领取的顺序不能反：刚崩溃留下的 in_flight 行不回收，
// 它们既领不到（不在 pending 集合里）也删不掉，队列会逐渐被僵死任务占满。
func (p *Processor) RunOnce(ctx context.Context) (int, error) {
	if n, err := p.tasks.ReclaimStale(ctx, p.cfg.StaleAfter); err != nil {
		// 回收失败不阻断本轮：领取走的是 pending 集合，
		// 僵死任务不影响新任务被领走。下轮再回收。
		p.logger.Warn("回收超时任务失败（不影响本轮领取）",
			slog.String("error", err.Error()),
			slog.Duration("stale_after", p.cfg.StaleAfter))
	} else if n > 0 {
		p.logger.Warn("回收了超时未完成的任务",
			slog.Int64("reclaimed", n),
			slog.Duration("stale_after", p.cfg.StaleAfter))
	}

	batch, err := p.tasks.Claim(ctx, p.cfg.Batch, p.cfg.ClaimedBy)
	if err != nil {
		return 0, fmt.Errorf("领取任务失败: %w", err)
	}
	if len(batch) == 0 {
		return 0, nil
	}

	var firstErr error
	for i := range batch {
		if err := p.processOne(ctx, &batch[i]); err != nil {
			p.logger.Error("采集任务执行失败",
				slog.Int64("task_id", batch[i].ID),
				slog.String("kind", string(batch[i].Kind)),
				slog.Int("attempts", batch[i].Attempts),
				slog.String("error", err.Error()))
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return len(batch), firstErr
}

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
	p.logCollectorResults(incidentID, results)

	// 「全部采集器都失败」才判任务失败（手册 §4.3）。
	// 「采到 0 条」是**成功**：「这段时间没有异常」是完全正常的结论，
	// 判它失败会让任务重试三次再进死信，而重试一百次结果也一样。
	if evidence.AllFailed(results) {
		return p.fail(ctx, t, errors.New("全部采集器均失败"))
	}

	applied := evidence.Apply([][]evidence.Evidence{collected}, p.cfg.MaxPerIncident)

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
	p.logger.Info("证据采集完成",
		slog.Int64("task_id", t.ID),
		slog.Int64("incident_id", incidentID),
		slog.Int("collected", applied.TotalCollected),
		slog.Int("kept", kept),
		slog.Bool("truncated", applied.Truncated),
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
		return fmt.Errorf("%w（且标记失败也失败: %v）", cause, err)
	}
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

// logCollectorResults 逐采集器记录结果。
//
// 失败**只记日志不返回**：手册 §4.3 要求单个采集器挂掉不影响其余（§4.5）。
// 但也不能完全静默 —— 「Prometheus 挂了」必须能在日志里看见，
// 否则页面上少了证据却没人知道为什么。
func (p *Processor) logCollectorResults(incidentID int64, results []evidence.CollectorResult) {
	for _, r := range results {
		switch {
		case r.Err != nil:
			p.logger.Warn("采集器失败（其余采集器照常落库）",
				slog.String("source", r.Source),
				slog.Int64("incident_id", incidentID),
				slog.String("error", r.Err.Error()))
		case r.Dropped > 0:
			p.logger.Warn("采集到非法证据已丢弃",
				slog.String("source", r.Source),
				slog.Int64("incident_id", incidentID),
				slog.Int("dropped", r.Dropped),
				slog.String("reason", errText(r.DropReason)))
		}
	}
}

// appendEvidenceEntry 写「采集完成」留痕；失败只记日志。
//
// 留痕失败**不**让任务失败：证据已经落库了，
// 为一条给人看的记录把整轮采集判成失败，用户看到的会是「没有证据」，
// 而实际上证据就在库里 —— 比少一条留痕严重得多。
func (p *Processor) appendEvidenceEntry(
	ctx context.Context,
	incidentID int64,
	applied evidence.Result,
	kept int,
	win evidence.Window,
	results []evidence.CollectorResult,
) {
	if p.timeline == nil {
		return
	}
	sources := make(map[string]int, len(results))
	for _, r := range results {
		sources[r.Source] += len(r.Evidence)
	}
	entry := incident.NewEvidenceFoundEntry(
		incidentID, applied.TotalCollected, kept, applied.Truncated,
		win.Start, win.End, sources, p.nowUTC())
	if err := p.timeline.AppendTimelineEntry(ctx, entry); err != nil {
		p.logger.Warn("证据采集留痕写入失败（不影响证据本身）",
			slog.Int64("incident_id", incidentID),
			slog.String("error", err.Error()))
	}
}

// nowUTC 是取时刻的唯一入口，便于测试注入固定时钟。
func (p *Processor) nowUTC() time.Time {
	if p.now == nil {
		return time.Now().UTC()
	}
	return p.now().UTC()
}

// SetClock 注入时钟（仅测试用）。
func (p *Processor) SetClock(now func() time.Time) {
	p.now = now
}

// errText 把 error 转成可读文本，nil 返回空串。
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
