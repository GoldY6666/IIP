package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// taskRunner 是 poller 对执行层的最小依赖。
//
// 抽成接口而不是直接持有 *worker.Processor：
// drain 与 panic recovery 是 polling 循环自身的机制（手册 §六 B8 只要求
// 装配，不要求这两项重测），而它们与「怎么采集证据」毫无关系。
// 打成接口后，这两条用例能用假实现精确控制「任务在跑 / 任务 panic」，
// 不必为了测 drain 去造一个真的 Processor + 数据库。
type taskRunner interface {
	// RunOnce 执行一轮，返回处理的任务数与遇到的第一个错误。
	RunOnce(ctx context.Context) (int, error)
}

// maintainRunner 是 poller 对分区维护层的最小依赖（T7.1）。
//
// 与 taskRunner 同思路：维护 tick 的用例能用假实现精确控制
// 「维护成功 / 失败 / 补建了几个分区」，不必连真库。
type maintainRunner interface {
	// RunMaintenance 执行一轮完整维护（补建分区 + 过期清理 + 重复扫描）。
	RunMaintenance(ctx context.Context) error
	// Ensure 只补建分区（worker 启动时调用，保证插入路径不撞 23514）。
	Ensure(ctx context.Context) ([]string, error)
}

// poller 持有 polling 循环的运行态。
//
// 命名说明：M0 时本类型叫 worker，与 M4 新增的 internal/worker 包同名冲突。
// 改叫 poller 如实反映它的职责 —— 它只管「按节奏调执行器」，
// 不再自己实现任何业务（原 M0 的 SELECT 1 占位已由 Processor 取代）。
//
// 刻意**不再持有 dbPinger**：M0 靠它做 SELECT 1 连通性检查，
// M4 的连通性由 Processor 领取任务时验证（领取失败即已记日志），
// 留着一个再也不用的字段只会让人以为「worker 不用 DB」。
type poller struct {
	logger *slog.Logger
	proc   taskRunner

	// maint 为 nil 或 maintInterval <= 0 时不跑分区维护 tick
	// （配置校验已拦 <=0；判 nil 是为了让「不传维护器」的用法——
	// 如只测循环机制的用例——不必伪造间隔）。
	maint         maintainRunner
	maintInterval time.Duration

	pollCount int64        // 已完成的轮询次数（原子）
	inFlight  atomic.Int64 // 当前 in-flight 任务数
}

func newPoller(logger *slog.Logger, proc taskRunner, maint maintainRunner, maintInterval time.Duration) *poller {
	return &poller{logger: logger, proc: proc, maint: maint, maintInterval: maintInterval}
}

// loop 是 polling 主循环。ctx 取消后进入 drain 并返回。
//
// 每轮任务在独立 goroutine 中执行，并计入 in-flight，
// 使 drain 能真实等待正在执行的任务（而非等主循环栈上的串行调用）。
func (p *poller) loop(ctx context.Context, poll time.Duration) error {
	ticks := time.NewTicker(poll)
	defer ticks.Stop()

	beat := time.NewTicker(heartbeatInterval)
	defer beat.Stop()

	// 分区维护 tick（T7.1）：与任务轮询不同频率（默认 1h vs 2s），故独立
	// ticker。nil channel 在 select 里恒不触发——这是「不配维护器就
	// 永远不进该分支」的最直白实现，不需要额外的 if 包裹每个 case。
	var maintTicks <-chan time.Time
	if p.maint != nil && p.maintInterval > 0 {
		mt := time.NewTicker(p.maintInterval)
		defer mt.Stop()
		maintTicks = mt.C
	}

	p.logger.Info("worker 启动", slog.Duration("poll_interval", poll))

	// 用 WaitGroup 等待所有已派发的任务 goroutine，供 drain 使用。
	var wg sync.WaitGroup

	for {
		select {
		case <-ctx.Done():
			p.drain(&wg)
			return nil
		case <-beat.C:
			p.heartbeat()
		case <-maintTicks:
			p.runMaintenance(ctx)
		case <-ticks.C:
			// 每次 tick 派发一个任务 goroutine，递增 in-flight。
			//
			// 传的是 **WithoutCancel** 的 ctx：收到 SIGTERM 时，
			// 直接把 signal ctx 传下去会让在跑的 HTTP 采集请求**立刻中断**，
			// 于是 drain 永远等不到东西、drainTimeout 形同虚设，
			// 而这一轮采到一半的证据要等 stale_after（默认 5 分钟）
			// 回收后才有人重采 —— 运维刚要重启反而丢了证据。
			//
			// WithoutCancel 保留 ctx 的值（trace id 等）但切断取消信号，
			// 真正兜住执行时长的是两个已有的超时：
			// 采集器 5s（EvidenceTimeout）+ drain 15s。
			workCtx := context.WithoutCancel(ctx)
			p.inFlight.Add(1)
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer p.inFlight.Add(-1)
				p.runOnce(workCtx)
				atomic.AddInt64(&p.pollCount, 1)
			}()
		}
	}
}

