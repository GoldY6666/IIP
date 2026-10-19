package config

import (
	"testing"
	"time"
)

// TestSlackNotifySources 钉住 T6.1 三个配置项的来源都真的生效：
// SlackWebhookURL / SlackTimeout / UIBaseURL，各自默认值、flag、环境变量。
//
// **三条必须分别验**（同 TestHTTPTimeoutSources 的理由）：flag 与 env 是
// koanf 两条独立 provider，只验一条时另一条漏接（如 slack.go 漏调
// readSlack、或 m5.go 漏读 ui-base-url）会全绿，而症状是
// 「配了 webhook 却收不到通知」——最耗时排查的那类。
func TestSlackNotifySources(t *testing.T) {
	const url = "https://hooks.slack.com/services/T000/B000/XXX"

	t.Run("默认关闭", func(t *testing.T) {
		c, err := Load([]string{"--pg-url=postgres://u:p@localhost:5432/iip"})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if c.SlackWebhookURL != "" {
			t.Errorf("默认 SlackWebhookURL 应为空（关闭通知），实际 %q", c.SlackWebhookURL)
		}
		if c.SlackTimeout != DefaultSlackTimeout {
			t.Errorf("默认 SlackTimeout = %s，want %s", c.SlackTimeout, DefaultSlackTimeout)
		}
		if c.UIBaseURL != DefaultUIBaseURL {
			t.Errorf("默认 UIBaseURL = %q，want %q", c.UIBaseURL, DefaultUIBaseURL)
		}
	})

	t.Run("flag覆盖", func(t *testing.T) {
		c, err := Load([]string{
			"--pg-url=postgres://u:p@localhost:5432/iip",
			"--slack-webhook-url=" + url,
			"--slack-timeout=2s",
			"--ui-base-url=http://iip.example:8080",
		})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if c.SlackWebhookURL != url {
			t.Errorf("flag 覆盖后 SlackWebhookURL = %q，want %q", c.SlackWebhookURL, url)
		}
		if c.SlackTimeout != 2*time.Second {
			t.Errorf("flag 覆盖后 SlackTimeout = %s，want 2s", c.SlackTimeout)
		}
		if c.UIBaseURL != "http://iip.example:8080" {
			t.Errorf("flag 覆盖后 UIBaseURL = %q", c.UIBaseURL)
		}
	})

	t.Run("环境变量覆盖", func(t *testing.T) {
		t.Setenv("IIP_SLACK_WEBHOOK_URL", url)
		t.Setenv("IIP_SLACK_TIMEOUT", "7s")
		t.Setenv("IIP_UI_BASE_URL", "http://ui.example:5173")
		c, err := Load([]string{"--pg-url=postgres://u:p@localhost:5432/iip"})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if c.SlackWebhookURL != url {
			t.Errorf("环境变量覆盖后 SlackWebhookURL = %q，want %q", c.SlackWebhookURL, url)
		}
		if c.SlackTimeout != 7*time.Second {
			t.Errorf("环境变量覆盖后 SlackTimeout = %s，want 7s", c.SlackTimeout)
		}
		if c.UIBaseURL != "http://ui.example:5173" {
			t.Errorf("环境变量覆盖后 UIBaseURL = %q", c.UIBaseURL)
		}
	})
}

// TestValidateSlack 钉住 validateSlack 的规则与「默认关闭」语义。
//
// 与 TestSlackNotifySources 是一对：后者验「配了能读进来」，
// 前者验「配错了起不来」——两条合起来才是 ADR-017 的
// 「默认关闭 + 打开必须真的能发」。
func TestValidateSlack(t *testing.T) {
	base := func() Config {
		c := m4Valid()
		c.SlackTimeout = DefaultSlackTimeout
		c.UIBaseURL = DefaultUIBaseURL
		return c
	}

	t.Run("默认关闭_空URL合法", func(t *testing.T) {
		c := base()
		c.SlackWebhookURL = ""
		if err := c.Validate(); err != nil {
			t.Errorf("未配 webhook（关闭通知）应合法: %v", err)
		}
	})

	t.Run("合法https地址", func(t *testing.T) {
		c := base()
		c.SlackWebhookURL = "https://hooks.slack.com/services/T000/B000/XXX"
		if err := c.Validate(); err != nil {
			t.Errorf("合法 webhook 应通过: %v", err)
		}
	})

	t.Run("缺scheme应报错", func(t *testing.T) {
		c := base()
		c.SlackWebhookURL = "hooks.slack.com/services/T000/B000/XXX"
		if err := c.Validate(); err == nil {
			t.Error("缺 scheme 的 webhook 应在启动期拦下")
		}
	})

	t.Run("http scheme允许_内网代理场景", func(t *testing.T) {
		c := base()
		c.SlackWebhookURL = "http://slack-proxy.internal:8080/services/T/B/X"
		if err := c.Validate(); err != nil {
			t.Errorf("http  scheme（内网代理）应允许: %v", err)
		}
	})

	t.Run("timeout零值应报错", func(t *testing.T) {
		c := base()
		c.SlackTimeout = 0
		if err := c.Validate(); err == nil {
			t.Error("SlackTimeout 为零会让通知恒超时，应拦下")
		}
	})

	t.Run("timeout负值应报错", func(t *testing.T) {
		c := base()
		c.SlackTimeout = -time.Second
		if err := c.Validate(); err == nil {
			t.Error("SlackTimeout 为负应拦下")
		}
	})
}
