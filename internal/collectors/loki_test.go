package collectors

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yu/iip/internal/evidence"
)

// 本文件覆盖手册 §七「采集器单测：三类各自的解析与降级」中 Loki 的部分，
// 手段是 httptest mock server（计划明确要求，不用真实 Loki）。

// lokiServer 是可编程的 Loki 假服务端。
type lokiServer struct {
	*httptest.Server
	// calls 记录收到的查询次数。
	calls atomic.Int32
	// lastQuery 记录最后一次收到的 LogQL。
	lastQuery atomic.Value // string
	// handler 允许逐用例替换响应行为。
	handler func(w http.ResponseWriter, r *http.Request)
}

func newLokiServer(t *testing.T) *lokiServer {
	t.Helper()
	s := &lokiServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.calls.Add(1)
		if q := r.URL.Query().Get("query"); q != "" {
			s.lastQuery.Store(q)
		}
		if s.handler != nil {
			s.handler(w, r)
			return
		}
		writeLokiResult(t, w, testLine(10*time.Minute, "connection refused"))
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *lokiServer) queryCount() int32 { return s.calls.Load() }

func (s *lokiServer) lastLogQL() string {
	v, _ := s.lastQuery.Load().(string)
	return v
}

// lokiLine 是构造 mock 响应用的一条日志。
type lokiLine struct {
	ts   time.Time
	line string
}

// writeLokiResult 回一个 Loki query_range 的成功响应。
func writeLokiResult(t *testing.T, w http.ResponseWriter, lines ...lokiLine) {
	t.Helper()
	values := make([]any, 0, len(lines))
	for _, l := range lines {
		values = append(values, []any{l.ts.UnixNano(), l.line})
	}
	w.Header().Set("Content-Type", "application/json")
	body := map[string]any{
		"status": "success",
		"data": map[string]any{
			"resultType": "streams",
			"result": []any{
				map[string]any{
					"stream": map[string]string{"service": "api"},
					"values": values,
				},
			},
		},
	}
	if err := json.NewEncoder(w).Encode(body); err != nil {
		t.Errorf("编码 mock 响应失败: %v", err)
	}
}

// writeLokiEmpty 回一个「查到了但没有日志」的响应。
func writeLokiEmpty(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	body := map[string]any{
		"status": "success",
		"data":   map[string]any{"resultType": "streams", "result": []any{}},
	}
	if err := json.NewEncoder(w).Encode(body); err != nil {
		t.Errorf("编码 mock 响应失败: %v", err)
	}
}

// isBaselineRequest 报告该请求是否落在基线窗口（前窗口）。
//
// 基线窗口是 [Start-width, Start)，事故窗口是 [Start, End) ——
// 两窗不重叠，所以用「start 是否早于事故窗口起点」判定。
func isBaselineRequest(r *http.Request) bool {
	var start int64
	if _, err := fmt.Sscanf(r.URL.Query().Get("start"), "%d", &start); err != nil {
		return false
	}
	return start < testWindow().Start.UnixNano()
}

