package evidence

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// 本文件覆盖 M4 手册 §七「必测的边界」中属于领域层的两条：
//   - 三个采集器全部失败 → 任务失败（不是静默成功）；
//   - 其中一个失败 → 其余仍要落库。
// 手段是注入假 Collector，不碰数据库与网络。

// fakeCollector 是可控的测试替身。
type fakeCollector struct {
	src Source
	evs []Evidence
	err error
	// calls 记录被调用次数，用于验证「失败后其余仍被调用」。
	calls int
	// in 记录收到的输入。
	in Input
}

func (f *fakeCollector) Source() Source { return f.src }

func (f *fakeCollector) Collect(_ context.Context, in Input) ([]Evidence, error) {
	f.calls++
	f.in = in
	if f.err != nil {
		return nil, f.err
	}
	return f.evs, nil
}

// 合法证据的构造器，省得每处都写全字段。
func validEvidence(src Source, ts time.Time) Evidence {
	return Evidence{
		Type:      TypeMetric,
		Source:    src,
		Query:     "up{job=\"x\"}",
		Value:     "1",
		Timestamp: ts,
	}
}

func fixedTime() time.Time {
	return time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
}

// 一个失败、另两个成功 → 成功的证据必须都在，且失败被单独记录。
func TestCollectAllKeepsGoingWhenOneCollectorFails(t *testing.T) {
	reg := NewRegistry()
	good1 := &fakeCollector{src: "loki", evs: []Evidence{validEvidence("loki", fixedTime())}}
	bad := &fakeCollector{src: "prometheus", err: errors.New("连接超时")}
	good2 := &fakeCollector{src: "k8s", evs: []Evidence{validEvidence("k8s", fixedTime())}}
	for _, c := range []Collector{good1, bad, good2} {
		reg.MustRegister(c)
	}

	in := Input{IncidentID: 1, Service: "api", Window: Window{Start: fixedTime(), End: fixedTime().Add(time.Hour)}}
	evs, results := CollectAll(context.Background(), reg, in)

	// 失败的那个**必须真的被调用过**，否则这条断言会因为「压根没跑」而假通过。
	for name, c := range map[string]*fakeCollector{"loki": good1, "prometheus": bad, "k8s": good2} {
		if c.calls != 1 {
			t.Errorf("采集器 %s 被调用 %d 次，期望 1 次——失败后跳过了后续采集器", name, c.calls)
		}
	}

	if len(evs) != 2 {
		t.Fatalf("保留证据 %d 条，期望 2 条（失败的 prometheus 不影响另两个）", len(evs))
	}
	// 来源按字典序遍历，结果顺序也应稳定：k8s < loki。
	if evs[0].Source != "k8s" || evs[1].Source != "loki" {
		t.Errorf("证据顺序为 [%s, %s]，期望 [k8s, loki]（注册表应按来源排序遍历）",
			evs[0].Source, evs[1].Source)
	}

	if AllFailed(results) {
		t.Error("有两个采集器成功，AllFailed 应为 false")
	}

	// 失败原因必须能被 worker 拿到（要记 error 日志）。
	var found bool
	for _, r := range results {
		if r.Source == "prometheus" {
			found = true
			if r.Err == nil {
				t.Error("prometheus 的结果里 Err 为 nil，失败原因丢了")
			}
		}
	}
	if !found {
		t.Error("结果里没有 prometheus 这一项，无法记录它的失败")
	}
}

// 三个全失败 → AllFailed 为 true（调用方据此让任务失败并重试）。
func TestCollectAllReportsAllFailed(t *testing.T) {
	reg := NewRegistry()
	for _, s := range []Source{"prometheus", "loki", "k8s"} {
		reg.MustRegister(&fakeCollector{src: s, err: fmt.Errorf("%s 挂了", s)})
	}

	in := Input{IncidentID: 1, Window: Window{Start: fixedTime(), End: fixedTime().Add(time.Hour)}}
	evs, results := CollectAll(context.Background(), reg, in)

	if len(evs) != 0 {
		t.Errorf("全失败时保留证据 %d 条，期望 0 条", len(evs))
	}
	if !AllFailed(results) {
		t.Error("三个采集器全失败，AllFailed 应为 true —— 否则会静默成功、证据永远缺失")
	}
	if len(results) != 3 {
		t.Errorf("结果条目 %d 条，期望 3 条", len(results))
	}
}

