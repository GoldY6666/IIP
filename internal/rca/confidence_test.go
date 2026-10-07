package rca

import (
	"math"
	"strconv"
	"testing"

	"github.com/yu/iip/internal/evidence"
)

// ev 造一条带指定 Metadata 的证据，省去每个用例重复 Type/Source/Timestamp。
func ev(t evidence.Type, md map[string]string) evidence.Evidence {
	return evidence.Evidence{
		Type:      t,
		Source:    "test",
		Query:     "q",
		Value:     "v",
		Timestamp: fixedTS,
		Metadata:  md,
	}
}

// --- Assess：三态是本判据的核心，逐个钉死 -------------------------------

func TestAssess_BaselinePresentMeansUnassessable(t *testing.T) {
	// 这是最容易搞错的一处：采集器基线查询失败时会写 baseline=中文说明，
	// 而 change_ratio 此时被 ratio() 置为 0。若不先看 baseline，
	// 「没有基线可比」会被算成「变化为 0 ⇒ 弱证据」。
	e := ev(evidence.TypeMetric, map[string]string{
		KeyBaseline:    "基线窗口查询失败，未能获取事故前数据",
		KeyChangeRatio: "0.0000",
	})
	band, basis := Assess(e)
	if band != BandUnassessable {
		t.Fatalf("有 baseline 说明时必须判为无法评估，实际=%v", band)
	}
	if basis == "" {
		t.Fatal("无法评估也必须给出复核依据，否则页面上无从解释为何留空")
	}
}

func TestAssess_MissingOrBadRatioIsUnassessable(t *testing.T) {
	cases := []struct {
		name string
		md   map[string]string
	}{
		{"缺 change_ratio", map[string]string{}},
		{"非数字", map[string]string{KeyChangeRatio: "很多"}},
		{"空串", map[string]string{KeyChangeRatio: ""}},
		{"NaN 字符串", map[string]string{KeyChangeRatio: "NaN"}},
		{"+Inf 字符串", map[string]string{KeyChangeRatio: "+Inf"}},
		{"-Inf 字符串", map[string]string{KeyChangeRatio: "-Inf"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			band, _ := Assess(ev(evidence.TypeMetric, c.md))
			if band != BandUnassessable {
				t.Fatalf("%s 必须判为无法评估，实际=%v", c.name, band)
			}
		})
	}
}

func TestAssess_BandThresholds(t *testing.T) {
	cases := []struct {
		ratio string
		want  Band
	}{
		{"0.0000", BandWeak},
		{"1.0000", BandWeak},
		{"1.9999", BandWeak},
		{"2.0000", BandMedium},
		{"4.9999", BandMedium},
		{"5.0000", BandStrong},
		{"100.0000", BandStrong},
	}
	for _, c := range cases {
		t.Run(c.ratio, func(t *testing.T) {
			band, basis := Assess(ev(evidence.TypeMetric, map[string]string{KeyChangeRatio: c.ratio}))
			if band != c.want {
				t.Fatalf("ratio=%s 期望 %v，实际 %v", c.ratio, c.want, band)
			}
			if basis == "" {
				t.Fatal("每个档位都必须带复核依据")
			}
		})
	}
}

func TestAssess_NaNFloatIsRejected(t *testing.T) {
	// 变异测试靶点：若 isNotFinite 漏掉 NaN（只判 ±Inf），
	// 下面的 NaN 会一路落到 default 变成 BandWeak ——
	// 「数据坏掉」被静默解释成「变化很弱」。
	if !isNotFinite(math.NaN()) {
		t.Fatal("NaN 必须被识别为非有限值")
	}
	if !isNotFinite(math.Inf(1)) || !isNotFinite(math.Inf(-1)) {
		t.Fatal("±Inf 必须被识别为非有限值")
	}
	if isNotFinite(0) || isNotFinite(1.5) || isNotFinite(maxFinite) {
		t.Fatal("正常有限值不得被误判为非有限值")
	}
}

// --- Compose：档位表逐档钉死 --------------------------------------------

func TestCompose_UnassessableAlwaysEmpty(t *testing.T) {
	for hit := 0; hit <= 6; hit++ {
		v, ok := Compose(BandUnassessable, hit)
		if ok {
			t.Fatalf("hit=%d 时不可评估必须返回 ok=false，实际 v=%v", hit, v)
		}
		if v != 0 {
			t.Fatalf("ok=false 时返回值必须是 0，实际 %v", v)
		}
	}
}

func TestCompose_CoverageLadder(t *testing.T) {
	// 覆盖度必须**单调递增**：命中类型越多，置信度不能反而更低。
	// 若有人把 coverageBase 改成无序 map 的遍历结果，这一条会红。
	prev := -1.0
	for hit := 1; hit <= 4; hit++ {
		v, ok := Compose(BandMedium, hit)
		if !ok {
			t.Fatalf("hit=%d 应可评估", hit)
		}
		if v <= prev {
			t.Fatalf("覆盖度非单调：hit=%d 得 %v，不大于上一次的 %v", hit, v, prev)
		}
		prev = v
	}
}