// TestLokiCollectsTopPatternsAndBurst 是核心用例：
// 同一窗口内两类错误各多条 → 聚合成 2 条模式证据 + 1 条突增证据。
func TestLokiCollectsTopPatternsAndBurst(t *testing.T) {
	// 日志时间必须落在**被查询的窗口内**。
	//
	// 这一点很容易写错：mock 随手用 time.Now() 造时间戳，
	// 而 testWindow 是固定在 2023 年的，于是返回的日志全在窗口之外。
	// 采集器信任 Loki、不会校验区间，测试照样绿 ——
	// 但那样的测试证明不了「我们把证据关联到了正确的时间段」。
	base := testWindow().Start
	srv := newLokiServer(t)
	srv.handler = func(w http.ResponseWriter, r *http.Request) {
		if isBaselineRequest(r) {
			// 基线：只有 1 条 db 超时。
			// 时间取 Start-30min —— 基线窗口是 [Start-1h, Start)，
			// 用 Start 本身会正好压在边界上（两窗首尾相接、不重叠）。
			writeLokiResult(t, w, lokiLine{
				ts: base.Add(-30 * time.Minute), line: "db timeout after 5000ms",
			})
			return
		}
		// 事故窗口：db 超时 3 条 + 空指针 2 条
		//
		// 两类都写成「同类不同变量」：超时的耗时不同、空指针的行号不同 ——
		// 这才是真实的突增形态。若写成两条字面完全一样的日志，
		// 测的就不是归一化而只是计数了。
		writeLokiResult(t, w,
			lokiLine{ts: base.Add(10 * time.Minute), line: "db timeout after 5000ms"},
			lokiLine{ts: base.Add(11 * time.Minute), line: "db timeout after 7000ms"},
			lokiLine{ts: base.Add(12 * time.Minute), line: "db timeout after 3000ms"},
			lokiLine{ts: base.Add(13 * time.Minute), line: "nil pointer dereference at handler.go:142"},
			lokiLine{ts: base.Add(14 * time.Minute), line: "nil pointer dereference at handler.go:207"},
		)
	}

	c := NewLokiCollector(srv.URL)
	got, err := c.Collect(context.Background(), evidence.Input{
		IncidentID: 1, Service: "api", Window: testWindow(),
	})
	if err != nil {
		t.Fatalf("采集不应失败: %v", err)
	}
	// 2 类模式 + 1 条突增
	if len(got) != 3 {
		t.Fatalf("采到 %d 条证据，期望 3 条（2 类模式 + 1 条突增）:\n%s",
			len(got), dumpEvidence(got))
	}

	// 第 1 类应是 db timeout（3 条 > 2 条）
	first := got[0]
	if first.Type != evidence.TypeLog {
		t.Errorf("第 0 条 type = %q，期望 %q", first.Type, evidence.TypeLog)
	}
	if first.Source != SourceLoki {
		t.Errorf("第 0 条 source = %q，期望 %q", first.Source, SourceLoki)
	}
	if first.Metadata["count"] != "3" {
		t.Errorf("第 0 类错误条数 = %q，期望 3（db timeout 出现 3 次）", first.Metadata["count"])
	}
	if first.Metadata["rank"] != "1" {
		t.Errorf("第 0 类 rank = %q，期望 1", first.Metadata["rank"])
	}
	// 归一化后 "db timeout after #ms" 三条必须聚成一条
	if !strings.Contains(first.Metadata["pattern"], "db timeout") {
		t.Errorf("第 0 类pattern = %q，期望含 \"db timeout\"（数字应被归一化）",
			first.Metadata["pattern"])
	}
	// 数字必须已被抹掉，否则三条不会聚成一条
	if strings.Contains(first.Metadata["pattern"], "5000") {
		t.Errorf("第 0 类 pattern 仍含具体数字 %q —— 归一化没生效，同类错误没聚到一起",
			first.Metadata["pattern"])
	}
	// 基线有 1 条 → 不是新出现的错误
	if first.Metadata["novelty"] != "" {
		t.Errorf("基线已有同类错误，不该标记 novelty=%q", first.Metadata["novelty"])
	}
	if first.Metadata["count_before"] != "1" {
		t.Errorf("第 0 类 count_before = %q，期望 1", first.Metadata["count_before"])
	}

	// 突增证据：5 条 vs 1 条 = 5 倍
	burst := got[2]
	if burst.Metadata["count_after"] != "5" {
		t.Errorf("突增 count_after = %q，期望 5", burst.Metadata["count_after"])
	}
	if burst.Metadata["count_before"] != "1" {
		t.Errorf("突增 count_before = %q，期望 1", burst.Metadata["count_before"])
	}
	if burst.Metadata["change_ratio"] != "5.0000" {
		t.Errorf("突增 change_ratio = %q，期望 5.0000", burst.Metadata["change_ratio"])
	}

	// query 字段必须可复现：含服务名与行过滤正则
	if !strings.Contains(first.Query, `service="api"`) {
		t.Errorf("query 未含服务名 label 匹配: %q", first.Query)
	}
	if !strings.Contains(first.Query, errorPattern) {
		t.Errorf("query 未含错误行过滤正则: %q", first.Query)
	}
	// 证据时间必须来自日志自身，且不得为采集时刻
	for i, e := range got {
		if e.Timestamp.IsZero() {
			t.Errorf("第 %d 条证据时间为空 —— 违反 §4.4", i)
		}
		if e.Timestamp.After(time.Now()) {
			t.Errorf("第 %d 条证据时间 %v 晚于当前时刻 —— 疑似用了采集时刻",
				i, e.Timestamp)
		}
		// 时间必须落在被查询的窗口内。
		//
		// 这条断言是补上一个真实的测试盲区：mock 曾用 time.Now() 造时间戳，
		// 而窗口固定在2023 年 —— 返回的日志全在区间之外。
		// 采集器信任 Loki 不做校验，测试照样绿，
		// 于是「证据关联到了正确时间段」这件事**从未被验证过**。
		if e.Timestamp.Before(testWindow().Start) || !e.Timestamp.Before(testWindow().End) {
			t.Errorf("第 %d 条证据时间 %v 不在采集窗口 [%v, %v) 内",
				i, e.Timestamp.UTC(), testWindow().Start.UTC(), testWindow().End.UTC())
		}
	}
	// 突增证据的时间应是最后一条错误日志的时刻，而不是窗口终点。
	//
	// 比较前把期望值也截到微秒：Loki 给纳秒而 PG 只存微秒，
	// 采集器侧已按 PG 精度截断（见 loki.go 的注释），两边必须同一口径。
	wantLast := base.Add(14 * time.Minute).UTC().Truncate(time.Microsecond)
	if !burst.Timestamp.Equal(wantLast) {
		t.Errorf("突增证据时间 = %v，期望等于最后一条错误日志时刻 %v",
			burst.Timestamp, wantLast)
	}
}

