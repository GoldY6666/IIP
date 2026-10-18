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

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yu/iip/internal/alert"
	"github.com/yu/iip/internal/api"
	"github.com/yu/iip/internal/config"
	"github.com/yu/iip/internal/correlation"
	"github.com/yu/iip/internal/incident"
	"github.com/yu/iip/internal/logging"
	appmw "github.com/yu/iip/internal/middleware"
	"github.com/yu/iip/internal/notify"
	"github.com/yu/iip/internal/repository"
	"github.com/yu/iip/internal/store"
	"github.com/yu/iip/internal/webui"
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
	r := newRouter(logger, pool, cfg, notifier)

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

// buildNotifier 按配置构造通知派发器；未配置 Slack webhook 时返回 nil。
//
// 只在服务端装配：worker 不产生事件，也就不发通知（它只消费采集任务）。
func buildNotifier(cfg *config.Config, pool *pgxpool.Pool, logger *slog.Logger) *notify.Service {
	if cfg.SlackWebhookURL == "" {
		// 默认形态。明确记一条 INFO 而不是静默：运维要能区分
		// 「没配所以没发」与「配了但发不出去」。
		logger.Info("未配置 Slack webhook，通知功能关闭")
		return nil
	}
	if cfg.UIBaseURL == "" {
		// validateSlack 已允许空值（消息降级为不带深链），这里再提示一次：
		// 没有深链的告警通知，on-call 要多点两次才能到事件页。
		logger.Warn("UIBaseURL 为空，Slack 消息将不带事件深链")
	}
	// 刻意不打印 webhook URL：它是凭据（拿到即可往通道发任意消息）。
	logger.Info("Slack 通知已启用",
		slog.Duration("timeout", cfg.SlackTimeout),
		slog.String("ui_base_url", cfg.UIBaseURL))
	return notify.New(
		[]notify.Sink{notify.NewSlackSink(cfg.SlackWebhookURL, cfg.UIBaseURL, cfg.SlackTimeout)},
		repository.NewIncidents(pool), // CountsProvider：消息里的告警数/证据数
		logger,
	)
}

