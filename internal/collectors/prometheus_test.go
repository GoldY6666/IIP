package collectors

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yu/iip/internal/evidence"
)

// 本文件覆盖手册 §七「采集器单测：三类各自的解析与降级」中 Prometheus 的部分，
// 手段是 httptest mock server（计划明确要求，不用真实 Prometheus）。

// promServer 是一个可编程的 Prometheus 假服务端。
type promServer struct {
	*httptest.Server
	// calls 记录收到的查询次数。
	calls atomic.Int32
	// lastQuery 记录最后一次收到的 PromQL（用于断言 query 可复现）。
	lastQuery atomic.Value // string
	// handler 允许逐用例替换响应行为。
	handler func(w http.ResponseWriter, r *http.Request)
}

func newPromServer(t *testing.T) *promServer {
	t.Helper()
	s := &promServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.calls.Add(1)
		q := r.URL.Query().Get("query")
		if q != "" {
			s.lastQuery.Store(q)
		}
		if s.handler != nil {
			s.handler(w, r)
			return
		}
		writePromResult(t, w, 1, "42") // 默认返回一个正常样本
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *promServer) queryCount() int32 { return s.calls.Load() }

func (s *promServer) lastPromQL() string {
	v, _ := s.lastQuery.Load().(string)
	return v
}

// writePromResult 回一个 Prometheus instant query 的成功响应。
func writePromResult(t *testing.T, w http.ResponseWriter, ts int64, value string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	body := map[string]any{
		"status": "success",
		"data": map[string]any{
			"resultType": "vector",
			"result": []any{
				map[string]any{
					"metric": map[string]string{"__name__": "http_requests_total"},
					"value":  []any{ts, value},
				},
			},
		},
	}
	if err := json.NewEncoder(w).Encode(body); err != nil {
		t.Errorf("写 mock 响应失败: %v", err)
	}
}

// writePromEmpty 回一个「查询成功但无样本」的响应。
func writePromEmpty(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	body := map[string]any{
		"status": "success",
		"data":   map[string]any{"resultType": "vector", "result": []any{}},
	}
	if err := json.NewEncoder(w).Encode(body); err != nil {
		t.Errorf("写 mock 响应失败: %v", err)
	}
}

// 固定窗口，起点为 Unix 整点便于构造样本时间。
func testWindow() evidence.Window {
	end := time.Unix(1_700_000_000, 0).UTC()
	return evidence.Window{Start: end.Add(-time.Hour), End: end}
}

// isAfterWindow 判断请求的查询时刻是否落在**事故窗口**内。
//
// 判据用「严格大于窗口起点」而非「小于等于」：实现里前窗口的查询时刻
// 恰好等于事故窗口的起点（两个窗口首尾相接、不重叠），
// 用 `<=` 会把前窗口的请求误判成事故窗口 ——
// 那个 mock 就会对「本该返回基线值」的请求返回事故值，
// 于是 change_ratio 恒等于 1.0000，看起来像「指标没变化」。
func isAfterWindow(ts int64) bool {
	return ts > testWindow().Start.Unix()
}

// mustTS 取出请求里的查询时刻。
func mustTS(t *testing.T, r *http.Request) int64 {
	t.Helper()
	var ts int64
	if _, err := fmt.Sscanf(r.URL.Query().Get("time"), "%d", &ts); err != nil {
		t.Errorf("解析查询时刻失败: %v", err)
	}
	return ts
}