// TestLokiNoLogsIsNotFailure 锁定「无错误日志」是正常结果。
//
// 这是最容易搞错的语义：事故窗口没有错误日志说明服务很健康，
// 若当成采集失败，任务会白白重试三次进死信，而页面永远等不到证据。
func TestLokiNoLogsIsNotFailure(t *testing.T) {
	srv := newLokiServer(t)
	srv.handler = func(w http.ResponseWriter, _ *http.Request) {
		writeLokiEmpty(t, w)
	}

	c := NewLokiCollector(srv.URL)
	got, err := c.Collect(context.Background(), evidence.Input{
		IncidentID: 1, Service: "api", Window: testWindow(),
	})
	if err != nil {
		t.Fatalf("无错误日志不该算采集失败: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("采到 %d 条证据，期望 0 条", len(got))
	}
}

// TestLokiNovelPatternIsFlagged 验证「新出现的错误模式」被单独标记。
//
// 事故前没有、现在有 —— 几乎必然与本次事故相关，
// 而长期存在的错误是背景噪声，两者不该同等呈现。
func TestLokiNovelPatternIsFlagged(t *testing.T) {
	base := time.Now().Add(-2 * time.Hour)
	srv := newLokiServer(t)
	srv.handler = func(w http.ResponseWriter, r *http.Request) {
		if isBaselineRequest(r) {
			// 基线只有一种「无害」错误
			writeLokiResult(t, w, lokiLine{ts: base, line: "cache miss for key abc"})
			return
		}
		writeLokiResult(t, w,
			lokiLine{ts: base.Add(5 * time.Minute), line: "connection pool exhausted size=10"},
			lokiLine{ts: base.Add(6 * time.Minute), line: "connection pool exhausted size=20"},
		)
	}

	c := NewLokiCollector(srv.URL)
	got, err := c.Collect(context.Background(), evidence.Input{
		IncidentID: 1, Service: "api", Window: testWindow(),
	})
	if err != nil {
		t.Fatalf("采集失败: %v", err)
	}
	// 1 类模式（pool exhausted，基线没有）+ 1 条突增
	if len(got) != 2 {
		t.Fatalf("采到 %d 条，期望 2 条:\n%s", len(got), dumpEvidence(got))
	}
	if got[0].Metadata["novelty"] == "" {
		t.Errorf("基线无同类错误，应标记 novelty，实际 novelty=%q count_before=%q",
			got[0].Metadata["novelty"], got[0].Metadata["count_before"])
	}
	// 突增：2 条 vs 1 条基线
	if got[1].Metadata["change_ratio"] != "2.0000" {
		t.Errorf("突增 change_ratio = %q，期望 2.0000", got[1].Metadata["change_ratio"])
	}
}

// TestLokiBurstFromZeroHasNoRatio 验证「从 0涨到 N」不给假倍数。
//
// 事故前 0 条时 ratio 无意义（0 或 +Inf），
// 说成「事故前无错误日志，本次为首次出现」比给一个假倍数有用。
func TestLokiBurstFromZeroHasNoRatio(t *testing.T) {
	srv := newLokiServer(t)
	srv.handler = func(w http.ResponseWriter, r *http.Request) {
		if isBaselineRequest(r) {
			writeLokiEmpty(t, w)
			return
		}
		writeLokiResult(t, w,
			testLine(20*time.Minute, "fatal: out of memory"),
		)
	}

	c := NewLokiCollector(srv.URL)
	got, err := c.Collect(context.Background(), evidence.Input{
		IncidentID: 1, Service: "api", Window: testWindow(),
	})
	if err != nil {
		t.Fatalf("采集失败: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("采到 %d 条，期望 2 条", len(got))
	}
	burst := got[1]
	if burst.Metadata["change_ratio"] != "" {
		t.Errorf("事故前 0 条时不该给 change_ratio，实际 = %q —— 那是假倍数",
			burst.Metadata["change_ratio"])
	}
	if burst.Metadata["count_before"] != "0" {
		t.Errorf("突增 count_before = %q，期望 0", burst.Metadata["count_before"])
	}
	if burst.Metadata["burst"] == "" {
		t.Errorf("事故前 0 条时应说明 burst 情况")
	}
}

// TestLokiTopNRespected 验证 Top N 生效且条数可配。
//
// 一条 panic 刷 500 次、另一条 3 次时，若不限制条数，
// 页面第一条永远是那个 panic，看不到别的错误类型。
func TestLokiTopNRespected(t *testing.T) {
	srv := newLokiServer(t)
	srv.handler = func(w http.ResponseWriter, r *http.Request) {
		if isBaselineRequest(r) {
			writeLokiEmpty(t, w)
			return
		}
		lines := make([]lokiLine, 0, 8)
		// 5 条 panic
		for i := 0; i < 5; i++ {
			lines = append(lines,
				testLine(time.Duration(10+i)*time.Minute, "panic: runtime error index out of range"))
		}
		// 3 条别的
		for i := 0; i < 3; i++ {
			lines = append(lines,
				testLine(time.Duration(20+i)*time.Minute, "failed to renew lease"))
		}
		writeLokiResult(t, w, lines...)
	}

	c := NewLokiCollector(srv.URL, WithTopN(1))
	got, err := c.Collect(context.Background(), evidence.Input{
		IncidentID: 1, Service: "api", Window: testWindow(),
	})
	if err != nil {
		t.Fatalf("采集失败: %v", err)
	}
	// TopN=1 → 1 条模式 + 1 条突增
	if len(got) != 2 {
		t.Fatalf("TopN=1 时采到 %d 条，期望 2 条（1 模式 + 1 突增）:\n%s",
			len(got), dumpEvidence(got))
	}
	if !strings.Contains(got[0].Metadata["pattern"], "panic") {
		t.Errorf("TopN=1 时应保留条数最多的 panic，实际 = %q", got[0].Metadata["pattern"])
	}
	if got[0].Metadata["count"] != "5" {
		t.Errorf("panic 条数 = %q，期望 5", got[0].Metadata["count"])
	}
	// 突增证据的 count_after 是**窗口总量**（8），不是 TopN 过滤后的条数。
	// 若这里也过滤，页面会显示「5 条错误」而实际有 8 条 —— 少报比多报更危险。
	if got[1].Metadata["count_after"] != "8" {
		t.Errorf("突增 count_after = %q，期望 8（窗口总量，不受 TopN 影响）",
			got[1].Metadata["count_after"])
	}
}

// TestLokiBaselineFailureIsRecorded 验证基线真失败被单独记为 failures。
//
// 基线查不到时Top N 证据仍有价值（那正是我们要的），
// 但必须说清「突增算不出来」而不是默默给个空值。
func TestLokiBaselineFailureIsRecorded(t *testing.T) {
	srv := newLokiServer(t)
	srv.handler = func(w http.ResponseWriter, r *http.Request) {
		if isBaselineRequest(r) {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		writeLokiResult(t, w,
			testLine(10*time.Minute, "fatal: db gone"),
		)
	}

	c := NewLokiCollector(srv.URL)
	got, err := c.Collect(context.Background(), evidence.Input{
		IncidentID: 1, Service: "api", Window: testWindow(),
	})
	if err != nil {
		t.Fatalf("基线失败不该让整个采集失败（Top N 证据仍有价值）: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("采到 %d 条，期望 2 条", len(got))
	}
	if !hasFailuresRecorded(got) {
		t.Error("基线窗口查询失败未被记录 —— 数据源故障被静默吞掉了")
	}
	// 突增证据必须说清「算不出来」且原因，不能给假倍数
	burst := got[1]
	if burst.Metadata["change_ratio"] != "" {
		t.Errorf("基线不可用时不该给 change_ratio，实际 = %q", burst.Metadata["change_ratio"])
	}
	if !strings.Contains(burst.Metadata["baseline"], "失败") {
		t.Errorf("基线不可用时 baseline 应说明「失败」，实际 = %q", burst.Metadata["baseline"])
	}
}

// TestLokiWindowFailureIsFatal 验证主窗口失败才算采集失败。
func TestLokiWindowFailureIsFatal(t *testing.T) {
	srv := newLokiServer(t)
	srv.handler = func(w http.ResponseWriter, r *http.Request) {
		if !isBaselineRequest(r) {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		writeLokiResult(t, w, testLineInBaseline("old error"))
	}

	c := NewLokiCollector(srv.URL)
	_, err := c.Collect(context.Background(), evidence.Input{
		IncidentID: 1, Service: "api", Window: testWindow(),
	})
	if err == nil {
		t.Fatal("事故窗口查询失败应让采集失败（否则页面永远等不到证据却显示成功）")
	}
	if !strings.Contains(err.Error(), "事故窗口") {
		t.Errorf("错误信息应指明是哪个窗口失败: %v", err)
	}
}

// TestLokiBothWindowsFailure 验证两窗都失败时报「均失败」。
func TestLokiBothWindowsFailure(t *testing.T) {
	srv := newLokiServer(t)
	srv.handler = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}

	c := NewLokiCollector(srv.URL)
	_, err := c.Collect(context.Background(), evidence.Input{
		IncidentID: 1, Service: "api", Window: testWindow(),
	})
	if err == nil {
		t.Fatal("两窗都失败应让采集失败")
	}
	if !strings.Contains(err.Error(), "均查询失败") {
		t.Errorf("错误信息应说明两个窗口都失败: %v", err)
	}
}

// TestLokiUnparsableResponseIsError 验证「有流但解析不出内容」是错误。
//
// 这是最隐蔽的一种失败：mock 返回了 stream 但结构不对，
// 若当成「无日志」返回空，证据会凭空消失而没有任何日志线索。
func TestLokiUnparsableResponseIsError(t *testing.T) {
	srv := newLokiServer(t)
	srv.handler = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// values 里的元素只有一个字段（结构异常）
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"streams",
			"result":[{"stream":{},"values":[["only-one-field"]]}]}}`))
	}

	c := NewLokiCollector(srv.URL)
	_, err := c.Collect(context.Background(), evidence.Input{
		IncidentID: 1, Service: "api", Window: testWindow(),
	})
	if err == nil {
		t.Fatal("结构异常应报错，不能当成「无错误日志」")
	}
	if !strings.Contains(err.Error(), "无法解析") {
		t.Errorf("错误信息应说明是解析失败: %v", err)
	}
}

// TestLokiStatusNotSuccess 验证 Loki 返回 status!=success 时报错。
func TestLokiStatusNotSuccess(t *testing.T) {
	srv := newLokiServer(t)
	srv.handler = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"error","error":"too many outstanding requests"}`))
	}

	c := NewLokiCollector(srv.URL)
	_, err := c.Collect(context.Background(), evidence.Input{
		IncidentID: 1, Service: "api", Window: testWindow(),
	})
	if err == nil {
		t.Fatal("status=error 应让采集失败")
	}
	if !strings.Contains(err.Error(), "too many outstanding requests") {
		t.Errorf("错误信息应带上Loki 的原始原因: %v", err)
	}
}

