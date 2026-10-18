package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yu/iip/internal/config"
	"github.com/yu/iip/internal/metrics"
)

// 本文件验 T6.3 的端点级契约：/metrics 的开关语义与「不受鉴权」这一条。
//
// 装配测试刻意用 nil 连接池：newRouter 只把 pool 交给仓储构造器（只存不用），
// 而本文件只请求 /metrics 与鉴权拒绝路径，不触达任何 SQL。真库路径的证据
// 由探针脚本（_probe_metrics.py）与 repository 集成用例提供。

func testRouter(t *testing.T, cfg *config.Config, m *metrics.Metrics) http.Handler {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	return newRouter(logger, nil, cfg, nil, m)
}

// metricsCfg 是只关心指标开关的最小合法配置。
//
// HTTPTimeout 必须显式给：Timeout 中间件的零值会让 context.WithTimeout(ctx, 0)
// 立刻过期，于是**所有**请求都 504 —— 那时 401/404 的断言会以「拿到 504」
// 的形式失败，而原因与被测行为无关。
func metricsCfg(metricsEnabled bool, authToken string) *config.Config {
	return &config.Config{
		HTTPAddr:        config.DefaultHTTPAddr,
		MetricsEnabled:  metricsEnabled,
		AuthToken:       authToken,
		HTTPTimeout:     config.DefaultHTTPTimeout,
		WebhookMaxBytes: config.DefaultWebhookMaxBytes,
		TopologyFile:    "testdata/no-such-topology.yaml",
	}
}

// TestMetricsEndpointEnabledServesBusinessMetrics 验「开了就能抓到」。
func TestMetricsEndpointEnabledServesBusinessMetrics(t *testing.T) {
	m := metrics.New()
	m.IncidentCreated.Add(7)
	r := testRouter(t, metricsCfg(true, ""), m)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", http.NoBody))

	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics 返回 %d，期望 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "iip_incident_created_total 7") {
		t.Errorf("/metrics 正文缺少业务指标（埋点没接到这个注册表）")
	}
	if !strings.Contains(body, "go_goroutines") {
		t.Errorf("/metrics 正文缺少运行时指标")
	}
}

// TestMetricsEndpointIsNotProtectedByToken 是「不受鉴权」的判据。
//
// **必须同时断言另一条**：带 token 的配置下，/api/v1/alerts 无头应 401。
// 只断言「/metrics 返回 200」是恒绿的 —— 若 RequireToken 压根没生效
// （比如 cfg.AuthToken 没被读到），/metrics 也是 200，而它此时证明不了任何事。
func TestMetricsEndpointIsNotProtectedByToken(t *testing.T) {
	const token = "s3cr3t-token"
	r := testRouter(t, metricsCfg(true, token), metrics.New())

	// 先证明鉴权确实在这个 router 上生效。
	guarded := httptest.NewRecorder()
	r.ServeHTTP(guarded, httptest.NewRequest(http.MethodGet, "/api/v1/alerts", http.NoBody))
	if guarded.Code != http.StatusUnauthorized {
		t.Fatalf("/api/v1/alerts 无 token 返回 %d，期望 401 —— 鉴权没生效，本用例的另一半断言将失去意义",
			guarded.Code)
	}

	// 再验 /metrics 不受它约束（Prometheus 抓取不带业务 token）。
	open := httptest.NewRecorder()
	r.ServeHTTP(open, httptest.NewRequest(http.MethodGet, "/metrics", http.NoBody))
	if open.Code != http.StatusOK {
		t.Fatalf("/metrics 无 token 返回 %d，期望 200（抓取端不会带业务 token）", open.Code)
	}
}

// TestMetricsEndpointDisabledReturns404 验关闭语义是「不存在」而非「禁止」。
//
// 关掉时注册路由而不是在 handler 里 403：404 才能与「打错路径」区分开，
// 也不会让扫描器一眼看出「这里本该有指标」。
func TestMetricsEndpointDisabledReturns404(t *testing.T) {
	r := testRouter(t, metricsCfg(false, ""), metrics.New())

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", http.NoBody))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("/metrics 在关闭状态下返回 %d，期望 404", rec.Code)
	}
}
