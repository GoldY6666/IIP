// Command iip-server 是 IIP 的 HTTP API 服务入口。
//
// 提供 /healthz（真查库）与 M1 业务路由：告警 webhook 接入、告警列表查询。
// 规格依据：docs/IIP-M0详细开发计划.md §T0.3 / §T0.4，docs/IIP-M1详细开发手册.md §3。
//
// 额外支持 --healthcheck 自检模式：请求自身的 /healthz 后按结果退出 0/1。
// 该模式供容器 healthcheck 使用——运行层是 distroless，镜像内没有
// sh/curl/wget，唯有二进制自身能充当探针。退出码即健康状态。
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yu/iip/internal/alert"
	"github.com/yu/iip/internal/api"
	"github.com/yu/iip/internal/config"
	"github.com/yu/iip/internal/logging"
	appmw "github.com/yu/iip/internal/middleware"
	"github.com/yu/iip/internal/repository"
	"github.com/yu/iip/internal/store"
)

const (
	readHeaderTimeout = 5 * time.Second
	shutdownTimeout   = 10 * time.Second

	// healthcheckFlag 触发自检模式；healthcheckTimeout 是自检请求的上限。
	healthcheckFlag    = "--healthcheck"
	healthcheckTimeout = 3 * time.Second
)

// exitCodeUnhealthy 是自检失败时的进程退出码（非 0 即不健康）。
const exitCodeUnhealthy = 1

// version 由构建时注入：-ldflags "-X main.version=v0.0-m0"。
// 未注入时保持 "dev"，便于区分本地裸编译与正式镜像。
var version = "dev"

func main() {
	if err := run(); err != nil {
		slog.Error("server 退出", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

// wantsHealthcheck 判断本次启动是否为自检模式。
//
// 同时接受 "--healthcheck" 与 "--healthcheck=true"（pflag 风格），
// 便于在 compose 的 healthcheck.test 里书写。
func wantsHealthcheck(args []string) bool {
	for _, a := range args {
		if a == healthcheckFlag {
			return true
		}
		if strings.HasPrefix(a, healthcheckFlag+"=") {
			v := strings.TrimPrefix(a, healthcheckFlag+"=")
			return v == "" || v == "true" || v == "1"
		}
	}
	return false
}

// runHealthcheck 请求本机 /healthz，按其结果决定退出码。
//
// 注意：自检**不**解析完整配置（不需要 PGURL），只依赖 --http-addr。
// 这样即便配置有问题，探针本身仍能可靠地反映 HTTP 层的真实状态。
func runHealthcheck(args []string) error {
	addr := config.DefaultHTTPAddr
	for _, a := range args {
		if v, ok := strings.CutPrefix(a, "--http-addr="); ok {
			addr = v
		}
	}

	url := "http://" + localURL(addr) + "/healthz"

	ctx, cancel := context.WithTimeout(context.Background(), healthcheckTimeout)
	defer cancel()

	// GET 且无请求体时显式用 http.NoBody：nil 会让标准库无法判断"无 body"
	// 与"body 稍后设置"之别（gocritic httpNoBody）。
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return fmt.Errorf("构造自检请求失败: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("自检请求失败 (%s): %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("自检不健康: %s 返回 HTTP %d", url, resp.StatusCode)
	}
	fmt.Println("healthy")
	return nil
}

// localURL 把监听地址规范成可直连的 "host:port"。
//
// HTTPAddr 通常是 ":8080" 这种"监听所有网卡"的形式，空 host 在
// 发起请求时不合法，需替换为 127.0.0.1。
func localURL(addr string) string {
	host, port, ok := strings.Cut(addr, ":")
	if !ok {
		return addr + ":8080"
	}
	if host == "" {
		host = "127.0.0.1"
	}
	return host + ":" + port
}

func run() error {
	// 0) 自检模式：不加载完整配置，直接探本机 /healthz 后退出。
	if wantsHealthcheck(os.Args[1:]) {
		if err := runHealthcheck(os.Args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, err.Error())
			os.Exit(exitCodeUnhealthy)
		}
		return nil
	}

	// 1) 加载配置
	cfg, err := config.Load(os.Args[1:])
	if err != nil {
		return fmt.Errorf("加载配置失败: %w", err)
	}
	logger, _ := logging.Setup(cfg.LogLevel)

	// 2) 信号上下文（SIGTERM / Interrupt）
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger.Info("iip-server 启动", slog.String("version", version))

	// 3) 连接数据库（含 Ping 快速失败）
	pool, err := store.Connect(ctx, cfg.PGURL)
	if err != nil {
		return fmt.Errorf("连接数据库失败: %w", err)
	}
	defer pool.Close()
	logger.Info("已连接 PostgreSQL")

	// 4) 启动时自动迁移（幂等）
	if err := store.Migrate(ctx, cfg.PGURL); err != nil {
		return fmt.Errorf("执行数据库迁移失败: %w", err)
	}
	logger.Info("数据库迁移完成")

	// 5) 路由装配
	r := newRouter(logger, pool, cfg.WebhookMaxBytes)

	// 6) HTTP 服务
	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           r,
		ReadHeaderTimeout: readHeaderTimeout,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("HTTP 服务启动", slog.String("addr", cfg.HTTPAddr))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	// 7) 优雅退出
	select {
	case <-ctx.Done():
		logger.Info("收到退出信号，开始优雅关闭")
	case err := <-errCh:
		return fmt.Errorf("HTTP 服务异常: %w", err)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("优雅关闭失败: %w", err)
	}
	logger.Info("服务已干净退出")
	return nil
}

// newRouter 装配中间件与路由。业务逻辑不得写入此处（AGENTS.md 约束 2）。
func newRouter(logger *slog.Logger, pool *pgxpool.Pool, webhookMaxBytes int64) http.Handler {
	r := chi.NewRouter()
	r.Use(
		chimw.RequestID,
		chimw.RealIP,
		appmw.AccessLog(logger),
		chimw.Recoverer,
	)

	r.Get("/healthz", handleHealthz(pool))

	// M1：告警接入。此处只有装配——五阶段流水线在 internal/alert，
	// SQL 在 internal/repository，handler 只负责 HTTP 契约（手册 §4.5）。
	alertSvc := alert.NewService(repository.NewAlerts(pool), logger)
	r.Post("/api/v1/alerts/webhook", api.AlertWebhook(alertSvc, webhookMaxBytes, logger))

	// M1：告警列表（手册 §3.1 P1）。只读查询，返回最新 100 条、不做 status 过滤。
	r.Get("/api/v1/alerts", api.AlertList(alertSvc, logger))

	// M2 起：事件（incident）关联与查询，不在 M1 范围（手册 §2 已列明不做）。
	// M2: r.Get("/api/v1/incidents", ...)
	// M2: r.Get("/api/v1/incidents/{id}", ...)

	return r
}

// handleHealthz 真查库（SELECT 1），成功 200 "ok"，失败 503。
func handleHealthz(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		if err := store.HealthCheck(ctx, pool); err != nil {
			slog.Warn("healthz 数据库检查失败", slog.String("error", err.Error()))
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("unavailable"))
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}
}