// TestLokiRejectsEmptyService 空服务名必须直接失败。
//
// `{service=""}` 会匹配所有没有 service label 的日志流 ——
// 那是整集群的日志，证据看起来「有内容」实则全是噪声。
func TestLokiRejectsEmptyService(t *testing.T) {
	srv := newLokiServer(t)
	c := NewLokiCollector(srv.URL)
	for _, svc := range []string{"", "   "} {
		_, err := c.Collect(context.Background(), evidence.Input{
			IncidentID: 1, Service: svc, Window: testWindow(),
		})
		if err == nil {
			t.Errorf("服务名 = %q 时应拒绝采集（空 label 会捞回整集群日志）", svc)
		}
	}
	if n := srv.queryCount(); n != 0 {
		t.Errorf("服务名为空时仍发了 %d 次请求 —— 应该连查都不查", n)
	}
}

// TestLokiRejectsInvalidWindow 窗口不合法必须失败。
func TestLokiRejectsInvalidWindow(t *testing.T) {
	srv := newLokiServer(t)
	c := NewLokiCollector(srv.URL)
	bad := evidence.Window{Start: time.Now(), End: time.Now().Add(-time.Hour)}
	if _, err := c.Collect(context.Background(), evidence.Input{
		IncidentID: 1, Service: "api", Window: bad,
	}); err == nil {
		t.Error("结束早于开始的窗口应被拒绝（否则会被当成「这段时间没有异常」）")
	}
}

