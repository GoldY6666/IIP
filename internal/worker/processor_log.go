// 采集结果的观测与日志（从 processor.go 拆出，OPEN-M6-1）。
//
// 单文件 ≤ 300 行（docs/IIP-M1详细开发手册.md §4）。这里只做「把结果说出去」：
// 打点、逐采集器日志、假设日志、置信度格式化，不改任何状态。
package worker

import (
	"log/slog"
	"strconv"

	"github.com/yu/iip/internal/evidence"
	"github.com/yu/iip/internal/rca"
)

// observeCollectorDurations 把每个采集器的耗时记进直方图（T6.3）。
//
// 失败的那些**也记**：CollectAll 对失败的调用同样计时，而「某个采集器
// 一直超时」恰恰是最该被看到的形态 —— 只记成功的会把这条线索整段丢掉，
// 表现出来就是「证据突然变少，但所有耗时指标都很健康」。
//
// 按索引取址而非 range 拷贝：CollectorResult 带切片与 error，
// 复制一份纯属浪费（gocritic rangeValCopy）。
func (p *Processor) observeCollectorDurations(results []evidence.CollectorResult) {
	for i := range results {
		r := &results[i]
		p.metrics.EvidenceDuration.WithLabelValues(string(r.Source)).Observe(r.Duration.Seconds())
	}
}

// logCollectorResults 逐采集器记录结果。
//
// 失败**只记日志不返回**：手册 §4.3 要求单个采集器挂掉不影响其余（§4.5）。
// 但也不能完全静默 —— 「Prometheus 挂了」必须能在日志里看见，
// 否则页面上少了证据却没人知道为什么。
func (p *Processor) logCollectorResults(incidentID int64, results []evidence.CollectorResult) {
	for _, r := range results {
		switch {
		case r.Err != nil:
			p.logger.Warn("采集器失败（其余采集器照常落库）",
				slog.String("source", r.Source),
				slog.Int64("incident_id", incidentID),
				slog.String("error", r.Err.Error()))
		case r.Dropped > 0:
			p.logger.Warn("采集到非法证据已丢弃",
				slog.String("source", r.Source),
				slog.Int64("incident_id", incidentID),
				slog.Int("dropped", r.Dropped),
				slog.String("reason", errText(r.DropReason)))
		}
	}
}

// logHypotheses 把根因假设候选打进日志，**只建议不写库**。
//
// 为什么不落 incidents.root_causes（手册 §2.2 明确列为不做）：
// 自动写根因看起来聪明，但错的根因会误导处置且难以发现 ——
// 与 `confidence` 填 1.0 是同一类错误。置信度是有判据、可复核的，
// 根因没有；所以置信度可以入库，根因只能给人看。
//
// 必须在 Assign **之后**调：SuggestFrom 只用有置信度的证据，
// 顺序反了它会按设计全部跳过，恒返回空列表 —— 而那个空列表
// 看起来与「确实没有可推断的假设」完全一样。
//
// 一条都不打的情况有两种，措辞必须不同：
//   - 有证据但全部不可评估（没基线）→ 正常，说明判据在起作用
//   - 压根没有证据→ 无话可说，不必记
func (p *Processor) logHypotheses(incidentID int64, evs []evidence.Evidence) {
	if len(evs) == 0 {
		return
	}
	s := rca.SuggestFrom(evs)
	if s.AssessedCount == 0 {
		p.logger.Info("根因假设暂不可推断（证据均无可复核的置信度来源）",
			slog.Int64("incident_id", incidentID),
			slog.Int("skipped", s.Skipped))
		return
	}
	p.logger.Info("根因假设候选（仅建议，不写库，待人工确认）",
		slog.Int64("incident_id", incidentID),
		slog.Int("assessed", s.AssessedCount),
		slog.Int("skipped", s.Skipped),
		slog.Int("hypotheses", len(s.Hypotheses)))
	for _, h := range s.Hypotheses {
		conf := "未评估"
		if h.Confidence != nil {
			conf = formatConfidence(*h.Confidence)
		}
		p.logger.Info("  根因假设",
			slog.String("summary", h.Summary),
			slog.String("confidence", conf),
			slog.Int("support", h.Support),
			slog.String("basis", h.Basis))
	}
}

// formatConfidence 把置信度格式化成两位小数的字符串。
//
// nil 的情形由调用方先判掉再决定用不用它 —— 这里不返回 "未评估"
// 是因为「不可评估」与「0.00」在日志里必须长得不一样，
// 而把两件事塞进一个函数只会诱使调用方顺手打出一个 0.00。
func formatConfidence(v float64) string {
	return strconv.FormatFloat(v, 'f', 2, 64)
}
