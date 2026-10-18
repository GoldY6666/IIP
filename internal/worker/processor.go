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
	"fmt"
	"log/slog"
	"time"

	"github.com/yu/iip/internal/evidence"
	"github.com/yu/iip/internal/incident"
	"github.com/yu/iip/internal/metrics"
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

	// metrics 永不为 nil（未注入时为 metrics.Noop()），见 SetMetrics。
	metrics *metrics.Metrics

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
		metrics:   metrics.Noop(),
	}
}

// SetMetrics 注入自监控指标（M6 T6.3）。
//
// 用 setter 而不是加构造参数：NewProcessor 已有 7 个参数，再塞一个会让
// 每个既有调用点与测试都得改，而指标是纯旁路能力，本不该影响装配形态。
// 传 nil 兜底为 Noop —— 否则下一次 RunOnce 就是 nil 解引用。
func (p *Processor) SetMetrics(m *metrics.Metrics) {
	if m == nil {
		m = metrics.Noop()
	}
	p.metrics = m
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
		// 回收条数按「本次回收了几个任务」累加，不是按「回收了几次」：
		// 一次 tick 里回收 12 个与回收 1 个是完全不同的信号
		//（前者说明有批 worker 一起崩了），而这个计数正是用来看这个的。
		p.metrics.TaskRetry.WithLabelValues(metrics.RetryStaleReclaim).Add(float64(n))
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
