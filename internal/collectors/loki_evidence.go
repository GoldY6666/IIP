package collectors

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/yu/iip/internal/evidence"
)

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
