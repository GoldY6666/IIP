package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"

	"github.com/yu/iip/internal/evidence"
	"github.com/yu/iip/internal/metrics"
)

// 本文件覆盖 T6.3 的 worker 侧埋点：采集耗时与重试。
//
// 两个进程各埋各的（server 管接入、worker 管采集），
// 因此这里的断言都只针对 worker 进程会产出的那两个指标。

// evidenceStat 返回某来源的（样本数，耗时总和）。
func evidenceStat(t *testing.T, m *metrics.Metrics, source string) (uint64, float64) {
	t.Helper()
	obs := m.EvidenceDuration.WithLabelValues(source)
	h, ok := obs.(prometheus.Histogram)
	if !ok {
		t.Fatalf("source=%q 的子指标不是 Histogram（实际 %T）", source, obs)
	}
	var d dto.Metric
	if err := h.Write(&d); err != nil {
		t.Fatalf("读取直方图失败: %v", err)
	}
	return d.GetHistogram().GetSampleCount(), d.GetHistogram().GetSampleSum()
}

// TestMetricsEvidenceDurationIncludesFailures 验证「失败也计时」。
//
// 这是本组最重要的一条：只给成功的采集计时，会让「Prometheus 一直超时」
// 这件事在指标上完全消失 —— 图上只剩 Loki 的健康耗时，
// 而证据缺失的原因永远查不到。故 bad 采集器必须同样贡献一个样本。
//
// 用真实 sleep 而不是假时钟：耗时是在 CollectAll 内部用 time.Now 量的，
// 要证明它真量到了，只能让被量对象真的花时间。阈值 15ms / 实际 20ms
// 给调度抖动留了余量，同时与「漏写 Duration」的 0 有足够距离。
func TestMetricsEvidenceDurationIncludesFailures(t *testing.T) {
	const nap = 20 * time.Millisecond
	const floor = 0.015

	m := metrics.New()
	good := &fakeCollector{src: "loki", delay: nap, evs: []evidence.Evidence{
		metricEv("loki", "a", testNow.Add(-time.Minute)),
	}}
	bad := &fakeCollector{src: "prometheus", delay: nap, err: errors.New("连接被拒绝")}

	reg := evidence.NewRegistry()
	reg.MustRegister(good)
	reg.MustRegister(bad)

	store := newFakeStore()
	targets := &fakeTargets{target: Target{Service: "api", StartedAt: testNow.Add(-20 * time.Minute)}}
	store.enqueue(collectTask(t, 7, time.Time{}))

	p := newTestProcessor(store, &fakeEvidenceStore{}, targets, reg, nil, Config{})
	p.SetMetrics(m)

	if _, err := p.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce 返回错误 = %v，期望 nil（单个采集器失败不该让任务失败）", err)
	}

	for _, src := range []string{"loki", "prometheus"} {
		n, sum := evidenceStat(t, m, src)
		if n != 1 {
			t.Errorf("source=%s 样本数=%d，期望 1（每次调用计一次）", src, n)
		}
		if sum < floor {
			t.Errorf("source=%s 耗时总和=%v，期望 ≥ %v（耗时没被测量）", src, sum, floor)
		}
	}
	// 未注册的来源不该有样本：否则说明标签被写成了固定值，
	// 那样所有来源会挤在一个桶里，看图上就只有一条线。
	if n, _ := evidenceStat(t, m, "k8s"); n != 0 {
		t.Errorf("source=k8s 样本数=%d，期望 0（未注册的采集器）", n)
	}
}

