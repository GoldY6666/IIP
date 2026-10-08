package worker

// F3（S1-b）置信度落地的 worker 层用例。
//
// 单独成文件而不是并进 processor_test.go：这一组用例验的是
// 「编排层怎么把 rca.Assign 接进采集流程」，与 M4 那批
// 「采集流程本身对不对」的用例是两个关注点。混在一起会让
// 后人为了找一条用例得读完 700 行。

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yu/iip/internal/evidence"
	"github.com/yu/iip/internal/incident"
)

// ratioEv 造一条带 change_ratio 的证据。
//
// ratio 传 "1.5000" 之类**字符串**而不是 float64：判据关心的是
// Metadata 里那个键的字面值，而 rca.Assess 也只读那个键。
// 用 float64 造证据会让人误以为判据在验数值精度。
func ratioEv(t evidence.Type, src, ratio string, ts time.Time) evidence.Evidence {
	return evidence.Evidence{
		Type:      t,
		Source:    src,
		Query:     "q",
		Value:     "v",
		Timestamp: ts,
		Metadata:  map[string]string{"change_ratio": ratio},
	}
}

// noveltyEv 造一条「基线为 0、改写 novelty」的证据。
//
// 它是**不可评估**的合法形态：采集器给了足够的信息说明
// 「这是新出现的」，但没给倍数。这类证据必须留空 confidence，
// 而「留空」不能被读成「采集器忘了填」。
func noveltyEv(t evidence.Type, src string, ts time.Time) evidence.Evidence {
	return evidence.Evidence{
		Type:      t,
		Source:    src,
		Query:     "q",
		Value:     "v",
		Timestamp: ts,
		Metadata:  map[string]string{"novelty": "事故前窗口无同类事件，本次为新出现"},
	}
}

// confOf 取第 i 条落库证据的 confidence 指针。
func confOf(t *testing.T, evs []evidence.Evidence, i int) *float64 {
	t.Helper()
	if i >= len(evs) {
		t.Fatalf("落库证据只有 %d 条，取不到第 %d 条", len(evs), i)
	}
	return evs[i].Confidence
}

// byType 按证据类型取落库结果里**唯一**的那一条。
//
// **刻意不按落库索引取**：evidence.Registry.Collectors() 按来源字典序
// 遍历（k8s < loki < prometheus），所以「第 0 条」是 k8s 而不是注册顺序。
// 用索引写判据等于把「顺序」当契约 —— 日后有人把来源改个名、
// 或加一个排在字母序前面的采集器，判据就会指向另一条证据，
// 而且是**静默地**指向错的（不是报错，是断言了另一件事）。
//
// 这与前端那条「序号选择器必须另配一条『数量与顺序固定』的用例」
// 是同一个坑的两面：那条要求顺序固定时要有守卫，
// 这条是干脆别依赖顺序。
func byType(t *testing.T, evs []evidence.Evidence, typ evidence.Type) evidence.Evidence {
	t.Helper()
	var found []evidence.Evidence
	for _, e := range evs {
		if e.Type == typ {
			found = append(found, e)
		}
	}
	if len(found) != 1 {
		t.Fatalf("类型 %s 的证据 = %d 条，期望恰好 1 条", typ, len(found))
	}
	return found[0]
}

// runOnce 跑一轮并返回落库的证据。
func runOnce(t *testing.T, reg *evidence.Registry, targets *fakeTargets) []evidence.Evidence {
	t.Helper()
	store := newFakeStore()
	store.enqueue(collectTask(t, 1, time.Time{}))
	evStore := &fakeEvidenceStore{}
	p := newTestProcessor(store, evStore, targets, reg, &fakeTimeline{}, Config{})
	if _, err := p.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce 返回错误: %v", err)
	}
	evStore.mu.Lock()
	defer evStore.mu.Unlock()
	return append([]evidence.Evidence(nil), evStore.saved...)
}

