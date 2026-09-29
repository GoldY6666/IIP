package correlation

import "testing"

// TestDecide 锁定三档决策的边界语义：一律取「大于等于」，
// 即得分恰好等于阈值时归入更高一档（0.80 判 MERGE 而非 REVIEW）。
func TestDecide(t *testing.T) {
	th := DefaultConfig().Thresholds // merge 0.80 / review 0.50

	cases := []struct {
		name  string
		score float64
		want  string
	}{
		{"满分_合并", 1.0, ActionMerge},
		{"恰好等于merge阈值_合并", 0.80, ActionMerge},
		{"略低于merge_复核", 0.7999, ActionReview},
		{"区间中点_复核", 0.65, ActionReview},
		{"恰好等于review阈值_复核", 0.50, ActionReview},
		{"略低于review_新建", 0.4999, ActionNew},
		{"零分_新建", 0, ActionNew},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Decide(tc.score, th); got != tc.want {
				t.Fatalf("Decide(%v) = %q，期望 %q", tc.score, got, tc.want)
			}
		})
	}
}

// TestDecideDegenerateThresholds：merge 与 review 相等（合法配置）时不存在复核档。
func TestDecideDegenerateThresholds(t *testing.T) {
	th := Thresholds{Merge: 0.60, Review: 0.60}
	if err := (Config{Weights: DefaultConfig().Weights, Thresholds: th, Enabled: true}).Validate(); err != nil {
		t.Fatalf("merge==review 应为合法配置: %v", err)
	}
	if got := Decide(0.60, th); got != ActionMerge {
		t.Errorf("恰好达阈应合并，实得 %q", got)
	}
	if got := Decide(0.59, th); got != ActionNew {
		t.Errorf("未达阈应新建（此配置下无复核档），实得 %q", got)
	}
}
