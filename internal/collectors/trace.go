package collectors

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

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

// traceSearchResponse 是 /api/traces 的响应形状。
//
// Total / Limit / Offset 三个字段**刻意不解析**：它们对生成证据没有用处，
// 而 Jaeger 与 Tempo 在这几个字段上的类型不一致（有的给数字、有的给字符串），
// 收进来只会多一处解析失败的可能。
type traceSearchResponse struct {
	Data []traceData `json:"data"`
}

// traceData 是一条 trace。
type traceData struct {
	TraceID string      `json:"traceID"`
	Spans   []traceSpan `json:"spans"`

	// ProcPool 是进程定义池（processID -> process）。
	//
	// 放在 trace 而不是 span 上，是因为 Jaeger 把它放在这一层 ——
	// 挂在 span 上的话每个 span 都会重复携带同一份进程定义，
	// 而且多数后端实际并不会在每个 span 里内联它。
	ProcPool map[string]traceProcess `json:"processes"`
}

// traceSpan 是一个 span。
type traceSpan struct {
	SpanID        string `json:"spanID"`
	ParentSpanID  string `json:"parentSpanID"`
	OperationName string `json:"operationName"`
	ProcessID     string `json:"processID"`

	//StartTime 是 Unix 微秒。
	//
	// 用 json.RawMessage 而非 int64：极少部分后端会把这里给成字符串，
	// 而整批响应解析失败会让**窗口内所有链路证据一起丢失** ——
	// 一个字段的类型差异不该有这么大的杀伤范围。
	StartTime json.RawMessage `json:"startTime"`
	Duration  int64           `json:"duration"`

	Tags    []traceTag    `json:"tags"`
	Process *traceProcess `json:"process"`
}

// traceTag 是 span 上的标签。
type traceTag struct {
	Key   string          `json:"key"`
	Type  string          `json:"type"`
	Value json.RawMessage `json:"value"`
}

// traceProcess 是 span 所属进程。
type traceProcess struct {
	ServiceName string `json:"serviceName"`
}

// earliestStart 返回该 trace 最早的 span 起始时刻。
//
// 取**最早**而不是「根 span」的时刻：真正的根 span 可能没被上报
// （采样、跨服务埋点缺失都会造成这种情况），此时拿不到根就没有时间。
// 而「这条 trace 最早于何时开始」总是有答案的。
func (t traceData) earliestStart() (time.Time, bool) {
	best := int64(math.MaxInt64)
	found := false
	for i := range t.Spans {
		us, ok := parseUnixMicros(t.Spans[i].StartTime)
		if !ok {
			continue
		}
		if us < best {
			best = us
		}
		found = true
	}
	if !found {
		return time.Time{}, false
	}
	return time.UnixMicro(best), true
}

// rootSpan 找根 span，找不到返回 nil。
//
// 两步判定而非只看有没有 parentSpanID：
//  1. 没有父 span 的就是根；
//  2. 全都带父 ID（父没被上报）时，退化取**起始最早**且**结束最晚**的那个。
//
// 这只是一个**启发式**，故 root 为 nil 时证据照样产出（Value 里说清来历），
// 不因此丢证据 —— 少一条 metadata 远好过丢一条链路。
func (t traceData) rootSpan() *traceSpan {
	for i := range t.Spans {
		if strings.TrimSpace(t.Spans[i].ParentSpanID) == "" {
			return &t.Spans[i]
		}
	}
	bestIdx := -1
	var bestEnd int64 = -1
	for i := range t.Spans {
		us, ok := parseUnixMicros(t.Spans[i].StartTime)
		if !ok {
			continue
		}
		end := us + t.Spans[i].Duration
		if end > bestEnd {
			bestEnd = end
			bestIdx = i
		}
	}
	if bestIdx < 0 {
		return nil
	}
	return &t.Spans[bestIdx]
}

// serviceName 取 span 的服务名。
//
// 两个来源都要试：Jaeger 老版本的 span 里内联 process，
// 新的（以及大部分导出器）把进程定义提到 trace 的 processes 池里，
// span 只留 processID 引用。只认一个来源会在换后端后
// **静默丢掉服务名** —— 而服务名是页面上分组的依据。
func (s *traceSpan) serviceName(t *traceData) string {
	if s.Process != nil && strings.TrimSpace(s.Process.ServiceName) != "" {
		return strings.TrimSpace(s.Process.ServiceName)
	}
	if p, ok := t.ProcPool[s.ProcessID]; ok && strings.TrimSpace(p.ServiceName) != "" {
		return strings.TrimSpace(p.ServiceName)
	}
	return ""
}

