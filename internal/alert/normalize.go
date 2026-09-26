package alert

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// endsAtSentinel 是 Alertmanager 表示「尚无结束时间」的哨兵值
// （即 Go time.Time 零值的 RFC3339 序列化结果）。
//
// **必须映射为 SQL NULL**，否则 alerts.ends_at 会写进一个公元 1 年的时间戳，
// 后续任何按时间排序或区间比较都会被污染。
const endsAtSentinel = "0001-01-01T00:00:00Z"

// unknownSeverityFallback 是 severity 缺失或无法识别时的兜底值。
//
// 兜底而非报错：上游 severity 命名不可控（page / emergency / none / 自定义...），
// 报错会把「上游改了个标签名」升级成「告警全部丢失」。
// 取 info 是刻意选择——最保守，不会把普通告警误升级成 critical；
// 同时返回 warning 让调用方记日志，保证问题可见。见手册 §4.2 阶段二。
const unknownSeverityFallback = SeverityInfo

// severityAliases 把上游 severity 归一到 0001 允许的三个取值。
// 键一律小写——比较前会先 trim + 转小写。
var severityAliases = map[string]string{
	"critical":  SeverityCritical,
	"page":      SeverityCritical,
	"emergency": SeverityCritical,
	"fatal":     SeverityCritical,
	"error":     SeverityCritical,
	"warning":   SeverityWarning,
	"warn":      SeverityWarning,
	"info":      SeverityInfo,
	"notice":    SeverityInfo,
	"none":      SeverityInfo,
	"debug":     SeverityInfo,
}

// MapSeverity 归一 severity。
//
// 第二个返回值表示该值是否被**显式识别**：false 意味着走了兜底，
// 调用方应据此打 WARN 日志，避免上游命名漂移被静默吞掉。
func MapSeverity(raw string) (string, bool) {
	if mapped, ok := severityAliases[strings.ToLower(strings.TrimSpace(raw))]; ok {
		return mapped, true
	}
	return unknownSeverityFallback, false
}

// Normalize 把单条原始告警转成领域对象。
//
// topStatus 是 payload 顶层的 status，仅用于一致性告警：两者不一致时
// **以单条为准**（单条才是该告警自身的状态），并把差异记入 warnings。
// warnings 供调用方记日志，不影响处理结果。
func Normalize(a *RawAlert, raw json.RawMessage, topStatus string) (Alert, []string, error) {
	if a == nil {
		return Alert{}, nil, errors.New("告警为空")
	}

	var warnings []string

	severity, recognized := MapSeverity(a.Labels[labelSeverity])
	if !recognized {
		warnings = append(warnings, fmt.Sprintf(
			"severity %q 无法识别，已兜底为 %s", a.Labels[labelSeverity], unknownSeverityFallback))
	}

	status := a.Status
	if topStatus != "" && topStatus != status {
		warnings = append(warnings, fmt.Sprintf(
			"顶层 status=%q 与单条 status=%q 不一致，以单条为准", topStatus, status))
	}

	startsAt, err := parseRFC3339(a.StartsAt)
	if err != nil {
		return Alert{}, warnings, fmt.Errorf("startsAt 非法: %w", err)
	}
	endsAt, err := parseEndsAt(a.EndsAt)
	if err != nil {
		return Alert{}, warnings, fmt.Errorf("endsAt 非法: %w", err)
	}

	labels, err := marshalLabels(a.Labels)
	if err != nil {
		return Alert{}, warnings, fmt.Errorf("序列化 labels 失败: %w", err)
	}
	annotations, err := marshalLabels(a.Annotations)
	if err != nil {
		return Alert{}, warnings, fmt.Errorf("序列化 annotations 失败: %w", err)
	}

	return Alert{
		Fingerprint: GenerateFingerprint(a.Labels),
		Source:      SourceAlertmanager,
		Severity:    severity,
		Status:      status,
		Labels:      labels,
		Annotations: annotations,
		StartsAt:    startsAt,
		EndsAt:      endsAt,
		RawPayload:  raw,
		IncidentID:  nil,
	}, warnings, nil
}

// parseRFC3339 解析告警源的时间戳（Alertmanager 输出 RFC3339，允许小数秒）。
func parseRFC3339(s string) (time.Time, error) {
	return time.Parse(time.RFC3339, strings.TrimSpace(s))
}

// parseEndsAt 解析 endsAt，并把「无结束时间」的哨兵值映射为 nil（SQL NULL）。
func parseEndsAt(s string) (*time.Time, error) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" || trimmed == endsAtSentinel {
		return nil, nil
	}
	t, err := parseRFC3339(trimmed)
	if err != nil {
		return nil, err
	}
	if t.IsZero() {
		return nil, nil
	}
	return &t, nil
}

// marshalLabels 把标签 map 序列化进 JSONB 列。
//
// nil / 空 map 必须兜成 {} —— 列定义是 NOT NULL DEFAULT '{}'，
// 传 SQL NULL 会被直接拒绝（而不会走 DEFAULT）。
func marshalLabels(m map[string]string) (json.RawMessage, error) {
	if len(m) == 0 {
		return json.RawMessage("{}"), nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return b, nil
}
