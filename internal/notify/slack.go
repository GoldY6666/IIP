package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// SlackSink 把通知发到 Slack Incoming Webhook（T6.1，ADR-024）。
//
// payload 用最简单的 {"text": "..."} 形态而非 Block Kit：两类消息的
// 信息密度一张纯文本卡片就够，Block Kit 的构造代码量翻倍而本期无
// 对应需求（要按钮/分区展示是 T6.2 之后的事，届时再加不破坏文本形态——
// Slack 对带 text 的 blocks 会退回显示 text）。
type SlackSink struct {
	webhookURL string
	uiBaseURL  string
	client     *http.Client
}

// 编译期断言：*SlackSink 必须满足 Sink。
var _ Sink = (*SlackSink)(nil)

// maxSlackRespBytes 是错误响应体的读取上限。
//
// Slack 的失败响应（invalid_token 等）本体很小；设上限是防某个代理
// 把 502 页面整页灌进错误信息——那会让日志里出现几千字符的 HTML。
const maxSlackRespBytes = 64 << 10

// NewSlackSink 构造 Slack 出口。
//
// timeout 直接作为 http.Client.Timeout（与采集器 WithTimeout 同思路：
// 一个挂死的 webhook 不能占住派发线程）。uiBaseURL 用于拼事件深链，
// 传空则消息不带链接（降级可用，但不推荐——详见 ADR-024 的代价节）。
func NewSlackSink(webhookURL, uiBaseURL string, timeout time.Duration) *SlackSink {
	return &SlackSink{
		webhookURL: webhookURL,
		uiBaseURL:  strings.TrimRight(uiBaseURL, "/"),
		// 与通用 webhook 同一套拒重定向策略：webhook URL 本身就是凭据，
		// 一次 302 就能把它交给第三方（见 noRedirectClient 注释）。
		client: noRedirectClient(timeout),
	}
}

// Name 实现 Sink。
func (s *SlackSink) Name() string { return "slack" }

// Send 实现 Sink：POST 一条文本消息。非 2xx 返回带状态与响应体摘要的
// 可读错误（运维按这个错误就能判断是 token 失效还是网络不通）。
func (s *SlackSink) Send(ctx context.Context, ev *Event) error {
	body, err := json.Marshal(map[string]string{"text": formatText(ev, s.uiBaseURL)})
	if err != nil {
		return fmt.Errorf("序列化 Slack 消息失败: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.webhookURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("构造 Slack 请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("发送 Slack 消息失败: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, maxSlackRespBytes))
		// 错误串以中文开头：staticcheck ST1005 禁止首字母大写（英文语境规则），
		// 把出口名放到句中既避开该规则，也让中文读起来更顺。
		return fmt.Errorf("发送 Slack 消息失败: webhook 返回 %s: %s", resp.Status, strings.TrimSpace(string(snippet)))
	}
	// 响应体必须读完再关：连接复用依赖它；失败响应在上面已读。
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxSlackRespBytes))
	return nil
}

// formatText 把事件渲染成 Slack 文本消息。**纯函数**——无 HTTP、无时钟，
// 全部输入来自 ev 与 uiBaseURL，故消息形状可直接单测（不 mock 服务器）。
func formatText(ev *Event, uiBaseURL string) string {
	var b strings.Builder
	switch ev.Kind {
	case KindIncidentCreated:
		// 只用 Title：关联引擎生成的标题本身已带服务（"LatencyHigh (checkout)"），
		// 再拼一次会出现 "LatencyHigh (checkout)（checkout）"。
		fmt.Fprintf(&b, "🔔 新事件 #%d：%s", ev.Incident.ID, ev.Incident.Title)
	case KindStatusChanged:
		actor := ev.Actor
		if actor == "" {
			actor = "系统"
		}
		fmt.Fprintf(&b, "🔔 事件 #%d 状态变更：%s → %s（操作人: %s）",
			ev.Incident.ID, ev.FromStatus, ev.ToStatus, actor)
	default:
		// 不可达：Kind 只有两个取值。写出来是为了将来加第三种时
		// 消息形态有明确落脚点，而不是落进空文本。
		fmt.Fprintf(&b, "🔔 事件 #%d 通知", ev.Incident.ID)
	}

	fmt.Fprintf(&b, "\n• 标题: %s", ev.Incident.Title)
	fmt.Fprintf(&b, "\n• 严重度: %s｜服务: %s", ev.Incident.Severity, ev.Incident.Service)
	fmt.Fprintf(&b, "\n• 关联告警: %d 条｜证据: %d 条", ev.AlertCount, ev.EvidenceCount)

	if ev.Kind == KindIncidentCreated {
		fmt.Fprintf(&b, "\n• 开始于: %s", ev.Incident.StartedAt.Format(time.RFC3339))
	}
	if link := incidentLink(uiBaseURL, ev.Incident.ID); link != "" {
		fmt.Fprintf(&b, "\n• 详情: %s", link)
	}
	return b.String()
}

// incidentLink 拼事件深链。uiBaseURL 为空返回空串（调用方据此省略该行）。
func incidentLink(uiBaseURL string, id int64) string {
	if uiBaseURL == "" {
		return ""
	}
	return uiBaseURL + "/incidents/" + strconv.FormatInt(id, 10)
}