// errorSpanCount 统计「有错误迹象」的 span 数。
//
// 判据只有两条、都写死在这里：
//   - tag `error` 为真值（Jaeger 生态的惯例）；
//   - `http.status_code` >= 400。
//
// 刻意**不做更多推断**（比如按 operationName 猜）：推断会让
// 「有多少 span 出错」变成一个随数据形态漂移的数字，无法复核。
func (t traceData) errorSpanCount() int {
	n := 0
	for i := range t.Spans {
		for _, tag := range t.Spans[i].Tags {
			switch tag.Key {
			case "error":
				if tag.boolValue() {
					n++
				}
			case "http.status_code":
				if code, ok := tag.intValue(); ok && code >= 400 {
					n++
				}
			}
		}
	}
	return n
}

// boolValue 取布尔标签值。非 bool（如字符串 "true"）也算 true。
//
// 为什么要兼容字符串：各语言 SDK 写 error tag 的类型并不统一，
// Jaeger 也有曾把 bool 序列化成字符串的客户端。一律按 JSON bool 解析的话，
// 那些语言的错误 span 会被算成「没有错误」，而这个偏差是**静默**的。
func (t traceTag) boolValue() bool {
	var b bool
	if err := json.Unmarshal(t.Value, &b); err == nil {
		return b
	}
	var s string
	if err := json.Unmarshal(t.Value, &s); err == nil {
		return strings.EqualFold(strings.TrimSpace(s), "true")
	}
	return false
}

// intValue 取整型标签值。字符串形态的数字也收。
func (t traceTag) intValue() (int64, bool) {
	var n int64
	if err := json.Unmarshal(t.Value, &n); err == nil {
		return n, true
	}
	var f float64
	if err := json.Unmarshal(t.Value, &f); err == nil {
		return int64(f), true
	}
	var s string
	if err := json.Unmarshal(t.Value, &s); err == nil {
		if v, pErr := strconv.ParseInt(strings.TrimSpace(s), 10, 64); pErr == nil {
			return v, true
		}
	}
	return 0, false
}

// summary 生成人类可读的一句话摘要。
//
// 不写 Confidence：本采集器**没有评估置信度的能力**，
// 填任何值都等于无依据地宣称（OPEN-M4-1，M6 S1 才引入评估依据）。
func (t traceData) summary(root *traceSpan, spans int) string {
	parts := make([]string, 0, 4)
	if root != nil {
		parts = append(parts,
			"根="+firstNonEmpty(root.OperationName, "<未命名 span>"),
			fmt.Sprintf("耗时 %.1fms", float64(root.Duration)/1000))
	}
	parts = append(parts, fmt.Sprintf("%d span", spans))
	if n := t.errorSpanCount(); n > 0 {
		parts = append(parts, fmt.Sprintf("%d 个 span 出错", n))
	}
	return fmt.Sprintf("trace %s：%s", shortTraceID(t.TraceID), strings.Join(parts, "，"))
}

// firstNonEmpty 返回第一个非空串。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// shortTraceID 截断 traceID 便于展示。
//
// 32 位的十六进制 traceID 会把列表的一列撑爆。
// 保留前 8 位：足以人工区分同一窗口内的 trace，
// 而完整的 ID 仍在 metadata.trace_id 里（那里不截断）。
const shortTraceIDLen = 8

func shortTraceID(id string) string {
	if len(id) <= shortTraceIDLen {
		return id
	}
	return id[:shortTraceIDLen]
}

// parseUnixMicros 解析微秒时间戳。
//
// 返回 (值, 是否可用) 而不是 (值, error)：
// 调用方对本函数的失败处理策略各不相同（有的跳过、有的判全批失败），
// 让它自己决定怎么包装错误比返回 error 更清楚。
// parseUnixMicros 解析微秒时间戳。
//
// 返回 (值, 是否可用) 而不是 (值, error)：
// 调用方对本函数的失败处理策略各不相同（有的跳过、有的判全批失败），
// 让它自己决定怎么包装错误比返回 error 更清楚。
//
// **0 与负值判为不可用**，这是单测抓出来的真实缺陷：
// JSON 的 `null` 能被 json.Unmarshal 顺利解析成 int64 的零值，
// 于是 time.UnixMicro(0) 得到 1970-01-01 —— 而它的 IsZero() 是 **false**，
// 会一路穿过 Evidence.Validate() 落到库里，变成
// 「这条 trace 发生在 1970 年」。那正是 ADR-003 禁止的伪造时间，
// 且症状只在页面显示一条离谱日期时才可见。
func parseUnixMicros(raw json.RawMessage) (int64, bool) {
	var n int64
	if err := json.Unmarshal(raw, &n); err == nil {
		return n, n > 0
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		n = int64(f)
		return n, n > 0
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		s = strings.TrimSpace(s)
		if v, pErr := strconv.ParseInt(s, 10, 64); pErr == nil {
			return v, v > 0
		}
		if v, pErr := strconv.ParseFloat(s, 64); pErr == nil {
			n = int64(v)
			return n, n > 0
		}
	}
	return 0, false
}