// 基线窗口**真失败**（而非无数据）必须被单独记为 failures。
//
// 两者都导致「拿不到基线」，但性质相反：
// 无数据是客观事实（不该重试），真失败是数据源故障（该重试）。
// 混同会让 Prometheus 局部故障被静默吞掉，页面显示「指标平稳」而实际没查到。
func TestPrometheusBaselineFailureIsRecorded(t *testing.T) {
	srv := newPromServer(t)
	srv.handler = func(w http.ResponseWriter, r *http.Request) {
		if !isAfterWindow(mustTS(t, r)) {
			// 基线窗口一律 503
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		writePromResult(t, w, mustTS(t, r), "100")
	}

	c := NewPrometheusCollector(srv.URL)
	got, err := c.Collect(context.Background(), evidence.Input{
		IncidentID: 1, Service: "api", Window: testWindow(),
	})
	if err != nil {
		t.Fatalf("基线失败不该让整次采集失败: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("采到 %d 条，期望 3 条（事故窗口有数据就该落库）", len(got))
	}
	if !hasFailuresRecorded(got) {
		t.Error("基线窗口查询失败未被记录 —— 数据源故障被静默吞掉了")
	}
	// baseline 字段必须存在，且措辞是「查询失败」而不是「无样本」。
	//
	// 这里刻意**要求** baseline 非空：它正是用来区分
	// 「事故前本来就没数据」（客观事实）与「基线查询失败」（数据源故障）的。
	// 两者都导致算不出变化幅度，但后者该重试、该让运维去查 Prometheus。
	// 把失败说成「无样本」，运维会以为服务本来就没埋这个指标。
	for i, e := range got {
		bl := e.Metadata["baseline"]
		if bl == "" {
			t.Errorf("第 %d 条缺 baseline 说明 —— 无法区分「无数据」与「查询失败」", i)
			continue
		}
		if !strings.Contains(bl, "失败") {
			t.Errorf("第 %d 条把查询失败说成了「%s」，运维会误以为服务本来就没数据", i, bl)
		}
		if strings.Contains(bl, "无样本") {
			t.Errorf("第 %d 条 baseline 措辞含「无样本」，但实际是查询失败: %q", i, bl)
		}
	}
}

// 核心用例：三条 RED 指标都查到，且前后对比的幅度正确。
func TestPrometheusCollectsRedMetricsWithDelta(t *testing.T) {
	srv := newPromServer(t)
	// 让「后窗口」的值是「前窗口」的 10 倍，便于验证变化幅度。
	srv.handler = func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		var ts int64
		if _, err := fmt.Sscanf(r.URL.Query().Get("time"), "%d", &ts); err != nil {
			ts = 0
		}
		// errors 指标 1 -> 10（10 倍），rate 与 duration 保持不变（1 倍）
		val := "1"
		if strings.Contains(q, "5..") && isAfterWindow(ts) {
			val = "10"
		}
		writePromResult(t, w, ts, val)
	}

	c := NewPrometheusCollector(srv.URL)
	got, err := c.Collect(context.Background(), evidence.Input{
		IncidentID: 1,
		Service:    "checkout",
		Window:     testWindow(),
	})
	if err != nil {
		t.Fatalf("Collect 返回错误 = %v，期望 nil", err)
	}
	if len(got) != 3 {
		t.Fatalf("采到 %d 条证据，期望 3 条（rate/errors/duration 各一条）", len(got))
	}

	// 按 metric 找 errors 那条核对变化幅度。
	var errorsEv *evidence.Evidence
	for i := range got {
		if got[i].Metadata["metric"] == "errors" {
			errorsEv = &got[i]
			break
		}
	}
	if errorsEv == nil {
		t.Fatalf("未找到 errors 指标证据，实际 metric 列表: %v", metricsOf(got))
	}
	if got := errorsEv.Metadata["change_ratio"]; got != "10.0000" {
		t.Errorf("errors 的 change_ratio = %q，期望 10.0000（1 -> 10）", got)
	}

	// rate 未变，幅度应为 1。
	for i := range got {
		if got[i].Metadata["metric"] != "rate" {
			continue
		}
		if r := got[i].Metadata["change_ratio"]; r != "1.0000" {
			t.Errorf("rate 的 change_ratio = %q，期望 1.0000（前后未变）", r)
		}
	}
}

// query 字段必须存**可复现的 PromQL**，且带上了服务名。
//
// 这是验收标准第 2 条「query 可复制后复现」的直接证据。
func TestPrometheusStoresReproducibleQuery(t *testing.T) {
	srv := newPromServer(t)
	c := NewPrometheusCollector(srv.URL)

	got, err := c.Collect(context.Background(), evidence.Input{
		IncidentID: 1, Service: "checkout", Window: testWindow(),
	})
	if err != nil {
		t.Fatalf("Collect 返回错误 = %v", err)
	}
	if len(got) == 0 {
		t.Fatal("未采到任何证据")
	}

	e := got[0]
	if e.Query == "" {
		t.Fatal("query 字段为空——用户无法复制复现")
	}
	if !strings.Contains(e.Query, `service="checkout"`) {
		t.Errorf("query = %q，不含服务名 label", e.Query)
	}
	if !strings.Contains(e.Query, "http_requests_total") {
		t.Errorf("query = %q，不是可执行的 PromQL", e.Query)
	}
	// 服务端确实收到了同一个表达式 —— 证明「存下来的」就是「发出去的」。
	if !strings.Contains(srv.lastPromQL(), `service="checkout"`) {
		t.Errorf("服务端收到的 query = %q，与落库的 query 不一致", srv.lastPromQL())
	}
}

// 时间必须取自样本，而非采集时刻（§4.4 / ADR-003）。
func TestPrometheusUsesSampleTimestampNotNow(t *testing.T) {
	// 样本时间固定为 2020 年，与「现在」相距 6 年 ——
	// 若实现偷用 time.Now()，这个断言必红。
	const sampleTS int64 = 1_577_836_800 // 2020-01-01T00:00:00Z
	srv := newPromServer(t)
	srv.handler = func(w http.ResponseWriter, r *http.Request) {
		writePromResult(t, w, sampleTS, "1")
	}

	c := NewPrometheusCollector(srv.URL)
	got, err := c.Collect(context.Background(), evidence.Input{
		IncidentID: 1, Service: "api", Window: testWindow(),
	})
	if err != nil {
		t.Fatalf("Collect 返回错误 = %v", err)
	}
	if len(got) == 0 {
		t.Fatal("未采到证据")
	}
	for i, e := range got {
		if e.Timestamp.IsZero() {
			t.Errorf("第 %d 条 timestamp 为空", i)
		}
		if e.Timestamp.Unix() != sampleTS {
			t.Errorf("第 %d 条 timestamp = %v（Unix %d），期望等于样本时间 %d —— 实现用了采集时刻兜底",
				i, e.Timestamp, e.Timestamp.Unix(), sampleTS)
		}
	}
}

// 事故前无数据：必须如实标「无基线」，不能当成 0 去算增幅。
//
// 若当成 0，ratio 会是 +Inf，JSON 序列化直接失败；
// 而「从 0 涨到 100」本来就该说成「没有基线可比」。
func TestPrometheusReportsMissingBaseline(t *testing.T) {
	srv := newPromServer(t)
	srv.handler = func(w http.ResponseWriter, r *http.Request) {
		var ts int64
		if _, err := fmt.Sscanf(r.URL.Query().Get("time"), "%d", &ts); err != nil {
			ts = 0
		}
		if !isAfterWindow(ts) {
			writePromEmpty(t, w) // 前窗口无数据
			return
		}
		writePromResult(t, w, ts, "100")
	}

	c := NewPrometheusCollector(srv.URL)
	got, err := c.Collect(context.Background(), evidence.Input{
		IncidentID: 1, Service: "api", Window: testWindow(),
	})
	if err != nil {
		t.Fatalf("Collect 返回错误 = %v，期望 nil（有后窗口数据就不算失败）", err)
	}
	if len(got) == 0 {
		t.Fatal("未采到证据")
	}
	for i, e := range got {
		if _, ok := e.Metadata["change_ratio"]; ok {
			t.Errorf("第 %d 条在无基线时仍给了 change_ratio = %q，应改为说明无基线",
				i, e.Metadata["change_ratio"])
		}
		if e.Metadata["baseline"] == "" {
			t.Errorf("第 %d 条缺少 baseline 说明", i)
		}
		// 证据本身必须能被 JSON 序列化（+Inf 会失败）。
		if _, err := json.Marshal(e.Metadata); err != nil {
			t.Errorf("第 %d 条 metadata 无法序列化: %v", i, err)
		}
	}
}

// 空服务名必须直接失败，而不是拿空串去查。
func TestPrometheusRejectsEmptyService(t *testing.T) {
	srv := newPromServer(t)
	c := NewPrometheusCollector(srv.URL)

	_, err := c.Collect(context.Background(), evidence.Input{
		IncidentID: 1, Service: "  ", Window: testWindow(),
	})
	if err == nil {
		t.Fatal("空服务名应报错——`service=\"\"` 会匹配所有空 label 序列，捞回全是噪声")
	}
	if srv.queryCount() != 0 {
		t.Errorf("空服务名时仍发了 %d 次查询，期望 0 次（应在查询前就拒绝）", srv.queryCount())
	}
}

// 窗口不合法（倒置 / 零长 / 未设置）必须被拒。
func TestPrometheusRejectsInvalidWindow(t *testing.T) {
	srv := newPromServer(t)
	c := NewPrometheusCollector(srv.URL)
	end := time.Unix(1_700_000_000, 0).UTC()

	cases := map[string]evidence.Window{
		"零值":  {},
		"缺结束": {Start: end.Add(-time.Hour)},
		"倒置":  {Start: end, End: end.Add(-time.Hour)},
		"零长":  {Start: end, End: end},
	}
	for name, w := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := c.Collect(context.Background(), evidence.Input{
				IncidentID: 1, Service: "api", Window: w,
			})
			if err == nil {
				t.Fatal("不合法窗口应报错")
			}
		})
	}
}

