package incident

import (
	"fmt"
	"time"
)

// EventType 是时间线条目的类型。取值与 0004 迁移里 timeline.event_type 的 CHECK
// 约束一致；本文件只定义 M3 **状态流转**这一类要写的 7 个专用值（手册 §4.2）。
//
// 刻意不复用既有的通用类型 status_change：专用类型让时间线可以只靠 event_type
// 就被前端分组着色，不必去解析 metadata。
type EventType string

// 7 个状态流转专用类型（对应 6 个目标状态 + 重开）。
const (
	EventAlertLinked          EventType = "alert_linked"          // 告警合入事件（M2 关联建边时）
	EventAcknowledged         EventType = "acknowledged"          // 认领 / 确认
	EventInvestigationStarted EventType = "investigation_started" // 开始调查
	EventMitigated            EventType = "mitigated"             // 标记缓解
	EventResolved             EventType = "resolved"              // 解决
	EventIgnored              EventType = "ignored"               // 判为误报
	EventReopened             EventType = "reopened"              // 重开
)

// transitionEventTypes 是「目标状态 → 事件类型」的映射。
//
// 用目标状态做键而非 (from,to) 组合：状态流转的语义由**落到哪个状态**决定，
// 从哪来无关——open→resolved 与 mitigated→resolved 写的是同一类事件，
// 差异由 metadata 里的 from 承载。这样 6 个状态只需 6 条映射。
var transitionEventTypes = map[Status]EventType{
	StatusOpen:          EventReopened, // 只有终态能回到 open，故即重开
	StatusAcknowledged:  EventAcknowledged,
	StatusInvestigating: EventInvestigationStarted,
	StatusMitigated:     EventMitigated,
	StatusResolved:      EventResolved,
	StatusIgnored:       EventIgnored,
}

// EventTypeForTransition 给出一次状态流转应写入的事件类型。
//
// from → open 只在「终态重开」时合法，因此落到这里必是 reopened，无需额外判断。
// to 非法（未知状态）时返回错误——宁可写不进去，也不要写入一条语义不明的留痕。
func EventTypeForTransition(to Status) (EventType, error) {
	et, ok := transitionEventTypes[to]
	if !ok {
		return "", fmt.Errorf("目标状态 %q 没有对应的时间线事件类型", to)
	}
	return et, nil
}

// NewTransitionEntry 构造一条状态流转留痕。
//
// Metadata 固定带 from / to：前端渲染「从 X 到 Y」时不该去解析 description 文本。
// Timestamp 由调用方传入而非取 time.Now()——流转服务需要可注入时钟，
// 否则「resolved_at 与 timeline.timestamp 是否同一刻」这类断言无法稳定验证。
func NewTransitionEntry(incidentID int64, from, to Status, actor string, ts time.Time) (Entry, error) {
	eventType, err := EventTypeForTransition(to)
	if err != nil {
		return Entry{}, err
	}
	return Entry{
		IncidentID:  incidentID,
		EventType:   eventType,
		Description: fmt.Sprintf("状态变更 %s → %s", from, to),
		Actor:       actor,
		Metadata: map[string]string{
			"from": string(from),
			"to":   string(to),
		},
		Timestamp: ts,
	}, nil
}
