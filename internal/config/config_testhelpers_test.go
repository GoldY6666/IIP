package config

import (
	"time"
)

// m4Defaults 返回 M4 新增字段的默认取值集合。
//
// 单独抽出来的原因：这些字段有 11 个，逐个用例内联书写既冗长又容易漏，
// 而漏掉的那个字段会以零值参与比较，失败信息只说「配置不匹配」——
// 看不出到底哪个字段错了。集中一处后，新增字段只需改这里。
func m4Defaults() Config {
	return Config{
		WorkerTaskBatch:      DefaultWorkerTaskBatch,
		WorkerTaskStaleAfter: DefaultWorkerTaskStaleAfter,
		WorkerMaxAttempts:    DefaultWorkerMaxAttempts,

		EvidencePromEnabled:    DefaultEvidencePromEnabled,
		EvidenceLokiEnabled:    DefaultEvidenceLokiEnabled,
		EvidenceK8sEnabled:     DefaultEvidenceK8sEnabled,
		EvidenceMaxPerIncident: DefaultEvidenceMaxPerIncident,
		EvidenceWindow:         DefaultEvidenceWindow,
		EvidenceTimeout:        DefaultEvidenceTimeout,
	}
}

// m5Defaults 返回 M5 新增字段的默认取值集合。
//
// 沿 m4Defaults 的同一理由（集中列举，避免用例里逐个内联书写）。
//
// **M5 字段不能靠「默认值恰好是零值」蒙过去**：WebRoot 默认空确实等于零值，
// 但四个 UI 地址有非空默认值（localhost 系列），漏写会立刻红 ——
// 而失败信息只说「配置不匹配」，看不出是哪个字段对不上。
func m5Defaults() Config {
	return Config{
		WebRoot:         DefaultWebRoot,
		UIPrometheusURL: DefaultUIPrometheusURL,
		UIGrafanaURL:    DefaultUIGrafanaURL,
		UILokiURL:       DefaultUILokiURL,
		UILokiTenantID:  DefaultUILokiTenantID,
		UIBaseURL:       DefaultUIBaseURL,
	}
}

// mergeM4 把 M4 的默认字段合并进 base 的副本。
func mergeM4(base Config) Config {
	d := m4Defaults()
	base.WorkerTaskBatch = d.WorkerTaskBatch
	base.WorkerTaskStaleAfter = d.WorkerTaskStaleAfter
	base.WorkerMaxAttempts = d.WorkerMaxAttempts
	base.EvidencePromEnabled = d.EvidencePromEnabled
	base.EvidenceLokiEnabled = d.EvidenceLokiEnabled
	base.EvidenceK8sEnabled = d.EvidenceK8sEnabled
	base.EvidenceMaxPerIncident = d.EvidenceMaxPerIncident
	base.EvidenceWindow = d.EvidenceWindow
	base.EvidenceTimeout = d.EvidenceTimeout
	return base
}

// mergeM5 把 M5 字段的默认值合并进 base。
//
// 判据用 UIPrometheusURL == "" 而非 WebRoot == ""：WebRoot 的默认值
// 本身就是空字符串，拿它当哨兵等于「永远合并」，
// 将来想显式测「WebRoot 为空」的那个用例就永远测不到自己的意图。
// UIPrometheusURL 默认非空且没有用例去覆盖它，因此可安全作哨兵 ——
// 与 mergeM4 用 WorkerTaskBatch 同一思路。
func mergeM5(base Config) Config {
	d := m5Defaults()
	base.WebRoot = d.WebRoot
	base.UIPrometheusURL = d.UIPrometheusURL
	base.UIGrafanaURL = d.UIGrafanaURL
	base.UILokiURL = d.UILokiURL
	base.UILokiTenantID = d.UILokiTenantID
	base.UIBaseURL = d.UIBaseURL
	return base
}

// t61Defaults 返回 T6.1 Slack 通知字段的默认取值集合（ADR-024）。
//
// SlackWebhookURL 默认空（关闭通知）而 SlackTimeout 默认 5s 非零 ——
// 合并的哨兵因此只能用 SlackTimeout：拿空串当哨兵会得到「永远已合并」。
func t61Defaults() Config {
	return Config{
		SlackWebhookURL: DefaultSlackWebhookURL,
		SlackTimeout:    DefaultSlackTimeout,
	}
}

// mergeT61 把 T6.1 字段的默认值合并进 base。
func mergeT61(base Config) Config {
	d := t61Defaults()
	base.SlackWebhookURL = d.SlackWebhookURL
	base.SlackTimeout = d.SlackTimeout
	return base
}

// t62Defaults 返回 T6.2 通用 webhook 字段的默认取值集合（ADR-025）。
//
// 与 T6.1 同构：URL/Token 默认空串（不能作合并哨兵），
// 所以哨兵只能用 NotifyWebhookTimeout == 0。
func t62Defaults() Config {
	return Config{
		NotifyWebhookURL:     DefaultNotifyWebhookURL,
		NotifyWebhookTimeout: DefaultNotifyWebhookTimeout,
		NotifyWebhookToken:   DefaultNotifyWebhookToken,
	}
}

