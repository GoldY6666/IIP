package middleware

import (
	"net/http"
	"time"

	"github.com/yu/iip/internal/metrics"
)

// WebhookMetrics 记录告警 webhook 的端到端处理时长（M6 T6.3）。
//
// 只挂在这一个路由上（r.With(WebhookMetrics(m)).Post("/api/v1/alerts/webhook")），
// 不做成全局中间件：本项目其余端点都是毫秒级只读查询，
// 把它们混进同一个直方图会让 webhook 的 P99 被稀释得看不出问题 ——
// 而这个指标的用途恰恰是「接入链路有没有开始变慢」。
//
// 不加快照响应体大小/状态码标签：状态码会让基数 ×5（200/400/413/422/503），
// 而「哪类失败」已经由 iip_alert_ingest_total{result} 回答得更准
// （那个指标是逐条的，比按请求统计的 4xx 精确得多）。
func WebhookMetrics(m *metrics.Metrics) func(http.Handler) http.Handler {
	if m == nil {
		// 传 nil 兜底成 Noop：调用方（main）通常已经保证非 nil，
		// 但这层兜底能避免「测试里传了个零值 → nil 解引用 panic」这种
		// 只在装配顺序被改动时才出现的故障。
		m = metrics.Noop()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			// 用 defer 而不是在 next 之后直接调：handler panic 时
			// next.ServeHTTP 不会正常返回，只有 defer 还能记下这条耗时 ——
			// 而 panic 的请求恰恰是最该在耗时图上被看见的。
			defer func() { m.WebhookDuration.Observe(time.Since(start).Seconds()) }()
			next.ServeHTTP(w, r)
		})
	}
}
