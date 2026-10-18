package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// GenericSink 把通知 POST 成**机器可读**的 JSON，给外部系统接收（T6.2）。
//
// 与 SlackSink 的分工：Slack 是给人看的纯文本，这里是给程序看的结构化字段；
// 两者可以同时开启，共用同一条异步队列与同一套限流。
type GenericSink struct {
	url    string
	token  string
	client *http.Client
}

// 编译期断言：*GenericSink 必须满足 Sink。
var _ Sink = (*GenericSink)(nil)

// maxWebhookRespBytes 是错误响应体的读取上限。
//
// 与 Slack 同理由：避免某个代理把整页 HTML 灌进日志里的错误串。
const maxWebhookRespBytes = 64 << 10

// NewGenericSink 构造通用出站。token 为空时不带 Authorization 头。
func NewGenericSink(url, token string, timeout time.Duration) *GenericSink {
	return &GenericSink{
		url:    url,
		token:  strings.TrimSpace(token),
		client: noRedirectClient(timeout),
	}
}

// Name 实现 Sink。两个出口日志里用 "slack" / "webhook" 区分。
func (s *GenericSink) Name() string { return "webhook" }

// webhookIncident 是 payload 里的事件摘要。
type webhookIncident struct {
	ID        int64     `json:"id"`
	Title     string    `json:"title"`
	Severity  string    `json:"severity"`
	Service   string    `json:"service"`
	Status    string    `json:"status"`
	StartedAt time.Time `json:"started_at"`
}

// webhookPayload 是出站 JSON 契约（ADR-025）。
//
// 用 snake_case 与 API 响应保持一致的读法；from/to/actor 只在状态变更时有值，
// 故 omitempty —— 创建通知里出现空的 "from_status":"" 会让接收端疑惑。
type webhookPayload struct {
	Kind          string          `json:"kind"`
	Incident      webhookIncident `json:"incident"`
	FromStatus    string          `json:"from_status,omitempty"`
	ToStatus      string          `json:"to_status,omitempty"`
	Actor         string          `json:"actor,omitempty"`
	AlertCount    int64           `json:"alert_count"`
	EvidenceCount int64           `json:"evidence_count"`
}

// newWebhookPayload 把 Event 映射成出站契约。**纯函数**，便于单测。
func newWebhookPayload(ev *Event) webhookPayload {
	return webhookPayload{
		Kind: string(ev.Kind),
		Incident: webhookIncident{
			ID:        ev.Incident.ID,
			Title:     ev.Incident.Title,
			Severity:  ev.Incident.Severity,
			Service:   ev.Incident.Service,
			Status:    ev.Incident.Status,
			StartedAt: ev.Incident.StartedAt,
		},
		FromStatus:    ev.FromStatus,
		ToStatus:      ev.ToStatus,
		Actor:         ev.Actor,
		AlertCount:    ev.AlertCount,
		EvidenceCount: ev.EvidenceCount,
	}
}

// Send 实现 Sink。
//
// 错误串**不含 token**：日志与错误信息经常被集中收集，凭据不能跟着跑。
func (s *GenericSink) Send(ctx context.Context, ev *Event) error {
	body, err := json.Marshal(newWebhookPayload(ev))
	if err != nil {
		return fmt.Errorf("序列化通用 webhook 消息失败: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("构造通用 webhook 请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("发送通用 webhook 消息失败: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, maxWebhookRespBytes))
		return fmt.Errorf("发送通用 webhook 消息失败: 接收端返回 %s: %s",
			resp.Status, strings.TrimSpace(string(snippet)))
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxWebhookRespBytes))
	return nil
}

// noRedirectClient 返回**不跟随重定向**的 HTTP 客户端。
//
// 为什么两个 sink 都必须要：接收端一个 302 就能把请求体（含事件详情）
// 连同 Authorization 头转发到任意主机——出站白名单由配置固定，
// 不允许在运行期被服务端牵着走。3xx 直接落进非 2xx 错误路径，
// 运维能从错误串里立刻看到「接收端给了个跳转」。
func noRedirectClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}
