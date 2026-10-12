package config

import (
	"fmt"
	"time"

	"github.com/knadh/koanf/v2"
	"github.com/spf13/pflag"
)

// HTTP 请求超时的配置项（2026-10-12，S5）。
//
// 单独成文件，理由与 m5.go 完全相同：config.go 已超 AGENTS.md 的300 行约束，
// 再往上堆只会让「该拆了」没人认领。
//
// 字段声明在 config.go（字段必须在结构体内），其余全在本文件。

// DefaultHTTPTimeout 是单个 HTTP 请求的处理超时上限。
//
// **取 15s 的依据**（不是拍脑袋，也不是抄某个默认值）：
//   - 本项目实测的 16 个真实请求：P50 = 2.1ms，全局最大 72.7ms。
//     15s 是那个最大值的两百倍余量 —— 真实负载下不可能误伤。
//   - 单个证据采集器自身的超时是 5s（EvidenceTimeout），而采集发生在
//     worker 进程、不在 HTTP 请求路径上。留出 3 倍于它的余量。
//
// 为什么需要它：服务一旦暴露到网络，一个卡死的查询（锁等待、连接池耗尽、
// 某个外部依赖挂住）会**一直占着连接不放**。没有超时，10 个慢请求就能让
// 整个服务对健康检查以外的一切彻底无响应，且症状是「服务假死」而非报错。
const DefaultHTTPTimeout = 15 * time.Second

// registerHTTPTimeoutFlags 注册请求超时的命令行参数。
func registerHTTPTimeoutFlags(fs *pflag.FlagSet) {
	fs.Duration("http-timeout", DefaultHTTPTimeout,
		"单个 HTTP 请求的处理超时上限，超时返回 504")
}

// readHTTPTimeout 从 koanf 读取请求超时并写回 cfg。
func readHTTPTimeout(k *koanf.Koanf, cfg *Config) {
	cfg.HTTPTimeout = k.Duration("http-timeout")
}

// validateHTTPTimeout 校验请求超时必须为正数。
//
// **零值必须报错而不是「当作不限时」**：中间件的零值会让
// context.WithTimeout(ctx, 0) 立刻超时，等于「所有请求都 504」；
// 而负值在 Go 里直接被context 当作「已过期」。
// 两种都是「配错→服务全挂」，因此都在启动期拦下，而不是等第一个请求。
func validateHTTPTimeout(c *Config) error {
	if c.HTTPTimeout <= 0 {
		return fmt.Errorf("HTTPTimeout 必须为正数，当前为 %s", c.HTTPTimeout)
	}
	return nil
}
