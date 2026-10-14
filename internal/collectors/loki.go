// 本文件是 Loki 采集器（T4.4）：用 LogQL 查该服务的错误日志，
// 产出「错误模式 Top N」与「错误量突增」两类证据。
// 通用部分（超时、HTTP 骨架、数值解析）在 collectors.go。
//
// 设计要点（每条都有理由）：
//
//  1. **Top N 按消息模式聚合，不是按条。** 一次数据库连接池打满会刷出
//     一万条几乎相同的 "connection refused" —— 按条排，前十条全是同一句话，
//     什么信息都没有。LogQL 侧用 `| label_format` + `sum by (msg)` 之外的
//     做法在本项目里不可行（我们不控制对方的 label 结构），
//     所以聚合在**采集器侧**做：拉取有限条数后在本地归一化。
//
//  2. **突增必须和「绝对量」一起给。** 「错误 5 条」可能完全正常
//     （每天固定 5 条探测失败），「错误从 0涨到 5 条」才是信号。
//     只给绝对量的证据会让页面在低流量服务上永远显示红色。
//
//  3. **错误级别用正则而非单个 label 值。** 真实集群里级别可能是
//     `level="error"` / `level="ERROR"` / `level="err"` / `level="E"`，
//     只匹配一个值会漏掉其余三种。用 `|~ "(?i)error|err|fatal|panic"`
//     覆盖常见写法，并在 metadata 里记下用的正则 —— 漏匹配时用户能自己看出来
//     该改哪里，而不是面对「没有错误日志」却不知道是不是采集器坏了。
//
//  4. **不给真实时间的证据不写**（§4.4）：时间取日志条目自带的 ns 时间戳。
package collectors

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/yu/iip/internal/evidence"
)

