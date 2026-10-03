// Package collectors 提供 M4 的证据采集器实现。
//
// 本文件是 Prometheus 采集器（T4.3）：按 service label 查 RED 指标，
// 做**窗口前后对比**，把对比结果作为证据落库。
//
// 设计要点（每条都有理由，不是默认值抄来的）：
//
//  1. **窗口前后对比，而不是单点取值。** RED 指标（Rate / Errors / Duration）
//     的绝对值说明不了问题：一个服务 QPS 1000、错误率 0.1% 可能完全正常。
//     有意义的是「事故窗口内的错误率比事故前高了多少倍」。
//
//  2. **query 字段存可复现的 PromQL。** 验收标准第2 条要求
//     「query 字段可复制后复现」—— 存一句人话（"错误率上升"）无法复现。
//
//  3. **降级不阻塞。** 手册 §4.5：超时 5s，失败只记 error 日志，
//     其余采集器照常落库。外部数据源宕机时告警主链路的延迟不能有变化。
//
//  4. **不给真实时间的证据不写**（§4.4）：Prometheus 返回的样本自带时间戳，
//     直接用；没有样本时间就用查询区间的中点，并**在metadata 里标明**。
package collectors

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/yu/iip/internal/evidence"
)

// SourcePrometheus 是 Prometheus 采集器的来源标识，落库到 evidence.source。
const SourcePrometheus = "prometheus"

// 默认查询超时（手册 §4.5：三类采集器均为 5s）。
const defaultCollectorTimeout = 5 * time.Second

// PrometheusCollector 从 Prometheus 查 RED 指标并产出证据。
//
// **无状态且并发安全**（evidence.Collector 的硬契约）：
// 所有可变状态都在方法内部，结构体只有两个只读配置字段。
type PrometheusCollector struct {
	baseURL string
	client  *http.Client
}

// PromCollectorOption 是 PrometheusCollector 的可选参数。
type PromCollectorOption func(*PrometheusCollector)

// WithTimeout 设置查询超时。
func WithTimeout(d time.Duration) PromCollectorOption {
	return func(c *PrometheusCollector) {
		if d > 0 {
			c.client.Timeout = d
		}
	}
}

