package correlation

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"

	"github.com/yu/iip/internal/alert"
	"github.com/yu/iip/internal/metrics"
)

// 本文件覆盖 T6.3 的关联埋点。
//
// 关键判据不是「有分数被记录」，而是**分数记在哪个档位上**：
// 抑制会把 review 降级成 new，若 observeScore 记早了，
// 那条分数会落在 review 桶里 —— 而「多少条被人工抑制规则挡下」
// 恰好是最需要被数出来的信号。故第三条用例专门验这个先后顺序。

// scoreStat 返回某档位的（样本数，分数总和）。
//
// 不走 Registry().Gather() 而是直接 Write：HistogramVec 的子指标
// 本身就是 prometheus.Histogram，省掉一次全量采集，也让失败信息更聚焦。
func scoreStat(t *testing.T, m *metrics.Metrics, action string) (uint64, float64) {
	t.Helper()
	obs := m.CorrelationScore.WithLabelValues(action)
	h, ok := obs.(prometheus.Histogram)
	if !ok {
		t.Fatalf("action=%q 的子指标不是 Histogram（实际 %T）", action, obs)
	}
	var d dto.Metric
	if err := h.Write(&d); err != nil {
		t.Fatalf("读取直方图失败: %v", err)
	}
	return d.GetHistogram().GetSampleCount(), d.GetHistogram().GetSampleSum()
}

// TestMetricsNewIncidentCounted 覆盖 NEW 档：无候选事件时分数恒 0、事件数 +1。
func TestMetricsNewIncidentCounted(t *testing.T) {
	m := metrics.New()
	store := &fakeIncidentStore{}
	cfgStore := &fakeConfigStore{cfg: DefaultConfig()}
	svc := newCorrService(store, cfgStore, nil).WithMetrics(m)

	if err := svc.CorrelateAlerts(context.Background(), []alert.StoredAlert{newFiringOnCheckout(101, "pod-a")}); err != nil {
		t.Fatalf("CorrelateAlerts 返回意外错误: %v", err)
	}

	if v := testutil.ToFloat64(m.IncidentCreated); v != 1 {
		t.Errorf("incident_created = %v，期望 1", v)
	}
	n, sum := scoreStat(t, m, ActionNew)
	if n != 1 || sum != 0 {
		t.Errorf("score{action=new} 样本数=%d 总和=%v，期望 1 与 0（无候选事件时最高分为 0）", n, sum)
	}
	// 另外两档一次都不该有样本：Vec 是惰性的，没 Observe 就没有子指标。
	if n, _ := scoreStat(t, m, ActionReview); n != 0 {
		t.Errorf("score{action=review} 样本数=%d，期望 0", n)
	}
	if n, _ := scoreStat(t, m, ActionMerge); n != 0 {
		t.Errorf("score{action=merge} 样本数=%d，期望 0", n)
	}
}

// TestMetricsMergeScoreRecorded 覆盖 MERGE 档：分数按 action=merge 记，且不数新建事件。
//
// 0.80 是恰达阈值的边界值（拓扑把依赖维拉到 0.5），
// 用它断言总和能同时验证「记的是真实分数」而不是某个常量。
func TestMetricsMergeScoreRecorded(t *testing.T) {
	m := metrics.New()
	topo := &Topology{}
	topo.AddEdges(map[string][]string{"checkout": {"payment"}})
	store := &fakeIncidentStore{candidates: []CandidateAlert{candidateOnCheckout(7, "pod-old")}}
	svc := newCorrService(store, &fakeConfigStore{cfg: DefaultConfig()}, topo).WithMetrics(m)

	if err := svc.CorrelateAlerts(context.Background(), []alert.StoredAlert{newFiringOnCheckout(101, "pod-new")}); err != nil {
		t.Fatalf("CorrelateAlerts 返回意外错误: %v", err)
	}

	n, sum := scoreStat(t, m, ActionMerge)
	if n != 1 || !almostEq(sum, 0.80) {
		t.Errorf("score{action=merge} 样本数=%d 总和=%v，期望 1 与 0.80", n, sum)
	}
	if v := testutil.ToFloat64(m.IncidentCreated); v != 0 {
		t.Errorf("incident_created = %v，期望 0（合入已有事件不新建）", v)
	}
}

// TestMetricsSuppressionDowngradeCountsAsNew 是「记在最终档位」的判据。
//
// 构造：分数 0.70（review 区间）+ 抑制命中 → 最终 action 应为 new。
// 期望：分数落在 new 档（总和仍是 0.70），review 档一个样本都没有。
//
// 变异（把 observeScore 挪到抑制判断之前）会让本条红在
// 「review 样本数=1」与「new 样本数=0」两处 —— 这是本条存在的唯一理由。
func TestMetricsSuppressionDowngradeCountsAsNew(t *testing.T) {
	m := metrics.New()
	store := &fakeIncidentStore{candidates: []CandidateAlert{candidateOnCheckout(7, "pod-old")}}
	svc := newCorrService(store, &fakeConfigStore{cfg: DefaultConfig()}, nil).
		WithSuppressions(&fakeSuppressions{hit: true}).
		WithMetrics(m)

	if err := svc.CorrelateAlerts(context.Background(), []alert.StoredAlert{newFiringOnCheckout(101, "pod-new")}); err != nil {
		t.Fatalf("CorrelateAlerts 返回意外错误: %v", err)
	}

	if len(store.created) != 1 {
		t.Fatalf("抑制命中后应另起事件（new），实得 %d 个", len(store.created))
	}
	n, sum := scoreStat(t, m, ActionNew)
	if n != 1 || !almostEq(sum, 0.70) {
		t.Errorf("score{action=new} 样本数=%d 总和=%v，期望 1 与 0.70", n, sum)
	}
	if n, sum := scoreStat(t, m, ActionReview); n != 0 || sum != 0 {
		t.Errorf("score{action=review} 样本数=%d 总和=%v，期望 0（降级后不该记在 review 档）", n, sum)
	}
	if v := testutil.ToFloat64(m.IncidentCreated); v != 1 {
		t.Errorf("incident_created = %v，期望 1（降级为 new 会建事件）", v)
	}
}

// TestWithMetricsNilFallsBackToNoop 守住注入契约：nil 不能让下一次关联 panic。
func TestWithMetricsNilFallsBackToNoop(t *testing.T) {
	svc := newCorrService(&fakeIncidentStore{}, &fakeConfigStore{cfg: DefaultConfig()}, nil).WithMetrics(nil)
	if svc.metrics == nil {
		t.Fatal("WithMetrics(nil) 后 metrics 仍为 nil")
	}
	if err := svc.CorrelateAlerts(context.Background(), []alert.StoredAlert{newFiringOnCheckout(101, "pod-a")}); err != nil {
		t.Fatalf("未注入指标时关联失败: %v", err)
	}
}
