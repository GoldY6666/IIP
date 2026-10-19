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
	"strings"

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
