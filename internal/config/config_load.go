// 配置加载（从 config.go 拆出，OPEN-M5-7 / OPEN-M6-1）。
//
// 优先级见包注释：flag > 环境变量 > config.local.yaml > 内置默认值。
package config

import (
	"fmt"
	"strings"

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/providers/posflag"
	"github.com/knadh/koanf/v2"
	"github.com/spf13/pflag"
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
	fs.Bool("evidence-trace-enabled", DefaultEvidenceTraceEnabled, "启用 trace 采集器（默认关闭）")
	fs.Int("evidence-max-per-incident", DefaultEvidenceMaxPerIncident, "单事件证据上限，超限截断并标记")
	fs.Duration("evidence-window", DefaultEvidenceWindow, "证据采集时间窗口")
	fs.Duration("evidence-timeout", DefaultEvidenceTimeout, "单个采集器超时上限")
	// T7.1：分区维护。三个默认值都不改既有部署的行为（维护 tick 是新增旁路职责）。
	fs.Int("alert-retention-days", DefaultAlertRetentionDays,
		"告警保留天数（按 starts_at）；过期行由 worker 维护 tick 删除并级联清理事件链接")
	fs.Int("partition-ahead-months", DefaultPartitionAheadMonths,
		"预创建的未来月分区数（不含当月）；0=只建当月")
	fs.Duration("partition-maintenance-interval", DefaultPartitionMaintenanceInterval,
		"分区维护 tick 间隔（补建分区 + 过期清理 + 重复 firing 扫描）")
	// T7.5：采集目标 TLS。两个都不改默认行为：默认仍走系统根证书校验，
	// 显式开了 insecure 只是放宽校验，不改变默认部署的任何链路。
	fs.Bool("evidence-tls-insecure", DefaultEvidenceTLSInsecure,
		"跳过采集目标 TLS 证书校验（仅限内网自签证书，默认关闭；开启时启动日志打 WARN）")
	fs.String("evidence-tls-ca-file", DefaultEvidenceTLSCAFile,
		"校验采集目标服务器证书的 CA 文件路径（PEM），留空=用系统根证书池")
	// T6.3：自监控指标。默认开 server 的 /metrics，worker 独立监听 :9091。
	registerMetricsFlags(fs)
	fs.String("prom-url", "", "Prometheus 查询端点根地址（启用该采集器时必填）")
	fs.String("loki-url", "", "Loki 查询端点根地址（启用该采集器时必填）")
	// 空串 = 不开鉴权，故help 里明写这一点，避免「设了变量但没配值」
	// 被误读成「配了但没生效」。
	fs.String("auth-token", "", "访问 /api/v1/* 的静态 token；留空=不开鉴权（仅建议内网部署时开启）")
	fs.String("k8s-url", "", "K8s API Server 根地址（启用该采集器时必填）")
	fs.String("k8s-namespace", "", "K8s 采集器要查询的命名空间（启用该采集器时必填）")
	fs.String("trace-url", "", "链路追踪后端根地址（启用该采集器时必填）")
	// M5：前端静态托管（手册 §4.3）。注册在 m5.go，默认空 = 关闭。
	registerM5Flags(fs)
	// S5：HTTP 请求超时。注册在 httptimeout.go。
	registerHTTPTimeoutFlags(fs)
	// T6.1：Slack 通知。注册在 slack.go，默认空 = 关闭。
	registerSlackFlags(fs)
	registerWebhookFlags(fs)
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
		// AuthToken 无 Validate 必填规则：空串是合法的「不开鉴权」（见字段注释）。
		AuthToken: k.String("auth-token"),

		WorkerTaskBatch:      k.Int("worker-task-batch"),
		WorkerTaskStaleAfter: k.Duration("worker-task-stale-after"),
		WorkerMaxAttempts:    k.Int("worker-max-attempts"),

		EvidencePromEnabled:    k.Bool("evidence-prom-enabled"),
		EvidenceLokiEnabled:    k.Bool("evidence-loki-enabled"),
		EvidenceK8sEnabled:     k.Bool("evidence-k8s-enabled"),
		EvidenceTraceEnabled:   k.Bool("evidence-trace-enabled"),
		EvidenceMaxPerIncident: k.Int("evidence-max-per-incident"),
		EvidenceWindow:         k.Duration("evidence-window"),
		EvidenceTimeout:        k.Duration("evidence-timeout"),

		AlertRetentionDays:           k.Int("alert-retention-days"),
		PartitionAheadMonths:         k.Int("partition-ahead-months"),
		PartitionMaintenanceInterval: k.Duration("partition-maintenance-interval"),

		EvidenceTLSInsecure: k.Bool("evidence-tls-insecure"),
		EvidenceTLSCAFile:   k.String("evidence-tls-ca-file"),

		PromURL:  k.String("prom-url"),
		LokiURL:  k.String("loki-url"),
		K8sURL:   k.String("k8s-url"),
		TraceURL: k.String("trace-url"),
		// koanf 的 key 映射靠结构体 tag，k8s-namespace → K8sNamespace。
		K8sNamespace: k.String("k8s-namespace"),
	}

	// M5：WebRoot 为空即不启用静态托管（手册 §4.3）。
	readM5(k, cfg)

	// S5：HTTP 请求超时（见 httptimeout.go）。
	readHTTPTimeout(k, cfg)

	// T6.1：Slack 通知（见 slack.go）。
	readSlack(k, cfg)
	readWebhook(k, cfg)
	// T6.3：自监控指标（见 metrics.go）。
	readMetrics(k, cfg)

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
	cfg.evidenceTraceExplicit = flagChanged(fs, "evidence-trace-enabled") ||
		envExists("IIP_EVIDENCE_TRACE_ENABLED")

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}