func TestCompose_WeakIsCapped(t *testing.T) {
	// 弱规则命中时，即使覆盖度高也不得超过 WeakCap：
	// 否则一条「变化不足 2 倍」的噪声会代表整批证据的强度。
	for hit := 1; hit <= 4; hit++ {
		v, ok := Compose(BandWeak, hit)
		if !ok {
			t.Fatalf("hit=%d 弱规则仍可评估", hit)
		}
		if v > WeakCap+1e-9 {
			t.Fatalf("hit=%d 弱规则得 %v，超过上限 %v", hit, v, WeakCap)
		}
	}
	// hit=1 时 base 本身低于上限，不应被抬高到上限。
	v1, _ := Compose(BandWeak, 1)
	if v1 != BaseForCoverage(1) {
		t.Fatalf("hit=1 弱规则应取 base=%v，实际 %v", BaseForCoverage(1), v1)
	}
}

func TestCompose_StrongBonusCappedAtMax(t *testing.T) {
	for hit := 1; hit <= 4; hit++ {
		v, ok := Compose(BandStrong, hit)
		if !ok {
			t.Fatalf("hit=%d 强规则应可评估", hit)
		}
		if v > MaxConfidence+1e-9 {
			t.Fatalf("hit=%d 强规则得 %v，超过上限 %v", hit, v, MaxConfidence)
		}
	}
	// hit=4 时 base+bonus 恰好等于 MaxConfidence。
	v, _ := Compose(BandStrong, 4)
	if math.Abs(v-MaxConfidence) > 1e-9 {
		t.Fatalf("hit=4 强规则期望 %v，实际 %v", MaxConfidence, v)
	}
}

func TestCompose_ZeroHitNeverWrites(t *testing.T) {
	// 「有可评估证据却命中 0 种类型」是自相矛盾的输入，
	// 必须返回 ok=false 而不是写 0。
	v, ok := Compose(BandStrong, 0)
	if ok {
		t.Fatal("命中类型数为 0 时不得给出置信度")
	}
	if v != 0 {
		t.Fatalf("ok=false 时返回值必须 0，实际 %v", v)
	}
}

// --- Assign：幂等 + 三态落库 -------------------------------------------

func TestAssign_UnassessableStaysNil(t *testing.T) {
	// 手册 §4.3 第 2 条：给不出依据的证据仍留空。
	evs := []evidence.Evidence{
		ev(evidence.TypeTrace, map[string]string{"nothing": "trace 无可比字段"}),
		ev(evidence.TypeMetric, map[string]string{KeyBaseline: "基线不可用"}),
	}
	st := Assign(evs)
	for i := range evs {
		if evs[i].Confidence != nil {
			t.Fatalf("第 %d 条不可评估却写入了 %v", i, *evs[i].Confidence)
		}
	}
	if st.Assigned != 0 {
		t.Fatalf("Assigned 应为 0，实际 %d", st.Assigned)
	}
	if st.Unassessable != 2 {
		t.Fatalf("Unassessable 应为 2，实际 %d", st.Unassessable)
	}
}

func TestAssign_CoverageCountsAssessedTypesOnly(t *testing.T) {
	// 命中类型数只数「可评估且带规则档位」的类型。
	// 若把不可评估的也数进去，「采到 3 条但全无基线」会被算成覆盖度 3/6。
	evs := []evidence.Evidence{
		ev(evidence.TypeMetric, map[string]string{KeyChangeRatio: "6.0000"}),
		ev(evidence.TypeMetric, map[string]string{KeyChangeRatio: "7.0000"}),
		ev(evidence.TypeLog, map[string]string{KeyBaseline: "基线不可用"}),
		ev(evidence.TypeTrace, map[string]string{}),
	}
	st := Assign(evs)
	if st.CoverageHit != 1 {
		t.Fatalf("只有 metric 可评估，CoverageHit 应为 1，实际 %d", st.CoverageHit)
	}
	if st.Assigned != 2 {
		t.Fatalf("应有 2 条被赋值，实际 %d", st.Assigned)
	}
}

func TestAssign_IsIdempotent(t *testing.T) {
	// 重复调用不得改写已有结论 —— 否则第二批证据到达时
	// 会把第一批的置信度悄悄改掉，而那没人能察觉。
	evs := []evidence.Evidence{
		ev(evidence.TypeMetric, map[string]string{KeyChangeRatio: "6.0000"}),
		ev(evidence.TypeLog, map[string]string{KeyChangeRatio: "8.0000"}),
	}
	Assign(evs)
	first := make([]float64, len(evs))
	for i := range evs {
		first[i] = *evs[i].Confidence
	}
	st2 := Assign(evs)
	for i := range evs {
		if *evs[i].Confidence != first[i] {
			t.Fatalf("第 %d 条置信度被二次调用改写：%v → %v", i, first[i], *evs[i].Confidence)
		}
	}
	if st2.Assigned != 0 || st2.Skipped != 2 {
		t.Fatalf("二次调用应全部跳过，实际 %s", st2)
	}
}

