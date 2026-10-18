// 内置默认值（从 config.go 拆出，OPEN-M5-7 / OPEN-M6-1）。
//
// 单文件 ≤ 300 行（docs/IIP-M1详细开发手册.md §4）。默认值集中一处便于
// 单测断言，但不必和结构体定义挤在同一个文件里。
package config

import "time"

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
	DefaultEvidenceTraceEnabled   = false // M6 §5：无真实后端可验，默认关闭
	DefaultEvidenceMaxPerIncident = 50
	DefaultEvidenceWindow         = time.Hour
	DefaultEvidenceTimeout        = 5 * time.Second

	// --- T7.5：采集目标 TLS 默认值 ---

	DefaultEvidenceTLSInsecure = false
	DefaultEvidenceTLSCAFile   = ""

	// --- T7.1：alerts 分区维护默认值（ADR-022）---

	// DefaultAlertRetentionDays 取 90 天：覆盖「一个季度的事后复盘」窗口，
	// 同时把 alerts 表的行数压在有界范围（月度分区 + 90 天 ⇒ 常驻 3-4 个分区）。
	DefaultAlertRetentionDays = 90

	// DefaultPartitionAheadMonths 与 0008 迁移的初始分区、partition 包的
	// DefaultAheadMonths 三处一致（互相引用不到，改一处必须同步另两处）。
	DefaultPartitionAheadMonths = 2

	// DefaultPartitionMaintenanceInterval 取 1h：分区补建是「月度」粒度的
	// 动作，1h 的 tick 只为在月初/跨月重火时尽快补上，不追求分钟级。
	DefaultPartitionMaintenanceInterval = time.Hour
)