// TestF3_AssignFillsConfidenceForAssessableEvidence 是F3 的主判据：
// 可评估的证据必须拿到 confidence，不可评估的必须留空。
//
// 两条必须**同时**断言。把「不可评估的仍留空」省掉的话，
// 一个「全部填 0.45」的实现照样全绿 —— 那是 rca 变异测试
// 的 M1 靶点曾经漏掉的退化。
func TestF3_AssignFillsConfidenceForAssessableEvidence(t *testing.T) {
	reg := evidence.NewRegistry()
	reg.MustRegister(&fakeCollector{src: "prometheus", evs: []evidence.Evidence{
		ratioEv(evidence.TypeMetric, "prometheus", "6.0000", testNow.Add(-time.Minute)),
		noveltyEv(evidence.TypeLog, "prometheus", testNow.Add(-2*time.Minute)),
	}})

	got := runOnce(t, reg, &fakeTargets{target: Target{Service: "api", StartedAt: testNow}})
	if len(got) != 2 {
		t.Fatalf("落库证据 = %d 条，期望 2", len(got))
	}

	metric := byType(t, got, evidence.TypeMetric)
	logEv := byType(t, got, evidence.TypeLog)
	if metric.Confidence == nil {
		t.Error("有 change_ratio 的 metric 证据 confidence 为 nil —— " +
			"processor 没调 rca.Assign，或调了但被跳过")
	}
	if logEv.Confidence != nil {
		t.Errorf("novelty 证据（无基线）confidence = %v，应为 nil —— "+
			"「没有可比数据」被算成了「评估结果」", *logEv.Confidence)
	}
}

// TestF3_ConfidenceVariesWithStrength 是**防退化实现**的核心判据。
//
// 只断言「confidence 非 nil」的话，一个把它恒填 0.45 的实现
// 会全绿 —— 而那正是最危险的退化：页面看起来一切正常，
// 实际上没有任何一条证据的强度参与过计算。
//
// 因此必须用**不同强度、不同覆盖度**的输入，断言落库值真的不同，
// 且不同的方向与 rca 的档位表一致（见 confidence.go 的 Compose）。
func TestF3_ConfidenceVariesWithStrength(t *testing.T) {
	// 两类命中（覆盖度 2/6 → base 0.60），但强度一个 weak 一个 strong：
	// weak 被 WeakCap 压到 0.50，strong 拿 base+0.05 = 0.65。
	reg := evidence.NewRegistry()
	reg.MustRegister(&fakeCollector{src: "prometheus", evs: []evidence.Evidence{
		ratioEv(evidence.TypeMetric, "prometheus", "1.5000", testNow.Add(-time.Minute)),
	}})
	reg.MustRegister(&fakeCollector{src: "loki", evs: []evidence.Evidence{
		ratioEv(evidence.TypeLog, "loki", "9.0000", testNow.Add(-2*time.Minute)),
	}})

	got := runOnce(t, reg, &fakeTargets{target: Target{Service: "api", StartedAt: testNow}})
	if len(got) != 2 {
		t.Fatalf("落库证据 = %d 条，期望 2", len(got))
	}

	weak := byType(t, got, evidence.TypeMetric).Confidence
	strong := byType(t, got, evidence.TypeLog).Confidence
	if weak == nil || strong == nil {
		t.Fatalf("confidence 有 nil：weak=%v strong=%v", weak, strong)
	}
	if *weak == *strong {
		t.Fatalf("weak（1.5 倍）与 strong（9 倍）的 confidence 都是 %.4f —— "+
			"「值必须随强度变化」这条判据失效，恒定填数的退化实现会全绿",
			*weak)
	}
	if *weak >= *strong {
		t.Errorf("weak=%.4f 应当< strong=%.4f（档位方向反了）", *weak, *strong)
	}

	// 值必须落在 rca 的合法取值集合内，不能是任意浮点。
	// 这一条独立于上面两条：一个把值算成 0.87 的实现
	// 仍能通过「weak < strong」，但 0.87 不在任何档位表里。
	for _, c := range []struct {
		name string
		v    float64
	}{{"weak", *weak}, {"strong", *strong}} {
		if !legalConfidence(c.v) {
			t.Errorf("%s 的 confidence = %.4f，不在 rca 档位表的合法取值内", c.name, c.v)
		}
	}
}

// legalConfidence 报告 v 是否是 rca 档位表能产出的合法取值。
//
// 取值集合 = {1→0.45, 2→0.60, 3→0.75, 4→0.90}，叠加 WeakCap=0.50
// 与 StrongBonus=+0.05（封顶 0.95）后可能出现的全部值。
// 写成函数而不是内联在用例里，是为了让它能跟着 rca 的常量一起改；
// 若 rca 改了档位表而这里没改，本用例会立刻变红 —— 那正是我们要的。
func legalConfidence(v float64) bool {
	legal := []float64{
		0.45, 0.50, 0.55, 0.60, 0.65, 0.70, 0.75, 0.80, 0.85, 0.90, 0.95,
	}
	for _, x := range legal {
		if v == x {
			return true
		}
	}
	return false
}