// TestMetricsStaleReclaimAddsReclaimCount 验证回收计数按**条数**累加。
//
// 按次累加（一次 tick +1）是很容易写错的版本，而它会让「一批 worker 同时崩掉、
// 一次回收 12 个任务」与「队列平稳、每次回收 1 个」在指标上一模一样。
func TestMetricsStaleReclaimAddsReclaimCount(t *testing.T) {
	m := metrics.New()
	store := newFakeStore()
	store.reclaimed = 3
	store.enqueue(collectTask(t, 1, time.Time{}))

	reg := evidence.NewRegistry()
	reg.MustRegister(&fakeCollector{src: "loki"})

	p := newTestProcessor(store, &fakeEvidenceStore{}, &fakeTargets{
		target: Target{Service: "api", StartedAt: testNow.Add(-time.Minute)},
	}, reg, nil, Config{})
	p.SetMetrics(m)

	if _, err := p.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce 返回错误 = %v", err)
	}

	if v := testutil.ToFloat64(m.TaskRetry.WithLabelValues(metrics.RetryStaleReclaim)); v != 3 {
		t.Errorf("task_retry{reason=stale_reclaim} = %v，期望 3（按条数而非按次）", v)
	}
}

// TestMetricsAttemptFailedCounted 验证失败计数：全采集器失败才计一次。
//
// 「一条任务失败」记 1，而不是「每个失败的采集器记 1」：
// 指标口径要与任务重试次数对齐，否则「重试风暴」的读数会放大 N 倍，
// 照着它去扩容只会把集群越调越忙。
func TestMetricsAttemptFailedCounted(t *testing.T) {
	m := metrics.New()
	store := newFakeStore()
	store.enqueue(collectTask(t, 9, time.Time{}))

	reg := evidence.NewRegistry()
	reg.MustRegister(&fakeCollector{src: "loki", err: errors.New("挂了")})
	reg.MustRegister(&fakeCollector{src: "prometheus", err: errors.New("也挂了")})

	p := newTestProcessor(store, &fakeEvidenceStore{}, &fakeTargets{
		target: Target{Service: "api", StartedAt: testNow.Add(-time.Minute)},
	}, reg, nil, Config{})
	p.SetMetrics(m)

	if _, err := p.RunOnce(context.Background()); err == nil {
		t.Fatal("全部采集器失败时应返回错误（供日志与健康观测）")
	}

	if v := testutil.ToFloat64(m.TaskRetry.WithLabelValues(metrics.RetryAttemptFailed)); v != 1 {
		t.Errorf("task_retry{reason=attempt_failed} = %v，期望 1（按任务计，不按采集器计）", v)
	}
	if len(store.failed) != 1 {
		t.Errorf("失败标记 %d 次，期望 1", len(store.failed))
	}
}

// TestMetricsAttemptFailedNotCountedOnSuccess 是反向判据：
// 成功的任务不该留下 attempt_failed 样本。
//
// 没有这条的话，把 Inc() 写错位置（例如写在 RunOnce 入口）依然能通过上面那条。
func TestMetricsAttemptFailedNotCountedOnSuccess(t *testing.T) {
	m := metrics.New()
	store := newFakeStore()
	store.enqueue(collectTask(t, 3, time.Time{}))

	reg := evidence.NewRegistry()
	reg.MustRegister(&fakeCollector{src: "loki"})

	p := newTestProcessor(store, &fakeEvidenceStore{}, &fakeTargets{
		target: Target{Service: "api", StartedAt: testNow.Add(-time.Minute)},
	}, reg, nil, Config{})
	p.SetMetrics(m)

	if _, err := p.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce 返回错误 = %v", err)
	}
	// Vec 惰性：没有 Inc 就没有子指标，ToFloat64 只会读到 0。
	if v := testutil.ToFloat64(m.TaskRetry.WithLabelValues(metrics.RetryAttemptFailed)); v != 0 {
		t.Errorf("task_retry{reason=attempt_failed} = %v，期望 0（成功任务不该计失败）", v)
	}
}

// TestSetMetricsNilFallsBackToNoop 守住注入契约。
func TestSetMetricsNilFallsBackToNoop(t *testing.T) {
	p := newTestProcessor(newFakeStore(), &fakeEvidenceStore{}, &fakeTargets{},
		evidence.NewRegistry(), nil, Config{})
	p.SetMetrics(nil)
	if p.metrics == nil {
		t.Fatal("SetMetrics(nil) 后 metrics 仍为 nil：下一次 RunOnce 会 panic")
	}
	if _, err := p.RunOnce(context.Background()); err != nil {
		t.Fatalf("未注入指标时 RunOnce 失败: %v", err)
	}
}
