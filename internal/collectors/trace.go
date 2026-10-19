package collectors

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/yu/iip/internal/evidence"
)

// TraceCollector 从分布式追踪后端（Jaeger / Tempo 等）采链路类证据。
//
// # 本采集器的定位：只有接口与降级逻辑，不声称能连通真实后端
//
// M6 §5 的裁定：本机既没有 Jaeger 也没有 Tempo（无容器、无 kubeconfig、
// 无 RBAC 清单），因此本实现**只用 mock server 单测**证明「解析正确」与
// 「降级行为正确」，**不宣称已接入任何真实集群**。
// 写一个「看起来在工作、实际从未连过真实后端」的采集器比不写更危险 ——
// 运维会以为「证据页为什么没链路」是自己数据的问题。
// 接入是**部署侧的事**：用户自带后端 + 打开开关。这一点与 K8sCollector
// 的处理保持一致（偏差 B1），不引新规则。
//
// # 接口形状
//
// 采用 Jaeger Query API 的 /api/traces 查询形态，因为它支持我们要的
// 「按服务 + 按时间窗 + 限条数」三个参数：
//
//	GET /api/traces?service=<svc>&start=<us>&end=<us>&limit=<n>
//	-> {"data":[{"traceID":"...","spans":[...],"processes":{...}}]}
//
// Tempo 只提供按 traceID 直查的单条接口，没有按服务搜索，
// 因此**不在本采集器内实现**；两者的交集（traceID 语义）体现在证据内容里。
//
// # 无状态且并发安全
//
// 所有可变状态都在方法内，结构体只有只读配置。
type TraceCollector struct {
	baseURL string
	client  *http.Client
	topN    int
}

// NewTraceCollector 建一个 trace 采集器。
//
// baseURL 是追踪后端根地址（形如 `http://jaeger.internal:16686`）。
// **地址为空时 Collect 报错**而非静默返回空：静默返回空会让
// 「没配地址」与「这段窗口没有链路数据」在页面上长得一模一样。
func NewTraceCollector(baseURL string, opts ...Option) *TraceCollector {
	o := resolveOptions(opts)
	return &TraceCollector{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		client:  newHTTPClient(o),
		topN:    o.topN,
	}
}

// Source 返回来源标识。
func (c *TraceCollector) Source() string { return SourceTrace }

// traceWarnings 是 metadata 里的降级说明键，落「采到了但需要注意」的情况。
//
// 为什么不像别的采集器那样用 failures：**降级不等于失败**。
// 「这段窗口没有链路数据」是正常结果（服务可能压根没埋点），
// 把它记成 failures 会让页面显示「采集失败」并触发无谓重试。
const traceWarnings = "trace_warnings"