// TestF3_CoverageCountsAcrossCollectors 固定住两级架构的核心语义：
// 覆盖度是**跨采集器**算的。
//
// 单个采集器只知道自己这一类，算不出覆盖度 —— 这正是
// Assign 必须由编排层调一次的原因。若哪天有人为了「省一次调用」
// 把 Assign 挪进某个采集器，这条用例会红：那一类证据会拿到
// hit=1 的 0.45，而不是两类的 0.60/0.65。
func TestF3_CoverageCountsAcrossCollectors(t *testing.T) {
	one := evidence.NewRegistry()
	one.MustRegister(&fakeCollector{src: "prometheus", evs: []evidence.Evidence{
		ratioEv(evidence.TypeMetric, "prometheus", "9.0000", testNow.Add(-time.Minute)),
	}})
	oneOnly := runOnce(t, one, &fakeTargets{target: Target{Service: "api", StartedAt: testNow}})

	two := evidence.NewRegistry()
	two.MustRegister(&fakeCollector{src: "prometheus", evs: []evidence.Evidence{
		ratioEv(evidence.TypeMetric, "prometheus", "9.0000", testNow.Add(-time.Minute)),
	}})
	two.MustRegister(&fakeCollector{src: "loki", evs: []evidence.Evidence{
		ratioEv(evidence.TypeLog, "loki", "9.0000", testNow.Add(-2*time.Minute)),
	}})
	twoTypes := runOnce(t, two, &fakeTargets{target: Target{Service: "api", StartedAt: testNow}})

	c1 := byType(t, oneOnly, evidence.TypeMetric).Confidence
	c2 := byType(t, twoTypes, evidence.TypeMetric).Confidence
	if c1 == nil || c2 == nil {
		t.Fatalf("confidence 有 nil：单类=%v 双类=%v", c1, c2)
	}
	if *c1 >= *c2 {
		t.Errorf("命中 1 类=%.4f，命中 2 类=%.4f —— 后者必须更高。"+
			"覆盖度没跨采集器统计（Assign 被挪进单个采集器了？）",
			*c1, *c2)
	}
}

// TestF3_ConfidenceCountsOnlyAssessableTypes 固定住「只数可评估的类型」。
//
// 三类证据里两类不可评估时，覆盖度必须是 1 而不是 3：
// 否则「采到 3 条但全无基线」会被算成覆盖度 3/6，
// 页面显示的置信度会比实际可复核的程度高得多。
func TestF3_ConfidenceCountsOnlyAssessableTypes(t *testing.T) {
	reg := evidence.NewRegistry()
	reg.MustRegister(&fakeCollector{src: "k8s", evs: []evidence.Evidence{
		ratioEv(evidence.TypeMetric, "k8s", "9.0000", testNow.Add(-time.Minute)),
		noveltyEv(evidence.TypeDeployment, "k8s", testNow.Add(-2*time.Minute)),
		noveltyEv(evidence.TypeConfigChange, "k8s", testNow.Add(-3*time.Minute)),
	}})

	got := runOnce(t, reg, &fakeTargets{target: Target{Service: "api", StartedAt: testNow}})
	if len(got) != 3 {
		t.Fatalf("落库证据 = %d 条，期望 3", len(got))
	}
	c := byType(t, got, evidence.TypeMetric).Confidence
	if c == nil {
		t.Fatal("可评估的 metric 证据 confidence 为 nil")
	}
	// 覆盖度 1 → base 0.45，strong 档 +0.05 = 0.50。
	// 若错误地把 novelty 也数进覆盖度，会变成 3 类 → 0.75+0.05=0.80。
	if *c != 0.50 {
		t.Errorf("confidence = %.4f，期望 0.50（覆盖度只数 1 类可评估）—— "+
			"把不可评估的类型也数进覆盖度了", *c)
	}
}