// newRouter 装配中间件与路由。业务逻辑不得写入此处（AGENTS.md 约束 2）。
func newRouter(logger *slog.Logger, pool *pgxpool.Pool, cfg *config.Config, notifier *notify.Service) http.Handler {
	r := chi.NewRouter()
	r.Use(
		chimw.RequestID,
		// **客户端 IP 来源：ClientIPFromRemoteAddr，不是 RealIP**（2026-10-12）。
		//
		// chi 的 `RealIP` 已被官方标记弃用（SA1019）并点名安全缺陷
		// （GHSA-3fxj-6jh8-hvhx / GHSA-rjr7-jggh-pgcp / GHSA-9g5q-2w5x-hmxf）：
		// 它**无条件**把 r.RemoteAddr 改写成 X-Forwarded-For 最左值、
		// True-Client-IP 或 X-Real-IP —— 无论本进程前面到底有没有代理、
		// 代理有没有覆写这些头。任何客户端都能发一个
		// `X-Forwarded-For: 10.0.0.1` 就把自己的地址写进访问日志。
		//
		// `ClientIPFromRemoteAddr` 只认 TCP 连接对端（fail-closed：
		// 解析不出合法 IP 就不设值，不回退到更低可信的来源），
		// 并把 v4-mapped IPv6（::ffff:a.b.c.d）折叠成纯 v4 —— 否则
		// 攻击者可用两种写法绕过任何基于前缀的可信列表。
		//
		// **为什么这里不用 ClientIPFromXFF**：本项目部署形态是
		// compose 直连（deploy/docker-compose.yml 中 iip-server 直接
		// `8080:8080`，无nginx/traefik 等反代）。没有反代时
		// RemoteAddr 就是真实客户端 IP；一旦哪天真加了反代，
		// 正确改法是显式配置 `ClientIPFromXFF(可信CIDR...)`，
		// **不是**换回 RealIP —— 那正是本条修复要消除的信任假设。
		chimw.ClientIPFromRemoteAddr,
		appmw.AccessLog(logger),
		// S4：静态 token 鉴权。位置在 AccessLog 之后 —— 被拒的请求
		// 也必须留下访问日志，否则「配了 token 后前端全401」这件事
		// 在日志里完全看不见（那是本批最容易踩的排查坑）。
		//
		// 放Recoverer 之前：鉴权本身不 panic，真panic 由 Recoverer兜。
		// AuthToken 为空时中间件直接放行，本地开发与既有用法零影响。
		appmw.RequireToken(cfg.AuthToken),
		// S5：请求超时。位置在 AccessLog **内层**、RequireToken **外层**：
		//
		//  - 在 AccessLog 内层：AccessLog 是在 next 返回后才打日志的，
		//    只有这样它记下的 status 才是最终状态（超时则 504）。
		//    放外面的话，日志里会看到 200 而客户端收到 504 ——
		//    排查时会被彻底带偏。
		//  - 在 RequireToken 外层：鉴权本身是常数时间的字符串比较，不会慢，
		//    但被拒的请求仍应走完整的超时包装，语义统一。
		//
		// 在 Recoverer 之外：panic 由 Recoverer 处理，超时只管「慢」不管「崩」。
		appmw.Timeout(cfg.HTTPTimeout),
		chimw.Recoverer,
	)

	r.Get("/healthz", handleHealthz(pool))

	// S4：前端凭据来源。**必须注册在这里且不受鉴权**（见 api.AuthSession 注释）。
	// 无凭据时也返回 200 + auth_required=false：那是合法状态而非错误。
	r.Get(api.AuthSessionPath(), api.AuthSession(cfg.AuthToken, logger))

	if appmw.AuthEnabled(cfg.AuthToken) {
		// 刻意只说「已启用」不回显 token 值：启动日志常被收集到集中式日志系统，
		// 那里不该出现凭据。是否真生效由容器链路的 401/200 两条断言验。
		logger.Info("UI 访问鉴权已启用，受保护路径前缀 /api/v1/")
	}

	// 关联引擎（M2）：拓扑 + 配置仓储 + 服务。此处只装配，评分/决策在 internal/correlation。
	//
	// 三分支改用 switch-true 而非 if/else-if/else 链（gocritic ifElseChain）。
	// case 按序求值，故 err != nil 命中时不会再去调 topo.Size()（拓扑可能为 nil）。
	topo, err := correlation.LoadTopology(cfg.TopologyFile)
	switch {
	case err != nil:
		// 拓扑解析失败不应让服务起不来：记 WARN 并以空拓扑运行（依赖维恒 0）。
		logger.Warn("加载服务拓扑失败，依赖相似度将恒为 0",
			slog.String("error", err.Error()),
			slog.String("topology_file", cfg.TopologyFile))
		topo = &correlation.Topology{}
	case topo.Size() == 0:
		logger.Warn("服务拓扑为空，依赖相似度将恒为 0",
			slog.String("topology_file", cfg.TopologyFile))
	default:
		// 成功路径也给一条正向日志（M2 起只有失败才出声）：
		// 否则「拓扑到底加载上没有」只能靠「没看到 WARN」反推，
		// 验收断言也就只能写成反证。有这条 INFO，容器链路可以正向断言 services>0。
		logger.Info("服务拓扑已加载",
			slog.Int("services", topo.Size()),
			slog.String("topology_file", cfg.TopologyFile))
	}

	cfgStore := repository.NewCorrelationConfig(pool)
	// M3：给关联路径挂上「时间线留痕」与「REVIEW 抑制」两个可选能力。
	// 两者都在 correlation 内部做成 best-effort（失败只记日志），
	// 因此这里的注入不会让 webhook 的成功与否取决于它们。
	lifecycle := repository.NewLifecycle(pool)
	// M4：挂上「证据采集入队」。同样 best-effort ——
	// 入队失败只记日志，绝不让 webhook 失败（否则 Alertmanager 会重投整批告警）。
	//
	// 挂在 iip-server 而不是 iip-worker：事件在这里创建/合入，
	// 队列是入库的同一批事务外动作，而 worker 只消费不生产。
	// 若把入队放到 worker 里，就得让 worker 反查「哪些事件还没采过」，
	// 那是把「谁创建了事件」这个事实藏进轮询逻辑里。
	corrSvc := correlation.NewService(repository.NewIncidents(pool), cfgStore, topo, logger).
		WithTimeline(lifecycle).
		WithSuppressions(repository.NewReviews(pool)).
		WithEvidenceTasks(repository.NewTasks(pool))
	// M6 T6.1：NEW 档建档成功后发通知。nil 时不注入（默认形态），
	// 关联路径与 M2 完全一致。
	if notifier != nil {
		corrSvc = corrSvc.WithNotifier(notifier)
	}

	// M1：告警接入。此处只有装配——五阶段流水线在 internal/alert，
	// SQL 在 internal/repository，handler 只负责 HTTP 契约（手册 §4.5）。
	alertSvc := alert.NewService(repository.NewAlerts(pool), logger)
	// M2：落库后同步关联（手册 §4.10）。开关关闭时不注入，行为退回 M1。
	if cfg.CorrelationEnabled {
		alertSvc.SetCorrelator(corrSvc)
	}
	r.Post("/api/v1/alerts/webhook", api.AlertWebhook(alertSvc, cfg.WebhookMaxBytes, logger))

	// M1：告警列表（手册 §3.1 P1）。只读查询，返回最新 100 条、不做 status 过滤。
	r.Get("/api/v1/alerts", api.AlertList(alertSvc, logger))

	// M2：关联配置读写（手册 §4.4）。改后免重启即生效。
	r.Get("/api/v1/correlation/config", api.CorrelationConfigGet(cfgStore, logger))
	r.Put("/api/v1/correlation/config", api.CorrelationConfigPut(cfgStore, logger))

	// M3：事件查询与生命周期（手册 §4.3）。装配处只连线，业务在 internal/incident，
	// SQL 在 internal/repository —— 与 M1/M2 的分层一致。
	incidentSvc := incident.NewService(lifecycle)
	// M6 T6.1：状态流转提交成功后发通知。与关联侧同一实例、同一队列。
	if notifier != nil {
		incidentSvc = incidentSvc.WithNotifier(notifier)
	}
	r.Get("/api/v1/incidents", api.IncidentList(incidentSvc, logger))
	r.Get("/api/v1/incidents/{id}", api.IncidentDetail(incidentSvc, logger))
	r.Post("/api/v1/incidents/{id}/transition", api.IncidentTransition(incidentSvc, logger))
	r.Post("/api/v1/incidents/{id}/comments", api.IncidentComment(incidentSvc, logger))

	// M3：REVIEW 队列与人工复核（手册 §4.5）。
	r.Get("/api/v1/reviews", api.ReviewList(incidentSvc, logger))
	r.Post("/api/v1/reviews/{incidentID}/{alertID}", api.ReviewDecide(incidentSvc, logger))

	// M4：证据查询（手册 T4.6 / §4.2）。挂在事件路由下而不是独立的
	// /api/v1/evidence —— 证据必须挂在一个事件上才有意义
	//（evidence 表 incident_id 是 NOT NULL 外键），而路径里带上事件 id
	// 也让「这是谁的证据」不用看 body 就知道。
	evidenceRepo := repository.NewEvidence(pool)
	r.Get("/api/v1/incidents/{id}/evidence", api.EvidenceList(evidenceRepo, logger))

	// M5 S3：前端静态托管（手册 §4.3）。注册细节在 internal/webui 内部，
	// 这里只调一次 —— 顺序要求（必须最后）由该包自己保证，
	// 不留在本文件里以免将来被误改。
	webui.Register(r, cfg.WebRoot, webui.UIConfig{
		Prometheus:   cfg.UIPrometheusURL,
		Grafana:      cfg.UIGrafanaURL,
		Loki:         cfg.UILokiURL,
		LokiTenantID: cfg.UILokiTenantID,
	}, logger)

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