// TestLokiSendsTwoWindowQueries 验证确实查了两个窗口。
//
// 只查一个窗口就拿不到基线，「突增」这个证据就是假的 ——
// 而它的字段名叫 change_ratio，页面会照着算给人看。
func TestLokiSendsTwoWindowQueries(t *testing.T) {
	srv := newLokiServer(t)
	srv.handler = func(w http.ResponseWriter, r *http.Request) {
		if isBaselineRequest(r) {
			writeLokiEmpty(t, w)
			return
		}
		writeLokiResult(t, w, testLine(10*time.Minute, "error: x"))
	}

	c := NewLokiCollector(srv.URL)
	if _, err := c.Collect(context.Background(), evidence.Input{
		IncidentID: 1, Service: "api", Window: testWindow(),
	}); err != nil {
		t.Fatalf("采集失败: %v", err)
	}
	if n := srv.queryCount(); n != 2 {
		t.Errorf("发了 %d 次查询，期望 2 次（事故窗口 + 基线窗口）", n)
	}
}

// TestLokiQueryEscapesServiceName 服务名里的引号必须被转义。
//
// 不转义的话 LogQL 语法错误，而 Loki 返回的是 4xx HTML 错误页——
// 排查时极易误判成「网络问题」或「对方挂了」。
//
// 覆盖三种会破坏语法的字符：引号（提前闭合匹配器）、
// 反斜杠（吃掉后续字符）、换行（让表达式跨行）。
func TestLokiQueryEscapesServiceName(t *testing.T) {
	srv := newLokiServer(t)
	srv.handler = func(w http.ResponseWriter, _ *http.Request) {
		writeLokiResult(t, w, testLine(10*time.Minute, "error: x"))
	}

	c := NewLokiCollector(srv.URL)
	cases := []struct {
		name    string
		service string
		must    string
	}{
		{name: "引号", service: `api" or "x`, must: `\"`},
		{name: "反斜杠", service: `api\default`, must: `\\`},
		{name: "换行", service: "api\n注入", must: `\n`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := c.Collect(context.Background(), evidence.Input{
				IncidentID: 1, Service: tc.service, Window: testWindow(),
			}); err != nil {
				t.Fatalf("采集失败: %v", err)
			}
			if q := srv.lastLogQL(); !strings.Contains(q, tc.must) {
				t.Errorf("服务名 %q 未被正确转义，query = %q（期望含 %q）",
					tc.service, q, tc.must)
			}
		})
	}
}