// TestF3_ConfidenceSurvivesTruncation 固定住「Assign 在截断之后调」。
//
// 顺序反过来会有两个可观察的错误：
//  1. 覆盖度把已被砍掉的类型也数进去 —— 页面显示 N 条证据，
//     覆盖度却按更多类型算，两处对不上；
//  2. 那些随后被截断掉的证据白算了一遍 confidence。
//
// 造数：三个来源各 1 条，MaxPerIncident=2 ⇒ 只有一个被砍。
// Registry.Collectors() 按来源字典序遍历，所以被砍的是
// **prometheus**（k8s < loki < prometheus），活下来 k8s + loki。
// 判据不写死「谁被砍」而是反过来断言：**落库里必须没有 prometheus**，
// 且活下来的两条覆盖度是 2 类而非 3 类。
// 若 Assign 在截断前调，它们会按 3 类算出 0.80（正确是 0.65）。
func TestF3_ConfidenceSurvivesTruncation(t *testing.T) {
	reg := evidence.NewRegistry()
	reg.MustRegister(&fakeCollector{src: "prometheus", evs: []evidence.Evidence{
		ratioEv(evidence.TypeMetric, "prometheus", "9.0000", testNow.Add(-time.Minute)),
	}})
	reg.MustRegister(&fakeCollector{src: "loki", evs: []evidence.Evidence{
		ratioEv(evidence.TypeLog, "loki", "9.0000", testNow.Add(-2*time.Minute)),
	}})
	reg.MustRegister(&fakeCollector{src: "k8s", evs: []evidence.Evidence{
		ratioEv(evidence.TypeDeployment, "k8s", "9.0000", testNow.Add(-3*time.Minute)),
	}})

	store := newFakeStore()
	store.enqueue(collectTask(t, 1, time.Time{}))
	evStore := &fakeEvidenceStore{}
	p := newTestProcessor(store, evStore,
		&fakeTargets{target: Target{Service: "api", StartedAt: testNow}},
		reg, &fakeTimeline{}, Config{MaxPerIncident: 2})
	if _, err := p.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce 返回错误: %v", err)
	}

	evStore.mu.Lock()
	saved := append([]evidence.Evidence(nil), evStore.saved...)
	evStore.mu.Unlock()

	if len(saved) != 2 {
		t.Fatalf("落库证据 = %d 条，期望 2（MaxPerIncident=2）", len(saved))
	}
	// 被截断掉的那类绝不能出现在落库结果里，更不能有 confidence。
	for _, e := range saved {
		if e.Type == evidence.TypeMetric {
			t.Error("prometheus 的 metric 证据本该被截断（字典序最后），却落库了")
		}
	}
	// 活下来的两条，覆盖度按 2 类算（不是 3 类）。
	for _, e := range saved {
		if e.Confidence == nil {
			t.Errorf("类型 %s 的落库证据 confidence 为 nil", e.Type)
			continue
		}
		if *e.Confidence != 0.65 {
			t.Errorf("类型 %s 的 confidence = %.4f，期望 0.65"+
				"（覆盖度按**截断后**的 2 类算）—— Assign 被挪到 Apply 之前了",
				e.Type, *e.Confidence)
		}
	}
}

// TestF3_AllUnassessableLeavesEverythingNil 是「零命中」边界。
//
// 一条可评估的都没有时，全部留空 —— 不能写 0，更不能写 0.45。
// 这条判据守着 rca.Compose 的 `(0, false)` 契约在编排层没被绕过。
func TestF3_AllUnassessableLeavesEverythingNil(t *testing.T) {
	reg := evidence.NewRegistry()
	reg.MustRegister(&fakeCollector{src: "loki", evs: []evidence.Evidence{
		noveltyEv(evidence.TypeLog, "loki", testNow.Add(-time.Minute)),
		noveltyEv(evidence.TypeLog, "loki", testNow.Add(-2*time.Minute)),
	}})

	got := runOnce(t, reg, &fakeTargets{target: Target{Service: "api", StartedAt: testNow}})
	if len(got) != 2 {
		t.Fatalf("落库证据 = %d 条，期望 2", len(got))
	}
	for i, e := range got {
		if e.Confidence != nil {
			t.Errorf("第 %d 条 confidence = %v，全部不可评估时应全部为 nil", i, *e.Confidence)
		}
	}
}

// TestF3_NoEvidenceDoesNotPanic 验零证据路径。
//
// 「采到 0 条」是完全正常的结论（这段时间没异常）。
// 接入Assign 之后，这条路径上会多跑两次遍历
// （一次 Assign、一次 SuggestFrom），空切片遍历不该出问题。
// 空切片上写`evs[0]` 会 panic，而 panic 会被 poller 的recover 吞成一行日志 ——
// 表现是「任务静默不进队列」，极难定位。
func TestF3_NoEvidenceDoesNotPanic(t *testing.T) {
	reg := evidence.NewRegistry()
	reg.MustRegister(&fakeCollector{src: "loki", evs: []evidence.Evidence{}})

	got := runOnce(t, reg, &fakeTargets{target: Target{Service: "api", StartedAt: testNow}})
	if len(got) != 0 {
		t.Fatalf("落库证据 = %d 条，期望 0", len(got))
	}
}

