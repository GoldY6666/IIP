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

// slackCapture 记录一次 webhook 请求，供断言 payload 形状。
type slackCapture struct {
	mu      sync.Mutex
	content string
	body    string
	calls   int
}

func (c *slackCapture) handler(status int, respBody string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		c.calls++
		c.content = r.Header.Get("Content-Type")
		raw := make([]byte, 4096)
		n, _ := r.Body.Read(raw)
		c.body = string(raw[:n])
		c.mu.Unlock()

		w.WriteHeader(status)
		_, _ = w.Write([]byte(respBody))
	}
}

// TestSlackSinkSendsJSONPayload 钉住 webhook 契约：POST + JSON + {"text":...}，
// 且消息含标题、严重度、计数与深链。
func TestSlackSinkSendsJSONPayload(t *testing.T) {
	var cap slackCapture
	srv := httptest.NewServer(cap.handler(http.StatusOK, "ok"))
	defer srv.Close()

	sink := NewSlackSink(srv.URL, "http://ui.example.com", 2*time.Second)
	ev := Event{
		Kind:          KindIncidentCreated,
		Incident:      testRef(),
		AlertCount:    3,
		EvidenceCount: 5,
	}
	if err := sink.Send(context.Background(), &ev); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if cap.content != "application/json" {
		t.Errorf("Content-Type 应为 application/json，实际 %q", cap.content)
	}
	var payload map[string]string
	if err := json.Unmarshal([]byte(cap.body), &payload); err != nil {
		t.Fatalf("请求体不是 JSON: %v（body=%q）", err, cap.body)
	}
	text, ok := payload["text"]
	if !ok {
		t.Fatalf("payload 缺 text 字段: %v", payload)
	}
	for _, want := range []string{
		"HighLatency",
		"critical",
		"checkout",
		"关联告警: 3 条",
		"证据: 5 条",
		"http://ui.example.com/incidents/42",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("消息缺少 %q，实际:\n%s", want, text)
		}
	}
	if cap.calls != 1 {
		t.Errorf("应只发 1 次请求，实际 %d 次", cap.calls)
	}
}

// TestSlackSinkNon2xxReadableError 钉住失败可诊断：
// 错误里必须同时有状态码与响应体摘要，运维据此区分 token 失效与网络不通。
func TestSlackSinkNon2xxReadableError(t *testing.T) {
	var cap slackCapture
	srv := httptest.NewServer(cap.handler(http.StatusInternalServerError, "channel_not_found"))
	defer srv.Close()

	sink := NewSlackSink(srv.URL, "", time.Second)
	err := sink.Send(context.Background(), &Event{Kind: KindIncidentCreated, Incident: testRef()})
	if err == nil {
		t.Fatal("非 2xx 应返回错误")
	}
	msg := err.Error()
	if !strings.Contains(msg, "500") {
		t.Errorf("错误应含状态码 500，实际: %s", msg)
	}
	if !strings.Contains(msg, "channel_not_found") {
		t.Errorf("错误应含响应体摘要，实际: %s", msg)
	}
}

// TestSlackSinkTimeoutBounded 钉住「挂死的 webhook 不占住派发线程」：
// 客户端超时必须生效，Send 在超时附近返回而不是一直等。
func TestSlackSinkTimeoutBounded(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
	}))
	defer func() {
		close(release)
		srv.Close()
	}()

	sink := NewSlackSink(srv.URL, "", 100*time.Millisecond)
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

// TestFormatTextShape 钉住消息文本形状（纯函数，不需要 HTTP）：
// 状态变更带操作人，操作人为空显示「系统」；无 uiBaseURL 时不输出详情行。
func TestFormatTextShape(t *testing.T) {
	ev := Event{
		Kind:          KindStatusChanged,
		Incident:      testRef(),
		FromStatus:    "open",
		ToStatus:      "acknowledged",
		Actor:         "zhangsan",
		AlertCount:    1,
		EvidenceCount: 2,
	}
	got := formatText(&ev, "")
	for _, want := range []string{"open → acknowledged", "操作人: zhangsan", "关联告警: 1 条"} {
		if !strings.Contains(got, want) {
			t.Errorf("消息缺少 %q，实际:\n%s", want, got)
		}
	}
	if strings.Contains(got, "详情:") {
		t.Errorf("未配置 uiBaseURL 时不应有详情行，实际:\n%s", got)
	}

	ev.Actor = ""
	if got := formatText(&ev, ""); !strings.Contains(got, "操作人: 系统") {
		t.Errorf("操作人为空应显示「系统」，实际:\n%s", got)
	}

	// 事件创建消息带开始时间；状态变更不带。
	created := Event{Kind: KindIncidentCreated, Incident: testRef()}
	if got := formatText(&created, ""); !strings.Contains(got, "开始于: 2026-10-17T19:30:00Z") {
		t.Errorf("事件创建消息应含开始时间，实际:\n%s", got)
	}
}

// TestFormatTextNoDuplicateService 钉住「标题里已有服务名时不再追加」。
//
// 实测踩过：关联引擎的 incidentTitle 产出 "LatencyHigh (checkout)"，
// 消息模板又拼了一次服务 → "LatencyHigh (checkout)（checkout）"。
// 断言的是**不出现第二次服务名**，不是具体括号形态。
func TestFormatTextNoDuplicateService(t *testing.T) {
	ref := testRef()
	ref.Title = "LatencyHigh (checkout)"
	got := formatText(&Event{Kind: KindIncidentCreated, Incident: ref}, "")
	if strings.Contains(got, "(checkout)（") {
		t.Errorf("不应在标题后重复追加服务名，实际:\n%s", got)
	}
}