// lokiResponse 是 Loki query_range 的响应。
//
// 刻意只声明用得到的字段：完整结构有 20+ 字段，
// 多声明几个不会报错，但会让「我们依赖了哪些字段」变得不清楚。
type lokiResponse struct {
	Status string `json:"status"`
	Error  string `json:"error"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			// Values 是 [ unixNanos, "日志正文" ] 二元组的列表。
			//
			// **时间与正文都取自这里**（§4.4）：用采集时刻当证据时间
			// 会让所有证据看起来「同时发生」。
			Values [][]json.RawMessage `json:"values"`
		} `json:"result"`
	} `json:"data"`
}

// LokiCollector 从 Loki 查错误日志并产出证据。
//
// **无状态且并发安全**（evidence.Collector 的硬契约）。
type LokiCollector struct {
	baseURL string
	client  *http.Client
	topN    int
}

// errorPattern 是匹配错误日志的 LogQL 行过滤正则。
//
// 写成 (?i) 大小写不敏感且涵盖常见写法（见文件头第 3 条）。
// 刻意**不含** `warn`：警告通常是常态噪声，收进来会让 Top N 被
// 「每天一条的启动警告」占满，而真正的事故原因被挤出去。
const errorPattern = `(?i)error|err=|fatal|panic|exception|failed|failure|timeout`

// NewLokiCollector 建一个 Loki 采集器。
func NewLokiCollector(baseURL string, opts ...Option) *LokiCollector {
	o := resolveOptions(opts)
	return &LokiCollector{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		client:  newHTTPClient(o),
		topN:    o.topN,
	}
}

// Source 返回来源标识。
func (c *LokiCollector) Source() evidence.Source { return SourceLoki }

// Collect 采集 incident 在窗口内的错误日志证据。
func (c *LokiCollector) Collect(ctx context.Context, in evidence.Input) ([]evidence.Evidence, error) {
	// 空服务名直接失败：`{service=""}` 会匹配到**所有**没有 service label 的
	// 日志流，捞回整集群的错误日志，证据看起来「有内容」实则全是噪声。
	if strings.TrimSpace(in.Service) == "" {
		return nil, fmt.Errorf("loki 采集需要服务名，但输入为空")
	}
	if err := in.Window.Validate(); err != nil {
		return nil, fmt.Errorf("loki 采集的窗口不合法: %w", err)
	}

	// 突增对比需要基线：与 Prometheus 采集器同样取**等宽紧邻**的前窗口。
	// 不等宽则两个总量不可比（同样是 100 条错误，1 小时和 1 分钟差 60 倍）。
	width := in.Window.Duration()
	before := evidence.Window{Start: in.Window.Start.Add(-width), End: in.Window.Start}

	curEntries, curErr := c.queryRange(ctx, in.Service, in.Window)
	prevEntries, prevErr := c.queryRange(ctx, in.Service, before)

	// 与 Prometheus 同样的三态判定：
	// 「查到了但没有错误日志」是**正常结果**（这个服务很健康），
	// 「查询失败」才是故障。把前者报成后者会让任务白白重试三次进死信。
	curErr = stripNoLogs(curErr)
	prevErr = stripNoLogs(prevErr)

	switch {
	case curErr != nil && prevErr != nil:
		// 两个窗口都失败才算采集失败。基线缺失不影响 Top N 证据的价值，
		// 所以只在「主窗口也失败」时让整个采集失败。
		return nil, fmt.Errorf("loki 事故窗口与基线窗口均查询失败: %v / %v", curErr, prevErr)
	case curErr != nil:
		return nil, fmt.Errorf("loki 事故窗口查询失败: %w", curErr)
	}

	// 事故窗口零条 = 这个服务很健康，是「采到了但无内容」。
	// 返回空切片 + nil，让上层知道「查过了，确实没有」，
	// 而不是当成采集失败去重试三次。
	if len(curEntries) == 0 {
		return []evidence.Evidence{}, nil
	}

	// 基线状态要单独判定：它是「零条」「失败」还是「可用」，
	// 三者对 evidence 里怎么措辞的影响完全不同（见 baselineState）。
	base := classifyBaseline(prevEntries, prevErr)

	out := make([]evidence.Evidence, 0, c.topN+1)
	patterns := topPatterns(curEntries, c.topN)

	var failures []string
	// 只有「真失败」才记 failures。基线本就无日志是客观事实，
	// 把它写成失败会让页面显示一条红色告警，而那其实说明服务很健康。
	if base == baselineFailed {
		failures = append(failures,
			fmt.Sprintf("基线窗口查询失败，无法计算错误量突增（%v）", prevErr))
	}

	for i, p := range patterns {
		e, buildErr := buildLokiPatternEvidence(in, p, i+1, prevEntries, base)
		if buildErr != nil {
			failures = append(failures, fmt.Sprintf("错误模式 %q: %v", p.repr(), buildErr))
			continue
		}
		out = append(out, e)
	}

	// 突增证据独立成条：它回答的是「错误量涨了多少倍」，
	// 与 Top N 回答的「是哪几类错误」是不同的问题，混在一条里两边都说不清。
	burst, burstErr := buildLokiBurstEvidence(in, curEntries, prevEntries, base, prevErr)
	if burstErr != nil {
		failures = append(failures, fmt.Sprintf("错误量突增: %v", burstErr))
	} else {
		out = append(out, burst)
	}

	if len(failures) > 0 {
		for i := range out {
			out[i].Metadata = withFailures(out[i].Metadata, failures)
			break
		}
	}
	return out, nil
}

// lokiEntry 是一条错误日志。
type lokiEntry struct {
	// ts 是日志自身的时间（纳秒精度来自 Loki，落库时截到秒）。
	ts time.Time
	// line 是日志正文，归一化后用于聚合。
	line string
}

// logPattern 是一个归一化后的错误模式及其统计。
type logPattern struct {
	// key 是归一化后的模式（数字与引号已抹掉）。
	key string
	// count 是事故窗口内命中该模式的条数。
	count int
	// sample 是该模式的一条**原文**样本，进evidence.value 供人阅读。
	//
	// 保留原文而不是归一化后的 key：key 是给聚合用的，
	// 「10.0.0.5 连接超时」与「10.0.0.9 连接超时」聚合后是同一个 key，
	// 但运维需要看到真实的那一行。
	sample string
	// last 是该模式最后一次出现的时刻。
	last time.Time
}

// repr 返回适合放进错误信息的短标识。
func (p logPattern) repr() string {
	return truncate(p.key, 60)
}

// topPatterns 把日志按模式聚合并取前 n 个。
func topPatterns(entries []lokiEntry, n int) []logPattern {
	if n <= 0 {
		n = 10
	}
	buckets := make(map[string]*logPattern, len(entries))
	for _, e := range entries {
		k := normalizeLogLine(e.line)
		b, ok := buckets[k]
		if !ok {
			b = &logPattern{key: k, sample: e.line}
			buckets[k] = b
		}
		b.count++
		if e.ts.After(b.last) {
			b.last = e.ts
		}
	}

	out := make([]logPattern, 0, len(buckets))
	for _, p := range buckets {
		out = append(out, *p)
	}
	// 按条数降序；条数相同时按 key 升序 —— 字典序兜底让结果**可复现**，
	// 否则 map 遍历顺序会让每次采集的 Top N 顺序都不同，
	// 页面上的证据顺序会随机跳动，测试也无法断言。
	sort.Slice(out, func(i, j int) bool {
		if out[i].count != out[j].count {
			return out[i].count > out[j].count
		}
		return out[i].key < out[j].key
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// normalizeLogLine 抹掉会��每条日志都不同的部分，让同类错误聚到一起。
//
// 具体抹三类：
//   - 十六进制/UUID 形态的 token（trace id、span id）；
//   - 纯数字（含小数，如耗时 12.34ms、端口 8080、错误码 500）；
//   - 连续引号内的内容（常含 id、路径、sql片段）。
//
// 目的是让「同一条业务逻辑抛出的 1000 条日志」聚成 1 条。
// 抹得太狠会���不同故障合并（那比漏聚合更糟），
// 因此只抹**格式上确定是变量**的部分，不做语义猜测。
func normalizeLogLine(line string) string {
	var b strings.Builder
	b.Grow(len(line))
	for i := 0; i < len(line); {
		switch ch := line[i]; {
		case ch == '"' || ch == '\'':
			// 引号内内容整体抹成 ?，含引号本身。
			quote := ch
			j := i + 1
			for j < len(line) && line[j] != quote {
				j++
			}
			if j < len(line) {
				j++ // 把结束引号也吃掉
			}
			b.WriteString("?")
			i = j
		case ch >= '0' && ch <= '9':
			j := i
			for j < len(line) && (isDigit(line[j]) || line[j] == '.') {
				j++
			}
			// 结尾的点是标点而非小数部分（"value is 1."）时不能整段吃掉，
			// 否则相邻的两个句子会被粘成一个 token，聚类就失真了。
			if line[j-1] == '.' {
				j--
			}
			// 判据是数字**前面**紧跟字母，说明它是标识符的一部分
			// （err500、http2）—— 整段保留，拆开反而制造出假的「同一模式」。
			//
			// 注意不能看后面：`5000ms` 后面也跟字母，但那是**单位**不是标识符，
			// 抹掉数字才正是我们想要的（耗时会变，错误类型不会变）。
			if i > 0 && isASCIILetter(line[i-1]) {
				b.WriteString(line[i:j])
				i = j
				continue
			}
			b.WriteString("#")
			i = j
		default:
			b.WriteByte(ch)
			i++
		}
	}
	return b.String()
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isASCIILetter(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// buildLogql 构造可复现的 LogQL 查询（手册验收标准第 2 条）。
//
// 窗口**不进**表达式：Loki 的时间范围是 query_range 的 start/end 参数，
// 写进 LogQL 反而会让「复制到 Grafana 复现」变复杂。
// 证据的 window 字段已经记下了确切区间。
//
// 服务名交给 %q 转义而不是手工拼引号：Go 的 strconv.Quote 与 LogQL
// 字符串字面量的转义规则一致（\" \\ \n），两者手工对齐过一次，
// 但手工那版漏过「服务名含换行」这种边角。
func buildLogql(service string) string {
	return fmt.Sprintf(`{service=%q} |~ %q`, service, errorPattern)
}

// buildLokiPatternEvidence 组装一条「错误模式」证据。
func buildLokiPatternEvidence(
	in evidence.Input, p logPattern, rank int,
	prev []lokiEntry, base baselineState,
) (evidence.Evidence, error) {
	if p.last.IsZero() {
		// §4.4：没有真实时间就不写这条证据，不用采集时刻兜底。
		return evidence.Evidence{}, fmt.Errorf("该模式没有可用的日志时间戳")
	}
	// 基线里同一模式出现多少次 —— 用于「这条错误是新出现的还是长期存在的」。
	// 新出现的错误几乎必然与本次事故相关；长期存在的错误是背景噪声。
	//
	// 只在基线「可用」时统计：基线失败时 prev 是空的，
	// 算出来的 0 会被误当成「事故前没有」——那正是最不能搞错的一个数。
	baseCount := 0
	for _, e := range prev {
		if normalizeLogLine(e.line) == p.key {
			baseCount++
		}
	}

	md := map[string]string{
		"pattern":       p.key,
		"rank":          strconv.Itoa(rank),
		"count":         strconv.Itoa(p.count),
		"window":        fmt.Sprintf("%s/%s", in.Window.Start.UTC().Format(time.RFC3339), in.Window.End.UTC().Format(time.RFC3339)),
		"last_seen":     p.last.UTC().Format(time.RFC3339),
		"error_pattern": errorPattern,
	}
	switch base {
	case baselineUsable, baselineEmpty:
		md["count_before"] = strconv.Itoa(baseCount)
		if baseCount == 0 {
			// 「事故前没有、现在有」是最强的信号，值得单独说明。
			md["novelty"] = "事故前窗口未出现，属新出现的错误模式"
		} else {
			md["change_ratio"] = formatFloat(ratio(float64(p.count), float64(baseCount)))
		}
	case baselineFailed:
		md["baseline"] = "基线窗口查询失败，无法判断该模式是新出现还是长期存在"
	}

	value := fmt.Sprintf("第 %d 类错误（%d 条）：%s", rank, p.count, truncate(p.sample, 200))
	return evidence.Evidence{
		Type:      evidence.TypeLog,
		Source:    SourceLoki,
		Query:     buildLogql(in.Service),
		Value:     value,
		Timestamp: p.last,
		Metadata:  md,
		// Confidence 留空（OPEN-M4-1），理由同 Prometheus 采集器。
	}, nil
}

// buildLokiBurstEvidence 组装一条「错误量突增」证据。
func buildLokiBurstEvidence(
	in evidence.Input, cur, prev []lokiEntry,
	base baselineState, prevErr error,
) (evidence.Evidence, error) {
	if len(cur) == 0 {
		return evidence.Evidence{}, fmt.Errorf("事故窗口无错误日志，无需计算突增")
	}
	// 突增证据的时间取「最后一条错误日志的时刻」——
	// 这是错误量达到峰值的真实时刻，比窗口终点更贴近事实。
	// 不用 window.End：那不是「错误发生的时间」，是「我们停止观察的时间」。
	last := cur[0].ts
	for _, e := range cur[1:] {
		if e.ts.After(last) {
			last = e.ts
		}
	}

	md := map[string]string{
		"count_after":   strconv.Itoa(len(cur)),
		"window":        fmt.Sprintf("%s/%s", in.Window.Start.UTC().Format(time.RFC3339), in.Window.End.UTC().Format(time.RFC3339)),
		"error_pattern": errorPattern,
	}
	switch base {
	case baselineUsable, baselineEmpty:
		// baselineEmpty 也要写 count_before=0 ——「事故前 0 条」是事实，
		// 不是「没有基线」。两者在页面上必须长得不一样。
		md["count_before"] = strconv.Itoa(len(prev))
		if len(prev) == 0 {
			// 从 0 涨到 N：倍数无意义（ratio 会是 0 或 +Inf），
			// 说成「事故前无错误日志」比给一个假倍数有用。
			md["burst"] = "事故前窗口无错误日志，本次为首次出现"
		} else {
			md["change_ratio"] = formatFloat(ratio(float64(len(cur)), float64(len(prev))))
		}
	case baselineFailed:
		md["baseline"] = fmt.Sprintf("基线窗口查询失败，无法计算错误量突增（%v）", prevErr)
	}

	value := fmt.Sprintf("错误日志 %d 条", len(cur))
	switch base {
	case baselineUsable:
		value += fmt.Sprintf("（事故前 %d 条，变化 %s 倍）",
			len(prev), formatFloat(ratio(float64(len(cur)), float64(len(prev)))))
	case baselineEmpty:
		value += "（事故前窗口 0 条，为首次出现）"
	case baselineFailed:
		value += "（基线窗口不可用，无突增对比）"
	}

	return evidence.Evidence{
		Type:      evidence.TypeLog,
		Source:    SourceLoki,
		Query:     buildLogql(in.Service),
		Value:     value,
		Timestamp: last,
		Metadata:  md,
	}, nil
}

// queryRange 用 query_range 接口取窗口内的错误日志。
func (c *LokiCollector) queryRange(
	ctx context.Context, service string, w evidence.Window,
) ([]lokiEntry, error) {
	v := url.Values{}
	v.Set("query", buildLogql(service))
	// Loki 的 query_range 用 RFC3339 或纳秒时间戳。统一用纳秒整数：
	// 秒级会让「1 分钟窗口」在Loki 侧被当成 0 宽度区间而返回空。
	v.Set("start", strconv.FormatInt(w.Start.UnixNano(), 10))
	v.Set("end", strconv.FormatInt(w.End.UnixNano(), 10))
	// limit 是**单次返回条数上限**，不是我们要的总数。
	// 取 topN 的数倍：Top N 需要能覆盖「前 N 类各若干条」，
	// 若 limit 只等于 N，第 1 类占满全部名额时后面全是它。
	limit := c.topN * 50
	if limit < 100 {
		limit = 100
	}
	v.Set("limit", strconv.Itoa(limit))
	v.Set("direction", "backward")

	endpoint := c.baseURL + "/loki/api/v1/query_range?" + v.Encode()

	var parsed lokiResponse
	if err := httpGetJSON(ctx, c.client, endpoint, &parsed); err != nil {
		return nil, err
	}
	if parsed.Status != "success" {
		return nil, fmt.Errorf("查询未成功: %s", truncate(parsed.Error, 200))
	}
	if len(parsed.Data.Result) == 0 {
		return nil, errNoLogs
	}

	var out []lokiEntry
	var malformed int
	for _, stream := range parsed.Data.Result {
		for _, v2 := range stream.Values {
			if len(v2) != 2 {
				// 计数而不静默跳过：结构异常意味着我们的解析器
				// 可能跟对方格式对不上，静默丢会让证据凭空少一大块
				// 而页面完全看不出来。
				malformed++
				continue
			}
			ns, err := parseUnixSeconds(v2[0])
			if err != nil {
				malformed++
				continue
			}
			var text string
			if err := json.Unmarshal(v2[1], &text); err != nil {
				malformed++
				continue
			}
			// 时间截到**微秒**：PG 的 TIMESTAMPTZ 只存微秒，
			// Loki 却给纳秒。不在这里截断的话，采集器报告的
			// timestamp 与实际落库的值会差几百纳秒 ——
			// 页面上「证据发生时刻」与库里对不上，而这种偏差
			// 永远查不出来（两条单独看都「正常」）。
			out = append(out, lokiEntry{
				ts:   time.Unix(0, ns).UTC().Truncate(time.Microsecond),
				line: text,
			})
		}
	}
	if len(out) == 0 {
		// 有 stream 但一条都没解析出来 —— 是解析器跟对方格式对不上，
		// 不是「没有错误日志」。必须报错，否则等于静默丢弃全部证据。
		if malformed > 0 {
			return nil, fmt.Errorf("loki 返回的 %d 个日志流全部无法解析（格式可能已变更）", malformed)
		}
		return nil, errNoLogs
	}
	return out, nil
}

// errNoLogs 表示「查询成功但该区间没有匹配的错误日志」。
//
// 与 Prometheus 采集器的 errNoData 同理：这是**正常结果**（服务很健康），
// 不是采集故障。混同会让任务白白重试三次再进死信。
var errNoLogs = errors.New("该区间无匹配的日志")

// stripNoLogs 把哨兵错误「无日志」抹成 nil，保留真正的采集故障。
//
// 只处理「是不是故障」这一个维度；「有没有数据」由调用方看
// 条目数自己判断 —— 两者混在一个返回值里就会丢掉
// 「查询成功且恰好零条」这个有意义的状态。
func stripNoLogs(err error) error {
	if errors.Is(err, errNoLogs) {
		return nil
	}
	return err
}

// baselineState 是基线窗口的三种状态。
//
// 刻意区分「零条」与「不可用」：两者都算不出变化倍数，
// 但含义完全相反 —— 「零条」说明事故前这个服务**干干净净**，
// 是新问题的强信号；「不可用」说明数据源故障，该重试。
// 混为一谈会让「从 0 涨到 100」被显示成「无基线可比」，
// 而前者恰恰是最该被看见的情况。
type baselineState int

const (
	// baselineFailed 表示基线窗口查询失败。
	baselineFailed baselineState = iota
	// baselineEmpty 表示基线窗口查询成功但一条日志都没有。
	baselineEmpty
	// baselineUsable 表示基线窗口有可比数据。
	baselineUsable
)

// classifyBaseline 由「基线条目 + 错误」判定基线状态。
func classifyBaseline(entries []lokiEntry, err error) baselineState {
	switch {
	case err != nil:
		return baselineFailed
	case len(entries) == 0:
		return baselineEmpty
	default:
		return baselineUsable
	}
}
