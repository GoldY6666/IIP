// Command iip-worker 是 IIP 的后台 Worker 入口（polling 循环）。
//
// M0 阶段 runOnce 仅为 SELECT 1 占位；M4 起改为「领取采集任务 → 调采集器 →
// 证据落库」（手册 §六 B8）。执行逻辑在 internal/worker，本文件只做装配。
//
// 规格依据：docs/IIP-M0详细开发计划.md §T0.3 / §T0.5；
// M4 部分见 docs/IIP-M4详细开发手册.md §4.3 / §4.5 / §4.6。
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/yu/iip/internal/config"
	"github.com/yu/iip/internal/db"
	"github.com/yu/iip/internal/logging"
	"github.com/yu/iip/internal/metrics"
	"github.com/yu/iip/internal/partition"
	"github.com/yu/iip/internal/repository"
	"github.com/yu/iip/internal/store"
	"github.com/yu/iip/internal/worker"
)

const (
	heartbeatInterval = 60 * time.Second // 心跳日志间隔
	drainTimeout      = 15 * time.Second // drain 等待 in-flight 的上限

	// metricsShutdownTimeout 是关闭指标监听的上限（T6.3）。
	// 取 3s：它只服务 Prometheus 抓取，没有业务请求，
	// 快速关掉不会丢任何东西。
	metricsShutdownTimeout = 3 * time.Second
	// metricsReadHeaderTimeout 是指标端点的读头超时。
	// 只读端点也该有上限：没有它，一个只发一半头的连接能一直占着。
	metricsReadHeaderTimeout = 5 * time.Second
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

	registry, err := buildRegistry(cfg, logger)
	if err != nil {
		return fmt.Errorf("装配采集器失败: %w", err)
	}
	proc := worker.NewProcessor(
		repository.NewTasks(pool),
		repository.NewEvidence(pool),
		repository.NewCollectTargets(pool),
		registry,
		repository.NewLifecycle(pool),
		worker.Config{
			Batch:          cfg.WorkerTaskBatch,
			StaleAfter:     cfg.WorkerTaskStaleAfter,
			MaxAttempts:    cfg.WorkerMaxAttempts,
			MaxPerIncident: cfg.EvidenceMaxPerIncident,
			WindowWidth:    cfg.EvidenceWindow,
			ClaimedBy:      instanceID(),
		},
		logger,
	)

	// T6.3：自监控指标。worker 没有 HTTP 服务，故单独监听一个只读端口
	// （理由见 internal/config/metrics.go 的 DefaultWorkerMetricsAddr）。
	//
	// 起不来只 WARN 不退出：指标是旁路能力，让「监控端口被占」
	// 把采集进程拖死是方向反了 —— 那时连证据都不采了。
	m := metrics.New()
	proc.SetMetrics(m)
	if srv := startWorkerMetrics(logger, cfg.WorkerMetricsAddr, m); srv != nil {
		defer shutdownWorkerMetrics(srv)
	}

	// T7.1（ADR-022）：分区维护器。挂在 worker 而不是 server：server 是
	// 请求路径（webhook 插入有 23514 重试兜底），补建/清理这类周期性
	// 旁路职责应由后台进程承担。三个参数全部来自配置，默认值见 config。
	maint, err := partition.NewMaintainer(
		db.New(pool), logger,
		cfg.PartitionAheadMonths, cfg.AlertRetentionDays)
	if err != nil {
		return fmt.Errorf("装配分区维护器失败: %w", err)
	}

	// 启动即补建一次分区：server 是独立进程、不依赖 worker 也可能已在插入，
	// 不能等第一个维护 tick（默认 1h 后）。失败不致命——插入路径有 23514
	// 重试兜底——但必须留痕：「知道维护没成」与「以为维护成了」
	// 在日志里是唯一可区分的线索。
	if created, err := maint.Ensure(ctx); err != nil {
		logger.Error("启动时补建 alerts 分区失败（插入路径有 23514 重试兜底，维护 tick 会重试）",
			slog.String("error", err.Error()))
	} else if len(created) > 0 {
		logger.Info("启动时已补建 alerts 分区", slog.Any("partitions", created))
	}

	p := newPoller(logger, proc, maint, cfg.PartitionMaintenanceInterval)
	return p.loop(ctx, cfg.WorkerPoll)
}