// runOnce 执行一轮轮询：领取采集任务并逐条执行。
//
// 内置 panic recovery：单个任务 panic 不得带崩整个 worker。
func (p *poller) runOnce(ctx context.Context) {
	defer func() {
		if rec := recover(); rec != nil {
			p.logger.Error("runOnce panic 已恢复",
				slog.Any("panic", rec),
				slog.Int64("poll_count", atomic.LoadInt64(&p.pollCount)),
			)
		}
	}()

	// ctx 取消导致的失败在 drain 期间属预期，降级为 debug。
	n, err := p.proc.RunOnce(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			p.logger.Debug("runOnce 被取消", slog.String("error", err.Error()))
			return
		}
		// 任务级失败已在 Processor 内逐条记过日志；这里只记一条汇总，
		// 避免同一个错误在日志里出现 N 次。
		p.logger.Error("本轮采集有任务失败", slog.String("error", err.Error()))
	}
	if n > 0 {
		p.logger.Info("本轮处理任务数", slog.Int("count", n))
	}
}

// runMaintenance 派发一轮分区维护（T7.1），自带 panic recovery。
//
// 与 runOnce 的三点同构，理由也同构：
//   - 独立 goroutine：维护（DELETE 大批过期行）不得阻塞任务轮询主循环；
//   - panic recovery：维护逻辑的 bug 不得带崩 worker；
//   - WithoutCancel：SIGTERM 时在跑的维护不被打断——它幂等且周期重来，
//     真正兜住时长的是 Maintainer 内部各查询自身的超时。
//
// 失败只记 Error 不退出：维护是旁路职责。但绝不静默——
// 补建失败会让插入路径持续撞 23514，日志是唯一线索。
func (p *poller) runMaintenance(ctx context.Context) {
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				p.logger.Error("runMaintenance panic 已恢复", slog.Any("panic", rec))
			}
		}()
		if err := p.maint.RunMaintenance(context.WithoutCancel(ctx)); err != nil {
			p.logger.Error("分区维护失败", slog.String("error", err.Error()))
		}
	}()
}

// drain 停止接新任务，等待 in-flight 完成，上限 drainTimeout。
func (p *poller) drain(wg *sync.WaitGroup) {
	p.logger.Info("收到退出信号，进入 drain",
		slog.Int64("in_flight", p.inFlight.Load()),
		slog.Duration("timeout", drainTimeout),
	)

	// 用 WaitGroup + 超时 channel 实现：等全部 in-flight 完成或超时。
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		p.logger.Info("drain 完成，worker 已干净退出",
			slog.Int64("poll_count", atomic.LoadInt64(&p.pollCount)),
		)
	case <-time.After(drainTimeout):
		p.logger.Warn("drain 超时，仍有 in-flight 任务未完成",
			slog.Int64("in_flight", p.inFlight.Load()),
		)
	}
}

// heartbeat 每 60s 输出一条存活日志。
func (p *poller) heartbeat() {
	n := atomic.LoadInt64(&p.pollCount)
	p.logger.Info(fmt.Sprintf("worker alive, poll_count=%d", n),
		slog.Int64("poll_count", n),
		slog.Int64("in_flight", p.inFlight.Load()),
	)
}