// 全部三条都失败才算采集失败。
func TestPrometheusAllQueriesFailIsError(t *testing.T) {
	srv := newPromServer(t)
	srv.handler = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}

	c := NewPrometheusCollector(srv.URL)
	_, err := c.Collect(context.Background(), evidence.Input{
		IncidentID: 1, Service: "api", Window: testWindow(),
	})
	if err == nil {
		t.Fatal("三条 RED 指标全失败时应返回错误 —— 否则任务会静默成功、证据永远缺失")
	}
}

// 部分失败：已拿到的证据照常落库，失败原因写进 metadata。
//
// 这是手册 §4.3「Collect 失败不得让整批任务失败」的直接验证。
func TestPrometheusPartialFailureKeepsGoodMetrics(t *testing.T) {
	srv := newPromServer(t)
	srv.handler = func(w http.ResponseWriter, r *http.Request) {
		// errors 指标（query 里含 5..）全部 500，另两条正常。
		if strings.Contains(r.URL.Query().Get("query"), "5..") {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		var ts int64
		if _, err := fmt.Sscanf(r.URL.Query().Get("time"), "%d", &ts); err != nil {
			ts = 0
		}
		writePromResult(t, w, ts, "1")
	}

	c := NewPrometheusCollector(srv.URL)
	got, err := c.Collect(context.Background(), evidence.Input{
		IncidentID: 1, Service: "api", Window: testWindow(),
	})
	if err != nil {
		t.Fatalf("一条失败不应让整次采集失败，返回错误 = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("采到 %d 条，期望 2 条（rate 与 duration 成功、errors 失败）", len(got))
	}
	// 失败原因必须可查 —— 页面上要能说明「哪条没查到」。
	if !hasFailuresRecorded(got) {
		t.Error("未在 metadata 中记录失败原因——页面上无法说明哪条指标缺失")
	}
}

// 服务端返回错误状态时，错误信息要带 HTTP 码，便于排查。
func TestPrometheusReportsHTTPStatus(t *testing.T) {
	srv := newPromServer(t)
	srv.handler = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("service unavailable"))
	}

	c := NewPrometheusCollector(srv.URL)
	_, err := c.Collect(context.Background(), evidence.Input{
		IncidentID: 1, Service: "api", Window: testWindow(),
	})
	if err == nil {
		t.Fatal("503 应导致采集失败")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("错误信息 = %q，不含 HTTP 状态码", err)
	}
}