// TestF3_CrossRoundDoesNotRecompute 固定住「跨轮不重算」这个已知取舍。
//
// 背景：Assign 只算「本轮采到的那批」，且有幂等保护
// （不覆盖已有非 nil）。所以第一轮只采到 metric 时它拿 0.50，
// 第二轮才采到 log，那批 metric **仍是 0.50**，不会变成 0.65。
//
// 这个行为已被裁定为「接受并登记为OPEN-M6-4」，不是缺陷。
// 但它是**有意选择的产物**，所以必须有判据把它钉住——
// 否则日后有人「顺手优化」成按事件累计重算时，
// 没人会意识到自己改了一个已裁定的语义。
// 判据的措辞必须是「仍是 0.50」而不是「应该变成 0.65」。
func TestF3_CrossRoundDoesNotRecompute(t *testing.T) {
	store := newFakeStore()
	evStore := &fakeEvidenceStore{}
	targets := &fakeTargets{target: Target{Service: "api", StartedAt: testNow}}

	// 第一轮：只有 metric。
	reg1 := evidence.NewRegistry()
	reg1.MustRegister(&fakeCollector{src: "prometheus", evs: []evidence.Evidence{
		ratioEv(evidence.TypeMetric, "prometheus", "9.0000", testNow.Add(-time.Minute)),
	}})
	store.enqueue(collectTask(t, 1, time.Time{}))
	p1 := newTestProcessor(store, evStore, targets, reg1, &fakeTimeline{}, Config{})
	if _, err := p1.RunOnce(context.Background()); err != nil {
		t.Fatalf("第一轮 RunOnce 返回错误: %v", err)
	}

	// 第二轮：来了 log。metric 那条已在库里，不会被重算。
	reg2 := evidence.NewRegistry()
	reg2.MustRegister(&fakeCollector{src: "loki", evs: []evidence.Evidence{
		ratioEv(evidence.TypeLog, "loki", "9.0000", testNow.Add(-2*time.Minute)),
	}})
	store.enqueue(collectTask(t, 1, time.Time{}))
	p2 := newTestProcessor(store, evStore, targets, reg2, &fakeTimeline{}, Config{})
	if _, err := p2.RunOnce(context.Background()); err != nil {
		t.Fatalf("第二轮 RunOnce 返回错误: %v", err)
	}

	evStore.mu.Lock()
	saved := append([]evidence.Evidence(nil), evStore.saved...)
	evStore.mu.Unlock()

	var metricConf, logConf *float64
	for _, e := range saved {
		switch e.Type {
		case evidence.TypeMetric:
			metricConf = e.Confidence
		case evidence.TypeLog:
			logConf = e.Confidence
		}
	}
	if metricConf == nil || logConf == nil {
		t.Fatalf("两类证据都该有 confidence：metric=%v log=%v", metricConf, logConf)
	}
	// 第一轮 metric 是 1 类覆盖度 → 0.45+0.05 = 0.50。
	// 它**不因第二轮多了 log 类而变 0.65**。
	if *metricConf != 0.50 {
		t.Errorf("第一轮的 metric confidence = %.4f，期望 0.50 —— "+
			"「跨轮按事件累计重算」已被裁定不做（OPEN-M6-4），"+
			"若这里变了说明有人重算了已落库证据", *metricConf)
	}
	// 第二轮 log 是它自己那批的 1 类覆盖度 → 同样 0.50。
	if *logConf != 0.50 {
		t.Errorf("第二轮的 log confidence = %.4f，期望 0.50（它自己那批只命中 1 类）", *logConf)
	}
}

