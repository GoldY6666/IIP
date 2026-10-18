package config

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/knadh/koanf/v2"
	"github.com/spf13/pflag"
)

// T6.2 的通用出站 webhook 配置。与 slack.go 同构（声明 + 注册 + 读取 + 校验），
// 单独成文件同 m5.go / httptimeout.go 的理由：config.go 已超 300 行约束。
//
// 与 Slack 出口的区别：Slack 是给人看的纯文本，这里是给外部系统看的 JSON
// （payload 契约见 internal/notify/webhook.go 与 ADR-025）。

// DefaultNotifyWebhookURL 默认空 = 不启用（与 Slack 键一致：默认关闭）。
const DefaultNotifyWebhookURL = ""

// DefaultNotifyWebhookToken 默认空 = 不带 Authorization 头。
//
// 有些内网接收端不需要鉴权；需要时用 Bearer token，避免把凭据写进 URL
// （URL 更容易被日志、代理配置与浏览器历史记录带走）。
const DefaultNotifyWebhookToken = ""

// DefaultNotifyWebhookTimeout 与 Slack 同档 5s。
const DefaultNotifyWebhookTimeout = 5 * time.Second

// registerWebhookFlags 注册 T6.2 的命令行参数。
func registerWebhookFlags(fs *pflag.FlagSet) {
	fs.String("notify-webhook-url", DefaultNotifyWebhookURL,
		"通用出站 webhook 地址（JSON payload）；留空=不启用（默认关闭）")
	fs.Duration("notify-webhook-timeout", DefaultNotifyWebhookTimeout,
		"单次通用 webhook POST 超时上限；超时只丢该条并记日志")
	fs.String("notify-webhook-token", DefaultNotifyWebhookToken,
		"通用 webhook 的 Bearer token（可选）；空=不发 Authorization 头")
}

// readWebhook 从 koanf 读取通用 webhook 配置并写回 cfg。
func readWebhook(k *koanf.Koanf, cfg *Config) {
	cfg.NotifyWebhookURL = strings.TrimSpace(k.String("notify-webhook-url"))
	cfg.NotifyWebhookTimeout = k.Duration("notify-webhook-timeout")
	cfg.NotifyWebhookToken = strings.TrimSpace(k.String("notify-webhook-token"))
}

// validateWebhook 校验通用 webhook 配置。由 Config.Validate 调用。
//
// 规则与 validateSlack 对齐（同样的两类误配）：
// 零超时会让通知恒丢；URL 漏 scheme 只会在运行时炸且只记一行日志。
// 另：配了 token 却没配 URL 时**不报错**——token 可能预先写在 .env 里，
// 等 URL 补上再生效；这种「有凭据没出口」不会造成任何发送。
func validateWebhook(c *Config) error {
	if c.NotifyWebhookTimeout <= 0 {
		return fmt.Errorf("NotifyWebhookTimeout 必须为正数，当前为 %s"+
			"（零值会让通知恒超时；参见 %sNOTIFY_WEBHOOK_TIMEOUT）", c.NotifyWebhookTimeout, envPrefix)
	}
	if c.NotifyWebhookURL == "" {
		return nil // 未启用，无从校验
	}
	u, err := url.Parse(c.NotifyWebhookURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("NotifyWebhookURL 不是合法的 http(s) 地址: %q"+
			"（需含 scheme 与主机名；确认不用可留空关闭）", c.NotifyWebhookURL)
	}
	return nil
}
