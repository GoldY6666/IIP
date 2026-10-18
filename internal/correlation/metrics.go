package correlation

import "github.com/yu/iip/internal/metrics"

// WithMetrics 注入自监控指标（M6 T6.3），返回自身以便链式装配。
//
// 与 WithTimeline / WithSuppressions 同一形态：不注入时行为与 M2 完全一致。
// 传 nil 兜底为 Noop，使埋点处无需判空 —— 那种判空漏写一次就是运行期 panic。
func (s *Service) WithMetrics(m *metrics.Metrics) *Service {
	if m == nil {
		m = metrics.Noop()
	}
	s.metrics = m
	return s
}

// observeScore 记录一条告警的**最终**关联分数。
//
// 必须在「抑制降级」之后调用，不能在 Decide 之后立刻调：
// 抑制会把 review 降级成 new，两个档位在运维上含义完全不同
// （前者是「拿不准，等人看」，后者是「确认不相关，另起事件」）。
// 记早了会让 action=new 的分布里混进被抑制的 review，而那正是最该被数出来的信号。
//
// 无候选事件时 bestScore 为 0、action 为 new，0 也照记 —— 它确实是本次的最高分，
// 抹掉它会让「有多少告警是在空窗口下建的档」这个信息消失。
func (s *Service) observeScore(action string, score float64) {
	s.metrics.CorrelationScore.WithLabelValues(action).Observe(score)
}