// TestF3_ConfidenceBasisWrittenToMetadata 验人工可复核性。
//
// 判据的价值全在「这个 0.65 从哪来能被回答」。Assign 会把
// basis 与覆盖度写进 metadata；若这一步没做，页面上就是一个
// 没人能解释的数字 —— 那与 OPEN-M4-1 时期随手填 1.0
// 属于同一类问题，只是换了个小数。
func TestF3_ConfidenceBasisWrittenToMetadata(t *testing.T) {
	reg := evidence.NewRegistry()
	reg.MustRegister(&fakeCollector{src: "prometheus", evs: []evidence.Evidence{
		ratioEv(evidence.TypeMetric, "prometheus", "9.0000", testNow.Add(-time.Minute)),
	}})

	got := runOnce(t, reg, &fakeTargets{target: Target{Service: "api", StartedAt: testNow}})
	if len(got) != 1 {
		t.Fatalf("落库证据 = %d 条，期望 1", len(got))
	}
	md := got[0].Metadata

	basis := md["confidence_basis"]
	if basis == "" {
		t.Fatal("metadata 缺 confidence_basis —— 置信度没有复核依据")
	}
	// basis 必须提到真实倍数，否则它与 confidence 无关（各说各的）。
	if !strings.Contains(basis, "9.00") {
		t.Errorf("confidence_basis = %q，未提到实际倍数 9.00 —— "+
			"复核的人无法确认它与 confidence 同源", basis)
	}

	cov := md["confidence_coverage"]
	if cov != "1/6" {
		t.Errorf("confidence_coverage = %q，期望 \"1/6\"（命中 1 类 / 全集 6 类）", cov)
	}
}

// TestF3_StatsLogMentionsCoverage 验日志里能看出本轮赋了多少值。
//
// 不是「日志好看」而是排障必需：confidence 全空时，
// 运维需要立刻分辨是「采集器没给 change_ratio」（判据第②步）
// 还是「Assign 没被调用」—— 前者要查采集器，后者要查编排层。
// 只看 confidence 列两者都是「空」，分不开。
func TestF3_StatsLogMentionsCoverage(t *testing.T) {
	var buf strings.Builder
	reg := evidence.NewRegistry()
	reg.MustRegister(&fakeCollector{src: "prometheus", evs: []evidence.Evidence{
		ratioEv(evidence.TypeMetric, "prometheus", "9.0000", testNow.Add(-time.Minute)),
		noveltyEv(evidence.TypeLog, "prometheus", testNow.Add(-2*time.Minute)),
	}})

	store := newFakeStore()
	store.enqueue(collectTask(t, 1, time.Time{}))
	logger := newCaptureLogger(&buf)
	p := NewProcessor(store, &fakeEvidenceStore{},
		&fakeTargets{target: Target{Service: "api", StartedAt: testNow}},
		reg, &fakeTimeline{}, Config{}, logger)
	p.SetClock(func() time.Time { return testNow })

	if _, err := p.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce 返回错误: %v", err)
	}

	out := buf.String()
	for _, want := range []string{"assigned=1", "unassessable=1", "coverage=1/6"} {
		if !strings.Contains(out, want) {
			t.Errorf("日志缺 %q —— 排障时无法区分「采集器没给倍数」与「Assign 没调用」\n日志：\n%s",
				want, out)
		}
	}
}

// TestF3_HypothesesLoggedNotWritten 验根因假设只进日志。
//
// 手册 §2.2 把「自动写 root_causes」列为明确不做。
// 本用例从**可观察面**守这条约束：落库的证据里不该多出
// 任何根因字段，而日志里必须出现假设文本。
//
// 反向操作也要做：若哪天有人把假设写进了 timeline 或
// incidents，本用例不会红（它只看证据与日志）——
// 这一点已在 docs/S1-置信度判据设计.md §九 如实标注为
// 「靠代码走查与人工评审保证，非机械判据」。
func TestF3_HypothesesLoggedNotWritten(t *testing.T) {
	var buf strings.Builder
	// metric + log 两类且都有 change_ratio ⇒ 「指标突增伴随错误日志增长」模板可触发。
	reg := evidence.NewRegistry()
	reg.MustRegister(&fakeCollector{src: "prometheus", evs: []evidence.Evidence{
		ratioEv(evidence.TypeMetric, "prometheus", "9.0000", testNow.Add(-time.Minute)),
	}})
	reg.MustRegister(&fakeCollector{src: "loki", evs: []evidence.Evidence{
		ratioEv(evidence.TypeLog, "loki", "9.0000", testNow.Add(-2*time.Minute)),
	}})

	store := newFakeStore()
	store.enqueue(collectTask(t, 1, time.Time{}))
	evStore := &fakeEvidenceStore{}
	tl := &fakeTimeline{}
	p := NewProcessor(store, evStore,
		&fakeTargets{target: Target{Service: "api", StartedAt: testNow}},
		reg, tl, Config{}, newCaptureLogger(&buf))
	p.SetClock(func() time.Time { return testNow })

	if _, err := p.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce 返回错误: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "根因假设") {
		t.Errorf("日志里没有根因假设 —— SuggestFrom 没被调用，"+
			"或被调用的位置在 Assign 之前（那样它恒返回空）\n日志：\n%s", out)
	}
	if !strings.Contains(out, "不写库") {
		t.Errorf("日志未标明「仅建议不写库」—— 后人会误以为根因已入库\n日志：\n%s", out)
	}
	// 支撑类型列表按**字典序**拼（rca.joinTypes 走 sort.Slice），
	// 所以是 log/metric 而不是 metric/log —— 判据不能按注册顺序写期望。
	if !strings.Contains(out, "log/metric") {
		t.Errorf("假设文本里没有支撑类型列表 —— 每条假设必须能回答"+
			"「凭什么这么说」\n日志：\n%s", out)
	}

	// 留痕里只应有「采集完成」一条，不能混进根因内容。
	tl.mu.Lock()
	entries := append([]incident.Entry(nil), tl.entries...)
	tl.mu.Unlock()
	if len(entries) != 1 {
		t.Fatalf("时间线留痕 = %d 条，期望 1（只有「采集完成」）", len(entries))
	}
	if strings.Contains(entries[0].Description, "根因") {
		t.Errorf("时间线里混进了根因内容：%q", entries[0].Description)
	}
}

