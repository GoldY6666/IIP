// Package config 负责加载并校验 IIP 运行时配置。
//
// 来源优先级（高 → 低）：
//
//	命令行 flag  >  环境变量  >  config.local.yaml（可选）  >  内置默认值
//
// 规格依据：docs/IIP-M0详细开发计划.md §T0.3。
package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/providers/posflag"
	"github.com/knadh/koanf/v2"
	"github.com/spf13/pflag"
)

// 环境变量前缀。所有 IIP_* 变量映射为 koanf 小写键（去掉 IIP_ 前缀）。
const envPrefix = "IIP_"

// 本地可选配置文件路径（进 .gitignore）。
const localConfigPath = "config.local.yaml"

// Config 是 IIP server / worker 的运行时配置。
//
// 注意：这些字段与数据库中 6 张表无直接关系，仅描述进程运行参数。
type Config struct {
	HTTPAddr   string        // IIP_HTTP_ADDR，默认 ":8080"
	PGURL      string        // IIP_PG_URL，必填（无默认）
	LogLevel   string        // IIP_LOG_LEVEL，默认 "info"
	WorkerPoll time.Duration // IIP_WORKER_POLL_INTERVAL，默认 2s
	DBPollOnly bool          // IIP_REDIS_DISABLED，V1.0 默认 true（DB polling 模式）

	// WebhookMaxBytes 是告警 webhook 请求体上限（字节），超限返回 413。
	//
	// 必须有上限：该端点对外暴露，没有上限时一个超大请求体就能把服务内存吃满。
	// 规格依据：docs/IIP-M1详细开发手册.md §4.7。
	WebhookMaxBytes int64 // IIP_WEBHOOK_MAX_BYTES，默认 1048576（1 MiB）
}

// 内置默认值集中定义，便于单测断言。
const (
	DefaultHTTPAddr   = ":8080"
	DefaultLogLevel   = "info"
	DefaultWorkerPoll = 2 * time.Second
	DefaultDBPollOnly = true

	// DefaultWebhookMaxBytes 对 Alertmanager 单批 payload 的常见体量留有充裕余量，
	// 同时远小于「一请求打爆内存」的量级。
	DefaultWebhookMaxBytes int64 = 1048576
)

// Load 按优先级加载配置。
//
// args 传入命令行参数（调用方通常传 os.Args[1:]）；传 nil 时按空参数处理，
// 便于单测直接注入，无需依赖进程真实参数。
func Load(args []string) (*Config, error) {
	k := koanf.New(".")

	// 1) flag（最高优先级）
	fs := pflag.NewFlagSet("iip", pflag.ContinueOnError)
	fs.String("http-addr", DefaultHTTPAddr, "HTTP 监听地址")
	fs.String("pg-url", "", "PostgreSQL 连接串（必填）")
	fs.String("log-level", DefaultLogLevel, "日志级别：debug/info/warn/error")
	fs.Duration("worker-poll", DefaultWorkerPoll, "worker 轮询间隔")
	fs.Bool("db-poll-only", DefaultDBPollOnly, "禁用 Redis，仅用 DB polling")
	fs.Int64("webhook-max-bytes", DefaultWebhookMaxBytes, "告警 webhook 请求体上限（字节），超限返回 413")
	// 自检模式开关。真正的分支在 main 里、早于本函数执行；此处仅注册以让
	// "--healthcheck" / "--healthcheck=false" 都能被解析，不会报 unknown flag。
	fs.Bool("healthcheck", false, "自检模式：请求本机 /healthz 后按结果退出")

	if args == nil {
		args = []string{}
	}
	if err := fs.Parse(args); err != nil {
		return nil, fmt.Errorf("解析命令行参数失败: %w", err)
	}

	// 加载顺序即优先级顺序（koanf 后加载覆盖前加载）：
	//   1) config.local.yaml（最低）
	//   2) 环境变量
	//   3) flag（最高）

	// 1) config.local.yaml（可选，不存在则跳过）
	//    文件不存在属预期；其他读取错误也不阻断（后续必填校验兜底）。
	if err := k.Load(file.Provider(localConfigPath), yaml.Parser()); err != nil {
		// 有意忽略：本地配置文件为可选来源。
		_ = err
	}

	// 2) 环境变量：IIP_HTTP_ADDR -> http-addr，IIP_PG_URL -> pg-url
	if err := k.Load(env.Provider(envPrefix, ".", func(s string) string {
		return strings.ReplaceAll(strings.ToLower(strings.TrimPrefix(s, envPrefix)), "_", "-")
	}), nil); err != nil {
		return nil, fmt.Errorf("加载环境变量失败: %w", err)
	}

	// 3) flag（最高优先级）
	if err := k.Load(posflag.Provider(fs, ".", k), nil); err != nil {
		return nil, fmt.Errorf("加载 flag 失败: %w", err)
	}

	cfg := &Config{
		HTTPAddr:        k.String("http-addr"),
		PGURL:           k.String("pg-url"),
		LogLevel:        k.String("log-level"),
		WorkerPoll:      k.Duration("worker-poll"),
		DBPollOnly:      k.Bool("db-poll-only"),
		WebhookMaxBytes: k.Int64("webhook-max-bytes"),
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

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
	if c.WebhookMaxBytes <= 0 {
		// 0 或负数会让 MaxBytesReader 拒绝一切请求体（含合法告警），
		// 表现为"所有告警都 413"——必须在启动时拦下，而不是上线后才发现。
		return fmt.Errorf("WebhookMaxBytes 必须为正数，当前为 %d", c.WebhookMaxBytes)
	}
	return nil
}