func TestAssign_VariesWithStrength(t *testing.T) {
	// 手册 §4.3 第 1 条：confidence 随证据强度变化，不是常量。
	// 这是「所有证据都填同一个数」那类退化实现的照妖镜。
	evs := []evidence.Evidence{
		ev(evidence.TypeMetric, map[string]string{KeyChangeRatio: "0.5000"}), // weak
		ev(evidence.TypeLog, map[string]string{KeyChangeRatio: "3.0000"}),    // medium
		ev(evidence.TypeTrace, map[string]string{KeyChangeRatio: "9.0000"}),  // strong
	}
	Assign(evs)
	seen := map[float64]bool{}
	for i := range evs {
		if evs[i].Confidence == nil {
			t.Fatalf("第 %d 条未赋值", i)
		}
		seen[*evs[i].Confidence] = true
	}
	if len(seen) < 3 {
		t.Fatalf("三条强度不同的证据算出了 %d 个不同值（%v），判据未生效", len(seen), seen)
	}
}

func TestAssign_WritesReviewBasis(t *testing.T) {
	// 复核依据必须与置信度同批写入：页面上要能回答
	// 「这个 0.75 从哪来」，而不是只能问作者。
	evs := []evidence.Evidence{
		ev(evidence.TypeMetric, map[string]string{KeyChangeRatio: "6.0000"}),
	}
	Assign(evs)
	md := evs[0].Metadata
	if md[KeyConfidence] == "" {
		t.Fatal("缺少 " + KeyConfidence + " 复核依据")
	}
	if md[KeyCoverage] == "" {
		t.Fatal("缺少 " + KeyCoverage + " 覆盖度说明")
	}
	want := "1/" + strconv.Itoa(CoverageTotal)
	if md[KeyCoverage] != want {
		t.Fatalf("覆盖度说明期望 %q，实际 %q", want, md[KeyCoverage])
	}
}

func TestAssign_EmptyInput(t *testing.T) {
	st := Assign(nil)
	if st.Total != 0 || st.Assigned != 0 || st.CoverageHit != 0 {
		t.Fatalf("空输入应返回零值统计，实际 %s", st)
	}
}

// --- CoverageTotal 不得漂移 --------------------------------------------

func TestCoverageTotal_MatchesEvidenceTypes(t *testing.T) {
	// 若有人手写了常量 6 而证据类型变成 7，这条会红。
	// 它是「覆盖度分母不漂移」的唯一守卫。
	if CoverageTotal != len(evidence.SortedTypes()) {
		t.Fatalf("分母 %d 与实际类型数 %d 不符", CoverageTotal, len(evidence.SortedTypes()))
	}
	if CoverageTotal != 6 {
		t.Fatalf("当前证据表 CHECK 约束含 6 种类型，实际算出 %d", CoverageTotal)
	}
}

// --- 变异测试靶点（实测结论，非预测）------------------------------------
//
// 以下六条已在 D:\_iip_setup\_f2_mut_test.py 上真跑过，8/8 符合预期
// （脚本报告：docs/S1-置信度判据设计.md §六）。
//
//	M1 让 Assign 恒填 0.45（而非不赋值）
//	   → TestAssign_VariesWithStrength 变红。
//	   **这条是本组判据的核心**：早先写的是「摘掉赋值」，那样 Confidence
//	   恒为 nil，任何「非 nil」判据都能抓到它。真正的危险退化是
//	   「所有证据都填同一个数」——只有「值必须随强度变化」这条能抓。
//	M2 把 Compose 的 BandWeak 分支去掉（直接取 base）
//	   → TestCompose_WeakIsCapped + TestAssign_VariesWithStrength 变红。
//	M3 把 Assess 里的 baseline 检查摘掉
//	   → TestAssess_BaselinePresentMeansUnassessable 变红。
//	M4 把 isNotFinite 里的 NaN 判断摘掉
//	   → TestAssess_NaNFloatIsRejected + TestAssess_MissingOrBadRatioIsUnassessable
//	     变红（实测 3 条红，含 "NaN 字符串" 子用例）。
//	M5 把 Assign 的幂等保护摘掉（每次都重算）
//	   → TestAssign_IsIdempotent 变红。
//	M6 把 coverageBase 改成 3→0.75 / 4→0.70（破坏单调）
//	   → TestCompose_CoverageLadder + TestCompose_StrongBonusCappedAtMax 变红。
//	   注意：早先写的变异是 3→0.85 / 4→0.86，**那个变异打红的是
//	   StrongBonusCappedAtMax 而非 CoverageLadder** —— 因为 0.85<0.86
//	   仍是单调的，只破坏了「4+bonus==Max」这条等式。
//	   靶点必须挑真正破坏被验不变量的那一个。
//
// 反过来，下面这些变异**不应当**让判据变红，因为它们不改变语义：
//	R1 把 joinTypes 的分隔符从"/" 换成 "、" —— 无判据依赖它。实测全绿。
//	R2 改 Basis 里的措辞 —— 判据只查非空。实测全绿。