// TestF3_HypothesesSkipLoggedWhenAllUnassessable 验「不可推断」也有日志。
//
// 全不可评估时 SuggestFrom 按设计返回空列表。**不能静默** ——
// 「没有假设」与「假设功能没接」在日志上长得一样，
// 而两者的排查方向完全相反。
func TestF3_HypothesesSkipLoggedWhenAllUnassessable(t *testing.T) {
	var buf strings.Builder
	reg := evidence.NewRegistry()
	reg.MustRegister(&fakeCollector{src: "loki", evs: []evidence.Evidence{
		noveltyEv(evidence.TypeLog, "loki", testNow.Add(-time.Minute)),
	}})

	store := newFakeStore()
	store.enqueue(collectTask(t, 1, time.Time{}))
	p := NewProcessor(store, &fakeEvidenceStore{},
		&fakeTargets{target: Target{Service: "api", StartedAt: testNow}},
		reg, &fakeTimeline{}, Config{}, newCaptureLogger(&buf))
	p.SetClock(func() time.Time { return testNow })

	if _, err := p.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce 返回错误: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "暂不可推断") {
		t.Errorf("全部不可评估时应有「暂不可推断」日志，"+
			"否则「没有假设」与「假设功能没接」无法区分\n日志：\n%s", out)
	}
	// 不能出现具体的假设条目 —— 一条都没有可评估证据时
	// 若还打出「部署变更与指标突变」这种句子，那是拿缺数据当证据。
	if strings.Contains(out, "根因假设  ") {
		t.Errorf("全不可评估时不该打出任何具体假设\n日志：\n%s", out)
	}
}

