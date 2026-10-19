package collectors

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/yu/iip/internal/evidence"
)

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
