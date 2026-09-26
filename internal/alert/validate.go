package alert

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ParsePayload 把 webhook 请求体解码为 Payload。
//
// 有意不启用 DisallowUnknownFields：上游（Alertmanager）新增字段不应让整个
// webhook 失败，向前兼容优先。真正的结构问题由 ValidatePayload 判断。
func ParsePayload(body []byte) (*Payload, error) {
	var p Payload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("请求体不是合法的 JSON: %w", err)
	}
	return &p, nil
}

// ValidatePayload 做结构级校验。失败意味着**整批**不可用（HTTP 400）。
//
// 只校验结构，不校验单条字段——单条问题由 ValidateAlert 处理，且不牵连同批。
func ValidatePayload(p *Payload) error {
	if p == nil {
		return errors.New("payload 为空")
	}
	if strings.TrimSpace(p.Version) == "" {
		return errors.New(`缺少 version 字段（Alertmanager v4 应为 "4"）`)
	}
	if p.Alerts == nil {
		return errors.New("缺少 alerts 字段（应为数组，可为空）")
	}
	return nil
}

// DecodeRawAlert 把单条原始 JSON 解码为 RawAlert。
func DecodeRawAlert(raw json.RawMessage) (RawAlert, error) {
	var a RawAlert
	if err := json.Unmarshal(raw, &a); err != nil {
		return RawAlert{}, fmt.Errorf("单条告警 JSON 解析失败: %w", err)
	}
	return a, nil
}

// ValidateAlert 做单条字段级校验。
//
// 返回错误只影响该条（计为 rejected），不牵连同批其他告警——否则一条脏数据
// 会让整批告警丢失。调用方负责把错误与条目下标一起记入 Result.Errors。
func ValidateAlert(a *RawAlert) error {
	if a == nil {
		return errors.New("告警为空")
	}
	if strings.TrimSpace(a.Labels[LabelAlertname]) == "" {
		return fmt.Errorf("labels 缺少 %s", LabelAlertname)
	}
	if a.Status != StatusFiring && a.Status != StatusResolved {
		return fmt.Errorf("status 非法: %q（只接受 %s / %s）", a.Status, StatusFiring, StatusResolved)
	}
	if _, err := parseRFC3339(a.StartsAt); err != nil {
		return fmt.Errorf("startsAt 非法: %q", a.StartsAt)
	}
	return nil
}
