package config

import (
	"testing"
	"time"
)

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
