package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// hookCapture 记录一次出站请求。
type hookCapture struct {
	mu      sync.Mutex
	calls   int
	content string
	auth    string
	body    string
}

func (c *hookCapture) snapshot() (int, string, string, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls, c.content, c.auth, c.body
}

// TestGenericSinkSendsJSONPayload 钉住出站契约：
// POST + JSON + snake_case 字段 + 计数 + 鉴权头。
func TestGenericSinkSendsJSONPayload(t *testing.T) {
	var cap hookCapture
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := make([]byte, 8192)
		n, _ := r.Body.Read(raw)
		cap.mu.Lock()
		cap.calls++
		cap.content = r.Header.Get("Content-Type")
		cap.auth = r.Header.Get("Authorization")
		cap.body = string(raw[:n])
		cap.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	sink := NewGenericSink(srv.URL, "s3cr3t", 2*time.Second)
	ev := &Event{
		Kind:          KindStatusChanged,
		Incident:      testRef(),
		FromStatus:    "open",
		ToStatus:      "acknowledged",
		Actor:         "zhangsan",
		AlertCount:    3,
		EvidenceCount: 5,
	}
	if err := sink.Send(context.Background(), ev); err != nil {
		t.Fatalf("Send: %v", err)
	}

	calls, content, auth, body := cap.snapshot()
	if calls != 1 {
		t.Fatalf("应发 1 次，实际 %d", calls)
	}
	if content != "application/json" {
		t.Errorf("Content-Type 应为 application/json，实际 %q", content)
	}
	if auth != "Bearer s3cr3t" {
		t.Errorf("Authorization 头不符: %q", auth)
	}

	var got map[string]any
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("请求体不是 JSON: %v（body=%q）", err, body)
	}
	want := map[string]any{
		"kind":           "status_changed",
		"from_status":    "open",
		"to_status":      "acknowledged",
		"actor":          "zhangsan",
		"alert_count":    float64(3),
		"evidence_count": float64(5),
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v，期望 %v", k, got[k], v)
		}
	}
	inc, ok := got["incident"].(map[string]any)
	if !ok {
		t.Fatalf("payload 缺 incident 对象: %v", got)
	}
	if inc["id"] != float64(42) || inc["title"] != "HighLatency" || inc["service"] != "checkout" {
		t.Errorf("incident 摘要不符: %v", inc)
	}
}

// TestGenericSinkOmitsEmptyStatusFields：创建通知不应出现空的 from/to。
func TestGenericSinkOmitsEmptyStatusFields(t *testing.T) {
	p := newWebhookPayload(&Event{Kind: KindIncidentCreated, Incident: testRef(), AlertCount: 1})
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, k := range []string{"from_status", "to_status", "actor"} {
		if strings.Contains(string(raw), k) {
			t.Errorf("创建通知不应包含 %s，实际: %s", k, raw)
		}
	}
	if !strings.Contains(string(raw), `"kind":"incident_created"`) {
		t.Errorf("kind 不符: %s", raw)
	}
}

// TestGenericSinkNon2xxReadableError 钉住失败可诊断，且**不泄露 token**。
func TestGenericSinkNon2xxReadableError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("receiver exploded"))
	}))
	defer srv.Close()

	sink := NewGenericSink(srv.URL, "topsecret", time.Second)
	err := sink.Send(context.Background(), &Event{Kind: KindIncidentCreated, Incident: testRef()})
	if err == nil {
		t.Fatal("非 2xx 应返回错误")
	}
	msg := err.Error()
	if !strings.Contains(msg, "500") || !strings.Contains(msg, "receiver exploded") {
		t.Errorf("错误应含状态码与响应摘要，实际: %s", msg)
	}
	if strings.Contains(msg, "topsecret") {
		t.Errorf("错误串不得包含 token，实际: %s", msg)
	}
}

// TestGenericSinkTimeoutBounded：挂死的接收端不能占住派发线程。
func TestGenericSinkTimeoutBounded(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		<-release
	}))
	defer func() {
		close(release)
		srv.Close()
	}()

	sink := NewGenericSink(srv.URL, "", 100*time.Millisecond)
	start := time.Now()
	err := sink.Send(context.Background(), &Event{Kind: KindIncidentCreated, Incident: testRef()})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("超时应返回错误")
	}
	if elapsed > 3*time.Second {
		t.Errorf("Send 耗时 %v，超时未生效", elapsed)
	}
}

// TestSinksDoNotFollowRedirect 钉住两个出口的拒重定向：
// 接收端 302 到第三方时，第三方**一次请求都不该收到**，且错误里能看到 302。
//
// 泄露面：通用 webhook 的 Authorization 头、Slack 的密钥 URL。
func TestSinksDoNotFollowRedirect(t *testing.T) {
	var thirdParty int32
	var mu sync.Mutex
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		thirdParty++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer evil.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, evil.URL+"/steal", http.StatusFound)
	}))
	defer redirector.Close()

	cases := []struct {
		name string
		sink Sink
	}{
		{"generic", NewGenericSink(redirector.URL, "token-abc", time.Second)},
		{"slack", NewSlackSink(redirector.URL, "", time.Second)},
	}
	ev := &Event{Kind: KindIncidentCreated, Incident: testRef()}
	for _, c := range cases {
		err := c.sink.Send(context.Background(), ev)
		if err == nil {
			t.Errorf("%s: 302 应返回错误（不跟随）", c.name)
			continue
		}
		if !strings.Contains(err.Error(), "302") {
			t.Errorf("%s: 错误应指明 302，实际: %v", c.name, err)
		}
	}

	mu.Lock()
	got := thirdParty
	mu.Unlock()
	if got != 0 {
		t.Fatalf("重定向目标不应收到任何请求，实际 %d 次", got)
	}
}
