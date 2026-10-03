// Package evidence 承载 M4 的证据采集领域逻辑：证据类型、采集器契约与注册表。
//
// 本包与 task 包同构：领域规则（类型是否合法、注册表怎么查、预算怎么算）
// 一律零 SQL、零网络，可在无库无网环境下完整测试。
//
// 规格来源：docs/IIP-M4详细开发手册.md §4.2 / §4.3 / §4.4。
package evidence

import (
	"context"
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

// Collector 采集一类证据。
//
// 契约（三条都是硬要求）：
//  1. 实现必须**无状态且并发安全**——同一实例会被多个 worker goroutine 同时调用。
//  2. 返回空切片表示「采到了但无内容」，是**正常结果**；
//     返回错误表示「采集本身失败」（连接不上、超时、解析不了）。
//     两者混为一谈会让「这段时间没有异常」被记成采集失败并触发无谓重试。
//  3. Collect 失败**不得让整批任务失败**——三类数据源任一挂掉，
//     其余两类仍要落库（手册 §4.3）。
type Collector interface {
	// Source 返回证据来源标识，落库到 evidence.source。
	Source() Source

	// Collect 采集 incident 在window 窗口内的证据。
	Collect(ctx context.Context, in Input) ([]Evidence, error)
}

// Registry 是采集器注册表，按 Source 建索引。
//
// 「新增采集器不改既有代码」的兑现方式：worker 只与注册表打交道，
// 而注册表的填充是**装配期**的一行声明，不需要改任何调用点。
type Registry struct {
	bySource map[Source]Collector
}

// NewRegistry 建一个空注册表。
func NewRegistry() *Registry {
	return &Registry{bySource: make(map[Source]Collector)}
}

// Register 注册一个采集器。
//
// 同一 source 重复注册**报错**而非静默覆盖：两个采集器写同一个 source
// 会让「证据来自哪个系统」变得不确定，而那是事后无法追查的。
func (r *Registry) Register(c Collector) error {
	if c == nil {
		return errors.New("采集器不能为 nil")
	}
	src := c.Source()
	if src == "" {
		return errors.New("采集器的 Source() 不能为空")
	}
	if _, dup := r.bySource[src]; dup {
		return fmt.Errorf("来源 %q 已被注册，不能重复注册", src)
	}
	r.bySource[src] = c
	return nil
}

// MustRegister 是 Register 的「必须成功」版本，用于装配期的静态声明。
//
// 装配期注册失败意味着代码写错了（比如忘了配开关），
// 那属于启动就该暴露的编程错误，不该等到第一次采集才报错。
func (r *Registry) MustRegister(c Collector) {
	if err := r.Register(c); err != nil {
		panic("evidence: 注册采集器失败: " + err.Error())
	}
}

// Get 按来源取采集器。
func (r *Registry) Get(src Source) (Collector, bool) {
	c, ok := r.bySource[src]
	return c, ok
}

// Collectors 返回全部已注册采集器，**按来源排序**。
//
// 排序不是为了好看：并发采集时若遍历顺序随机，测试的失败就不可复现；
// 而「先采 Prometheus 再采 Loki」这种固定顺序能���错误信息可读。
func (r *Registry) Collectors() []Collector {
	out := make([]Collector, 0, len(r.bySource))
	for _, c := range r.bySource {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Source() < out[j].Source() })
	return out
}

