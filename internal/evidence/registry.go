package evidence

import (
	"context"
	"errors"
	"fmt"
	"sort"
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
