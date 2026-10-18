// 路由与中间件装配（从 main.go 拆出，OPEN-M5-7 / OPEN-M6-1）。
//
// 拆分的判据是**行数**，不是「架构更优雅」：`cmd/iip-server/main.go` 曾涨到 485 行，
// 而项目约束是单文件 ≤ 300 行（`docs/IIP-M1详细开发手册.md` §4）。装配代码集中在
// 本文件后，main.go 只留「起进程、等信号、优雅退出」那条主干。
//
// 纪律不变：业务逻辑不得写入此处（AGENTS.md 约束 2）。
package main

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yu/iip/internal/alert"
	"github.com/yu/iip/internal/api"
	"github.com/yu/iip/internal/config"
	"github.com/yu/iip/internal/correlation"
	"github.com/yu/iip/internal/incident"
	"github.com/yu/iip/internal/metrics"
	appmw "github.com/yu/iip/internal/middleware"
	"github.com/yu/iip/internal/notify"
	"github.com/yu/iip/internal/repository"
	"github.com/yu/iip/internal/store"
	"github.com/yu/iip/internal/webui"
)

// newRouter 装配中间件与路由。业务逻辑不得写入此处（AGENTS.md 约束 2）。
func newRouter(
	logger *slog.Logger,
	pool *pgxpool.Pool,
	cfg *config.Config,
	notifier *notify.Service,
	m *metrics.Metrics,
) http.Handler {
	if m == nil {
		m = metrics.Noop()
	}
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

	// T6.3：自监控端点。与 /healthz 同级，**不受 RequireToken 保护** ——
	// 那个中间件只拦 /api/v1/* 前缀，本路径天然在保护范围之外。
	//
	// 为什么刻意不要求鉴权：Prometheus 抓取时不会带业务 token，
	// 要求鉴权等于「要么抓不到，要么把 token 写进抓取配置」（后者会把
	// 一个业务凭据扩散到监控系统，反而多出一处泄漏面）。
	// 而指标本身不含告警内容：标签全是固定枚举，没有 service/incident_id
	// 这类业务值（见 internal/metrics 的基数约束）。
	//
	// 仍需提醒：暴露到公网时应在反代限制来源网段 —— 进程数、事件速率
	// 这类量级信息本身也是侦察素材。README 的部署段已写明。
	if cfg.MetricsEnabled {
		// r.Handle 而非 r.Get：promhttp 返回 http.Handler 而不是 HandlerFunc。
		r.Handle("/metrics", m.Handler())
		logger.Info("自监控端点已启用", slog.String("path", "/metrics"))
	} else {
		// 打日志而不是静默：/metrics 404 时「关了」与「路由没注册上」
		// 在外部看起来一模一样，启动日志是唯一的区分线索。
		logger.Info("自监控端点已关闭，/metrics 将返回 404",
			slog.String("env", "IIP_METRICS_ENABLED"))
	}

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
		WithEvidenceTasks(repository.NewTasks(pool)).
		WithMetrics(m)
	// M6 T6.1：NEW 档建档成功后发通知。nil 时不注入（默认形态），
	// 关联路径与 M2 完全一致。
	if notifier != nil {
		corrSvc = corrSvc.WithNotifier(notifier)
	}

	// M1：告警接入。此处只有装配——五阶段流水线在 internal/alert，
	// SQL 在 internal/repository，handler 只负责 HTTP 契约（手册 §4.5）。
	alertSvc := alert.NewService(repository.NewAlerts(pool), logger)
	alertSvc.SetMetrics(m)
	// M2：落库后同步关联（手册 §4.10）。开关关闭时不注入，行为退回 M1。
	if cfg.CorrelationEnabled {
		alertSvc.SetCorrelator(corrSvc)
	}
	// T6.3：webhook 的端到端耗时。挂在这一个路由上而不是全局中间件 ——
	// 其余端点都是毫秒级只读查询，混进同一张直方图会把 webhook 的 P99 稀释掉。
	r.With(appmw.WebhookMetrics(m)).
		Post("/api/v1/alerts/webhook", api.AlertWebhook(alertSvc, cfg.WebhookMaxBytes, logger))

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