// TestF3_CoverageMetadataMatchesConfidenceOrder 交叉验覆盖度键的形状。
//
// 「confidence_coverage」是给人复核用的，它与实际覆盖度不符时
// 比没有更坏：人会照着它去核对，然后发现对不上。
// 这里从「几类可评估」与「metadata 写的分母」两侧同时断言。
//
// 顺带验一条**批内恒等**：同一批里所有可评估证据的 confidence
// 必然相同，因为它们共享同一个 CoverageHit。写成「批内单调」是
// 伪命题（相同即满足 ≤），这里写「批内全等」才是真的那条不变量 ——
// 而它恰好能抓住「有人给每条证据各算一次覆盖度」的错误实现。
func TestF3_CoverageMetadataMatchesConfidenceOrder(t *testing.T) {
	cases := []struct {
		name    string
		assess  []evidence.Type // 每类一条，都带 change_ratio
		wantHit int
	}{
		{"只命中 1 类", []evidence.Type{evidence.TypeMetric}, 1},
		{"命中 2 类", []evidence.Type{evidence.TypeMetric, evidence.TypeLog}, 2},
		{"命中 3 类", []evidence.Type{
			evidence.TypeMetric, evidence.TypeLog, evidence.TypeDeployment}, 3},
	}
	// strong 档（9.0 倍）下各覆盖度对应的期望值。
	wantConf := map[int]float64{1: 0.50, 2: 0.65, 3: 0.80}
	srcs := map[evidence.Type]string{
		evidence.TypeMetric:     "prometheus",
		evidence.TypeLog:        "loki",
		evidence.TypeDeployment: "k8s",
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg := evidence.NewRegistry()
			for _, typ := range tc.assess {
				reg.MustRegister(&fakeCollector{src: srcs[typ], evs: []evidence.Evidence{
					ratioEv(typ, srcs[typ], "9.0000", testNow.Add(-time.Minute)),
				}})
			}
			got := runOnce(t, reg, &fakeTargets{
				target: Target{Service: "api", StartedAt: testNow}})
			if len(got) != tc.wantHit {
				t.Fatalf("落库证据 = %d 条，期望 %d", len(got), tc.wantHit)
			}
			wantCov := formatCoverageKey(tc.wantHit)
			for _, e := range got {
				if e.Metadata["confidence_coverage"] != wantCov {
					t.Errorf("类型 %s 的 confidence_coverage = %q，期望 %q",
						e.Type, e.Metadata["confidence_coverage"], wantCov)
				}
				if e.Confidence == nil {
					t.Fatalf("类型 %s 的 confidence 为 nil", e.Type)
				}
				// 批内恒等：全部等于期望值。
				if *e.Confidence != wantConf[tc.wantHit] {
					t.Errorf("类型 %s 的 confidence = %.4f，期望 %.4f"+
						"（覆盖度 %d 类）", e.Type, *e.Confidence,
						wantConf[tc.wantHit], tc.wantHit)
				}
			}
		})
	}
}

// TestF3_HigherCoverageYieldsHigherConfidence 验「覆盖度越高越可信」
// 这条最基本的规律在 worker 侧真的成立。
//
// 为什么要独立成一条而不是靠上个子用例的顺序隐含：
// 「覆盖度单调」是**跨批次**的规律（单批内覆盖度只有一个值），
// 把它塞进子用例表里就成了「相信这三个期望值恰好递增」——
// 那是把判据换成了另一条判据。这条用例显式跑三档并逐档比对。
//
// 全部用 strong 档（9.0 倍），让band 这一维保持不变，
// 于是 confidence 的差异**只可能**来自覆盖度。
func TestF3_HigherCoverageYieldsHigherConfidence(t *testing.T) {
	srcs := map[evidence.Type]string{
		evidence.TypeMetric:     "prometheus",
		evidence.TypeLog:        "loki",
		evidence.TypeDeployment: "k8s",
	}
	order := []evidence.Type{
		evidence.TypeMetric, evidence.TypeLog, evidence.TypeDeployment,
	}

	prev := 0.0
	for hit := 1; hit <= len(order); hit++ {
		reg := evidence.NewRegistry()
		for _, typ := range order[:hit] {
			reg.MustRegister(&fakeCollector{src: srcs[typ], evs: []evidence.Evidence{
				ratioEv(typ, srcs[typ], "9.0000", testNow.Add(-time.Minute)),
			}})
		}
		got := runOnce(t, reg, &fakeTargets{
			target: Target{Service: "api", StartedAt: testNow}})
		if len(got) != hit {
			t.Fatalf("覆盖度 %d 时落库 %d 条，期望 %d", hit, len(got), hit)
		}
		c := got[0].Confidence
		if c == nil {
			t.Fatalf("覆盖度 %d 时 confidence 为 nil", hit)
		}
		if *c <= prev {
			t.Errorf("覆盖度 %d 的 confidence = %.4f，未高于上一档 %.4f —— "+
				"「证据类型越多越可信」这条规律不成立", hit, *c, prev)
		}
		prev = *c
	}
}

// formatCoverageKey 造 "N/6" 形式的覆盖度键。
//
// 分母写 6 而不是读 rca.CoverageTotal：rca 侧已经有一条
// 漂移守卫（TestCoverageTotal_MatchesEvidenceTypes）断言它等于 6，
// 本用例要验的是**worker 侧有没有正确使用这个键**，
// 两者关注的不是同一件事。
func formatCoverageKey(hit int) string {
	return strconv.Itoa(hit) + "/6"
}

// newCaptureLogger 造一个把日志写进 buf 的 slog.Logger。
//
// 不能用 discardLogger：F3 的判据里有三条直接读日志内容
// （assigned/unassessable/coverage 统计、假设文本、「不写库」标注），
// 判据看不见日志就只能靠人读，那不叫判据。
func newCaptureLogger(buf *strings.Builder) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}