// Collect 采集窗口内的链路证据。
//
// 三条硬性约定：
//  1. **证据时间取自链路自身的 startTime**（ADR-003），绝不用 time.Now() 兜底。
//     用采集时刻会让所有链路看起来「同时发生」，而 Root Cause 恰恰依赖先后次序。
//  2. 拿不到可判定起始时间的 trace **整条丢弃**，而不是给它一个近似时间。
//  3. Query 字段必须是**可直接复制复现的完整 URL**（含起止时刻），
//     否则运维打不开原始面板，这条证据就失去意义（同 LokiCollector）。
func (c *TraceCollector) Collect(ctx context.Context, in evidence.Input) ([]evidence.Evidence, error) {
	if strings.TrimSpace(c.baseURL) == "" {
		return nil, errors.New("trace 采集器未配置后端地址")
	}
	if err := in.Window.Validate(); err != nil {
		return nil, fmt.Errorf("trace 采集窗口不合法: %w", err)
	}
	if strings.TrimSpace(in.Service) == "" {
		// Input 的契约：服务名为空表示「这个事件没有可用服务标识」，
		// 此时应跳过而不是拿空串去查 —— 空 service 在很多后端
		// 等价于「不限服务」，会把整个集群的链路都捞回来。
		return nil, nil
	}

	endpoint, err := c.buildSearchURL(in)
	if err != nil {
		return nil, err
	}
	var resp traceSearchResponse
	if err := httpGetJSON(ctx, c.client, endpoint, &resp); err != nil {
		return nil, fmt.Errorf("trace 查询失败: %w", err)
	}

	var warnings []string
	if len(resp.Data) == 0 {
		// 明确区分「查成功但没有数据」与「查失败」：
		// 前者是正常结果（Collector 契约第 2 条）。
		return []evidence.Evidence{}, nil
	}

	out := make([]evidence.Evidence, 0, len(resp.Data))
	skipped := 0
	for _, tr := range resp.Data {
		startTime, ok := tr.earliestStart()
		if !ok {
			// 时间不可判定就丢弃，不用「整批起点」或「现在」兜底。
			skipped++
			continue
		}
		root := tr.rootSpan()
		spans := len(tr.Spans)

		ev := evidence.Evidence{
			Type:      evidence.TypeTrace,
			Source:    SourceTrace,
			Query:     endpoint,
			Value:     tr.summary(root, spans),
			Timestamp: startTime.UTC(),
			Metadata: map[string]string{
				"trace_id":   tr.TraceID,
				"service":    in.Service,
				"span_count": strconv.Itoa(spans),
			},
		}
		if root != nil {
			ev.Metadata["root_operation"] = root.OperationName
			// 毫秒级保留一位小数：Jaeger 的 duration 是微秒，
			// 直接存会让页面显示 812300（单位不明）。
			ev.Metadata["duration_ms"] = fmt.Sprintf("%.1f", float64(root.Duration)/1000)
			if svc := root.serviceName(&tr); svc != "" {
				ev.Metadata["root_service"] = svc
			}
		}
		if n := tr.errorSpanCount(); n > 0 {
			ev.Metadata["error_spans"] = strconv.Itoa(n)
		}
		out = append(out, ev)
	}
	if skipped > 0 {
		warnings = append(warnings,
			fmt.Sprintf("丢弃 %d 条无法确定起始时间的 trace（不用窗口起点兜底）", skipped))
	}

	// 按耗时倒序取 Top N：故障时值班要看的是「最慢的那几条」，
	// 而不是随便 N 条。只看 N 条而不排序等于把最关键的链路随机丢掉。
	sort.SliceStable(out, func(i, j int) bool {
		return evidenceDurationMS(out[i].Metadata) > evidenceDurationMS(out[j].Metadata)
	})
	if len(out) > c.topN {
		out = out[:c.topN]
		warnings = append(warnings,
			fmt.Sprintf("窗口内 %d 条 trace，按耗时保留 Top %d", len(out), c.topN))
	}

	if len(warnings) > 0 {
		for i := range out {
			out[i].Metadata[traceWarnings] = strings.Join(warnings, "; ")
		}
	}
	return out, nil
}

// buildSearchURL 拼查询 URL。
//
// 时间参数必须是**微秒**：Jaeger 的 start/end 用 Unix 微秒，
// 传 10 位秒级会被当作 1970 年，症状是「接口返回 200 但永远空」——
// 比直接报错难查得多（这一点与 Loki 直达链接的毫秒要求同源）。
func (c *TraceCollector) buildSearchURL(in evidence.Input) (string, error) {
	q := url.Values{}
	q.Set("service", strings.TrimSpace(in.Service))
	q.Set("start", strconv.FormatInt(in.Window.Start.UTC().UnixMicro(), 10))
	q.Set("end", strconv.FormatInt(in.Window.End.UTC().UnixMicro(), 10))
	q.Set("limit", strconv.Itoa(defaultSearchLimit))
	u, err := url.Parse(c.baseURL + "/api/traces")
	if err != nil {
		return "", fmt.Errorf("trace 后端地址不合法: %w", err)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// defaultSearchLimit 是向后端请求的最大条数。
//
// 与 topN 分开：topN 是「留给证据的条数」，而向后端多要一些
// 才能在**全部结果**里挑出最慢的 —— 只向后端要 topN 条，
// 排序就变成了在后端给的任意子集里排，没有意义。
const defaultSearchLimit = 100

// evidenceDurationMS 从 metadata 取耗时，供排序使用。取不到按 0 处理。
//
// 不把它做成 Evidence 的字段是有意的：metadata 才是不定结构的容器，
// 为排序临时加一个强类型字段会让结构体随排序需求增长。
func evidenceDurationMS(md map[string]string) float64 {
	if md == nil {
		return 0
	}
	v, err := strconv.ParseFloat(md["duration_ms"], 64)
	if err != nil {
		return 0
	}
	return v
}