// 一个都没注册 → 不是「采集失败」，是配置问题。
//
// 判据差异很实际：当成失败会触发无谓的重试与死信，
// 而真问题是「采集器没装配上」，该在启动时就暴露。
func TestCollectAllWithEmptyRegistryIsNotFailure(t *testing.T) {
	reg := NewRegistry()
	evs, results := CollectAll(context.Background(), reg, Input{IncidentID: 1})
	if len(evs) != 0 {
		t.Errorf("空注册表保留证据 %d 条，期望 0 条", len(evs))
	}
	if AllFailed(results) {
		t.Error("空注册表不应判为「全失败」——那是配置问题，不是数据源宕机")
	}
}

// 采到空切片 → 成功，不是失败（「采到了但无内容」是正常结果，手册 §七）。
func TestEmptyResultIsSuccessNotFailure(t *testing.T) {
	reg := NewRegistry()
	reg.MustRegister(&fakeCollector{src: "prometheus", evs: nil})
	reg.MustRegister(&fakeCollector{src: "loki", evs: []Evidence{}})

	in := Input{IncidentID: 1, Window: Window{Start: fixedTime(), End: fixedTime().Add(time.Hour)}}
	evs, results := CollectAll(context.Background(), reg, in)

	if len(evs) != 0 {
		t.Errorf("采到空结果时证据数 = %d，期望 0", len(evs))
	}
	if AllFailed(results) {
		t.Error("两个采集器都成功返回（只是无内容），不应判为全失败 —— 会导致无谓重试")
	}
}

// 部分非法证据被丢弃时，其余证据仍要保留，且丢弃被单独计数。
func TestInvalidEvidenceIsDroppedButRestKept(t *testing.T) {
	reg := NewRegistry()
	//第二条缺 timestamp（违反 ADR-003：不得用当前时刻兜底）。
	bad := validEvidence("prometheus", time.Time{})
	reg.MustRegister(&fakeCollector{src: "prometheus", evs: []Evidence{
		validEvidence("prometheus", fixedTime()),
		bad,
		validEvidence("prometheus", fixedTime().Add(time.Minute)),
	}})

	in := Input{IncidentID: 1, Window: Window{Start: fixedTime(), End: fixedTime().Add(time.Hour)}}
	evs, results := CollectAll(context.Background(), reg, in)

	if len(evs) != 2 {
		t.Fatalf("保留证据 %d 条，期望 2 条（3 条里丢弃 1 条）", len(evs))
	}
	for _, e := range evs {
		if e.Timestamp.IsZero() {
			t.Error("保留了 timestamp 为空的证据 —— 违反 ADR-003")
		}
	}

	res := results[0]
	if res.Err != nil {
		t.Errorf("丢弃单条非法证据不该把整个采集器判为失败，Err = %v", res.Err)
	}
	if res.Dropped != 1 {
		t.Errorf("Dropped = %d，期望 1", res.Dropped)
	}
	if res.DropReason == nil {
		t.Error("DropReason 为 nil，丢弃原因丢了")
	}
	if AllFailed(results) {
		t.Error("采集器实际返回了数据，不该判为全失败")
	}
}

// Apply 的截断：超限截断且带上真实总数与标记。
func TestApplyTruncatesAndReportsTruth(t *testing.T) {
	a := make([]Evidence, 30)
	b := make([]Evidence, 43)
	for i := range a {
		a[i] = validEvidence("loki", fixedTime())
	}
	for i := range b {
		b[i] = validEvidence("prometheus", fixedTime())
	}

	got := Apply([][]Evidence{a, b}, DefaultMaxPerIncident)

	if len(got.Evidence) != DefaultMaxPerIncident {
		t.Errorf("保留 %d 条，期望 %d 条", len(got.Evidence), DefaultMaxPerIncident)
	}
	if !got.Truncated {
		t.Error("Truncated = false，期望 true —— 把「只保留 50 条」说成「一共 50 条」就是悄悄丢数据")
	}
	if got.TotalCollected != 73 {
		t.Errorf("TotalCollected = %d，期望 73（截断前的真实总数）", got.TotalCollected)
	}
}