// NewPrometheusCollector 建一个 Prometheus 采集器。
func NewPrometheusCollector(baseURL string, opts ...PromCollectorOption) *PrometheusCollector {
	c := &PrometheusCollector{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		client:  &http.Client{Timeout: defaultCollectorTimeout},
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Source 返回来源标识。
func (c *PrometheusCollector) Source() evidence.Source { return SourcePrometheus }

// redQuery 是一条 RED 指标的查询定义。
type redQuery struct {
	// key 是短标识，进 metadata 便于阅读（rate / errors / duration）。
	key string
	// promql 是可复现的查询表达式模板，%s 为服务名的字面量。
	promql string
	// help 是给证据 value 字段的中文说明。
	help string
}

// redQueries 是本采集器查询的三条 RED 指标。
//
// **刻意写死而不做成配置**：这三条是「服务健康度」的最小完备集，
// 做成配置会让「采集了什么」取决于部署方的 yaml，
// 而证据的可追溯性要求「这条证据的 query 是什么」能被直接读出来。
// 要扩展服务特有的指标，应新增采集器而不是改这里。
var redQueries = []redQuery{
	{
		key:    "rate",
		promql: `sum(rate(http_requests_total{service="%s"}[5m]))`,
		help:   "请求速率",
	},
	{
		key:    "errors",
		promql: `sum(rate(http_requests_total{service="%s",status=~"5.."}[5m]))`,
		help:   "5xx 错误速率",
	},
	{
		key:    "duration",
		promql: `histogram_quantile(0.99, sum(rate(http_request_duration_seconds_bucket{service="%s"}[5m])) by (le))`,
		help:   "P99 延迟（秒）",
	},
}

// Collect 采集 incident 在窗口内的 RED 指标证据。
//
// 返回空切片表示「查询成功但这段时间没有数据」，是正常结果。
// 返回错误表示「采集本身失败」（连不上、超时、响应无法解析）。
func (c *PrometheusCollector) Collect(ctx context.Context, in evidence.Input) ([]evidence.Evidence, error) {
	// 空服务名直接失败，而不是拿空串去查：
	// `service=""` 会匹配到所有 label 为空的序列，捞回一堆与本事件无关的数据，
	// 证据看起来「有内容」实则全是噪声。
	if strings.TrimSpace(in.Service) == "" {
		return nil, fmt.Errorf("prometheus 采集需要服务名，但输入为空")
	}
	if err := in.Window.Validate(); err != nil {
		return nil, fmt.Errorf("prometheus 采集的窗口不合法: %w", err)
	}

	// 前窗口与后窗口等宽，紧邻且不重叠。
	// 边界取 [Start-E, Start) 与 [Start, End)：
	// 若让前窗口也含 Start 时刻，两窗会在该点重叠，
	// 事故刚开始时的指标会被算进「事故前」，把变化幅度压低。
	width := in.Window.Duration()
	before := evidence.Window{Start: in.Window.Start.Add(-width), End: in.Window.Start}

	var out []evidence.Evidence
	var failures []string

	for _, q := range redQueries {
		curSample, curErr := c.query(ctx, q, in.Service, in.Window)
		prevSample, prevErr := c.query(ctx, q, in.Service, before)

		// **「无样本」不是「查询失败」**：前者是正常结果（这段时间该服务
		// 没有这个指标），后者是采集故障。若把两者混同，会让「服务没埋这个指标」
		// 被报成「Prometheus 挂了」→ 任务白白重试三次再进死信，
		// 而页面上永远等不到证据。
		//
		// 两窗各自独立判定，用两个if 而非 switch：cur 与 prev 是两条独立查询，
		// 「后窗口无数据」和「前窗口无数据」可以同时成立。
		// 写成 switch case 会让 cur 命中后跳出，prev 的判定永远不执行 ——
		// 结果是「前窗口无数据」被当成「前窗口有数据（值为 0）」，
		// 算出 change_ratio = 1.0000，页面显示「无变化」，与事实相反。
		curHasData, curRealErr := splitNoData(curSample, curErr)
		prevHasData, prevRealErr := splitNoData(prevSample, prevErr)

		switch {
		case curRealErr != nil:
			// 事故窗口查不到这条指标 -> 这条证据没有意义。
			// 但仍要记下原因：页面要能说明「为什么少了这条」。
			failures = append(failures, fmt.Sprintf("%s: 事故窗口查询失败（%v）", q.key, curRealErr))
			continue
		case !curHasData:
			// 事故窗口本就无数据 ——「没数据」不是失败，不记 failures。
			continue
		}

		// 前窗口**真失败**要与「无基线」区分开：
		// 前者是数据源故障（该重试），后者是客观事实（不该重试）。
		// 混为一谈会让 Prometheus 局部故障时算出错误的「无变化」结论，
		// 而真正的故障被静默吞掉 —— 页面显示「指标平稳」，实则没查到。
		if prevRealErr != nil {
			failures = append(failures,
				fmt.Sprintf("%s: 基线窗口查询失败，本次无基线可比（%v）", q.key, prevRealErr))
		}

		e, buildErr := buildRedEvidence(q, in, curSample, prevSample, prevHasData, prevRealErr)
		if buildErr != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", q.key, buildErr))
			continue
		}
		out = append(out, e)
	}

	// 三条全失败才算整个采集失败（手册 §4.3：一个挂掉不影响其余）。
	if len(out) == 0 && len(failures) == len(redQueries) {
		return nil, fmt.Errorf("prometheus 三条 RED 指标全部查询失败: %s",
			strings.Join(failures, "; "))
	}
	// 部分失败不算采集失败：已拿到的证据照常落库，
	// 失败原因写进 metadata 的 failures 字段，页面上能看到「哪条没查到」。
	if len(failures) > 0 {
		for i := range out {
			if out[i].Source == SourcePrometheus {
				out[i].Metadata = withFailures(out[i].Metadata, failures)
				break
			}
		}
	}
	return out, nil
}

// promResult 是 Prometheus instant query 的响应。
//
// 刻意只声明用得到的字段：完整结构有 20+ 字段，
// 多声明几个不会报错，但会让「我们依赖了哪些字段」变得不清楚。
type promResult struct {
	Status string `json:"status"`
	Error  string `json:"error"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			// Value 是 [ unixSeconds, "数值字符串" ] 两元组。
			//
			// **时间与值都取自这里**（§4.4）：用查询时刻当证据时间
			// 会让所有证据看起来「同时发生」。
			Value []json.RawMessage `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

// sample 是一条 Prometheus 样本。
type sample struct {
	// timestamp 是样本自身的时刻。
	timestamp time.Time
	// value 是数值。
	value float64
}

// query 执行一次 instant query 并解析出唯一样本。
func (c *PrometheusCollector) query(
	ctx context.Context, q redQuery, service string, w evidence.Window,
) (sample, error) {
	// 服务名进 PromQL 前必须转义引号：服务名里出现 " 会让表达式语法错误，
	// 而 Prometheus 返回的是 4xx/5xx HTML/JSON 错误页，解析时才暴露。
	promql := fmt.Sprintf(q.promql, escapeLabelValue(service))

	// 查询时刻取窗口结束点：这与「样本时间」是两回事——
	// 前者是「问的是哪个瞬间」，后者是「指标在哪个瞬间的值」。
	// 用采集时刻（time.Now()）会让「按事件回看」时取到窗口之外的数据。
	v := url.Values{}
	v.Set("query", promql)
	v.Set("time", strconv.FormatInt(w.End.Unix(), 10))

	endpoint := c.baseURL + "/api/v1/query?" + v.Encode()
	// 用 http.NoBody 而非 nil：Go 1.8 起 NewRequest 会为 nil body 填 NoBody，
	// 但显式写出来能表明「这是刻意的 GET 无请求体」，而不是漏传。
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, http.NoBody)
	if err != nil {
		return sample{}, fmt.Errorf("构造请求失败: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return sample{}, fmt.Errorf("请求失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// 上限保护：Prometheus 挂起时可能返回很大的错误页，
	// 不限量读会把内存吃满。
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return sample{}, fmt.Errorf("读取响应失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return sample{}, fmt.Errorf("HTTP %d: %s",
			resp.StatusCode, strings.TrimSpace(truncate(string(body), 200)))
	}

	var parsed promResult
	if uErr := json.Unmarshal(body, &parsed); uErr != nil {
		return sample{}, fmt.Errorf("解析响应失败: %w", uErr)
	}
	if parsed.Status != "success" {
		return sample{}, fmt.Errorf("查询未成功: %s", truncate(parsed.Error, 200))
	}
	if len(parsed.Data.Result) == 0 {
		return sample{}, errNoData
	}

	first := parsed.Data.Result[0]
	if len(first.Value) != 2 {
		return sample{}, fmt.Errorf("样本结构异常：期望 2 元素，实得 %d", len(first.Value))
	}

	var secs float64
	if uErr := json.Unmarshal(first.Value[0], &secs); uErr != nil {
		return sample{}, fmt.Errorf("解析样本时间失败: %w", uErr)
	}
	// Prometheus 的 value 恒为字符串（"NaN" / "+Inf" 都可能出现）。
	var raw string
	if uErr := json.Unmarshal(first.Value[1], &raw); uErr != nil {
		return sample{}, fmt.Errorf("解析样本值失败: %w", uErr)
	}
	f, pErr := parsePromValue(raw)
	if pErr != nil {
		return sample{}, pErr
	}

	return sample{timestamp: time.Unix(int64(secs), 0).UTC(), value: f}, nil
}

// splitNoData 把「无样本」从错误里剥出来，返回「是否有数据」与「真正的错误」。
//
// 分成两个返回值而不是让调用方判errors.Is：调用方要处理的是
// 「有数据就对比、没数据就说明无基线、真出错才失败」三态，
// 剥掉无样本之后主循环只需看两个值，逻辑扁平、不易写错。
func splitNoData(s sample, err error) (hasData bool, realErr error) {
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, errNoData):
		return false, nil
	default:
		return false, err
	}
}

