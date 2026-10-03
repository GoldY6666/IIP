// Package collectors 提供 M4 的证据采集器实现。
//
// 本文件只放**三类采集器共用**的东西：来源标识、HTTP 查询选项、
// LogQL/PromQL 共用的转义与数值解析。各采集器自己的逻辑在同名文件里。
//
// 为什么要抽出来：Prometheus 与 Loki 都需要「超时可配」，
// 早先 WithTimeout 挂在 PrometheusCollector 上，Loki 就得再写一个
// 同名不同签名的函数 —— 那是接口在表达「我以为只有一个采集器」。
package collectors

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// 证据来源标识，落库到 evidence.source。
const (
	SourcePrometheus = "prometheus"
	SourceLoki       = "loki"
	SourceK8s        = "k8s"
)

// defaultCollectorTimeout 是默认查询超时（手册 §4.5：三类采集器均为 5s）。
const defaultCollectorTimeout = 5 * time.Second

// maxResponseBytes 是响应体读取上限。
//
// 外部数据源故障时返回的往往不是小错误页而是一整页 HTML
// （nginx 502 页面、Prometheus 的 landing page），不限量读会把内存吃满。
const maxResponseBytes = 4 << 20

// options 是采集器的通用配置。
type options struct {
	timeout time.Duration

	// topN 是「Top N」类证据的保留条数。
	//
	// 做成配置而不是写死 10：Top10 是手册的默认值，
	// 但不同集群一个服务能刷出上万种错误消息，
	// 条数该由部署方按自己的日志基数调，而不是改代码。
	topN int
}

// Option 是采集器构造选项。
type Option func(*options)

// WithTimeout 设置查询超时。
//
// 非正值被忽略而非生效：传 0 得到「无超时」的客户端，
// 意味着一个挂死的外部数据源能把 worker 永久占住 ——
// 而手册 §4.5 要求的是「超时即降级，不阻塞主链路」。
func WithTimeout(d time.Duration) Option {
	return func(o *options) {
		if d > 0 {
			o.timeout = d
		}
	}
}

// WithTopN 设置「Top N」类证据的保留条数。
func WithTopN(n int) Option {
	return func(o *options) {
		if n > 0 {
			o.topN = n
		}
	}
}

// resolveOptions 归一化选项。
func resolveOptions(opts []Option) options {
	o := options{timeout: defaultCollectorTimeout, topN: 10}
	for _, f := range opts {
		if f != nil {
			f(&o)
		}
	}
	return o
}

// httpGetJSON 发起一次 GET 并把响应体解析成out。
//
// 三类采集器的 HTTP 骨架完全一样（超时、限量读、非 200 转可读错误、
// 解析失败带上下文），各自抄一遍只会让三份错误信息的措辞慢慢分叉。
func httpGetJSON(ctx context.Context, client *http.Client, endpoint string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, http.NoBody)
	if err != nil {
		return fmt.Errorf("构造请求失败: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("请求失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("读取响应失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(truncate(string(body), 200)))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("解析响应失败: %w", err)
	}
	return nil
}

// parseNumeric 解析 PromQL / LogQL 返回的数值字符串。
//
// 两种查询的数值都是**字符串**（"NaN" / "+Inf" 在 JSON 里不合法），
// 但 Loki 的 count 有时直接给数字。两种都收，
// 否则 Loki 偶发返回数字时会炸在json.Unmarshal 上 ——
// 那是外部系统的行为差异，不该由我们的解析器来承担。
func parseNumeric(raw json.RawMessage) (float64, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return parseNumericString(s)
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		return f, nil
	}
	return 0, fmt.Errorf("数值 %s 既不是字符串也不是数字", truncate(string(raw), 40))
}

// parseNumericString 解析数值字符串，含 Prometheus 特有的字面量。
func parseNumericString(s string) (float64, error) {
	t := strings.TrimSpace(s)
	switch t {
	case "NaN":
		return math.NaN(), nil
	case "+Inf", "Inf":
		return math.Inf(1), nil
	case "-Inf":
		return math.Inf(-1), nil
	}
	f, err := strconv.ParseFloat(t, 64)
	if err != nil {
		return 0, fmt.Errorf("数值 %q 无法解析: %w", s, err)
	}
	return f, nil
}

// parseUnixSeconds 解析时间戳字段，返回 Unix 秒。
//
// Loki 与 Prometheus 对同一概念的类型选择并不一致
// （Prometheus 给数字，Loki 有时给字符串），两种都收。
func parseUnixSeconds(raw json.RawMessage) (int64, error) {
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		return int64(f), nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		v, pErr := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if pErr != nil {
			return 0, fmt.Errorf("时间戳 %q 无法解析: %w", s, pErr)
		}
		return int64(v), nil
	}
	return 0, fmt.Errorf("时间戳 %s 既不是数字也不是字符串", truncate(string(raw), 40))
}

// formatFloat 统一浮点格式，避免 0.30000000000000004 这类噪声进证据。
func formatFloat(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "+Inf"
	case math.IsInf(f, -1):
		return "-Inf"
	}
	return strconv.FormatFloat(f, 'f', 4, 64)
}

// ratio 计算后值相对前值的倍数。
//
// 分母为 0 时返回 0 而非 +Inf：+Inf 在 JSON 里不合法，
// 而「从 0 涨到 100」用「无可比基线」表达更诚实——
// 这个区分由 hasPrev 与 baseline 字段承担。
func ratio(after, before float64) float64 {
	if before == 0 {
		return 0
	}
	return after / before
}

// escapeLabelValue 转义 label 匹配器里的引号、反斜杠与换行。
//
// PromQL 的 `{service="x"}` 与 LogQL 的 `{service="x"}` 在这一点上语法相同，
// 所以共用一个函数。不转义的话，服务名里出现 " 会让表达式语法错误，
// 而外部服务返回的是 4xx/5xx 的 HTML 错误页 —— 排查时
// 极易误以为是「网络问题」或「对方挂了」。
func escapeLabelValue(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(s)
}

// withFailures 给 metadata 追加失败原因。
func withFailures(md map[string]string, failures []string) map[string]string {
	if md == nil {
		md = make(map[string]string, 1)
	}
	md["failures"] = strings.Join(failures, "; ")
	return md
}

// truncate 截断长文本，保留前 n 个字符。
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