// Apply 未超限：不截断、总数等于保留数。
func TestApplyWithoutOverflow(t *testing.T) {
	a := make([]Evidence, 5)
	for i := range a {
		a[i] = validEvidence("loki", fixedTime())
	}
	got := Apply([][]Evidence{a}, DefaultMaxPerIncident)
	if got.Truncated {
		t.Error("未超限却标记 Truncated = true")
	}
	if got.TotalCollected != 5 || len(got.Evidence) != 5 {
		t.Errorf("TotalCollected = %d /保留 %d 条，期望均为 5", got.TotalCollected, len(got.Evidence))
	}
}

// 上限传0 / 负数 → 退回默认 50，而不是「不限制」。
//
// 「0 表示无限」是个诱人但危险的约定：一旦有人把配置读错传了 0，
// 高频指标就能把证据表撑爆。
func TestApplyNonPositiveMaxFallsBackToDefault(t *testing.T) {
	many := make([]Evidence, 80)
	for i := range many {
		many[i] = validEvidence("prometheus", fixedTime())
	}
	for _, max := range []int{0, -1} {
		got := Apply([][]Evidence{many}, max)
		if len(got.Evidence) != DefaultMaxPerIncident {
			t.Errorf("max=%d 时保留 %d 条，期望退回默认 %d 条", max, len(got.Evidence), DefaultMaxPerIncident)
		}
		if !got.Truncated {
			t.Errorf("max=%d 时 Truncated = false，期望 true", max)
		}
	}
}

// 注册表：重复 source 报错，nil 与空 source 被拒。
func TestRegistryRejectsBadRegistrations(t *testing.T) {
	reg := NewRegistry()
	if err := reg.Register(nil); err == nil {
		t.Error("注册 nil 采集器应报错")
	}
	if err := reg.Register(&fakeCollector{src: ""}); err == nil {
		t.Error("注册空 Source 的采集器应报错 —— 证据来源不能为空")
	}
	if err := reg.Register(&fakeCollector{src: "loki"}); err != nil {
		t.Fatalf("首次注册失败: %v", err)
	}
	if err := reg.Register(&fakeCollector{src: "loki"}); err == nil {
		t.Error("重复注册同一 source 应报错 —— 否则「证据来自哪个系统」变得不确定")
	}
	if reg.Len() != 1 {
		t.Errorf("Len() = %d，期望 1", reg.Len())
	}
	if _, ok := reg.Get("loki"); !ok {
		t.Error("Get(\"loki\") 应命中")
	}
	if _, ok := reg.Get("nope"); ok {
		t.Error("Get(\"nope\") 不应命中")
	}
}

// MustRegister 在重复注册时 panic（装配期的编程错误该立刻暴露）。
func TestMustRegisterPanicsOnDuplicate(t *testing.T) {
	reg := NewRegistry()
	reg.MustRegister(&fakeCollector{src: "loki"})
	defer func() {
		if recover() == nil {
			t.Error("重复注册时 MustRegister 应 panic —— 装配期就该暴露编程错误")
		}
	}()
	reg.MustRegister(&fakeCollector{src: "loki"})
}

// 类型合法性：6 个既有 CHECK 值全部通过，未知值被拒。
func TestTypeValidity(t *testing.T) {
	for _, tp := range SortedTypes() {
		if !tp.IsValid() {
			t.Errorf("类型 %q 应合法", tp)
		}
	}
	if got := len(SortedTypes()); got != 6 {
		t.Errorf("合法类型数 = %d，期望 6（与 chk_evidence_type 的 6 值一致）", got)
	}
	if Type("bogus").IsValid() {
		t.Error("未知类型 bogus 不应合法")
	}
	if Type("").IsValid() {
		t.Error("空类型不应合法")
	}
	// 排序必须稳定，否则错误信息与测试输入表会抖动。
	prev := Type("")
	for _, tp := range SortedTypes() {
		if tp <= prev {
			t.Errorf("SortedTypes 未按字典序：%q 出现在 %q 之后", tp, prev)
		}
		prev = tp
	}
}