// mergeT62 把 T6.2 字段的默认值合并进 base。
func mergeT62(base Config) Config {
	d := t62Defaults()
	base.NotifyWebhookURL = d.NotifyWebhookURL
	base.NotifyWebhookTimeout = d.NotifyWebhookTimeout
	base.NotifyWebhookToken = d.NotifyWebhookToken
	return base
}

// t63Defaults 返回 T6.3 自监控指标字段的默认取值（ADR-026）。
//
// 与其余 *Defaults 同一理由：用例里不内联这些字段，避免漏写后
// 以零值参与比较、失败信息却只说「配置不匹配」。
func t63Defaults() Config {
	return Config{
		MetricsEnabled:    DefaultMetricsEnabled,
		WorkerMetricsAddr: DefaultWorkerMetricsAddr,
	}
}

// mergeT63 把 T6.3 字段的默认值合并进 base。
//
// 哨兵用 MetricsEnabled 的零值（false）：默认是 true，故「未书写」
// 与「默认」正好互补。要测「关掉指标」的用例请走 metrics_test.go ——
// 本表把 false 当作「未书写」，那样写在表里会静默失效。
func mergeT63(base Config) Config {
	d := t63Defaults()
	base.MetricsEnabled = d.MetricsEnabled
	base.WorkerMetricsAddr = d.WorkerMetricsAddr
	return base
}

// t71Defaults 返回 T7.1 分区维护字段的默认取值集合（ADR-022）。
//
// 沿 m4Defaults / m5Defaults 的同一理由：集中列举，避免用例里逐个内联
// 书写；三个字段的默认值全部非零，漏写会以零值参与比较。
func t71Defaults() Config {
	return Config{
		AlertRetentionDays:           DefaultAlertRetentionDays,
		PartitionAheadMonths:         DefaultPartitionAheadMonths,
		PartitionMaintenanceInterval: DefaultPartitionMaintenanceInterval,
	}
}

// mergeT71 把 T7.1 字段的默认值合并进 base。
//
// 哨兵用 AlertRetentionDays == 0：三个字段默认全非零，且 0 恰是
// validatePartition 要拒绝的非法值（保留 0 天 = 删全表），
// 没有用例会把它显式设成 0 还期望通过。
func mergeT71(base Config) Config {
	d := t71Defaults()
	base.AlertRetentionDays = d.AlertRetentionDays
	base.PartitionAheadMonths = d.PartitionAheadMonths
	base.PartitionMaintenanceInterval = d.PartitionMaintenanceInterval
	return base
}

// mergeHTTPTimeout 把 S5 的 HTTPTimeout 默认值合并进 base。
//
// **哨兵不能用 HTTPTimeout == 0**：那正是「未合并」的状态，
// 而0 又恰好是validateHTTPTimeout 要拒绝的非法值 —— 用它当条件会得到
// 「合并前报错、合并后通过」的假象，真 bug 可能被这套合并逻辑顺手掩盖。
//
// 这里以 WantMerge 为唯一开关（由调用方按用例意图显式声明），
// 不从字段值反推：反推等于让判据依赖被测数据本身，是恒真判据的温床。
func mergeHTTPTimeout(base Config) Config {
	base.HTTPTimeout = DefaultHTTPTimeout
	return base
}

// m4 校验需要「地址已配齐」的合法配置，否则会先撞地址校验。
func m4Valid() Config {
	c := m4Defaults()
	c.PGURL = "postgres://x"
	c.HTTPAddr = ":8080"
	c.WorkerPoll = time.Second
	c.WebhookMaxBytes = DefaultWebhookMaxBytes
	c.PromURL = "http://prom:9090"
	c.LokiURL = "http://loki:3100"
	// S5：HTTPTimeout 也要给合法值，否则「测Webhook 上限」的用例
	// 会先撞上 HTTPTimeout=0，失败原因与被测意图无关 ——
	// 这正是本函数存在要避免的那类现象。
	c.HTTPTimeout = DefaultHTTPTimeout
	// T7.1：分区维护三个参数同样要给合法值，否则 validatePartition
	// 会先以 AlertRetentionDays=0 拦下，失败原因与被测意图无关。
	c.AlertRetentionDays = DefaultAlertRetentionDays
	c.PartitionAheadMonths = DefaultPartitionAheadMonths
	c.PartitionMaintenanceInterval = DefaultPartitionMaintenanceInterval
	// T6.1：SlackTimeout 同理（零值会被 validateSlack 拦下）；
	// UIBaseURL 无校验规则，但一并给默认值保持「基座=默认部署」语义。
	c.SlackTimeout = DefaultSlackTimeout
	c.UIBaseURL = DefaultUIBaseURL
	// T6.2：通用 webhook 的超时间理（零值会被 validateWebhook 拦下）。
	c.NotifyWebhookTimeout = DefaultNotifyWebhookTimeout
	return c
}
