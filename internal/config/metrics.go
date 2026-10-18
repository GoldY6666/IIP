package config

import (
	"fmt"
	"strings"

	"github.com/knadh/koanf/v2"
	"github.com/spf13/pflag"
)

// 自监控指标的配置项（M6 T6.3，ADR-026）。
//
// 单独成文件，理由与 m5.go / httptimeout.go / slack.go 相同：
// config.go 已超 AGENTS.md 的 300 行约束。
// 字段声明在 config.go（字段必须在结构体内），其余全在本文件。

const (
	// DefaultMetricsEnabled 默认**开启** server 的 /metrics。
	//
	// 「默认开」与通知类开关（Slack / webhook 默认关）是**有意的不对称**：
	//   - 通知默认关，因为打开它会把告警内容发到第三方系统 —— 那是数据出境，
	//     必须由人显式决定；
	//   - 指标默认开，因为它只读本进程的数字、不出站、不含告警内容
	//     （标签里一个业务值都没有，见 internal/metrics 的基数约束）。
	//     默认关的代价是「线上出问题时才发现没开」，而那时改配置要重启。
	DefaultMetricsEnabled = true

	// DefaultWorkerMetricsAddr 是 worker 指标监听的独立地址。
	//
	// 为什么 worker 需要**独立端口**而不是复用 HTTP 服务：worker 是纯后台进程，
	// 没有任何 HTTP 端点，若为指标去起一个完整业务路由，就得把 server 的
	// 中间件（鉴权/超时/访问日志）也拖进来 —— 那是给一个只吐指标的进程
	// 增加一整套攻击面。
	//
	// 取 9091 而非 9090：9090 是 Prometheus 自己的端口，本机开发时
	// 常常同时跑着 Prometheus，撞端口会让 worker 的指标静默不可用。
	DefaultWorkerMetricsAddr = ":9091"
)

// registerMetricsFlags 注册自监控指标的命令行参数。
func registerMetricsFlags(fs *pflag.FlagSet) {
	fs.Bool("metrics-enabled", DefaultMetricsEnabled,
		"启用 /metrics 自监控端点（默认开启；设 false 则该路径 404）")
	fs.String("worker-metrics-addr", DefaultWorkerMetricsAddr,
		"worker 指标监听地址（留空=关闭；端口被占用只记 WARN 不影响采集）")
}

// readMetrics 从 koanf 读取指标配置并写回 cfg。
func readMetrics(k *koanf.Koanf, cfg *Config) {
	cfg.MetricsEnabled = k.Bool("metrics-enabled")
	cfg.WorkerMetricsAddr = k.String("worker-metrics-addr")
}

// validateMetrics 校验指标配置。
//
// 只拦「地址写了但不成形」：空地址是合法形态（关闭 worker 指标），
// 而 `9091` 这种漏了冒号的写法会让 net.Listen 报一个
// "missing port in address" —— 那时错误发生在 worker 启动后的 goroutine 里，
// 被降级成 WARN 后没人会注意到指标根本没起来。故在启动期拦下。
//
// **不校验端口是否可用**：端口占用是本用例明确允许的降级形态
// （见 DefaultWorkerMetricsAddr 与 iip-worker 的 WARN 分支），
// 在这里拦下会把「监控起不来」升级成「服务起不来」，方向反了。
func validateMetrics(c *Config) error {
	addr := strings.TrimSpace(c.WorkerMetricsAddr)
	if addr == "" {
		return nil
	}
	if !strings.Contains(addr, ":") {
		return fmt.Errorf("WorkerMetricsAddr 缺少端口号：%q"+
			"（应形如 \":9091\" 或 \"127.0.0.1:9091\"；留空表示关闭 worker 指标，"+
			"见 %sWORKER_METRICS_ADDR）", addr, envPrefix)
	}
	return nil
}