// 证据校验：逐条覆盖会撞 CHECK 约束或违反 ADR-003 的情形。
func TestEvidenceValidate(t *testing.T) {
	tooHigh := 1.5
	tooLow := -0.1
	cases := []struct {
		name string
		ev   Evidence
		want string // 期望的错误片段，空表示期望通过
	}{
		{"合法", validEvidence("loki", fixedTime()), ""},
		{"类型非法", Evidence{Type: "bogus", Source: "loki", Timestamp: fixedTime()}, "类型"},
		{"来源为空", Evidence{Type: TypeLog, Source: "", Timestamp: fixedTime()}, "来源"},
		{"时间为空", Evidence{Type: TypeLog, Source: "loki"}, "不得用当前时刻兜底"},
		{"置信度超上界", Evidence{Type: TypeLog, Source: "loki", Timestamp: fixedTime(), Confidence: &tooHigh}, "置信度"},
		{"置信度超下界", Evidence{Type: TypeLog, Source: "loki", Timestamp: fixedTime(), Confidence: &tooLow}, "置信度"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.ev.Validate()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("期望通过，实际报错: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("期望报错（含 %q），实际通过", tc.want)
			}
			if !contains(err.Error(), tc.want) {
				t.Errorf("错误信息 %q 不含期望片段 %q", err.Error(), tc.want)
			}
		})
	}
}

// 置信度 0 与 1 是合法边界（不能把 0 当成「未设置」）。
func TestConfidenceBoundsAreInclusive(t *testing.T) {
	for _, v := range []float64{0, 1} {
		ev := Evidence{Type: TypeLog, Source: "loki", Timestamp: fixedTime(), Confidence: &v}
		if err := ev.Validate(); err != nil {
			t.Errorf("置信度 %v 应合法: %v", v, err)
		}
	}
}

// 窗口校验：未设置、倒置、零长都必须被拒。
func TestWindowValidate(t *testing.T) {
	start := fixedTime()
	end := start.Add(time.Hour)

	if err := (Window{Start: start, End: end}).Validate(); err != nil {
		t.Errorf("正常窗口应通过: %v", err)
	}
	if err := (Window{}).Validate(); err == nil {
		t.Error("零值窗口应报错")
	}
	if err := (Window{Start: start}).Validate(); err == nil {
		t.Error("缺结束时间的窗口应报错")
	}
	if err := (Window{Start: end, End: start}).Validate(); err == nil {
		t.Error("结束早于开始的窗口应报错 —— 否则时间对比会算出负区间")
	}
	if err := (Window{Start: start, End: start}).Validate(); err == nil {
		t.Error("零长窗口应报错 —— 无法做窗口前后对比")
	}
	if (Window{Start: start, End: end}).Duration() != time.Hour {
		t.Error("Duration 应为 1h")
	}
	if !(Window{}).IsZero() {
		t.Error("零值窗口 IsZero 应为 true")
	}
}

// 输入传给采集器时必须原样送达（采集器不自己算窗口）。
func TestInputReachesCollectorUnchanged(t *testing.T) {
	c := &fakeCollector{src: "loki", evs: []Evidence{validEvidence("loki", fixedTime())}}
	reg := NewRegistry()
	reg.MustRegister(c)

	in := Input{
		IncidentID: 42,
		Service:    "api",
		Window:     Window{Start: fixedTime(), End: fixedTime().Add(30 * time.Minute)},
	}
	_, _ = CollectAll(context.Background(), reg, in)

	if c.in.IncidentID != 42 {
		t.Errorf("IncidentID = %d，期望 42", c.in.IncidentID)
	}
	if c.in.Service != "api" {
		t.Errorf("Service = %q，期望 \"api\"", c.in.Service)
	}
	if c.in.Window != in.Window {
		t.Errorf("Window = %+v，期望 %+v", c.in.Window, in.Window)
	}
}

// contains 是极简子串匹配，避免为此引入 strings 而在断言里掩盖意图。
func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
