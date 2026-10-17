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
		// wantHTTPTimeout 为 true 时把 HTTPTimeout 的默认值合并进 want。
		// 由用例按意图显式声明，不从字段值反推（见 mergeHTTPTimeout 的说明）。
		wantHTTPTimeout bool
	}{
		{
			name:    "必填缺失_无PGURL_应报错",
			args:    nil,
			env:     map[string]string{},
			wantErr: true,
		},
		{
			name:            "仅提供PGURL_其余取默认值",
			args:            []string{"--pg-url=postgres://u:p@localhost:5432/iip"},
			env:             map[string]string{},
			wantHTTPTimeout: true,
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
			name:            "flag覆盖默认值",
			wantHTTPTimeout: true,
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
			name:            "环境变量提供PGURL",
			wantHTTPTimeout: true,
			args:            nil,
			env:             map[string]string{"IIP_PG_URL": "postgres://env@localhost:5432/iip"},
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
			name:            "环境变量提供webhook上限",
			wantHTTPTimeout: true,
			args:            nil,
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
			name:            "flag优先于环境变量",
			wantHTTPTimeout: true,
			args:            []string{"--log-level=warn"},
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
			// M5 同理：UI 四个地址有非空默认值（localhost 系列），
			// 漏合并必然让「期望全默认」的用例变红。
			if want.UIPrometheusURL == "" {
				want = mergeM5(want)
			}
			// S5 同理：HTTPTimeout 默认 15s，非零值，漏合并必然让
			// 「期望全默认」的用例变红。
			if tc.wantHTTPTimeout {
				want = mergeHTTPTimeout(want)
			}
			// T7.1 同理：三个分区维护参数默认全非零。
			if want.AlertRetentionDays == 0 {
				want = mergeT71(want)
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
	withTimeout := func(d time.Duration) Config {
		c := base
		c.HTTPTimeout = d
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
		// S5：零值会让中间件对**所有**请求立刻超时，负值在 Go 里
		// 直接被 context 当作已过期 —— 两者都是「配错→服务全挂」，
		// 因此都必须在启动期拦下。
		{"请求超时为零_报错", withTimeout(0), true},
		{"请求超时为负_报错", withTimeout(-time.Second), true},
		{"请求超时为正_通过", withTimeout(time.Millisecond), false},
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
	c.K8sNamespace = ""
	if err := c.Validate(); err == nil {
		t.Error("给了地址但未给命名空间，应报错（K8s 权限按命名空间授权）")
	}

	c.K8sNamespace = "prod"
	if err := c.Validate(); err != nil {
		t.Errorf("显式启用且地址与命名空间齐备，应通过: %v", err)
	}
}

// TestK8sDefaultOffDoesNotRequireNamespace 钉住偏差 B1 的「默认关闭」语义：
// 默认部署（K8s 未开启）不该被要求配任何 K8s 项。
//
// 这条与 TestK8sCollectorRequiresURL 是**一对**：只写后者的话，
// 有人把默认值误改成 true 也会全绿，而那会让默认部署直接起不来。
func TestK8sDefaultOffDoesNotRequireNamespace(t *testing.T) {
	c := m4Valid()
	c.EvidenceK8sEnabled = DefaultEvidenceK8sEnabled
	c.K8sURL = ""
	c.K8sNamespace = ""
	if c.EvidenceK8sEnabled {
		t.Fatal("前提失效：K8s 采集器默认值应为 false（B1 偏差）")
	}
	if err := c.Validate(); err != nil {
		t.Errorf("K8s 默认关闭时不应要求任何 K8s 配置项: %v", err)
	}
}

// TestHTTPTimeoutSources 钉住 HTTPTimeout 的三条来源都真的生效：
// 默认值、flag 覆盖、环境变量覆盖。
//
// **三条必须分别验**：koanf 的posflag provider 与 env provider 是两条独立
// 代码路径，只验其中一条时，另一条挂掉（比如新文件漏调 readHTTPTimeout
// 只接上flag、或env 键名映射写错）会全绿。而这类漏接的症状是
// 「配了超时但服务仍按默认跑」或「配了环境变量不生效」，极难排查。
func TestHTTPTimeoutSources(t *testing.T) {
	t.Run("默认15s", func(t *testing.T) {
		c, err := Load([]string{"--pg-url=postgres://u:p@localhost:5432/iip"})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if c.HTTPTimeout != DefaultHTTPTimeout {
			t.Errorf("默认 HTTPTimeout = %s，want %s", c.HTTPTimeout, DefaultHTTPTimeout)
		}
	})

	t.Run("flag覆盖", func(t *testing.T) {
		c, err := Load([]string{
			"--pg-url=postgres://u:p@localhost:5432/iip",
			"--http-timeout=42s",
		})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if c.HTTPTimeout != 42*time.Second {
			t.Errorf("flag 覆盖后 HTTPTimeout = %s，want 42s", c.HTTPTimeout)
		}
	})

	t.Run("环境变量覆盖", func(t *testing.T) {
		t.Setenv("IIP_HTTP_TIMEOUT", "7s")
		c, err := Load([]string{"--pg-url=postgres://u:p@localhost:5432/iip"})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if c.HTTPTimeout != 7*time.Second {
			t.Errorf("环境变量覆盖后 HTTPTimeout = %s，want 7s", c.HTTPTimeout)
		}
	})
}

// TestExplicitPromEnabledRequiresURL 覆盖 §4.6 的 2026-10-03 修订：
// 只有**显式**配置开关时才要求地址；未配置开关时走默认 true 但不强制地址。
//
// 这条是默认部署能起来的关键 —— compose 里没有 Prometheus/Loki，
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
