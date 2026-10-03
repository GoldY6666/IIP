package incident

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
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

// EventAlertFired 是 0001 就存在的既有多余类型，M3 起赋予它明确语义：**事件创建**
// （手册 §4.2：M2 NEW 档建事件时写一条）。
//
// 复用它而不是新增一个 incident_created：CHECK 里本就有这个值且零存量行，
// 复用既不扩大取值集合，也省掉一次迁移。
const EventAlertFired EventType = "alert_fired"

// EventEvidenceFound 是 0001 就存在、0004 保留的类型，M4 起赋予它明确语义：
// **一轮证据采集完成**（手册 §一 的交接物说明）。
//
// 复用而不新增：CHECK 里本就有这个值且 M3 之前零存量行，
// 复用既不扩大取值集合，也省掉一次迁移 —— 与 EventAlertFired 同样的理由。
const EventEvidenceFound EventType = "evidence_found"

// NewEvidenceFoundEntry 构造「一轮证据采集完成」留痕（M4 worker 采集后写入）。
//
// 存在的理由：证据列表页只显示**最新 50 条**（手册 §4.2 的硬上限），
// 而用户会问「这个事件一共采了多少条、是不是被截断了」。
// 答案不该只存在于 API 响应的 truncated 字段里 —— 那是程序读的；
// 时间线是给人读的，而「采到 73 条只留了 50 条」正是需要人说清的事。
//
// metadata 固定带四个键，前端不必解析 description 文本：
//   - collected：本次实际采到的条数（截断前）
//   - kept：落库条数
//   - truncated：是否因超限被截断
//   - window：采集窗口 [start, end]，RFC3339
//
// sources 记各采集器的产出条数（形如 "prometheus=3,loki=11"）：
// 只给总数的话，用户看到「采到 14 条」仍不知道是哪个数据源在工作，
// 而「Loki 一次贡献 11 条」直接说明日志量才是本次事故的主体。
func NewEvidenceFoundEntry(
	incidentID int64, collected, kept int, truncated bool,
	windowStart, windowEnd time.Time, sources map[string]int, ts time.Time,
) Entry {
	md := map[string]string{
		"collected": strconv.Itoa(collected),
		"kept":      strconv.Itoa(kept),
		"truncated": strconv.FormatBool(truncated),
	}
	if !windowStart.IsZero() && !windowEnd.IsZero() {
		md["window"] = windowStart.UTC().Format(time.RFC3339) + "/" +
			windowEnd.UTC().Format(time.RFC3339)
	}
	if len(sources) > 0 {
		// 按来源名字典序拼，保证同一批采集的留痕可逐字比对
		// （map 遍历顺序随机会让每次刷新看到的字符串都不一样）。
		keys := make([]string, 0, len(sources))
		for k := range sources {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s=%d", k, sources[k]))
		}
		md["sources"] = strings.Join(parts, ",")
	}

	desc := fmt.Sprintf("证据采集完成，采到 %d 条", collected)
	if truncated {
		desc += fmt.Sprintf("，超出单事件上限，仅保留最新 %d 条", kept)
	} else if kept == 0 {
		desc += "，本轮没有新证据"
	}
	return Entry{
		IncidentID:  incidentID,
		EventType:   EventEvidenceFound,
		Description: desc,
		Actor:       SystemActor,
		Metadata:    md,
		Timestamp:   ts,
	}
}

// EventUserComment 是 0001 就存在的既有类型，用于用户备注。//
// REVIEW 的确认 / 拒绝也复用它（metadata 带 action=review_confirm / review_reject），
// 而不是再新增两个专用类型——用户已锁定只新增 7 个专用值（手册 §4.2）。
const EventUserComment EventType = "user_comment"

// SystemActor 是系统自动写入时落在 actor 列的值。
//
// 写具体字符串而不是留 NULL：时间线要能一眼区分「谁做的」——
// NULL 只能表达「没有操作人」，表达不了「是系统做的」。
const SystemActor = "system"

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

// NewAlertFiredEntry 构造「事件创建」留痕（M2 NEW 档建事件时写入）。
//
// metadata 带 alert_id 与 fingerprint：前者供前端跳到告警，后者用于判断
// 「同一指纹再次起火」这类重复（fingerprint 已剔除 instance/pod 等动态键）。
func NewAlertFiredEntry(incidentID, alertID int64, fingerprint string, ts time.Time) Entry {
	return Entry{
		IncidentID:  incidentID,
		EventType:   EventAlertFired,
		Description: fmt.Sprintf("告警 #%d 触发，据此创建事件", alertID),
		Actor:       SystemActor,
		Metadata: map[string]string{
			"alert_id":    strconv.FormatInt(alertID, 10),
			"fingerprint": fingerprint,
		},
		Timestamp: ts,
	}
}

// NewAlertLinkedEntry 构造「告警合入事件」留痕（M2 MERGE / REVIEW 档建边时写入）。
//
// score 保留 4 位小数，与 incident_alerts.correlation_score 的 DECIMAL(5,4) 对齐；
// needs_review 直接写进 metadata，让前端不必为了标「待确认」再回查关联边。
func NewAlertLinkedEntry(incidentID, alertID int64, score float64, needsReview bool, ts time.Time) Entry {
	desc := fmt.Sprintf("告警 #%d 关联到本事件（score %.4f）", alertID, score)
	if needsReview {
		desc += "，待人工确认"
	}
	return Entry{
		IncidentID:  incidentID,
		EventType:   EventAlertLinked,
		Description: desc,
		Actor:       SystemActor,
		Metadata: map[string]string{
			"alert_id":     strconv.FormatInt(alertID, 10),
			"score":        strconv.FormatFloat(score, 'f', 4, 64),
			"needs_review": strconv.FormatBool(needsReview),
		},
		Timestamp: ts,
	}
}

// 人工复核的两种动作，写在 user_comment 的 metadata.action 上（手册 §4.2）。
const (
	ReviewActionConfirm = "review_confirm"
	ReviewActionReject  = "review_reject"
)

// NewReviewCommentEntry 构造一条人工复核留痕（confirm / reject 共用 user_comment）。
//
// 复用的原因：用户已锁定 M3 只新增 7 个专用事件类型，复核不再单列类型；
// 区别靠 metadata.action 承载，前端据此决定显示「已确认」还是「已拒绝」。
func NewReviewCommentEntry(incidentID, alertID int64, action, actor, reason string, ts time.Time) Entry {
	desc := "人工复核：确认该告警属于本事件"
	if action == ReviewActionReject {
		desc = "人工复核：判定该告警不属于本事件，已解除关联"
	}
	metadata := map[string]string{
		"action":   action,
		"alert_id": strconv.FormatInt(alertID, 10),
	}
	if reason != "" {
		metadata["reason"] = reason
	}
	return Entry{
		IncidentID:  incidentID,
		EventType:   EventUserComment,
		Description: desc,
		Actor:       actor,
		Metadata:    metadata,
		Timestamp:   ts,
	}
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
