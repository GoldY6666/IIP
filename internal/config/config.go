// Package config 负责加载并校验 IIP 运行时配置。
//
// 来源优先级（高 → 低）：
//
//	命令行 flag  >  环境变量  >  config.local.yaml（可选）  >  内置默认值
//
// 规格依据：docs/IIP-M0详细开发计划.md §T0.3。
package config

import "time"

// 环境变量前缀。所有 IIP_* 变量映射为 koanf 小写键（去掉 IIP_ 前缀）。
const envPrefix = "IIP_"

// 本地可选配置文件路径（进 .gitignore）。
const localConfigPath = "config.local.yaml"

// Config 是 IIP server / worker 的运行时配置。
//
// 注意：这些字段与数据库中 6 张表无直接关系，仅描述进程运行参数。
type Config struct {
	HTTPAddr   string        // IIP_HTTP_ADDR，默认 ":8080"
	PGURL      string        // IIP_PG_URL，必填（无默认）
	LogLevel   string        // IIP_LOG_LEVEL，默认 "info"
	WorkerPoll time.Duration // IIP_WORKER_POLL_INTERVAL，默认 2s
	DBPollOnly bool          // IIP_REDIS_DISABLED，V1.0 默认 true（DB polling 模式）

	// WebhookMaxBytes 是告警 webhook 请求体上限（字节），超限返回 413。
	//
	// 必须有上限：该端点对外暴露，没有上限时一个超大请求体就能把服务内存吃满。
	// 规格依据：docs/IIP-M1详细开发手册.md §4.7。
	WebhookMaxBytes int64 // IIP_WEBHOOK_MAX_BYTES，默认 1048576（1 MiB）

	// TopologyFile 是服务依赖拓扑文件路径（M2 的 DependencySimilarity 用）。
	// 文件不存在不报错，依赖维退化为恒 0。规格依据：docs/IIP-M2详细开发手册.md §4.5。
	TopologyFile string // IIP_TOPOLOGY_FILE，默认 "config/topology.yaml"

	// AuthToken 是访问 /api/v1/* 所需的静态 token（M6 S4）。
	//
	// **空串= 不开鉴权**，这是刻意设计而非「漏配」：
	// 本地开发与 `docker compose --profile app up` 的现有用法必须零改动可用，
	// 而判定「是不是漏配」需要猜用户的意图 —— 猜错的后果要么是 UI 打不开，
	// 要么是部署到内网却以为有鉴权。因此 Validate 里**没有**本字段的必填规则。
	//
	// 它是**明文**环境变量（不是文件、不是密钥管理），与手册 §八裁定#1 一致：
	// 只做「环境变量注入静态 token」，不做登录页与密钥轮换。
	// 它的定位是「挡住误暴露到公网的裸部署」，不是「抵御已能读到进程环境的人」。
	//
	// 只保护 /api/v1/*，**不保护 /healthz** —— 否则容器的 healthcheck
	// 与外部探针都会因缺 token 而判定服务不健康（详见 RequireToken）。
	AuthToken string // IIP_AUTH_TOKEN，默认 ""（不开鉴权）

	// CorrelationEnabled 是 M2 关联引擎总开关；false 时 webhook 落库后不关联。
	CorrelationEnabled bool // IIP_CORRELATION_ENABLED，默认 true

	// --- M4：任务队列（手册 §4.1 / §4.6）---

	// WorkerTaskBatch 是单次轮询最多领取几条任务。
	WorkerTaskBatch int // IIP_WORKER_TASK_BATCH，默认 10
	// WorkerTaskStaleAfter 是 in_flight 多久算「持有者已消失」可被回收。
	WorkerTaskStaleAfter time.Duration // IIP_WORKER_TASK_STALE_AFTER，默认 5m
	// WorkerMaxAttempts 是超过即进死信的尝试次数。
	WorkerMaxAttempts int // IIP_WORKER_MAX_ATTEMPTS，默认 3

	// --- M4：证据采集（手册 §4.6）---

	// EvidencePromEnabled 控制 Prometheus 采集器是否注册，默认 true。
	EvidencePromEnabled bool // IIP_EVIDENCE_PROM_ENABLED
	// EvidenceLokiEnabled 控制 Loki 采集器是否注册，默认 true。
	EvidenceLokiEnabled bool // IIP_EVIDENCE_LOKI_ENABLED
	// EvidenceK8sEnabled 控制 K8s 采集器是否注册，**默认 false**。
	//
	// 默认关闭是刻意的（手册 §三偏差 B1）：本仓库零K8s 依赖，
	// 写一个接不上任何真实集群的采集器比不写更危险 —— 它看起来在工作，
	// 但永远采不到东西。要接真实集群是部署侧的事（用户自带 kubeconfig）。
	EvidenceK8sEnabled bool // IIP_EVIDENCE_K8S_ENABLED
	// EvidenceTraceEnabled 控制 trace 采集器是否注册，**默认 false**。
	//
	// 与 K8s 同为「默认关」，但理由不同：K8s 是本仓库零依赖；
	// trace 是**协议通用**（采集器只依赖 Jaeger 兼容的 /api/traces 形状），
	// 而本机没有任何真实链路后端可验。默认关是为了不声称已接入
	// （M6 §5 降级裁定）。接入同样是部署侧的事。
	EvidenceTraceEnabled bool // IIP_EVIDENCE_TRACE_ENABLED
	// EvidenceMaxPerIncident 是单事件证据上限，超限截断并标记。
	EvidenceMaxPerIncident int // IIP_EVIDENCE_MAX_PER_INCIDENT，默认 50
	// EvidenceWindow 是采集时间窗口。
	EvidenceWindow time.Duration // IIP_EVIDENCE_WINDOW，默认 1h

	// PromURL 是 Prometheus 的查询端点根地址（形如 http://prom:9090）。
	PromURL string // IIP_PROM_URL
	// LokiURL 是 Loki 的查询端点根地址。
	LokiURL string // IIP_LOKI_URL
	// K8sURL 是 K8s API Server 根地址（仅在 EvidenceK8sEnabled 时必填）。
	K8sURL string // IIP_K8S_URL
	// K8sNamespace 是 K8s 采集器要查的命名空间（仅在 EvidenceK8sEnabled 时必填）。
	//
	// 刻意**不支持**一次查多个命名空间或留空代表全集群：K8s 的
	// events / deployments 权限按命名空间授权，跨命名空间会稳定 403，
	// 而 403 与「命名空间不存在」的 404 在排查时几乎无法区分。
	// 要求用户明确给出，反而能第一时间暴露「权限没给对命名空间」。
	K8sNamespace string // IIP_K8S_NAMESPACE
	// TraceURL 是链路追踪后端根地址（仅在 EvidenceTraceEnabled 时必填）。
	TraceURL string // IIP_TRACE_URL
	// EvidenceTimeout 是单个采集器的超时上限（手册 §4.5：三类均为 5s）。
	EvidenceTimeout time.Duration // IIP_EVIDENCE_TIMEOUT，默认 5s

	// --- T7.1：alerts 月度分区维护（ADR-022，见 migrations/0008）---

	// AlertRetentionDays 是告警保留天数（按 starts_at 计）。
	// worker 维护 tick 删除更早的行，并级联清理 incident_alerts 链接。
	AlertRetentionDays int // IIP_ALERT_RETENTION_DAYS，默认 90

	// PartitionAheadMonths 是预创建的未来月分区数（不含当月）。
	// 0 = 只建当月（配合插入路径 23514 重试仍可用，但失去「时钟漂移裕度」）。
	PartitionAheadMonths int // IIP_PARTITION_AHEAD_MONTHS，默认 2

	// PartitionMaintenanceInterval 是分区维护 tick 间隔：
	// 补建未来分区 + 过期清理 + 重复 firing 扫描。
	PartitionMaintenanceInterval time.Duration // IIP_PARTITION_MAINTENANCE_INTERVAL，默认 1h

	// --- T6.1：Slack 通知（M6 §10，ADR-024，实现见 internal/notify）---

	// SlackWebhookURL 是 Slack Incoming Webhook 地址。**空 = 关闭通知**。
	//
	// 沿用 ADR-017 的纪律：默认关闭，且「配了值必须真的能发」——
	// 配了却因 URL 畸形发不出去，要在启动期就拦下（见 validateSlack），
	// 而不是等第一条告警静默丢失。
	SlackWebhookURL string // IIP_SLACK_WEBHOOK_URL，默认空（不通知）
	// SlackTimeout 是单次 webhook POST 的超时上限。
	// 通知是 best-effort 旁路：超时只丢这一条并记日志，
	// 绝不反压 ingest / 状态流转主路径（派发是异步的，见 notify.Service）。
	SlackTimeout time.Duration // IIP_SLACK_TIMEOUT，默认 5s

	// --- T6.2：通用出站 webhook（M6 §10，ADR-025，实现见 internal/notify）---

	// NotifyWebhookURL 是通用出站地址（POST 机器可读 JSON）。**空 = 关闭**。
	// 与 Slack 键相互独立：可只开一个，也可两个同时开。
	NotifyWebhookURL string // IIP_NOTIFY_WEBHOOK_URL，默认空
	// NotifyWebhookTimeout 是单次 POST 的超时上限（同 Slack 的 5s 档）。
	NotifyWebhookTimeout time.Duration // IIP_NOTIFY_WEBHOOK_TIMEOUT，默认 5s
	// NotifyWebhookToken 是 Bearer token（可选，空则不发 Authorization 头）。
	//
	// 用头而不是 URL 查询参数承载凭据：URL 更容易被日志、代理配置与
	// 浏览器历史带走；头只存在于这一次请求里。
	NotifyWebhookToken string // IIP_NOTIFY_WEBHOOK_TOKEN，默认空

	// EvidenceTLSInsecure 跳过采集目标的服务器证书校验（T7.5）。
	//
	// 默认 false。仅在「内网自签证书、来不及铺 CA」的过渡场景打开，
	// 打开时 worker 启动日志必须打 WARN——它把中间人攻击的面重新打开，
	// 与「采集器只为内网服务」的定位绑定，不是生产默认形态。
	EvidenceTLSInsecure bool // IIP_EVIDENCE_TLS_INSECURE，默认 false
	// EvidenceTLSCAFile 是校验采集目标服务器证书用的 CA 证书文件（PEM，T7.5）。
	//
	// 空 = 用操作系统根证书池。给了路径就必须可读：读不了却在启动时
	// 静默降级成「不校验」，等于把「配了 CA」的部署悄悄变成裸 HTTP 信任模型，
	// 因此 Load/Validate 阶段对「非空但读不到」直接报错，不留给运行时。
	EvidenceTLSCAFile string // IIP_EVIDENCE_TLS_CA_FILE，默认空

	// --- T6.3：自监控指标（ADR-026，实现见 internal/metrics）---

	// MetricsEnabled 控制 server 是否暴露 /metrics。默认 true（见 DefaultMetricsEnabled）。
	//
	// 关掉时该路径直接 404（不注册），而不是返回 403：
	// 「关掉」的语义是「不存在这个端点」，让它 404 才能与「打错路径」区分开，
	// 也避免扫描器一眼看出「这里本该有指标」。
	MetricsEnabled bool // IIP_METRICS_ENABLED，默认 true
	// WorkerMetricsAddr 是 worker 指标监听地址。**空 = 关闭 worker 指标**。
	//
	// worker 没有 HTTP 服务，故单独监听；端口被占用只记 WARN 不影响采集
	// （见 cmd/iip-worker 的 startWorkerMetrics）。
	WorkerMetricsAddr string // IIP_WORKER_METRICS_ADDR，默认 ":9091"

	// --- M5：前端静态托管（手册 §4.3，实现见 m5.go）---

	// WebRoot 是前端产物（vite build 的输出目录）路径。
	// **空字符串即关闭静态托管**，此时服务行为与 M4 完全一致。
	WebRoot string // IIP_WEB_ROOT，默认空（不启用）

	// --- M5 S2：UI 侧面板地址（手册 §4.2.2）---
	//
	// 这组是**浏览器可达**的地址，与 EvidencePromEnabled 用的 PromURL
	// （采集器用的，可能是容器网络地址）**刻意分开**。
	// 共用一个变量的后果是：容器里能采集、但用户点的链接全打不开。
	UIPrometheusURL string // IIP_UI_PROMETHEUS_URL，默认 http://localhost:9090
	UIGrafanaURL    string // IIP_UI_GRAFANA_URL，默认 http://localhost:3000
	UILokiURL       string // IIP_UI_LOKI_URL，默认 http://localhost:3100
	UILokiTenantID  string // IIP_UI_LOKI_TENANT_ID，默认空（单租户）
	// UIBaseURL 是 IIP 自己的前端根地址（浏览器可达）。
	//
	// 通知消息里的事件深链由它拼出：{UIBaseURL}/incidents/{id}。
	// 与上面三个面板链接同一族：给人点的，必须解析到宿主机——
	// 容器内部地址（如 web:5173）点了打不开，比不配链接更难排查。
	UIBaseURL string // IIP_UI_BASE_URL，默认 http://localhost:5173

	// HTTPTimeout 是单个 HTTP 请求的处理超时上限（实现见 httptimeout.go）。
	//
	// 超时后返回 504 并取消请求的 ctx —— 本项目所有业务层方法首参都是 ctx
	// 且底层是 pgx，故取消会**真正中断**正在跑的数据库查询。
	//
	// 它与 EvidenceTimeout 是两回事：后者约束 worker 进程里的单次外部采集，
	// 从不出现在 HTTP 请求路径上。
	HTTPTimeout time.Duration // IIP_HTTP_TIMEOUT，默认 15s

	// 三个 evidence*Explicit 记录采集器开关是否被**显式**配置过。
	//
	// 不导出给业务用，只服务 validateM4：见该方法的说明。
	evidencePromExplicit  bool
	evidenceLokiExplicit  bool
	evidenceK8sExplicit   bool
	evidenceTraceExplicit bool
}
