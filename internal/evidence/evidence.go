// Package evidence 承载 M4 的证据采集领域逻辑：证据类型、采集器契约与注册表。
//
// 本包与 task 包同构：领域规则（类型是否合法、注册表怎么查、预算怎么算）
// 一律零 SQL、零网络，可在无库无网环境下完整测试。
//
// 规格来源：docs/IIP-M4详细开发手册.md §4.2 / §4.3 / §4.4。
package evidence

import (
	"errors"
	"fmt"
	"sort"
	"time"
)

// Type 是证据类型，取值与 0001 的 chk_evidence_type 一一对应。
type Type string

const (
	// TypeMetric 是指标类证据（Prometheus 的 RED 指标等）。
	TypeMetric Type = "metric"
	// TypeLog 是日志类证据（Loki 的错误日志 Top10、突增统计等）。
	TypeLog Type = "log"
	// TypeTrace 是链路类证据。**本期无采集器**（手册 OPEN-M4-2），
	// 但类型必须留着：它是 evidence 表既有 CHECK 约束的一部分，
	// 且M5 的 RCA 会用到。
	TypeTrace Type = "trace"
	// TypeDeployment 是部署变更类证据（K8s rollout 记录等）。
	TypeDeployment Type = "deployment"
	// TypeConfigChange 是配置变更类证据。
	TypeConfigChange Type = "config_change"
	// TypeHistorical 是历史类证据（与历史共现比对）。
	TypeHistorical Type = "historical"
)

// Type 的合法取值集合。
//
// 刻意用集合而不是「挨个 if 判断」：新增类型时只改这一处，
// 且能直接由它生成测试用例的输入表，避免「代码加了枚举、测试忘了改」。
var validTypes = map[Type]struct{}{
	TypeMetric:       {},
	TypeLog:          {},
	TypeTrace:        {},
	TypeDeployment:   {},
	TypeConfigChange: {},
	TypeHistorical:   {},
}

// IsValid 报告该类型是否在 evidence 表的 CHECK 约束内。
//
// 存在的意义：**入库前**就能拦住非法类型，而不是让 PG 抛
// chk_evidence_type 违规——后者发生在批量插入的中途，
// 会把前面已写的证据一起拖下水（若包在事务里）。
func (t Type) IsValid() bool {
	_, ok := validTypes[t]
	return ok
}

// Source 是证据来源标识，落库到 evidence.source。
//
// 用字符串而非枚举：来源是**外部系统**的名字（prometheus / loki / k8s / git），
// 本项目不可能穷举，也不该为了加一个来源就改代码。
type Source = string

// Input 是一次采集的输入。
type Input struct {
	// IncidentID 是目标事件。
	IncidentID int64

	// Service 是事件对应的服务名，采集器用它做 label 匹配。
	//
	// 允许为空：那表示「这个事件没有可用的服务标识」，
	// 采集器应据此跳过需要服务名的查询并记录原因，
	// 而不是拿空串去查（空 label 匹配会捞回一堆无关数据）。
	Service string

	// Window 是采集的时间窗口 [Start, End]。
	//
	// 显式传窗口而非让采集器自己取「最近一小时」：那样每次采集的
	// 实际区间都随调用时刻漂移，证据的 timestamp 就没法互相比较，
	// 也没法在页面上说清「这几条证据覆盖哪段时间」。
	Window Window
}

// Window 是采集时间窗口。
type Window struct {
	Start time.Time
	End   time.Time
}

// IsZero 报告窗口是否未设置。
func (w Window) IsZero() bool {
	return w.Start.IsZero() && w.End.IsZero()
}

// Validate 报告窗口是否可用于采集。
//
// 拒绝 End 在 Start 之前：那样的窗口会让「时间序列对比」算出负区间，
// 而多数采集器不会报错——它们只是返回空结果，
// 于是「采集失败」被伪装成「这段时间没有异常」。
func (w Window) Validate() error {
	if w.Start.IsZero() || w.End.IsZero() {
		return errors.New("采集窗口的起止时间都必须设置")
	}
	if w.End.Before(w.Start) {
		return fmt.Errorf("采集窗口的结束时间 %v 早于开始时间 %v", w.End, w.Start)
	}
	if w.End.Equal(w.Start) {
		return fmt.Errorf("采集窗口的起止时间相同（%v），无法做窗口前后对比", w.Start)
	}
	return nil
}

// Duration 返回窗口长度。
func (w Window) Duration() time.Duration {
	return w.End.Sub(w.Start)
}

// Evidence 是一条采集到的证据。
//
// Timestamp 是**证据自身的时间**（Prometheus 样本时刻、日志时间戳、
// K8s 事件发生时刻），**不是采集时刻**（手册 §4.4 / ADR-003）。
// 采不到真实时间的证据**一律不写**，不得用 time.Now() 兜底——
// 那会让所有证据看起来「同时发生」，而事件真相往往横跨一段时间。
type Evidence struct {
	Type      Type
	Source    Source
	Query     string
	Value     string
	Timestamp time.Time

	// Confidence 是对假设的支撑强度 0–1。
	//
	// **本期无采集器填写**（手册 OPEN-M4-1）：留空（nil）而非填 1.0——
	// 填 1.0 等于宣称「这条证据确凿」，而采集器并没有这个判断能力。
	// 留空的语义是「未评估」，前端据此不显示置信度，
	// 而不是显示「置信度 0」。
	Confidence *float64

	// Metadata 是补充信息，序列化成 evidence.metadata JSONB。
	Metadata map[string]string
}

// Validate 报告该条证据是否可入库。
//
// 这是入库前最后一道闸：非法 type、空 timestamp、空 source
// 三者都会在 PG 上被拒（type 有 CHECK、NOT NULL），但那时可能已经写了一半。
func (e Evidence) Validate() error {
	if !e.Type.IsValid() {
		return fmt.Errorf("证据类型 %q 非法（合法值：%v）", e.Type, SortedTypes())
	}
	if e.Source == "" {
		return errors.New("证据来源不能为空——没有来源的证据无法追溯")
	}
	if e.Timestamp.IsZero() {
		// 这是 §4.4 的强制项：宁可少一条证据，
		// 也不要把「采集时刻」伪装成「证据时刻」。
		return errors.New("证据时间不能为空，且不得用当前时刻兜底（ADR-003）")
	}
	if e.Confidence != nil && (*e.Confidence < 0 || *e.Confidence > 1) {
		return fmt.Errorf("置信度 %v 超出 0–1 区间", *e.Confidence)
	}
	return nil
}

// SortedTypes 返回全部合法类型，按字典序，供错误信息与测试使用。
func SortedTypes() []Type {
	out := make([]Type, 0, len(validTypes))
	for t := range validTypes {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// 采集器错误。
var (
	// ErrNoInput 表示采集器收到的输入不合法。
	ErrNoInput = errors.New("采集输入不合法")
)
