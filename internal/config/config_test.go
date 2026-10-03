package config

import (
	"testing"
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

// m4 校验需要「地址已配齐」的合法配置，否则会先撞地址校验。
func m4Valid() Config {
	c := m4Defaults()
	c.PGURL = "postgres://x"
	c.HTTPAddr = ":8080"
	c.WorkerPoll = time.Second
	c.WebhookMaxBytes = DefaultWebhookMaxBytes
	c.PromURL = "http://prom:9090"
	c.LokiURL = "http://loki:3100"
	return c
}

// TestLoad_DefaultsAndOverrides 表驱动验证：默认值、flag 覆盖、必填校验。
// 规格依据：docs/IIP-M0详细开发计划.md §T0.3（"表驱动验证默认值与必填校验"）。
func TestLoad_DefaultsAndOverrides(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		env     map[string]string
		want    Config
		wantErr bool
	}{
		{
			name:    "必填缺失_无PGURL_应报错",
			args:    nil,
			env:     map[string]string{},
			wantErr: true,
		},
		{
			name: "仅提供PGURL_其余取默认值",
			args: []string{"--pg-url=postgres://u:p@localhost:5432/iip"},
			env:  map[string]string{},
			want: Config{
				HTTPAddr:           DefaultHTTPAddr,
				PGURL:              "postgres://u:p@localhost:5432/iip",
				LogLevel:           DefaultLogLevel,
				WorkerPoll:         DefaultWorkerPoll,
				DBPollOnly:         DefaultDBPollOnly,
				WebhookMaxBytes:    DefaultWebhookMaxBytes,
				TopologyFile:       DefaultTopologyFile,
				CorrelationEnabled: DefaultCorrelationEnabled,
			},
		},
		{
			name: "flag覆盖默认值",
			args: []string{
				"--pg-url=postgres://u:p@localhost:5432/iip",
				"--http-addr=:9999",
				"--log-level=debug",
				"--worker-poll=5s",
				"--db-poll-only=false",
				"--webhook-max-bytes=2048",
				"--topology-file=/etc/iip/topo.yaml",
				"--correlation-enabled=false",
			},
			env: map[string]string{},
			want: Config{
				HTTPAddr:           ":9999",
				PGURL:              "postgres://u:p@localhost:5432/iip",
				LogLevel:           "debug",
				WorkerPoll:         5 * time.Second,
				DBPollOnly:         false,
				WebhookMaxBytes:    2048,
				TopologyFile:       "/etc/iip/topo.yaml",
				CorrelationEnabled: false,
			},
		},
		{
			name: "环境变量提供PGURL",
			args: nil,
			env:  map[string]string{"IIP_PG_URL": "postgres://env@localhost:5432/iip"},
			want: Config{
				HTTPAddr:           DefaultHTTPAddr,
				PGURL:              "postgres://env@localhost:5432/iip",
				LogLevel:           DefaultLogLevel,
				WorkerPoll:         DefaultWorkerPoll,
				DBPollOnly:         DefaultDBPollOnly,
				WebhookMaxBytes:    DefaultWebhookMaxBytes,
				TopologyFile:       DefaultTopologyFile,
				CorrelationEnabled: DefaultCorrelationEnabled,
			},
		},
		{
			name: "环境变量提供webhook上限",
			args: nil,
			env: map[string]string{
				"IIP_PG_URL":            "postgres://env@localhost:5432/iip",
				"IIP_WEBHOOK_MAX_BYTES": "4096",
			},
			want: Config{
				HTTPAddr:           DefaultHTTPAddr,
				PGURL:              "postgres://env@localhost:5432/iip",
				LogLevel:           DefaultLogLevel,
				WorkerPoll:         DefaultWorkerPoll,
				DBPollOnly:         DefaultDBPollOnly,
				WebhookMaxBytes:    4096,
				TopologyFile:       DefaultTopologyFile,
				CorrelationEnabled: DefaultCorrelationEnabled,
			},
		},
		{
			name: "flag优先于环境变量",
			args: []string{"--log-level=warn"},
			env: map[string]string{
				"IIP_PG_URL":    "postgres://env@localhost:5432/iip",
				"IIP_LOG_LEVEL": "debug",
			},
			want: Config{
				HTTPAddr:           DefaultHTTPAddr,
				PGURL:              "postgres://env@localhost:5432/iip",
				LogLevel:           "warn",
				WorkerPoll:         DefaultWorkerPoll,
				DBPollOnly:         DefaultDBPollOnly,
				WebhookMaxBytes:    DefaultWebhookMaxBytes,
				TopologyFile:       DefaultTopologyFile,
				CorrelationEnabled: DefaultCorrelationEnabled,
			},
		},
		{
			name: "webhook上限为零_应报错",
			args: []string{
				"--pg-url=postgres://u:p@localhost:5432/iip",
				"--webhook-max-bytes=0",
			},
			env:     map[string]string{},
			wantErr: true,
		},
		{
			name: "webhook上限为负_应报错",
			args: []string{
				"--pg-url=postgres://u:p@localhost:5432/iip",
				"--webhook-max-bytes=-1",
			},
			env:     map[string]string{},
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// 设置环境变量并在子测试结束后清理，避免污染其他用例。
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			// 确保必填相关变量不被宿主环境意外注入。
			if _, ok := tc.env["IIP_PG_URL"]; !ok {
				t.Setenv("IIP_PG_URL", "")
			}

			got, err := Load(tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("期望报错，实际成功，got=%+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("未期望报错，实得: %v", err)
			}
			// M4 字段的期望值统一由 m4Defaults() 合并进来。
			// 刻意不在用例里内联书写那 11 个字段：一是冗长，二是漏掉任何一个
			// 都会让它以零值参与比较，而失败信息只说「配置不匹配」——
			// 看不出到底哪个字段对不上。集中合并后，新增字段只改一处。
			want := tc.want
			if want.WorkerTaskBatch == 0 {
				want = mergeM4(want)
			}
			if *got != want {
				t.Fatalf("配置不匹配\n got=%+v\nwant=%+v", *got, want)
			}
		})
	}
}