// Prometheus 返回 status != success 时要报出它的 error 字段。
func TestPrometheusReportsUpstreamError(t *testing.T) {
	srv := newPromServer(t)
	srv.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"error","errorType":"bad_data","error":"parse error at char 3"}`))
	}

	c := NewPrometheusCollector(srv.URL)
	_, err := c.Collect(context.Background(), evidence.Input{
		IncidentID: 1, Service: "api", Window: testWindow(),
	})
	if err == nil {
		t.Fatal("status=error 应导致采集失败")
	}
	if !strings.Contains(err.Error(), "parse error") {
		t.Errorf("错误信息 = %q，未带出 Prometheus 侧的 error 文本", err)
	}
}

// 响应体不是合法 JSON 时必须报错，而不是静默返回 0 条。
func TestPrometheusRejectsMalformedResponse(t *testing.T) {
	srv := newPromServer(t)
	srv.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{not json`))
	}

	c := NewPrometheusCollector(srv.URL)
	_, err := c.Collect(context.Background(), evidence.Input{
		IncidentID: 1, Service: "api", Window: testWindow(),
	})
	if err == nil {
		t.Fatal("非法 JSON 应报错——静默当成「无数据」会把采集故障说成「这段时间正常」")
	}
}

// 超时必须可控（手册 §4.5：5s）。用一个永不响应的 handler 验证。
func TestPrometheusHonorsTimeout(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block // 永不返回
	}))
	defer srv.Close()
	defer close(block)

	c := NewPrometheusCollector(srv.URL, WithTimeout(200*time.Millisecond))
	start := time.Now()
	_, err := c.Collect(context.Background(), evidence.Input{
		IncidentID: 1, Service: "api", Window: testWindow(),
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("超时应导致采集失败")
	}
	// 6 条查询（3 指标 × 前后两窗），每条 200ms -> 至少 1.2s。
	// 断言「有时间成本」而非精确值：机器慢时不该假红。
	if elapsed < 200*time.Millisecond {
		t.Errorf("耗时 %v，未体现超时成本（期望 >= 200ms）", elapsed)
	}
}

