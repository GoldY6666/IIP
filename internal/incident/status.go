// Package incident 承载 M3 的事件生命周期领域逻辑：状态机、时间线与处置服务。
//
// 本包的领域层（status.go / event.go）一律**零 SQL**：状态流转的合法性是纯业务
// 规则，只有真正落库的部分才经过仓储层，这样 6×6 转换矩阵可以脱离数据库被完整测试。
package incident

import (
	"fmt"
	"strings"
)

// Status 是事件状态。取值全小写，与 0004 迁移里 incidents.status 的 CHECK 约束
// 一一对应——两者必须同步修改，否则「领域层放行、数据库拒绝」会在运行期才暴露。
type Status string

// 六个状态（手册 §4.1）。
const (
	StatusOpen          Status = "open"          // 刚创建，无人认领
	StatusAcknowledged  Status = "acknowledged"  // 已确认收到，尚未开始调查（M3 新增）
	StatusInvestigating Status = "investigating" // 正在调查
	StatusMitigated     Status = "mitigated"     // 已缓解，根因未除
	StatusResolved      Status = "resolved"      // 已解决（终态）
	StatusIgnored       Status = "ignored"       // 误报 / 无需处理（终态）
)

// CodeInvalidTransition 是非法状态转换的错误码，供 API 层写入响应体的 code 字段。
const CodeInvalidTransition = "invalid_transition"

// ladder 是状态的阶序，手册 §4.1 声明它是转换规则的**唯一真源**。
//
// 位置即含义：下标越大越靠后，「只许前向」就是下标递增。把 ignored 放在末端
// 不是随意的——这样「任意非终态 → ignored」无需单列规则即天然成立。
var ladder = []Status{
	StatusOpen,
	StatusAcknowledged,
	StatusInvestigating,
	StatusMitigated,
	StatusResolved,
	StatusIgnored,
}

// AllStatuses 返回全部合法状态，顺序与 ladder 一致。
//
// 返回**副本**：ladder 是本包判定逻辑的依据，一旦被外部改写，状态机会在无报错的
// 情况下改变行为，这类问题极难定位。
func AllStatuses() []Status {
	out := make([]Status, len(ladder))
	copy(out, ladder)
	return out
}

// severities 是事件严重度的合法取值，与 0001 的 chk_incidents_severity 一致。
var severities = []string{"critical", "warning", "info"}

// AllSeverities 返回全部合法严重度（副本，理由同 AllStatuses）。
//
// 列表过滤里「不过滤」= 传入全部合法取值，而不是在 SQL 里写 NULL 分支；
// 因此这个集合必须与数据库的 CHECK 约束保持同步。
func AllSeverities() []string {
	out := make([]string, len(severities))
	copy(out, severities)
	return out
}

// StatusList 返回以逗号分隔的合法取值，用于错误信息里直接给出可选范围。
func StatusList() string {
	parts := make([]string, len(ladder))
	for i, s := range ladder {
		parts[i] = string(s)
	}
	return strings.Join(parts, ", ")
}

// ParseStatus 把字符串解析为 Status；未知取值返回错误并列出合法范围。
//
// 空串不是合法状态：API 层若漏传 to 字段，必须在此被拦住，而不是被当成某个默认值。
func ParseStatus(raw string) (Status, error) {
	for _, s := range ladder {
		if string(s) == raw {
			return s, nil
		}
	}
	return "", fmt.Errorf("未知事件状态 %q，合法取值: %s", raw, StatusList())
}

// IsValid 报告该状态是否在六个合法取值之内。
func IsValid(s Status) bool {
	return indexOf(s) >= 0
}

// IsTerminal 报告该状态是否为终态（resolved / ignored）。
//
// 终态不是「不能动」，而是「只能经 open 重开」——真实值班里误关后重开很常见，
// 把终态做成彻底死锁会逼用户去改数据库。
func IsTerminal(s Status) bool {
	return s == StatusResolved || s == StatusIgnored
}

// CanTransition 判定 from → to 是否合法（手册 §4.1 的阶序口径）：
//
//	from == to                → false  同状态不算转换
//	from 或 to 未知            → false  未知状态一律不放行
//	from 是终态                → 仅 to == open 合法（重开）
//	to == open（from 非终态）  → false  不能回到起点
//	其余                      → 下标递增才合法（只许前向，越级前向允许）
func CanTransition(from, to Status) bool {
	if from == to {
		return false
	}
	fi, ti := indexOf(from), indexOf(to)
	if fi < 0 || ti < 0 {
		return false
	}
	if IsTerminal(from) {
		return to == StatusOpen
	}
	if to == StatusOpen {
		return false
	}
	return ti > fi
}

// Reason 说明一次转换被判非法的原因，用于生成可读的错误文案。
type Reason string

// 四种非法原因，与 CanTransition 的判定分支一一对应。
const (
	ReasonSameStatus Reason = "same_status"    // 同状态，无需转换
	ReasonTerminal   Reason = "terminal"       // 终态未走重开
	ReasonBackward   Reason = "backward"       // 试图回退
	ReasonUnknown    Reason = "unknown_status" // 出现未知状态
)

// InvalidTransitionError 描述一次非法的状态转换。
//
// 刻意把 From / To / Reason 都留在结构体上：API 层要据此拼 422 响应，
// 日志层要据此定位是谁在什么时候做了越权流转，只留一个拼好的字符串会两头不够用。
type InvalidTransitionError struct {
	From   Status
	To     Status
	Reason Reason
}

// Error 的错误文案必须同时包含 from 与 to 两个状态名（手册 §4.1），
// 这样前端可以直接展示，不必自己再拼一遍。
func (e *InvalidTransitionError) Error() string {
	switch e.Reason {
	case ReasonSameStatus:
		return fmt.Sprintf("事件已处于 %s 状态，无需再次转换（%s → %s）", e.From, e.From, e.To)
	case ReasonTerminal:
		return fmt.Sprintf("%s 是终态，只能重开为 %s（%s → %s）", e.From, StatusOpen, e.From, e.To)
	case ReasonBackward:
		return fmt.Sprintf("状态只可前向推进，不允许从 %s 转为 %s（%s → %s）", e.From, e.To, e.From, e.To)
	case ReasonUnknown:
		return fmt.Sprintf("状态 %s → %s 中存在未知状态，合法取值: %s", e.From, e.To, StatusList())
	default:
		return fmt.Sprintf("非法状态转换（%s → %s）", e.From, e.To)
	}
}

// Code 返回供 API 响应体使用的错误码。
func (e *InvalidTransitionError) Code() string {
	return CodeInvalidTransition
}

// NewTransitionError 在 from → to 非法时给出带原因的错误；合法时返回 nil。
//
// 复用 CanTransition 的判定顺序推导原因，避免两处各写一遍规则后 someday 不同步。
func NewTransitionError(from, to Status) error {
	if CanTransition(from, to) {
		return nil
	}
	e := &InvalidTransitionError{From: from, To: to}
	switch {
	case from == to:
		e.Reason = ReasonSameStatus
	case !IsValid(from) || !IsValid(to):
		e.Reason = ReasonUnknown
	case IsTerminal(from):
		e.Reason = ReasonTerminal
	default:
		e.Reason = ReasonBackward
	}
	return e
}

// indexOf 返回状态在 ladder 中的下标，-1 表示未知。
func indexOf(s Status) int {
	for i, cand := range ladder {
		if cand == s {
			return i
		}
	}
	return -1
}