// TestValidate 单独验证字段校验逻辑。
func TestValidate(t *testing.T) {
	// base 是除被测字段外全部合法的配置，避免每个用例重复书写。
	//
	// 用 m4Valid() 而非手写字面量：手写会漏 M4 字段，
	// 而 validateM4 会把它们判为非法 —— 于是「测Webhook 上限」的用例
	// 实际先撞上 WorkerTaskBatch=0，失败原因与被测意图无关。
	base := m4Valid()
	withWebhook := func(n int64) Config {
		c := base
		c.WebhookMaxBytes = n
		return c
	}
	withPG := func(s string) Config {
		c := base
		c.PGURL = s
		return c
	}
	withAddr := func(s string) Config {
		c := base
		c.HTTPAddr = s
		return c
	}
	withPoll := func(d time.Duration) Config {
		c := base
		c.WorkerPoll = d
		return c
	}

	cases := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{"空PGURL_报错", withPG(""), true},
		{"空白PGURL_报错", withPG("   "), true},
		{"空HTTPAddr_报错", withAddr(""), true},
		{"非正轮询间隔_报错", withPoll(0), true},
		{"webhook上限为零_报错", withWebhook(0), true},
		{"webhook上限为负_报错", withWebhook(-1024), true},
		{"webhook上限为正_通过", withWebhook(1), false},
		{"全部合法_通过", base, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if tc.wantErr && err == nil {
				t.Fatalf("期望报错，实际通过")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("未期望报错，实得: %v", err)
			}
		})
	}
}

// TestValidateM4QueueParams 覆盖 M4 队列参数的边界。
//
// 这些都是「零值或负值会让队列行为变得危险」的项：
// 例如 StaleAfter=0 会让 ReclaimStale 立刻回收所有 in_flight，
// 表现为「任务刚领走就被抢走」。
func TestValidateM4QueueParams(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{"批量为零_报错", func(c *Config) { c.WorkerTaskBatch = 0 }, true},
		{"批量为负_报错", func(c *Config) { c.WorkerTaskBatch = -1 }, true},
		{"最大尝试为零_报错", func(c *Config) { c.WorkerMaxAttempts = 0 }, true},
		{"超时阈值为零_报错", func(c *Config) { c.WorkerTaskStaleAfter = 0 }, true},
		{"证据上限为零_报错", func(c *Config) { c.EvidenceMaxPerIncident = 0 }, true},
		{"采集窗口为零_报错", func(c *Config) { c.EvidenceWindow = 0 }, true},
		{"采集超时为零_报错", func(c *Config) { c.EvidenceTimeout = 0 }, true},
		{"全部为正_通过", func(*Config) {}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := m4Valid()
			tc.mutate(&c)
			err := c.Validate()
			if tc.wantErr != (err != nil) {
				t.Fatalf("wantErr=%v，实际 err=%v", tc.wantErr, err)
			}
		})
	}
}

// TestK8sCollectorRequiresURL 覆盖偏差 B1 的配置侧：
// K8s 默认关闭，但一旦**显式打开**就必须要地址。
//
// 用显式（evidenceK8sExplicit）而非仅看开关值：默认 false 时不该要求地址，
// 而显式打开还不给地址一定是漏配，必须在启动时拦下——
// 否则表现为「任务进了死信」，排查时看不出是配置问题。
func TestK8sCollectorRequiresURL(t *testing.T) {
	c := m4Valid()
	c.EvidenceK8sEnabled = true
	c.K8sURL = ""
	if err := c.Validate(); err == nil {
		t.Error("显式启用 K8s 采集器但未给地址，应报错")
	}

	c.K8sURL = "https://k8s:6443"
	if err := c.Validate(); err != nil {
		t.Errorf("显式启用且给了地址，应通过: %v", err)
	}
}

// TestExplicitPromEnabledRequiresURL 覆盖 §4.6 的 2026-10-03 修订：
// 只有**显式**配置开关时才要求地址；未配置开关时走默认 true 但不强制地址。
//
// 这条是默认部署能起��的关键 —— compose 里没有 Prometheus/Loki，
// 若无条件要求地址，服务启动即失败。
func TestExplicitPromEnabledRequiresURL(t *testing.T) {
	// 未显式配置开关（explicit 为 false）+ 无地址 → 通过。
	c := m4Valid()
	c.PromURL = ""
	c.LokiURL = ""
	c.evidencePromExplicit = false
	c.evidenceLokiExplicit = false
	if err := c.Validate(); err != nil {
		t.Errorf("未显式配置开关时不应要求地址: %v", err)
	}

	// 显式配置开关为 true + 无地址 → 报错。
	c.evidencePromExplicit = true
	if err := c.Validate(); err == nil {
		t.Error("显式启用 Prometheus 却未给地址，应报错")
	}

	// 显式配置为 false + 无地址 → 通过（关掉就不必配地址）。
	c.evidencePromExplicit = false
	if err := c.Validate(); err != nil {
		t.Errorf("显式关闭 Prometheus 时不应要求地址: %v", err)
	}
}
