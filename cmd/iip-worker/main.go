// Command iip-worker 是 IIP 的后台 Worker 入口（polling 循环）。
//
// M0 阶段 runOnce 仅为 SELECT 1 占位；任务队列在 M4 落地
// （claim tasks ... FOR UPDATE SKIP LOCKED）。
// 规格依据：docs/IIP-M0详细开发计划.md §T0.3 / §T0.5。
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/yu/iip/internal/config"
	"github.com/yu/iip/internal/logging"
	"github.com/yu/iip/internal/store"
)

// dbPinger 是 worker 对数据库的最小依赖（便于单测注入）。
// 与 *pgxpool.Pool 的方法签名完全一致，可直接满足。
type dbPinger interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

const (
	heartbeatInterval = 60 * time.Second // 心跳日志间隔
	drainTimeout      = 15 * time.Second // drain 等待 in-flight 的上限
)

// version 由构建时注入：-ldflags "-X main.version=v0.0-m0"。
// 未注入时保持 "dev"，便于区分本地裸编译与正式镜像。
var version = "dev"

func main() {
	if err := run(); err != nil {
		slog.Error("worker 退出", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(os.Args[1:])
	if err != nil {
		return fmt.Errorf("加载配置失败: %w", err)
	}
	logger, _ := logging.Setup(cfg.LogLevel)
	logger.Info("iip-worker 启动", slog.String("version", version))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := store.Connect(ctx, cfg.PGURL)
	if err != nil {
		return fmt.Errorf("连接数据库失败: %w", err)
	}
	defer pool.Close()
	logger.Info("worker 已连接 PostgreSQL", slog.Duration("poll_interval", cfg.WorkerPoll))

	w := newWorker(logger, pool)
	return w.loop(ctx, cfg.WorkerPoll)
}

// worker 持有 polling 循环的运行态。
type worker struct {
	logger *slog.Logger
	pool   dbPinger

	pollCount int64        // 已完成的轮询次数（原子）
	inFlight  atomic.Int64 // 当前 in-flight 任务数
}

func newWorker(logger *slog.Logger, pool dbPinger) *worker {
	return &worker{logger: logger, pool: pool}
}

// loop 是 polling 主循环。ctx 取消后进入 drain 并返回。
//
// 每轮任务在独立 goroutine 中执行，并计入 in-flight，
// 使 drain 能真实等待正在执行的任务（而非等主循环栈上的串行调用）。
func (w *worker) loop(ctx context.Context, poll time.Duration) error {
	ticks := time.NewTicker(poll)
	defer ticks.Stop()

	beat := time.NewTicker(heartbeatInterval)
	defer beat.Stop()

	w.logger.Info("worker 启动", slog.Duration("poll_interval", poll))

	// 用 WaitGroup 等待所有已派发的任务 goroutine，供 drain 使用。
	var wg sync.WaitGroup

	for {
		select {
		case <-ctx.Done():
			w.drain(&wg)
			return nil
		case <-beat.C:
			w.heartbeat()
		case <-ticks.C:
			// 每次 tick 派发一个任务 goroutine，递增 in-flight。
			w.inFlight.Add(1)
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer w.inFlight.Add(-1)
				w.runOnce(ctx)
				atomic.AddInt64(&w.pollCount, 1)
			}()
		}
	}
}

// runOnce 执行一轮轮询。M0 用 SELECT 1 占位。
//
// 内置 panic recovery：单个任务 panic 不得带崩整个 worker。
func (w *worker) runOnce(ctx context.Context) {
	defer func() {
		if rec := recover(); rec != nil {
			w.logger.Error("runOnce panic 已恢复",
				slog.Any("panic", rec),
				slog.Int64("poll_count", atomic.LoadInt64(&w.pollCount)),
			)
		}
	}()

	// M0 占位：仅验证 DB 连通性。
	// M4: 在此 claim tasks（FOR UPDATE SKIP LOCKED）并分派执行。
	var one int
	if err := w.pool.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
		// ctx 取消导致的失败在 drain 期间属预期，降级为 debug。
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			w.logger.Debug("runOnce 被取消", slog.String("error", err.Error()))
			return
		}
		w.logger.Error("runOnce 查询失败", slog.String("error", err.Error()))
	}
}

// drain 停止接新任务，等待 in-flight 完成，上限 drainTimeout。
func (w *worker) drain(wg *sync.WaitGroup) {
	w.logger.Info("收到退出信号，进入 drain",
		slog.Int64("in_flight", w.inFlight.Load()),
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
		w.logger.Info("drain 完成，worker 已干净退出",
			slog.Int64("poll_count", atomic.LoadInt64(&w.pollCount)),
		)
	case <-time.After(drainTimeout):
		w.logger.Warn("drain 超时，仍有 in-flight 任务未完成",
			slog.Int64("in_flight", w.inFlight.Load()),
		)
	}
}

// heartbeat 每 60s 输出一条存活日志。
func (w *worker) heartbeat() {
	n := atomic.LoadInt64(&w.pollCount)
	w.logger.Info(fmt.Sprintf("worker alive, poll_count=%d", n),
		slog.Int64("poll_count", n),
		slog.Int64("in_flight", w.inFlight.Load()),
	)
}
