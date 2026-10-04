// Package config 负责加载并校验 IIP 运行时配置。
//
// 来源优先级（高 → 低）：
//
//	命令行 flag  >  环境变量  >  config.local.yaml（可选）  >  内置默认值
//
// 规格依据：docs/IIP-M0详细开发计划.md §T0.3。
package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/providers/posflag"
	"github.com/knadh/koanf/v2"
	"github.com/spf13/pflag"
)

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
	// EvidenceTimeout 是单个采集器的超时上限（手册 §4.5：三类均为 5s）。
	EvidenceTimeout time.Duration // IIP_EVIDENCE_TIMEOUT，默认 5s

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

	// 三个 evidence*Explicit 记录采集器开关是否被**显式**配置过。
	//
	// 不导出给业务用，只服务 validateM4：见该方法的说明。
	evidencePromExplicit bool
	evidenceLokiExplicit bool
	evidenceK8sExplicit  bool
}

// 内置默认值集中定义，便于单测断言。
const (
	DefaultHTTPAddr   = ":8080"
	DefaultLogLevel   = "info"
	DefaultWorkerPoll = 2 * time.Second
	DefaultDBPollOnly = true

	// DefaultWebhookMaxBytes 对 Alertmanager 单批 payload 的常见体量留有充裕余量，
	// 同时远小于「一请求打爆内存」的量级。
	DefaultWebhookMaxBytes int64 = 1048576

	// DefaultTopologyFile 是默认拓扑文件路径（相对进程工作目录）。
	DefaultTopologyFile = "config/topology.yaml"

	// DefaultCorrelationEnabled 默认开启关联：M2 的核心能力，不应默认静默。
	DefaultCorrelationEnabled = true

	// --- M4 默认值（手册 §4.1 / §4.6）---

	DefaultWorkerTaskBatch        = 10
	DefaultWorkerTaskStaleAfter   = 5 * time.Minute
	DefaultWorkerMaxAttempts      = 3
	DefaultEvidencePromEnabled    = true
	DefaultEvidenceLokiEnabled    = true
	DefaultEvidenceK8sEnabled     = false // 偏差 B1：默认关闭
	DefaultEvidenceMaxPerIncident = 50
	DefaultEvidenceWindow         = time.Hour
	DefaultEvidenceTimeout        = 5 * time.Second
)