// Sources 返回全部已注册来源，按字典序。
func (r *Registry) Sources() []Source {
	out := make([]Source, 0, len(r.bySource))
	for s := range r.bySource {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// Len 返回已注册采集器数量。
func (r *Registry) Len() int {
	return len(r.bySource)
}

// DefaultMaxPerIncident 是单个事件保留的证据上限（手册 §4.2）。
//
// 为什么必须有上限：Prometheus 的高频指标在 1 小时窗口里能出上千个点，
// 不截断的话一次事件就把证据表撑爆，而后续采集全部变慢。
const DefaultMaxPerIncident = 50

// Result 是一次采集的汇总结果。
type Result struct {
	// Evidence 是实际保留的证据。
	Evidence []Evidence

	// Truncated 报告是否因超限被截断。
	//
	// 必须能被 API 读到：把「只保留了 50 条」说成「一共 50 条」，
	// 用户会以为没有更多证据（与 M3 的 truncated 同一理由）。
	Truncated bool

	// TotalCollected 是截断**前**的采集总数。
	TotalCollected int
}

// Apply 汇总多个采集器的产出，并施加单事件上限。
//
// 刻意**不去重**：同一个 (type, source, timestamp, query) 出现两次，
// 可能是真的采到了两条（比如两个 Prometheus 分片都返回了该样本），
// 去重会静默丢数据。若将来确认要按某键去重，须先在手册里写明判据。
func Apply(results [][]Evidence, maxPerIncident int) Result {
	total := 0
	for _, r := range results {
		total += len(r)
	}

	if maxPerIncident <= 0 {
		maxPerIncident = DefaultMaxPerIncident
	}

	flat := make([]Evidence, 0, total)
	for _, r := range results {
		flat = append(flat, r...)
	}

	if len(flat) <= maxPerIncident {
		return Result{Evidence: flat, Truncated: false, TotalCollected: total}
	}
	return Result{
		Evidence:       flat[:maxPerIncident],
		Truncated:      true,
		TotalCollected: total,
	}
}

// CollectorResult 是单个采集器的产出与失败原因。
type CollectorResult struct {
	// Source 是该采集器的来源标识。
	Source Source

	// Evidence 是采到的合法证据。
	Evidence []Evidence

	// Err 是采集本身的失败原因（连接不上、超时、解析不了等）。
	// nil 表示采集成功——**包括采到空结果**。
	Err error

	// Dropped 是被丢弃的非法证据条数。
	//
	// 单独计数而不只记在Err 里：丢弃是**部分成功**，
	// 若与 Err 混在一起，调用方无法区分「整个采集器挂了」与
	// 「拿到了 8 条、其中 1 条时间戳非法」。前者该重试，后者不该。
	Dropped int

	// DropReason 说明丢弃原因（第一条）。
	DropReason error
}

// CollectAll 依次调用注册表里的每个采集器，**单个失败不影响其余**。
//
// 这是手册 §4.3 的关键约束：三类数据源任一挂掉，其余两类仍要落库。
// 若**全部**失败，调用方应把整个任务判为失败（而不是静默成功）——
// 「什么都没采到」和「采到了 0 条」是两回事。
func CollectAll(ctx context.Context, reg *Registry, in Input) ([]Evidence, []CollectorResult) {
	collectors := reg.Collectors()
	results := make([]CollectorResult, 0, len(collectors))
	all := make([]Evidence, 0, len(collectors))

	for _, c := range collectors {
		got, err := c.Collect(ctx, in)
		res := CollectorResult{Source: c.Source(), Err: err}
		if err == nil {
			// 单条非法证据不该拖垮整个采集器：跳过它，其余照收。
			for _, e := range got {
				if verr := e.Validate(); verr != nil {
					res.Dropped++
					if res.DropReason == nil {
						res.DropReason = fmt.Errorf("采集到非法证据已丢弃: %w", verr)
					}
					continue
				}
				res.Evidence = append(res.Evidence, e)
			}
		}
		all = append(all, res.Evidence...)
		results = append(results, res)
	}
	return all, results
}

// AllFailed 报告是否**所有**采集器都失败了。
//
// 判据是「有采集器被注册 且 每个都Err != nil」：
//   - 一个都没注册 → false（那是配置问题，不是采集失败，不该触发重试风暴）；
//   - 都成功但采到 0 条 → false（「采到了但无内容」是正常结果，手册 §七）。
func AllFailed(results []CollectorResult) bool {
	if len(results) == 0 {
		return false
	}
	for _, r := range results {
		if r.Err == nil {
			return false
		}
	}
	return true
}
