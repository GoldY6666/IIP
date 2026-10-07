package rca

import (
	"strings"
	"testing"
	"time"

	"github.com/yu/iip/internal/evidence"
)

// fixedTS 是测试用的固定时间戳。
//
// 刻意固定而非用 time.Now()：Confidence 与时间无关，
// 但若将来判据引入时间维度，固定值能让差异立刻显现。
var fixedTS = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

// --- SuggestFrom：只建议，不写库；无置信度即不参与推断 -----------------

func TestSuggest_SkipsEvidenceWithoutConfidence(t *testing.T) {
	// 手册 §4.1：给不出可复核来源 ⇒ 不得拿来当证据。
	// 全部 confidence 为 nil 时不应产出任何假设。
	evs := []evidence.Evidence{
		ev(evidence.TypeLog, map[string]string{KeyChangeRatio: "9.0000"}),
		ev(evidence.TypeMetric, map[string]string{KeyChangeRatio: "9.0000"}),
	}
	s := SuggestFrom(evs)
	if len(s.Hypotheses) != 0 {
		t.Fatalf("无置信度证据时不应产出假设，实际 %d 条", len(s.Hypotheses))
	}
	if s.Skipped != 2 || s.AssessedCount != 0 {
		t.Fatalf("统计错误：Skipped=%d AssessedCount=%d", s.Skipped, s.AssessedCount)
	}
}

func TestSuggest_DeploymentPlusMetric(t *testing.T) {
	evs := []evidence.Evidence{
		ev(evidence.TypeDeployment, map[string]string{KeyChangeRatio: "3.0000"}),
		ev(evidence.TypeMetric, map[string]string{KeyChangeRatio: "6.0000"}),
	}
	Assign(evs)
	s := SuggestFrom(evs)
	if len(s.Hypotheses) == 0 {
		t.Fatal("deployment+metric 应产出至少一条假设")
	}
	h := s.Hypotheses[0]
	if h.Confidence == nil {
		t.Fatal("有两类证据支撑时不应留空")
	}
	if h.Support != 2 {
		t.Fatalf("Support 应为 2，实际 %d", h.Support)
	}
	if h.Basis == "" {
		t.Fatal("假设必须带复核依据")
	}
}

func TestSuggest_SingleTypeStillLow(t *testing.T) {
	// 单类命中可以给假设，但置信度必须落在下限附近——
	// 不能因为「有证据」就给高分。
	evs := []evidence.Evidence{
		ev(evidence.TypeMetric, map[string]string{KeyChangeRatio: "3.0000"}),
	}
	Assign(evs)
	s := SuggestFrom(evs)
	if len(s.Hypotheses) == 0 {
		t.Fatal("单类证据也应产出假设（但置信度应低）")
	}
	h := s.Hypotheses[0]
	if h.Confidence == nil {
		t.Fatal("单类可评估证据不应留空")
	}
	if *h.Confidence > 0.5 {
		t.Fatalf("单类证据置信度过高：%v", *h.Confidence)
	}
}

