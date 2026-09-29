package correlation

import (
	"testing"
	"time"
)

func TestCalculateScore(t *testing.T) {
	w := DefaultConfig().Weights

	cases := []struct {
		name string
		d    DimensionScores
		want float64
	}{
		{"全零", DimensionScores{}, 0},
		{"各维全中_权重和为1", DimensionScores{Time: 1, Service: 1, Label: 1, Dependency: 1, Historical: 1}, 1.0},
		{"仅时间维", DimensionScores{Time: 1}, 0.25},
		{"仅服务维", DimensionScores{Service: 1}, 0.25},
		{"仅标签维", DimensionScores{Label: 1}, 0.20},
		{"仅依赖维", DimensionScores{Dependency: 1}, 0.20},
		{"仅历史维", DimensionScores{Historical: 1}, 0.10},
		// 手册 §4.2 的典型组合：同服务同标签同窗口、无依赖无历史 = 0.70
		{"同源但无依赖无历史_0.70", DimensionScores{Time: 1, Service: 1, Label: 1}, 0.70},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CalculateScore(tc.d, w); !almostEq(got, tc.want) {
				t.Fatalf("CalculateScore = %v，期望 %v", got, tc.want)
			}
		})
	}
}

// TestCalculateScoreClamps 确认异常输入被夹紧到 [0,1]：
// 阈值判定依赖值域，一旦越界会出现「0.95 分却判 NEW」这类怪象。
// TestCalculateScoreThresholdBoundary 锁定一个真实踩到的边界缺陷：
// 0.25+0.25+0.20+0.10 在 IEEE 754 下等于 0.7999999999999999（比 0.80 小 1 ulp），
// 若不量化，数学上恰好达到 merge 阈值、按 V1.0 §5.3 应判 MERGE 的组合会被误判为 REVIEW。
func TestCalculateScoreThresholdBoundary(t *testing.T) {
	cfg := DefaultConfig()

	// 时间、服务、标签三维满分 + 历史满分、无依赖 = 数学上恰好 0.80。
	d := DimensionScores{Time: 1, Service: 1, Label: 1, Historical: 1}
	got := CalculateScore(d, cfg.Weights)

	if got != cfg.Thresholds.Merge {
		t.Fatalf("得分 = %v（%.20f），期望恰为 %v", got, got, cfg.Thresholds.Merge)
	}
	if action := Decide(got, cfg.Thresholds); action != ActionMerge {
		t.Fatalf("恰好达到 merge 阈值应判 MERGE，实得 %q", action)
	}
	// 记录未量化时的原始浮点值，防止将来有人删掉量化后又以为测试多余。
	if raw := 0.25 + 0.25 + 0.20 + 0.10; raw < cfg.Thresholds.Merge {
		t.Logf("未量化原始值 = %.20f，比阈值少 %g（量化前会误判为 REVIEW）", raw, cfg.Thresholds.Merge-raw)
	}
}

func TestCalculateScoreClamps(t *testing.T) {
	// 权重和大于 1（非法配置）时结果可能超过 1。
	over := Weights{Time: 1, Service: 1, Label: 1, Dependency: 1, Historical: 1}
	if got := CalculateScore(DimensionScores{Time: 1, Service: 1, Label: 1, Dependency: 1, Historical: 1}, over); got != 1 {
		t.Errorf("超上界应夹到 1，实得 %v", got)
	}
	// 负权重（非法配置）时结果可能为负。
	neg := Weights{Time: -1}
	if got := CalculateScore(DimensionScores{Time: 1}, neg); got != 0 {
		t.Errorf("低于下界应夹到 0，实得 %v", got)
	}
}

// TestScorerDimensions 确认五维取值来自正确的来源，且历史维走注入的函数。
func TestScorerDimensions(t *testing.T) {
	ref := at(10 * time.Minute)
	topo := &Topology{}
	topo.AddEdges(map[string][]string{"checkout": {"payment"}})

	var gotPair [2]string
	scorer := NewScorer(DefaultConfig(), topo, func(fpA, fpB string) float64 {
		gotPair = [2]string{fpA, fpB}
		return 0.42
	}, ref)

	a := view("checkout", "prod", map[string]string{"alertname": "LatencyHigh"}, at(0), tp(10*time.Minute))
	a.Fingerprint = "fp-a"
	b := view("payment", "prod", map[string]string{"alertname": "LatencyHigh"}, at(0), tp(10*time.Minute))
	b.Fingerprint = "fp-b"

	d := scorer.Dimensions(a, b)

	if !almostEq(d.Time, 1.0) {
		t.Errorf("时间维 = %v，期望 1.0", d.Time)
	}
	if !almostEq(d.Service, 0.6) {
		t.Errorf("服务维（同命名空间异服务）= %v，期望 0.6", d.Service)
	}
	// 标签：{alertname,service,namespace} 中 alertname/namespace 相同 → 2/4 = 0.5
	if !almostEq(d.Label, 0.5) {
		t.Errorf("标签维 = %v，期望 0.5", d.Label)
	}
	if !almostEq(d.Dependency, 0.9) {
		t.Errorf("依赖维（直接依赖边）= %v，期望 0.9", d.Dependency)
	}
	if !almostEq(d.Historical, 0.42) {
		t.Errorf("历史维 = %v，期望注入的 0.42", d.Historical)
	}
	if gotPair != [2]string{"fp-a", "fp-b"} {
		t.Errorf("历史函数入参 = %v，期望 [fp-a fp-b]", gotPair)
	}

	// Score 应为同一次计算的加权和，避免两处口径漂移。
	wantScore := CalculateScore(d, DefaultConfig().Weights)
	if got := scorer.Score(a, b); !almostEq(got, wantScore) {
		t.Errorf("Score = %v，期望 %v", got, wantScore)
	}
}

// TestNewScorerNilHistory：冷启动（库中无历史）时历史维恒 0，而非 panic。
func TestNewScorerNilHistory(t *testing.T) {
	scorer := NewScorer(DefaultConfig(), nil, nil, at(0))
	a := view("checkout", "prod", map[string]string{"alertname": "X"}, at(0), tp(time.Minute))
	a.Fingerprint = "fp-a"
	b := view("checkout", "prod", map[string]string{"alertname": "X"}, at(0), tp(time.Minute))
	b.Fingerprint = "fp-b"

	if got := scorer.Dimensions(a, b).Historical; got != 0 {
		t.Errorf("nil 历史源应恒为 0，实得 %v", got)
	}
	// 同服务同标签同窗口 + 无依赖无历史 = 0.70
	if got := scorer.Score(a, b); !almostEq(got, 0.70) {
		t.Errorf("Score = %v，期望 0.70", got)
	}
}

func TestScorerConfig(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Thresholds.Merge = 0.9
	scorer := NewScorer(cfg, nil, nil, at(0))
	if got := scorer.Config(); got != cfg {
		t.Fatalf("Config = %+v，期望 %+v", got, cfg)
	}
}
