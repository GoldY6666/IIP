// 配置校验（从 config.go 拆出，OPEN-M5-7 / OPEN-M6-1）。
//
// 纯字段级校验，不触达外部依赖；连通性校验由调用方在拿到 URL 后执行。
package config

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/pflag"
)

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
	// S5：请求超时。零值会让中间件立刻超时 → 所有请求 504，故同样在启动期拦下。
	if err := validateHTTPTimeout(c); err != nil {
		return err
	}
	if c.WebhookMaxBytes <= 0 {
		// 0 或负数会让 MaxBytesReader 拒绝一切请求体（含合法告警），
		// 表现为"所有告警都 413"——必须在启动时拦下，而不是上线后才发现。
		return fmt.Errorf("WebhookMaxBytes 必须为正数，当前为 %d", c.WebhookMaxBytes)
	}
	if err := c.validateM4(); err != nil {
		return err
	}
	if err := c.validateEvidenceTLS(); err != nil {
		return err
	}
	if err := c.validatePartition(); err != nil {
		return err
	}
	if err := validateSlack(c); err != nil {
		return err
	}
	if err := validateWebhook(c); err != nil {
		return err
	}
	if err := validateMetrics(c); err != nil {
		return err
	}
	return nil
}

// validatePartition 校验 T7.1 分区维护参数（ADR-022）。
//
// retentionDays <= 0 必须拦下：它会让维护 tick 的 DELETE 删掉**全表**。
// 「保留 0 天」在任何运维语境下都是误配——要清库应是显式运维动作
// （psql 一把），而不是一个配置值在每次 tick 时静默生效。
// aheadMonths 允许 0（只建当月，插入路径有 23514 重试兜底）但不允许负：
// 负值无业务含义，且数据库函数对负值的行为是「静默按默认 2」，
// 那会让「配了 -1 实际建了 3 个月」成为无声偏差。
func (c *Config) validatePartition() error {
	if c.AlertRetentionDays <= 0 {
		return fmt.Errorf("AlertRetentionDays 必须为正数，当前为 %d"+
			"（0 或负数会让维护 tick 删除全部告警；要清库请走显式运维动作，"+
			"参见 %sALERT_RETENTION_DAYS）", c.AlertRetentionDays, envPrefix)
	}
	if c.PartitionAheadMonths < 0 {
		return fmt.Errorf("PartitionAheadMonths 不能为负，当前为 %d"+
			"（0 = 只建当月；负值会被数据库函数静默收敛为默认 2，"+
			"配置与实际行为不符，故在此拦下）", c.PartitionAheadMonths)
	}
	if c.PartitionMaintenanceInterval <= 0 {
		return fmt.Errorf("PartitionMaintenanceInterval 必须为正数，当前为 %s", c.PartitionMaintenanceInterval)
	}
	return nil
}

// validateEvidenceTLS 校验采集目标 TLS 配置（T7.5）。
//
// 只拦「CA 文件给了却读不到」这一种：insecure 单独开、两个都开
// （此时 insecure 优先）、都不开都是合法形态，语义在采集器构造时落实。
// 「读不到」必须在启动期拦下——若静默降级为不校验，
// 用户会以为 CA 已生效，实际信任模型已退回「谁来都认」。
func (c *Config) validateEvidenceTLS() error {
	path := strings.TrimSpace(c.EvidenceTLSCAFile)
	if path == "" {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("EvidenceTLSCAFile 不存在：%s"+
				"（请修正 %sEVIDENCE_TLS_CA_FILE，或删掉该配置改用系统根证书池）", path, envPrefix)
		}
		return fmt.Errorf("读取 EvidenceTLSCAFile 失败：%s: %w", path, err)
	}
	if info.IsDir() {
		return fmt.Errorf("EvidenceTLSCAFile 是目录而非文件：%s（需要 PEM 证书文件）", path)
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

	if c.EvidenceTraceEnabled {
		// 与 K8s 同规则：默认 false，打开即有意，缺地址必是漏配。
		// 不搞「缺了就跳过注册」——那会让开关变成死配置：
		// 日志说已启用、采到的 trace 证据恒为 0 条，而全程无报错。
		if strings.TrimSpace(c.TraceURL) == "" {
			return fmt.Errorf("已启用 trace 采集器但未提供 %sTRACE_URL"+
				"（trace 采集器默认关闭，本仓库不提供链路后端；"+
				"地址需指向兼容 Jaeger Query API 的后端）", envPrefix)
		}
	}
	return nil
}