// TestHypothesis_ConfidenceOrderedByCoverage 直接比对期望的**具体数值**：
// 覆盖度越高、支撑越强，置信度必须严格更高。
//
// 这条判据与 TestAssign_VariesWithStrength 互补：那条验「同一批内
// 不同强度算出不同值」，这条验「不同批之间的高低关系对」——
// 一个把所有假设都填同一个数的退化实现，两条都会红。
func TestHypothesis_ConfidenceOrderedByCoverage(t *testing.T) {
	mk := func(types ...evidence.Type) []evidence.Evidence {
		evs := make([]evidence.Evidence, 0, len(types))
		for _, t := range types {
			evs = append(evs, ev(t, map[string]string{KeyChangeRatio: "3.0000"}))
		}
		return evs
	}

	// 单类 → 两类 → 三类，覆盖度递增。
	cases := []struct {
		name  string
		evs   []evidence.Evidence
		types int
	}{
		{"单类", mk(evidence.TypeMetric), 1},
		{"两类", mk(evidence.TypeMetric, evidence.TypeLog), 2},
		{"三类", mk(evidence.TypeMetric, evidence.TypeLog, evidence.TypeDeployment), 3},
	}

	prev := -1.0
	for _, c := range cases {
		// 必须先 Assign：SuggestFrom 只用**有置信度**的证据。
		// 少了这一步，所有 Confidence 都是 nil，SuggestFrom 按设计全部跳过 ——
		// 那会让本判据以「应产出假设」失败，但那测的是构造错误不是判据。
		Assign(c.evs)
		s := SuggestFrom(c.evs)
		if len(s.Hypotheses) == 0 {
			t.Fatalf("%s：应产出假设", c.name)
		}
		got := 0.0
		for _, h := range s.Hypotheses {
			if h.Confidence != nil && *h.Confidence > got {
				got = *h.Confidence
			}
		}
		if got <= prev {
			t.Fatalf("%s：最高置信度 %v 未高于上一档的 %v", c.name, got, prev)
		}
		// 单类必须落在下限附近，不能因为「有证据」就给高分。
		if c.types == 1 && got > 0.5 {
			t.Fatalf("单类证据置信度过高：%v", got)
		}
		prev = got
	}
}

func TestSuggest_HasNoTypeStaysEmpty(t *testing.T) {
	// trace 采集器没有任何可比字段 —— 它永远不可评估。
	// 若这条不成立，说明判据在某处把「无信号」当成了「有信号」。
	evs := []evidence.Evidence{ev(evidence.TypeTrace, map[string]string{})}
	Assign(evs)
	s := SuggestFrom(evs)
	if len(s.Hypotheses) != 0 {
		t.Fatalf("仅 trace 证据不应产出假设，实际 %d 条", len(s.Hypotheses))
	}
}

func TestSuggest_SortedByConfidenceDesc(t *testing.T) {
	evs := []evidence.Evidence{
		ev(evidence.TypeMetric, map[string]string{KeyChangeRatio: "0.1000"}),
		ev(evidence.TypeLog, map[string]string{KeyChangeRatio: "9.0000"}),
		ev(evidence.TypeDeployment, map[string]string{KeyChangeRatio: "0.2000"}),
		ev(evidence.TypeConfigChange, map[string]string{KeyChangeRatio: "0.3000"}),
	}
	Assign(evs)
	s := SuggestFrom(evs)
	for i := 1; i < len(s.Hypotheses); i++ {
		a, b := s.Hypotheses[i-1], s.Hypotheses[i]
		if a.Confidence == nil || b.Confidence == nil {
			continue
		}
		if *a.Confidence < *b.Confidence-1e-9 {
			t.Fatalf("第 %d 条(%.2f)置信度低于第 %d 条(%.2f)，排序错",
				i-1, *a.Confidence, i, *b.Confidence)
		}
	}
}

func TestSuggest_EmptyInput(t *testing.T) {
	s := SuggestFrom(nil)
	if len(s.Hypotheses) != 0 || s.AssessedCount != 0 || s.Skipped != 0 {
		t.Fatalf("空输入应返回零值，实际 %+v", s)
	}
}

func TestBand_String(t *testing.T) {
	cases := map[Band]string{
		BandUnassessable: "unassessable",
		BandWeak:         "weak",
		BandMedium:       "medium",
		BandStrong:       "strong",
		Band(99):         "unknown(99)",
	}
	for b, want := range cases {
		if got := b.String(); got != want {
			t.Fatalf("Band(%d).String() 期望 %q，实际 %q", int(b), want, got)
		}
	}
}

func TestStats_String(t *testing.T) {
	// 日志里的统计必须自证字段齐全，否则「assigned=0」无从判断是
	// 「全不可评估」还是「压根没调用」。
	s := Stats{Total: 3, Assigned: 2, Unassessable: 1, Skipped: 0, CoverageHit: 2}
	got := s.String()
	for _, want := range []string{"total=3", "assigned=2", "unassessable=1", "coverage=2/6"} {
		if !strings.Contains(got, want) {
			t.Fatalf("统计串缺少 %q，实际 %q", want, got)
		}
	}
}
