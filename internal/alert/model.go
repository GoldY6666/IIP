// Package alert 实现 M1 的告警接入领域逻辑：解析、校验、归一化、指纹与落库编排。
//
// 包内除 Service 外均为**纯函数**（无 I/O），持久化能力通过 Repository 接口注入，
// 便于表驱动单测。规格来源：docs/IIP-M1详细开发手册.md §4。
package alert

import (
	"encoding/json"
	"errors"
	"time"
)

// SourceAlertmanager 是 M1 唯一支持的告警来源，写入 alerts.source。
const SourceAlertmanager = "alertmanager"

// 告警状态取值。受 0001 的 chk_alerts_status 约束，不得扩展。
const (
	StatusFiring     = "firing"
	StatusResolved   = "resolved"
	StatusSuppressed = "suppressed"
)

// 严重级别取值。受 0001 的 chk_alerts_severity 约束，不得扩展。
const (
	SeverityCritical = "critical"
	SeverityWarning  = "warning"
	SeverityInfo     = "info"
)

// 标签键名。
const (
	// LabelAlertname 标识「同一条告警规则」，是校验时的必填标签。
	LabelAlertname = "alertname"
	// labelSeverity 是被归一化映射的严重级别标签。
	labelSeverity = "severity"
)

// ErrNotFound 表示按条件未查到告警。
var ErrNotFound = errors.New("alert: 未找到")

// Payload 是 Alertmanager v4 webhook 的请求体。
//
// Alerts 保留为 json.RawMessage 切片而非结构体切片：单条告警既要结构化成
// RawAlert 做校验与归一化，又要原样存进 alerts.raw_payload（Evidence-First），
// 一次解码即可两用，且不必二次序列化。
type Payload struct {
	Version           string            `json:"version"`
	GroupKey          string            `json:"groupKey"`
	Status            string            `json:"status"`
	Receiver          string            `json:"receiver"`
	GroupLabels       map[string]string `json:"groupLabels"`
	CommonLabels      map[string]string `json:"commonLabels"`
	CommonAnnotations map[string]string `json:"commonAnnotations"`
	Alerts            []json.RawMessage `json:"alerts"`
}

// RawAlert 是 payload.alerts[] 中单条告警的原始形态。
type RawAlert struct {
	Status      string            `json:"status"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	StartsAt    string            `json:"startsAt"`
	EndsAt      string            `json:"endsAt"`

	// Fingerprint 是 Alertmanager 自算的指纹，M1 **有意不使用**。
	// 它把 instance 等动态标签也算在内，机器或容器重建后必然改变，
	// 与「同一故障跨实例重建仍能去重」的目标不符。IIP 一律按 GenerateFingerprint 自算。
	// 保留该字段仅为把这一事实写进代码，见手册 §4.3。
	Fingerprint string `json:"fingerprint"`
}

// Alert 是归一化后的领域对象（尚未落库，故不带主键）。
//
// 字段带 json tag：列表接口的响应直接序列化本类型（见 internal/api/alert_list.go），
// 须与手册 §3.1 的契约对齐——snake_case 且与 alerts 表列名一致。
// （Result 等响应体早已如此；Alert/StoredAlert 之前只用于内部流转，故补上。）
type Alert struct {
	Fingerprint string          `json:"fingerprint"`
	Source      string          `json:"source"`
	Severity    string          `json:"severity"`
	Status      string          `json:"status"`
	Labels      json.RawMessage `json:"labels"`
	Annotations json.RawMessage `json:"annotations"`
	StartsAt    time.Time       `json:"starts_at"`
	EndsAt      *time.Time      `json:"ends_at"`
	RawPayload  json.RawMessage `json:"raw_payload"`

	// IncidentID 在 M1 恒为 nil —— 关联属 M2，M1 不得臆造。
	IncidentID *int64 `json:"incident_id"`
}

// StoredAlert 是已落库的告警（带主键）。
//
// Alert 以匿名嵌入方式存在：encoding/json 会把内层字段提升为顶层，
// 故响应是扁平的 {id, fingerprint, source, ...}，而不是嵌套的 {id, Alert:{...}}。
type StoredAlert struct {
	ID int64 `json:"id"`
	Alert
}

// Result 是一次 webhook 投递的处理结果，直接映射为 HTTP 响应体。
type Result struct {
	Accepted int         `json:"accepted"`
	Skipped  int         `json:"skipped"`
	Updated  int         `json:"updated"`
	Rejected int         `json:"rejected"`
	Errors   []ItemError `json:"errors,omitempty"`
}

// ItemError 指出批量投递中具体哪一条告警被拒及其原因。
type ItemError struct {
	Index int    `json:"index"`
	Error string `json:"error"`
}
