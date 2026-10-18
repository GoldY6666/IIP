package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"

	"github.com/yu/iip/internal/metrics"
)

// webhookStat 返回 webhook 耗时直方图的（样本数，耗时总和）。
func webhookStat(t *testing.T, m *metrics.Metrics) (uint64, float64) {
	t.Helper()
	var d dto.Metric
	if err := m.WebhookDuration.Write(&d); err != nil {
		t.Fatalf("读取直方图失败: %v", err)
	}
	return d.GetHistogram().GetSampleCount(), d.GetHistogram().GetSampleSum()
}

// TestWebhookMetricsObservesDuration 验证时长真的被记下且数值合理。
//
// 用真实 sleep 而不是假时钟：耗时在中间件里用 time.Now 量，
// 要证明量到了只能让被量对象真的花时间。阈值 8ms / 实际 10ms 给抖动留余量，
// 同时与「漏写 Observe」的 0 有足够距离。
func TestWebhookMetricsObservesDuration(t *testing.T) {
	m := metrics.New()
	h := WebhookMetrics(m)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(10 * time.Millisecond)
		w.WriteHeader(http.StatusAccepted)
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/alerts/webhook", http.NoBody)
	h.ServeHTTP(rec, req)

	// 顺带断言中间件**不改变**响应：它只观察，不该影响契约。
	if rec.Code != http.StatusAccepted {
		t.Errorf("状态码 = %d，期望 202（中间件不该改响应）", rec.Code)
	}

	n, sum := webhookStat(t, m)
	if n != 1 {
		t.Fatalf("样本数 = %d，期望 1", n)
	}
	if sum < 0.008 {
		t.Errorf("耗时总和 = %v，期望 ≥ 0.008 秒（时长没被测量）", sum)
	}
}

// TestWebhookMetricsRecordsOnPanic 是「用 defer 而不是顺序调用」的判据。
//
// panic 的请求恰恰最该出现在耗时图上：它往往意味着处理卡到某处才炸，
// 而顺序调用的写法会让这条样本彻底消失。这里不 recover ——
// 真实链路里 panic 由 chi 的 Recoverer 兜住（它在下一层），
// 本用例只验证「panic 也留下了样本」。
func TestWebhookMetricsRecordsOnPanic(t *testing.T) {
	m := metrics.New()
	h := WebhookMetrics(m)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("handler 炸了")
	}))

	func() {
		defer func() {
			if rec := recover(); rec == nil {
				t.Error("handler 的 panic 应继续向上传播（由 Recoverer 兜）")
			}
		}()
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/v1/alerts/webhook", http.NoBody))
	}()

	if n, _ := webhookStat(t, m); n != 1 {
		t.Errorf("panic 请求的样本数 = %d，期望 1 —— 顺序调用会漏掉这条", n)
	}
}

// TestWebhookMetricsNilIsSafe 守住 nil 兜底：传零值不能变成运行期 panic。
func TestWebhookMetricsNilIsSafe(t *testing.T) {
	h := WebhookMetrics(nil)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/alerts/webhook", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Errorf("状态码 = %d，期望 200", rec.Code)
	}
}
