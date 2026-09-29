package correlation

import (
	"testing"
	"time"
)

// TestClusterInMemoryEmptyAndSingle 覆盖边界规模。
func TestClusterInMemoryEmptyAndSingle(t *testing.T) {
	ref := at(time.Hour)

	if got := ClusterInMemory(nil, DefaultConfig(), nil, nil, ref); len(got) != 0 {
		t.Fatalf("空输入应返回 0 组，实得 %d 组", len(got))
	}

	one := []AlertView{view("checkout", "prod", map[string]string{"alertname": "X"}, at(0), tp(time.Minute))}
	got := ClusterInMemory(one, DefaultConfig(), nil, nil, ref)
	if len(got) != 1 || len(got[0]) != 1 || got[0][0] != 0 {
		t.Fatalf("单条告警应自成一组 [0]，实得 %v", got)
	}
}

// TestClusterInMemoryUnrelatedSplits 确认完全无关的告警不会被并成一组
// （精确率的第一道防线）。
func TestClusterInMemoryUnrelatedSplits(t *testing.T) {
	alerts := []AlertView{
		view("alpha", "team-a", map[string]string{"alertname": "A1"}, at(0), tp(5*time.Minute)),
		view("beta", "team-b", map[string]string{"alertname": "B1"}, at(30*time.Minute), tp(35*time.Minute)),
	}
	got := ClusterInMemory(alerts, DefaultConfig(), nil, nil, at(time.Hour))
	if len(got) != 2 {
		t.Fatalf("无关告警应各自成组，实得 %v", got)
	}
}

// TestClusterInMemoryReviewStillGroups 固定评估口径：
// REVIEW 档在线上也会建立关联边（只是多打 needs_review 标记），
// 因此回放评估时同样视为「同组」。
func TestClusterInMemoryReviewStillGroups(t *testing.T) {
	alerts := []AlertView{
		view("checkout", "prod", map[string]string{"alertname": "LatencyHigh"}, at(0), tp(10*time.Minute)),
		view("checkout", "prod", map[string]string{"alertname": "LatencyHigh"}, at(0), tp(10*time.Minute)),
	}
	cfg := DefaultConfig()

	// 先证明这一对确实落在 REVIEW 档（同服务同标签同窗口、无依赖无历史 = 0.70），
	// 否则下面的断言就退化成「MERGE 也能并组」这种无信息量的检查。
	scorer := NewScorer(cfg, nil, nil, at(10*time.Minute))
	if score := scorer.Score(alerts[0], alerts[1]); !almostEq(score, 0.70) {
		t.Fatalf("构造的用例得分 %v，期望 0.70（REVIEW 档）", score)
	}
	if action := Decide(0.70, cfg.Thresholds); action != ActionReview {
		t.Fatalf("0.70 应为 REVIEW，实得 %q", action)
	}

	got := ClusterInMemory(alerts, cfg, nil, nil, at(10*time.Minute))
	if len(got) != 1 || len(got[0]) != 2 {
		t.Fatalf("REVIEW 档应并入同组，实得 %v", got)
	}
}

// TestClusterInMemoryUsesInjectedHistory 证明历史维被真正串入聚类：
// 把 merge/review 阈值都抬到 0.80 后，0.70 的告警对会被拆开；
// 注入「历史共现恒为 1」后得分升到 0.80，才被合并。
func TestClusterInMemoryUsesInjectedHistory(t *testing.T) {
	alerts := []AlertView{
		view("checkout", "prod", map[string]string{"alertname": "LatencyHigh"}, at(0), tp(10*time.Minute)),
		view("checkout", "prod", map[string]string{"alertname": "LatencyHigh"}, at(0), tp(10*time.Minute)),
	}
	cfg := DefaultConfig()
	cfg.Thresholds = Thresholds{Merge: 0.80, Review: 0.80}
	ref := at(10 * time.Minute)

	if got := ClusterInMemory(alerts, cfg, nil, nil, ref); len(got) != 2 {
		t.Fatalf("无历史时应拆为 2 组，实得 %v", got)
	}

	hist := func(_, _ string) float64 { return 1.0 }
	got := ClusterInMemory(alerts, cfg, nil, hist, ref)
	if len(got) != 1 || len(got[0]) != 2 {
		t.Fatalf("有历史加分后应合为 1 组，实得 %v", got)
	}
}

// TestClusterInMemoryPicksBestGroup 确认新告警归并到「得分最高的组」，
// 而不是先遇到的那个组——否则组序会左右聚类结果。
func TestClusterInMemoryPicksBestGroup(t *testing.T) {
	alerts := []AlertView{
		// 组 0：与后续告警毫无关系。
		view("alpha", "team-a", map[string]string{"alertname": "Noise"}, at(0), tp(time.Minute)),
		// 组 1：与第三条告警同服务同规则同窗口。
		view("checkout", "prod", map[string]string{"alertname": "LatencyHigh"}, at(10*time.Minute), tp(20*time.Minute)),
		// 第三条：应并入组 1。
		view("checkout", "prod", map[string]string{"alertname": "LatencyHigh"}, at(10*time.Minute), tp(20*time.Minute)),
	}
	got := ClusterInMemory(alerts, DefaultConfig(), nil, nil, at(30*time.Minute))

	want := []Group{{0}, {1, 2}}
	if len(got) != len(want) {
		t.Fatalf("分组数 = %d，期望 %d（实得 %v）", len(got), len(want), got)
	}
	for i := range want {
		if len(got[i]) != len(want[i]) {
			t.Fatalf("第 %d 组 = %v，期望 %v", i, got[i], want[i])
		}
		for j := range want[i] {
			if got[i][j] != want[i][j] {
				t.Fatalf("第 %d 组 = %v，期望 %v", i, got[i], want[i])
			}
		}
	}
}
