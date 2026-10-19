package collectors

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/yu/iip/internal/evidence"
)

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
