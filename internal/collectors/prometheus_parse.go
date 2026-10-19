package collectors

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"time"

	"github.com/yu/iip/internal/evidence"
)

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

	var parsed promResult
	if err := httpGetJSON(ctx, c.client, endpoint, &parsed); err != nil {
		return sample{}, err
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

	// 样本时间与值都取自响应（§4.4），一个都不拿采集时刻兜底。
	secs, err := parseUnixSeconds(first.Value[0])
	if err != nil {
		return sample{}, fmt.Errorf("解析样本时间失败: %w", err)
	}
	f, err := parseNumeric(first.Value[1])
	if err != nil {
		return sample{}, fmt.Errorf("解析样本值失败: %w", err)
	}

	return sample{timestamp: time.Unix(secs, 0).UTC(), value: f}, nil
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

// parsePromValue 解析 Prometheus 的数值字符串。
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
