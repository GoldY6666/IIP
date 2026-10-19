// Package collectors 提供 M4 的证据采集器实现。
//
// 本文件是 Prometheus 采集器（T4.3）：按 service label 查 RED 指标，
// 做**窗口前后对比**，把对比结果作为证据落库。
// 通用部分（超时、HTTP 骨架、数值解析）在 collectors.go。
//
// 设计要点（每条都有理由，不是默认值抄来的）：
//
//  1. **窗口前后对比，而不是单点取值。** RED 指标（Rate / Errors / Duration）
//     的绝对值说明不了问题：一个服务 QPS 1000、错误率 0.1% 可能完全正常。
//     有意义的是「事故窗口内的错误率比事故前高了多少倍」。
//
//  2. **query 字段存可复现的 PromQL。** 验收标准第 2 条要求
//     「query 字段可复制后复现」—— 存一句人话（"错误率上升"）无法复现。
//
//  3. **降级不阻塞。** 手册 §4.5：超时 5s，失败只记 error 日志，
//     其余采集器照常落库。外部数据源宕机时告警主链路的延迟不能有变化。
//
//  4. **不给真实时间的证据不写**（§4.4）：Prometheus 返回的样本自带时间戳，
//     直接用；没有样本时间就丢弃这条证据，不用采集时刻兜底。
package collectors

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/yu/iip/internal/evidence"
)

// PrometheusCollector 从 Prometheus 查 RED 指标并产出证据。
//
// **无状态且并发安全**（evidence.Collector 的硬契约）：
// 所有可变状态都在方法内部，结构体只有两个只读配置字段。
type PrometheusCollector struct {
	baseURL string
	client  *http.Client
}

// NewPrometheusCollector 建一个 Prometheus 采集器。
func NewPrometheusCollector(baseURL string, opts ...Option) *PrometheusCollector {
	o := resolveOptions(opts)
	return &PrometheusCollector{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		client:  newHTTPClient(o),
	}
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

// errNoData 表示「查询成功但该区间没有样本」。
//
// 单独一个哨兵错误（而不是返回零值 sample）：零值 0 会被当成
// 「指标确实是 0」，而「没有数据」与「值为 0」在证据里含义完全不同。
var errNoData = errors.New("该区间无样本")
