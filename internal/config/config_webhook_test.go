package config

import (
	"testing"
	"time"
)

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

// TestNotifyWebhookSources 验 T6.2 通用 webhook 的三来源（默认/flag/env）。
//
// 与 TestSlackNotifySources 同构：默认关闭是硬性要求（ADR-017 纪律），
// 打开后必须真的能读到值，否则「配了却不生效」只会在告警来临时才暴露。
func TestNotifyWebhookSources(t *testing.T) {
	const url = "http://receiver.internal:9000/hook"

	t.Run("默认关闭", func(t *testing.T) {
		c, err := Load([]string{"--pg-url=postgres://u:p@localhost:5432/iip"})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if c.NotifyWebhookURL != "" || c.NotifyWebhookToken != "" {
			t.Errorf("默认应关闭通用 webhook，实际 url=%q token=%q", c.NotifyWebhookURL, c.NotifyWebhookToken)
		}
		if c.NotifyWebhookTimeout != DefaultNotifyWebhookTimeout {
			t.Errorf("默认超时 = %s，want %s", c.NotifyWebhookTimeout, DefaultNotifyWebhookTimeout)
		}
	})

	t.Run("flag覆盖", func(t *testing.T) {
		c, err := Load([]string{
			"--pg-url=postgres://u:p@localhost:5432/iip",
			"--notify-webhook-url=" + url,
			"--notify-webhook-timeout=3s",
			"--notify-webhook-token=tok",
		})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if c.NotifyWebhookURL != url || c.NotifyWebhookToken != "tok" {
			t.Errorf("flag 覆盖后 url=%q token=%q", c.NotifyWebhookURL, c.NotifyWebhookToken)
		}
		if c.NotifyWebhookTimeout != 3*time.Second {
			t.Errorf("flag 覆盖后超时 = %s，want 3s", c.NotifyWebhookTimeout)
		}
	})

	t.Run("环境变量覆盖", func(t *testing.T) {
		t.Setenv("IIP_NOTIFY_WEBHOOK_URL", url)
		t.Setenv("IIP_NOTIFY_WEBHOOK_TIMEOUT", "8s")
		t.Setenv("IIP_NOTIFY_WEBHOOK_TOKEN", "env-tok")
		c, err := Load([]string{"--pg-url=postgres://u:p@localhost:5432/iip"})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if c.NotifyWebhookURL != url || c.NotifyWebhookToken != "env-tok" {
			t.Errorf("env 覆盖后 url=%q token=%q", c.NotifyWebhookURL, c.NotifyWebhookToken)
		}
		if c.NotifyWebhookTimeout != 8*time.Second {
			t.Errorf("env 覆盖后超时 = %s，want 8s", c.NotifyWebhookTimeout)
		}
	})
}

// TestValidateWebhook 钉住 validateWebhook 的四条规则。
func TestValidateWebhook(t *testing.T) {
	base := func() Config {
		c := m4Valid()
		c.NotifyWebhookTimeout = DefaultNotifyWebhookTimeout
		return c
	}

	t.Run("默认关闭_空URL合法", func(t *testing.T) {
		c := base()
		if err := c.Validate(); err != nil {
			t.Errorf("未配 webhook 应合法: %v", err)
		}
	})

	t.Run("合法地址通过", func(t *testing.T) {
		for _, u := range []string{"https://example.com/hook", "http://10.0.0.5:9000/hook"} {
			c := base()
			c.NotifyWebhookURL = u
			if err := c.Validate(); err != nil {
				t.Errorf("%s 应通过: %v", u, err)
			}
		}
	})

	t.Run("缺scheme应报错", func(t *testing.T) {
		c := base()
		c.NotifyWebhookURL = "receiver.internal:9000/hook"
		if err := c.Validate(); err == nil {
			t.Error("缺 scheme 应在启动期拦下")
		}
	})

	t.Run("非http协议应报错", func(t *testing.T) {
		c := base()
		c.NotifyWebhookURL = "ftp://receiver.internal/hook"
		if err := c.Validate(); err == nil {
			t.Error("ftp 协议应被拒绝")
		}
	})

	t.Run("超时为零应报错", func(t *testing.T) {
		c := base()
		c.NotifyWebhookTimeout = 0
		if err := c.Validate(); err == nil {
			t.Error("零超时会让通知恒丢，应报错")
		}
	})

	t.Run("有token无URL合法", func(t *testing.T) {
		c := base()
		c.NotifyWebhookToken = "pre-set-token"
		if err := c.Validate(); err != nil {
			t.Errorf("预置 token 但未配出口应合法: %v", err)
		}
	})
}