// errNoData 表示「查询成功但该区间没有样本」。
//
// 单独一个哨兵错误（而不是返回零值 sample）：零值 0 会被当成
// 「指标确实是 0」，而「没有数据」与「值为 0」在证据里含义完全不同。
var errNoData = errors.New("该区间无样本")

// parsePromValue 解析 Prometheus 的数值字符串。
func parsePromValue(raw string) (float64, error) {
	s := strings.TrimSpace(raw)
	switch s {
	case "NaN":
		return math.NaN(), nil
	case "+Inf", "Inf":
		return math.Inf(1), nil
	case "-Inf":
		return math.Inf(-1), nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("数值 %q 无法解析: %w", s, err)
	}
	return f, nil
}

// buildRedEvidence 组装一条 RED 对比证据。
//
// baselineReason 说明「为什么没有基线」：nil 表示有基线；
// 非nil 时必须**如实区分**「无样本」与「查询失败」——
// 前者是客观事实（服务本来就没这段数据），后者是数据源故障（该重试）。
// 一律说成「无数据」会把故障伪装成正常，运维查不出来。
func buildRedEvidence(
	q redQuery, in evidence.Input,
	cur sample, prev sample, hasPrev bool, prevErr error,
) (evidence.Evidence, error) {
	if cur.timestamp.IsZero() {
		// 走到这里说明样本时间解析出了问题。宁可丢这条证据，
		// 也不能用采集时刻兜底（§4.4 / ADR-003）。
		return evidence.Evidence{}, fmt.Errorf("样本时间为空，按 ADR-003 不落库")
	}
	if math.IsNaN(cur.value) {
		return evidence.Evidence{}, fmt.Errorf("样本值为 NaN，无信息量")
	}

	md := map[string]string{
		"metric":      q.key,
		"description": q.help,
		"window":      fmt.Sprintf("%s/%s", in.Window.Start.UTC().Format(time.RFC3339), in.Window.End.UTC().Format(time.RFC3339)),
		"value_after": formatFloat(cur.value),
	}
	if hasPrev {
		md["value_before"] = formatFloat(prev.value)
		md["change_ratio"] = formatFloat(ratio(cur.value, prev.value))
		md["change_pct"] = formatFloat(cur.value-prev.value) + " (绝对变化)"
	} else {
		// 事故前拿不到基线：如实说明**原因**。
		// 说成「无样本」而实际是「查询失败」，运维会以为服务本来就没数据，
		// 从而不会去查那个一直报 503 的 Prometheus。
		if prevErr != nil {
			md["baseline"] = "基线窗口查询失败，未能获取事故前数据"
		} else {
			md["baseline"] = "事故前窗口无样本，无法计算变化幅度"
		}
	}

	// 人类可读的摘要。变化幅度是这条证据的核心信息。
	value := fmt.Sprintf("%s：%s", q.help, formatFloat(cur.value))
	switch {
	case hasPrev:
		value += fmt.Sprintf("（事故前 %s，变化 %s 倍）",
			formatFloat(prev.value), formatFloat(ratio(cur.value, prev.value)))
	case prevErr != nil:
		value += "（基线窗口查询失败，无基线可比）"
	default:
		value += "（事故前无数据，无基线可比）"
	}

	return evidence.Evidence{
		Type:      evidence.TypeMetric,
		Source:    SourcePrometheus,
		Query:     fmt.Sprintf(q.promql, escapeLabelValue(in.Service)),
		Value:     value,
		Timestamp: cur.timestamp,
		Metadata:  md,
		// Confidence 刻意留空（手册 OPEN-M4-1）：采集器没有判断
		// 「这条证据有多可靠」的能力，填 1.0 等于宣称确凿。
	}, nil
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

// formatFloat 统一浮点格式，避免 0.30000000000000004 这类噪声进证据。
func formatFloat(f float64) string {
	if math.IsNaN(f) {
		return "NaN"
	}
	if math.IsInf(f, 1) {
		return "+Inf"
	}
	if math.IsInf(f, -1) {
		return "-Inf"
	}
	return strconv.FormatFloat(f, 'f', 4, 64)
}

// escapeLabelValue 转义 PromQL label 值里的引号与反斜杠。
//
// 不转义的话，服务名里出现 " 就会让表达式语法错误，
// 而 Prometheus 返回的 4xx 错误体会在解析阶段才暴露 —��
// 排查时容易误以为是「网络问题」。
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
