package config

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/knadh/koanf/v2"
	"github.com/spf13/pflag"
)

// T6.1 的配置项单独成文件（同 m5.go / httptimeout.go 的理由：
// config.go 已超 300 行约束，新功能不带旧债进来）。
//
// 本文件只放 Slack 通知的「声明 + 注册 + 读取 + 校验」——
// 通知的组装与派发在 internal/notify，hook 在 correlation / incident。

// DefaultSlackWebhookURL 默认空 = **不启用通知**（ADR-017 纪律：默认关闭）。
//
// 不用「公共示例 URL」之类占位：空串是最明确的「没配」，
// 而占位值会让「忘了配」伪装成「配了但发去了一个假地址」。
const DefaultSlackWebhookURL = ""

// DefaultSlackTimeout 与 EvidenceTimeout 取同一档（5s）：
// webhook POST 是轻量 JSON，5s 足够覆盖一次跨公网的往返；
// 通知又是 best-effort，等更久只会让在飞通知堆积。
const DefaultSlackTimeout = 5 * time.Second

// registerSlackFlags 注册 T6.1 的命令行参数。
func registerSlackFlags(fs *pflag.FlagSet) {
	fs.String("slack-webhook-url", DefaultSlackWebhookURL,
		"Slack Incoming Webhook 地址；留空=不发送通知（默认关闭）")
	fs.Duration("slack-timeout", DefaultSlackTimeout,
		"单次 Slack webhook POST 超时上限；超时只丢该条并记日志，不影响主路径")
}

// readSlack 从 koanf 读取 Slack 配置并写回 cfg。
func readSlack(k *koanf.Koanf, cfg *Config) {
	cfg.SlackWebhookURL = strings.TrimSpace(k.String("slack-webhook-url"))
	cfg.SlackTimeout = k.Duration("slack-timeout")
}

// validateSlack 校验 Slack 通知配置。由 Config.Validate 调用。
//
// 两条规则，对应两类真实误配：
//  1. 超时必须为正 —— 零值会让每次发送立刻超时，通知恒丢，
//     而现象是「配了 URL 却一条都收不到」；
//  2. URL 配了就必须是合法 http(s) 地址 —— 最常见的误配是从
//     Slack 页面直接复制，漏掉 https:// 前缀，那种串 POST 时
//     才在运行时炸，且只记一行错误日志，极易漏看。
func validateSlack(c *Config) error {
	if c.SlackTimeout <= 0 {
		return fmt.Errorf("SlackTimeout 必须为正数，当前为 %s"+
			"（零值会让通知恒超时；参见 %sSLACK_TIMEOUT）", c.SlackTimeout, envPrefix)
	}
	if c.SlackWebhookURL == "" {
		return nil // 未启用，无从校验
	}
	u, err := url.Parse(c.SlackWebhookURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("SlackWebhookURL 不是合法的 http(s) 地址: %q"+
			"（应从 Slack Incoming Webhook 页面整段复制，含 https://；"+
			"确认不用可留空关闭通知）", c.SlackWebhookURL)
	}
	return nil
}
