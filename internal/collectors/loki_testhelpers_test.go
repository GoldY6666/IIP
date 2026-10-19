package collectors

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

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