// Load 按优先级加载配置。
//
// args 传入命令行参数（调用方通常传 os.Args[1:]）；传 nil 时按空参数处理，
// 便于单测直接注入，无需依赖进程真实参数。
func Load(args []string) (*Config, error) {
	k := koanf.New(".")

	// 1) flag（最高优先级）
	fs := pflag.NewFlagSet("iip", pflag.ContinueOnError)
	fs.String("http-addr", DefaultHTTPAddr, "HTTP 监听地址")
	fs.String("pg-url", "", "PostgreSQL 连接串（必填）")
	fs.String("log-level", DefaultLogLevel, "日志级别：debug/info/warn/error")
	fs.Duration("worker-poll", DefaultWorkerPoll, "worker 轮询间隔")
	fs.Bool("db-poll-only", DefaultDBPollOnly, "禁用 Redis，仅用 DB polling")
	fs.Int64("webhook-max-bytes", DefaultWebhookMaxBytes, "告警 webhook 请求体上限（字节），超限返回 413")
	fs.String("topology-file", DefaultTopologyFile, "服务依赖拓扑文件路径（M2 依赖相似度用）")
	fs.Bool("correlation-enabled", DefaultCorrelationEnabled, "M2 关联引擎总开关")
	// M4 队列与采集器
	fs.Int("worker-task-batch", DefaultWorkerTaskBatch, "单次轮询最多领取几条任务")
	fs.Duration("worker-task-stale-after", DefaultWorkerTaskStaleAfter, "in_flight 多久算超时可回收")
	fs.Int("worker-max-attempts", DefaultWorkerMaxAttempts, "超过即进死信的尝试次数")
	fs.Bool("evidence-prom-enabled", DefaultEvidencePromEnabled, "启用 Prometheus 采集器")
	fs.Bool("evidence-loki-enabled", DefaultEvidenceLokiEnabled, "启用 Loki 采集器")
	fs.Bool("evidence-k8s-enabled", DefaultEvidenceK8sEnabled, "启用 K8s 采集器（默认关闭）")
	fs.Int("evidence-max-per-incident", DefaultEvidenceMaxPerIncident, "单事件证据上限，超限截断并标记")
	fs.Duration("evidence-window", DefaultEvidenceWindow, "证据采集时间窗口")
	fs.Duration("evidence-timeout", DefaultEvidenceTimeout, "单个采集器超时上限")
	fs.String("prom-url", "", "Prometheus 查询端点根地址（启用该采集器时必填）")
	fs.String("loki-url", "", "Loki 查询端点根地址（启用该采集器时必填）")
	fs.String("k8s-url", "", "K8s API Server 根地址（启用该采集器时必填）")
	fs.String("k8s-namespace", "", "K8s 采集器要查询的命名空间（启用该采集器时必填）")
	// M5：前端静态托管（手册 §4.3）。注册在 m5.go，默认空 = 关闭。
	registerM5Flags(fs)
	// 自检模式开关。真正的分支在 main 里、早于本函数执行；此处仅注册以让
	// "--healthcheck" / "--healthcheck=false" 都能被解析，不会报 unknown flag。
	fs.Bool("healthcheck", false, "自检模式：请求本机 /healthz 后按结果退出")

	if args == nil {
		args = []string{}
	}
	if err := fs.Parse(args); err != nil {
		return nil, fmt.Errorf("解析命令行参数失败: %w", err)
	}

	// 加载顺序即优先级顺序（koanf 后加载覆盖前加载）：
	//   1) config.local.yaml（最低）
	//   2) 环境变量
	//   3) flag（最高）

	// 1) config.local.yaml（可选，不存在则跳过）
	//    文件不存在属预期；其他读取错误也不阻断（后续必填校验兜底）。
	if err := k.Load(file.Provider(localConfigPath), yaml.Parser()); err != nil {
		// 有意忽略：本地配置文件为可选来源。
		_ = err
	}

	// 2) 环境变量：IIP_HTTP_ADDR -> http-addr，IIP_PG_URL -> pg-url
	if err := k.Load(env.Provider(envPrefix, ".", func(s string) string {
		return strings.ReplaceAll(strings.ToLower(strings.TrimPrefix(s, envPrefix)), "_", "-")
	}), nil); err != nil {
		return nil, fmt.Errorf("加载环境变量失败: %w", err)
	}

	// 3) flag（最高优先级）
	if err := k.Load(posflag.Provider(fs, ".", k), nil); err != nil {
		return nil, fmt.Errorf("加载 flag 失败: %w", err)
	}

	cfg := &Config{
		HTTPAddr:           k.String("http-addr"),
		PGURL:              k.String("pg-url"),
		LogLevel:           k.String("log-level"),
		WorkerPoll:         k.Duration("worker-poll"),
		DBPollOnly:         k.Bool("db-poll-only"),
		WebhookMaxBytes:    k.Int64("webhook-max-bytes"),
		TopologyFile:       k.String("topology-file"),
		CorrelationEnabled: k.Bool("correlation-enabled"),

		WorkerTaskBatch:      k.Int("worker-task-batch"),
		WorkerTaskStaleAfter: k.Duration("worker-task-stale-after"),
		WorkerMaxAttempts:    k.Int("worker-max-attempts"),

		EvidencePromEnabled:    k.Bool("evidence-prom-enabled"),
		EvidenceLokiEnabled:    k.Bool("evidence-loki-enabled"),
		EvidenceK8sEnabled:     k.Bool("evidence-k8s-enabled"),
		EvidenceMaxPerIncident: k.Int("evidence-max-per-incident"),
		EvidenceWindow:         k.Duration("evidence-window"),
		EvidenceTimeout:        k.Duration("evidence-timeout"),

		PromURL: k.String("prom-url"),
		LokiURL: k.String("loki-url"),
		K8sURL:  k.String("k8s-url"),
		// koanf 的 key 映射靠结构体 tag，k8s-namespace → K8sNamespace。
		K8sNamespace: k.String("k8s-namespace"),
	}

	// M5：WebRoot 为空即不启用静态托管（手册 §4.3）。
	readM5(k, cfg)

	// 采集器开关是否被**显式**配置过（见 validateM4 的说明）。
	//
	// 判据用 pflag 的 Changed（而非 koanf的键是否存在）：
	// posflag provider 会把**每个** flag 的默认值都写进 koanf，
	// 所以「键存在」对 flag 来源恒为真，分不出「默认值」与「用户显式设置」。
	// 环境变量来源直接查 os.LookupEnv——它天然区分「设过」与「没设」。
	cfg.evidencePromExplicit = flagChanged(fs, "evidence-prom-enabled") ||
		envExists("IIP_EVIDENCE_PROM_ENABLED")
	cfg.evidenceLokiExplicit = flagChanged(fs, "evidence-loki-enabled") ||
		envExists("IIP_EVIDENCE_LOKI_ENABLED")
	cfg.evidenceK8sExplicit = flagChanged(fs, "evidence-k8s-enabled") ||
		envExists("IIP_EVIDENCE_K8S_ENABLED")

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Validate 执行纯字段级校验（不触达外部依赖）。
// 连通性校验（pgxpool.Ping）由调用方在拿到 URL 后执行。
func (c *Config) Validate() error {
	if strings.TrimSpace(c.PGURL) == "" {
		return fmt.Errorf("PGURL 必填：请通过 --pg-url 或环境变量 %sPG_URL 提供 PostgreSQL 连接串", envPrefix)
	}
	if strings.TrimSpace(c.HTTPAddr) == "" {
		return fmt.Errorf("HTTPAddr 不能为空")
	}
	if c.WorkerPoll <= 0 {
		return fmt.Errorf("WorkerPoll 必须为正数，当前为 %s", c.WorkerPoll)
	}
	if c.WebhookMaxBytes <= 0 {
		// 0 或负数会让 MaxBytesReader 拒绝一切请求体（含合法告警），
		// 表现为"所有告警都 413"——必须在启动时拦下，而不是上线后才发现。
		return fmt.Errorf("WebhookMaxBytes 必须为正数，当前为 %d", c.WebhookMaxBytes)
	}
	if err := c.validateM4(); err != nil {
		return err
	}
	return nil
}

// flagChanged 报告该 flag 是否被用户在命令行显式设置过。
func flagChanged(fs *pflag.FlagSet, name string) bool {
	f := fs.Lookup(name)
	return f != nil && f.Changed
}

// envExists 报告该环境变量是否被设置过（哪怕值为空字符串）。
func envExists(key string) bool {
	_, ok := os.LookupEnv(key)
	return ok
}

// validateM4 校验 M4 的队列参数与采集器开关组合。
//
// 「启用了某个采集器却没给它的地址」必须在启动时报错：
// 若放任过去，worker 会在**第一次采集**时才失败，而那意味着
// 事件已经建好、任务已入队，证据却永远采不到 ——
// 表现为「任务进了死信」，排查时看不出是配置缺失。
func (c *Config) validateM4() error {
	if c.WorkerTaskBatch <= 0 {
		return fmt.Errorf("WorkerTaskBatch 必须为正数，当前为 %d", c.WorkerTaskBatch)
	}
	if c.WorkerMaxAttempts <= 0 {
		return fmt.Errorf("WorkerMaxAttempts 必须为正数，当前为 %d", c.WorkerMaxAttempts)
	}
	if c.WorkerTaskStaleAfter <= 0 {
		// 0 会让 ReclaimStale 把所有 in_flight 任务立刻回收，
		// 表现为「任务刚领走就被别人抢走」，且 attempts 飞快涨到死信。
		return fmt.Errorf("WorkerTaskStaleAfter 必须为正数，当前为 %s", c.WorkerTaskStaleAfter)
	}
	if c.EvidenceMaxPerIncident <= 0 {
		return fmt.Errorf("EvidenceMaxPerIncident 必须为正数，当前为 %d", c.EvidenceMaxPerIncident)
	}
	if c.EvidenceWindow <= 0 {
		return fmt.Errorf("EvidenceWindow 必须为正数，当前为 %s", c.EvidenceWindow)
	}
	if c.EvidenceTimeout <= 0 {
		return fmt.Errorf("EvidenceTimeout 必须为正数，当前为 %s", c.EvidenceTimeout)
	}

	// 采集器开关与地址的一致性。
	//
	// **仅在开关被显式配置过时才要求地址**（手册 §4.6 的2026-10-03 修订）：
	// 默认值是「启用 Prometheus / Loki」，若无条件要求地址，
	// 默认部署（compose 里根本没这两个外部服务）会在启动即失败。
	// 那等于拿可用性换「默认值好看」，而 M4 的验收标准恰恰是
	// 「事件创建后 2 分钟内 ≥5 条证据」——服务起不来，标准一条都过不了。
	//
	// 未配地址时的行为：装配阶段不注册该采集器，worker 日志明确说明原因。
	// 「说不清就不说」优于「让整个 worker 拒绝启动」。
	if c.evidencePromExplicit && c.EvidencePromEnabled && strings.TrimSpace(c.PromURL) == "" {
		return fmt.Errorf("已显式启用 Prometheus 采集器但未提供 %sPROM_URL", envPrefix)
	}
	if c.evidenceLokiExplicit && c.EvidenceLokiEnabled && strings.TrimSpace(c.LokiURL) == "" {
		return fmt.Errorf("已显式启用 Loki 采集器但未提供 %sLOKI_URL", envPrefix)
	}
	if c.EvidenceK8sEnabled {
		// K8s 不设「显式才校验」：它默认就是 false，一旦有人打开就必然是有意为之，
		// 此时缺地址一定是漏配，必须拦下。
		// 也不像 Prom/Loki 那样「缺了就跳过注册」——手册 §4.6 的 B6 修订写明了理由：
		// 开关是用户接集群的意图，跳过注册会让它变成死配置（开关已开、地址已填、
		// 日志显示已启用，但采到的证据恒为 0 条且无任何报错）。
		if strings.TrimSpace(c.K8sURL) == "" {
			return fmt.Errorf("已启用 K8s 采集器但未提供 %sK8S_URL", envPrefix)
		}
		if strings.TrimSpace(c.K8sNamespace) == "" {
			return fmt.Errorf("已启用 K8s 采集器但未提供 %sK8S_NAMESPACE"+
				"（K8s 权限按命名空间授权，留空会导致稳定 403，且与「命名空间不存在」"+
				"的 404 难以区分）", envPrefix)
		}
	}
	return nil
}