// 服务名里的引号必须被转义，否则 PromQL 语法错误。
//
// 未转义时服务端会返回 4xx，**看起来像网络问题**，
// 而真实原因是「用户的服务名里有个引号」。
func TestPrometheusEscapesServiceName(t *testing.T) {
	srv := newPromServer(t)
	c := NewPrometheusCollector(srv.URL)

	_, err := c.Collect(context.Background(), evidence.Input{
		IncidentID: 1, Service: `we"ird`, Window: testWindow(),
	})
	if err != nil {
		t.Fatalf("含引号的服务名不应导致失败: %v", err)
	}
	got := srv.lastPromQL()
	if strings.Contains(got, `service="we"ird"`) {
		t.Errorf("query = %q，引号未转义", got)
	}
	if !strings.Contains(got, `\"`) {
		t.Errorf("query = %q，未见转义后的反斜杠", got)
	}
}

// 采集器必须满足领域接口（编译期断言 + 一次真实调用）。
func TestPrometheusSatisfiesCollectorInterface(t *testing.T) {
	var c evidence.Collector = NewPrometheusCollector("http://x")
	if c.Source() != SourcePrometheus {
		t.Errorf("Source() = %q，期望 %q", c.Source(), SourcePrometheus)
	}
}

// 采到空结果不是错误：「查到了但这段时间没数据」是正常结果。
func TestPrometheusEmptyResultIsNotError(t *testing.T) {
	srv := newPromServer(t)
	srv.handler = func(w http.ResponseWriter, r *http.Request) { writePromEmpty(t, w) }

	c := NewPrometheusCollector(srv.URL)
	got, err := c.Collect(context.Background(), evidence.Input{
		IncidentID: 1, Service: "api", Window: testWindow(),
	})
	if err != nil {
		t.Fatalf("全部无样本不应算失败（那是「正常但无数据」）: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("采到 %d 条，期望 0 条", len(got))
	}
}

// NaN 值必须丢弃：它不是「指标是 NaN」，而是「这段区间算不出来」。
func TestPrometheusDropsNaN(t *testing.T) {
	srv := newPromServer(t)
	srv.handler = func(w http.ResponseWriter, r *http.Request) {
		var ts int64
		if _, err := fmt.Sscanf(r.URL.Query().Get("time"), "%d", &ts); err != nil {
			ts = 0
		}
		writePromResult(t, w, ts, "NaN")
	}

	c := NewPrometheusCollector(srv.URL)
	got, err := c.Collect(context.Background(), evidence.Input{
		IncidentID: 1, Service: "api", Window: testWindow(),
	})
	// 三条都是 NaN -> 全部丢弃 -> 视为采集失败（无任何可用证据）。
	if err == nil {
		t.Errorf("全部 NaN 时应报错或返回空，实得 %d 条证据、err=%v", len(got), err)
	}
	for _, e := range got {
		if math.IsNaN(parseOrPanic(t, e.Metadata["value_after"])) {
			t.Errorf("保留了 NaN 值证据: %+v", e)
		}
	}
}

// --- 测试辅助 ---

func metricsOf(evs []evidence.Evidence) []string {
	out := make([]string, 0, len(evs))
	for _, e := range evs {
		out = append(out, e.Metadata["metric"])
	}
	return out
}

func hasFailuresRecorded(evs []evidence.Evidence) bool {
	for _, e := range evs {
		if e.Metadata["failures"] != "" {
			return true
		}
	}
	return false
}

func parseOrPanic(t *testing.T, s string) float64 {
	t.Helper()
	var f float64
	if _, err := fmt.Sscanf(s, "%g", &f); err != nil {
		t.Fatalf("解析 %q 失败: %v", s, err)
	}
	return f
}
