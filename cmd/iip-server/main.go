// Command iip-server 是 IIP 的 HTTP API 服务入口。
//
// 提供 /healthz（真查库）与业务路由：告警 webhook 接入、告警列表查询（M1）、
// 关联配置读写（M2）。
// 规格依据：docs/IIP-M0详细开发计划.md §T0.3 / §T0.4，
// docs/IIP-M1详细开发手册.md §3，docs/IIP-M2详细开发手册.md §4.9 / §4.10。
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

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yu/iip/internal/config"
	"github.com/yu/iip/internal/logging"
	"github.com/yu/iip/internal/metrics"
	"github.com/yu/iip/internal/notify"
	"github.com/yu/iip/internal/repository"
	"github.com/yu/iip/internal/store"
)

const (
	readHeaderTimeout = 5 * time.Second
	shutdownTimeout   = 10 * time.Second

	// notifyCloseTimeout 是关闭时排空通知队列的上限。
	// 比 sink 自身的超时（默认 5s）略长，给「最后几条」留出生效空间；
	// 超时也不影响已完成的优雅关闭——通知本就是 best-effort。
	notifyCloseTimeout = 8 * time.Second

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

	// 4.5) 通知派发（M6 T6.1）。未配置 webhook 时返回 nil —— 两个消费包
	// 对 nil 安全（notify.Service 的方法本身也 nil-safe），故下游无需分支。
	//
	// 关闭顺序由 defer 的 LIFO 保证：本 defer 晚于 pool.Close 注册，
	// 因此先于 pool.Close 执行；而 srv.Shutdown 是显式调用，也在它之前。
	// 即 srv.Shutdown → notifier.Close → pool.Close（ADR-024）。
	notifier := buildNotifier(cfg, pool, logger)
	if notifier != nil {
		defer func() {
			closeCtx, cancel := context.WithTimeout(context.Background(), notifyCloseTimeout)
			defer cancel()
			if err := notifier.Close(closeCtx); err != nil {
				logger.Warn("通知队列排空超时，仍有少量通知未发出", slog.String("error", err.Error()))
			}
		}()
	}

	// 5) 路由装配
	//
	// T6.3：指标集合在装配期构造一次（不是懒加载）。它与 /metrics 是否暴露
	// 无关 —— 关掉端点只是不注册路由，埋点照常计数，这样「先跑一阵再打开
	// 端点」能立刻看到历史累计（进程内自启动起算），而不是从打开那一刻起归零。
	m := metrics.New()
	r := newRouter(logger, pool, cfg, notifier, m)

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

// buildNotifier 按配置构造通知派发器；一个出口都没配时返回 nil。
//
// 两个出口相互独立：Slack（给人看的文本）与通用 webhook（给程序看的 JSON），
// 可以只开一个、也可以同时开。共用同一个异步队列与同一套去风暴。
//
// 只在服务端装配：worker 不产生事件，也就不发通知（它只消费采集任务）。
func buildNotifier(cfg *config.Config, pool *pgxpool.Pool, logger *slog.Logger) *notify.Service {
	var sinks []notify.Sink

	if cfg.SlackWebhookURL != "" {
		if cfg.UIBaseURL == "" {
			// validateSlack 已允许空值（消息降级为不带深链），这里再提示一次：
			// 没有深链的告警通知，on-call 要多点两次才能到事件页。
			logger.Warn("UIBaseURL 为空，Slack 消息将不带事件深链")
		}
		// 刻意不打印 webhook URL：它是凭据（拿到即可往通道发任意消息）。
		logger.Info("Slack 通知已启用",
			slog.Duration("timeout", cfg.SlackTimeout),
			slog.String("ui_base_url", cfg.UIBaseURL))
		sinks = append(sinks, notify.NewSlackSink(cfg.SlackWebhookURL, cfg.UIBaseURL, cfg.SlackTimeout))
	}

	if cfg.NotifyWebhookURL != "" {
		// 同样不打印 URL：内网接收端地址可能带密钥或暴露拓扑。
		// 只报「带不带鉴权」——这决定接收端是否要校验 Authorization。
		logger.Info("通用 webhook 通知已启用",
			slog.Duration("timeout", cfg.NotifyWebhookTimeout),
			slog.Bool("auth", cfg.NotifyWebhookToken != ""))
		sinks = append(sinks, notify.NewGenericSink(cfg.NotifyWebhookURL, cfg.NotifyWebhookToken, cfg.NotifyWebhookTimeout))
	}

	if len(sinks) == 0 {
		// 默认形态。明确记一条 INFO 而不是静默：运维要能区分
		// 「没配所以没发」与「配了但发不出去」。
		logger.Info("未配置任何通知出口，通知功能关闭")
		return nil
	}
	return notify.New(sinks, repository.NewIncidents(pool), logger) // CountsProvider：消息里的告警数/证据数
}