// TestLokiTimeoutIsBounded 验证超时会生效。
//
// 手册 §4.5：外部数据源挂起时不能阻塞主链路。
// 传 0（无超时）会被忽略而不是变成「无超时」，
// 因为后者意味着一个挂死的 Loki 能把 worker 永久占住。
func TestLokiTimeoutIsBounded(t *testing.T) {
	srv := newLokiServer(t)
	srv.handler = func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(2 * time.Second)
		writeLokiResult(t, w, testLine(10*time.Minute, "error: too late"))
	}
	t.Cleanup(func() { srv.Close() })

	// 超时传 0 应被忽略，仍用默认 5s —— 但本例 sleep 2s < 5s，
	// 所以用一个真正更短的超时来验证超时**能**生效。
	c := NewLokiCollector(srv.URL, WithTimeout(100*time.Millisecond))
	start := time.Now()
	_, err := c.Collect(context.Background(), evidence.Input{
		IncidentID: 1, Service: "api", Window: testWindow(),
	})
	if err == nil {
		t.Fatal("超时后应返回错误（而不是一直挂着）")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("超时未生效，耗时 %v", elapsed)
	}
}

// TestNormalizeLogLine 归一化规则的直接单测。
//
// 这是整个采集器最容易被误改的地方：抹得太狠会把不同故障合并
// （那比漏聚合更糟），抹得太松则Top N 会被一条日志刷满。
func TestNormalizeLogLine(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "不同耗时的同类超时聚成一条",
			in:   "db timeout after 5000ms",
			want: "db timeout after #ms",
		},
		{
			name: "错误码与端口也被抹掉",
			in:   "connect to 10.0.0.5:8080 failed, code=500",
			want: "connect to #:# failed, code=#",
		},
		{
			name: "引号内的 id 被抹掉",
			in:   `user "u-8891" not found`,
			want: "user ? not found",
		},
		{
			name: "标识符里的数字不拆（err500 仍是一个词）",
			in:   "err500 raised",
			want: "err500 raised",
		},
		{
			name: "字母紧跟的数字不拆（http2）",
			in:   "http2 stream reset",
			want: "http2 stream reset",
		},
		{
			name: "无数字无引号的行原样保留",
			in:   "connection pool exhausted",
			want: "connection pool exhausted",
		},
		{
			name: "结尾是小数点时数字仍抹掉、点保留",
			in:   "value is 1.",
			want: "value is #.",
		},
		{
			name: "小数被归一化",
			in:   "took 12.34ms",
			want: "took #ms",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeLogLine(tc.in); got != tc.want {
				t.Errorf("normalizeLogLine(%q)\n got = %q\nwant = %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestNormalizeLogLineGroupsRealisticBurst 验证真实突增场景能聚成一条。
//
// 数据库连接池打满会刷出成百上千条只有数字不同的日志，
// 不聚合的话 Top N 全是同一句话，等于没有信息。
func TestNormalizeLogLineGroupsRealisticBurst(t *testing.T) {
	var entries []lokiEntry
	now := time.Now()
	for i := 0; i < 200; i++ {
		entries = append(entries, lokiEntry{
			ts:   now.Add(-time.Duration(i) * time.Second),
			line: fmt.Sprintf("dial tcp 10.0.%d.%d:5432: connect: connection refused", i%250, (i*7)%250),
		})
	}
	patterns := topPatterns(entries, 10)
	if len(patterns) != 1 {
		t.Fatalf("200 条同类连接错误应聚成 1 类，实际 %d 类:\n%s",
			len(patterns), dumpPatterns(patterns))
	}
	if patterns[0].count != 200 {
		t.Errorf("聚类条数 = %d，期望 200", patterns[0].count)
	}
	// 样本应保留一条原文（供人阅读），而不是归一化后的 key
	if !strings.Contains(patterns[0].sample, "dial tcp") {
		t.Errorf("样本应是原文，实际 = %q", patterns[0].sample)
	}
}

// TestTopPatternsOrderIsStable 验证排序可复现。
//
// map 遍历顺序随机 → 每次采集 Top N 顺序都不同 →
// 页面证据顺序随机跳动，测试也无法断言「第 1 类是哪个」。
func TestTopPatternsOrderIsStable(t *testing.T) {
	now := time.Now()
	var entries []lokiEntry
	// 三类各2 条，条数完全相同 —— 此时只能靠 key 字典序决定顺序
	for _, line := range []string{"alpha error", "beta error", "gamma error"} {
		for i := 0; i < 2; i++ {
			entries = append(entries, lokiEntry{ts: now.Add(-time.Duration(i) * time.Minute), line: line})
		}
	}
	first := topPatterns(entries, 3)
	want := []string{"alpha error", "beta error", "gamma error"}
	if len(first) != len(want) {
		t.Fatalf("应聚成 %d 类，实际 %d 类:\n%s", len(want), len(first), dumpPatterns(first))
	}
	// 连跑 20 次：map 遍历顺序随机，单次比对可能碰巧通过。
	// 这里要证明的是**排序规则本身**稳定，而不是某一次的结果对。
	for run := 0; run < 20; run++ {
		got := topPatterns(entries, 3)
		for i := range want {
			if got[i].key != want[i] {
				t.Fatalf("第 %d 次运行第 %d 类 = %q，期望 %q —— 排序不稳定",
					run, i, got[i].key, want[i])
			}
		}
	}
}

// TestLokiImplementsCollector 编译期契约：Loki 采集器必须满足接口。
func TestLokiImplementsCollector(t *testing.T) {
	var c evidence.Collector = NewLokiCollector("http://localhost:3100")
	if c.Source() != SourceLoki {
		t.Errorf("Source() = %q，期望 %q", c.Source(), SourceLoki)
	}
	// 注册表接受它
	reg := evidence.NewRegistry()
	if err := reg.Register(c); err != nil {
		t.Errorf("注册失败: %v", err)
	}
	if reg.Len() != 1 {
		t.Errorf("注册表长度 = %d，期望 1", reg.Len())
	}
}
