package collectors

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

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
