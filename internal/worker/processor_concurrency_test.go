package worker

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/yu/iip/internal/evidence"
	"github.com/yu/iip/internal/task"
)

// TestRunOnce_DropsInvalidEvidence 单条非法证据不该拖垮整个采集器。
func TestRunOnce_DropsInvalidEvidence(t *testing.T) {
	// 时间戳为零的证据违反 ADR-003（不得用 NOW() 兜底），必须被丢弃。
	reg := evidence.NewRegistry()
	reg.MustRegister(&fakeCollector{src: "loki", evs: []evidence.Evidence{
		{Type: evidence.TypeMetric, Source: "loki", Query: "up", Value: "bad"},
		metricEv("loki", "good", testNow.Add(-time.Minute)),
	}})

	store := newFakeStore()
	evStore := &fakeEvidenceStore{}
	targets := &fakeTargets{target: Target{Service: "api", StartedAt: testNow}}
	store.enqueue(collectTask(t, 1, time.Time{}))

	p := newTestProcessor(store, evStore, targets, reg, nil, Config{})
	if _, err := p.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce 返回错误: %v", err)
	}
	if len(evStore.saved) != 1 {
		t.Errorf("落库 %d 条，期望 1（非法那条应被丢弃而非拖垮整批）", len(evStore.saved))
	}
	if len(store.failed) != 0 {
		t.Errorf("失败 %d 次，期望 0 —— 丢弃单条非法证据属部分成功，不该重试", len(store.failed))
	}
}

// TestRunOnce_ClaimErrorPropagates 领取失败必须返回错误。
func TestRunOnce_ClaimErrorPropagates(t *testing.T) {
	store := newFakeStore()
	store.claimEr = errors.New("数据库不可达")

	p := newTestProcessor(store, &fakeEvidenceStore{},
		&fakeTargets{target: Target{}}, evidence.NewRegistry(), nil, Config{})
	n, err := p.RunOnce(context.Background())
	if err == nil {
		t.Fatal("领取失败时应返回错误")
	}
	if n != 0 {
		t.Errorf("处理任务数 = %d，期望 0", n)
	}
}

// TestRunOnce_ContinuesAfterOneTaskFails 一条任务失败不该影响同批其他任务。
//
// 处理器是逐条调用 RunOnce 的，若第一条失败就 return，
// 后面排着的任务会一直停在 in_flight 直到超时回收 —— 而它们本来是好的。
func TestRunOnce_ContinuesAfterOneTaskFails(t *testing.T) {
	store := newFakeStore()
	store.enqueue(collectTask(t, 1, time.Time{})) // 目标存在 → 应成功
	// 第二个任务用不存在的 kind，必然失败。
	store.enqueue(&task.Task{Kind: task.Kind("bogus"), Payload: json.RawMessage(`{}`)})

	reg := evidence.NewRegistry()
	reg.MustRegister(&fakeCollector{src: "loki", evs: []evidence.Evidence{
		metricEv("loki", "a", testNow.Add(-time.Minute)),
	}})
	evStore := &fakeEvidenceStore{}
	p := newTestProcessor(store, evStore,
		&fakeTargets{target: Target{Service: "api", StartedAt: testNow}},
		reg, nil, Config{})

	n, err := p.RunOnce(context.Background())
	if n != 2 {
		t.Errorf("处理任务数 = %d，期望 2", n)
	}
	if err == nil {
		t.Error("第二条失败时应返回错误供日志观测")
	}
	if len(store.completed) != 1 {
		t.Errorf("成功 %d 条，期望 1（坏任务不该连累好任务）", len(store.completed))
	}
	if len(store.failed) != 1 {
		t.Errorf("失败 %d 条，期望 1", len(store.failed))
	}
}

// TestRunOnce_ConcurrentCallsAreSafe 并发调用同一 Processor 不得崩溃或串数据。
//
// Processor 的契约明说「同一实例会被多个 worker goroutine 同时调用」，
// 窗口推导、游标推进、证据汇总三处都有共享写入点。
func TestRunOnce_ConcurrentCallsAreSafe(t *testing.T) {
	reg := evidence.NewRegistry()
	reg.MustRegister(&fakeCollector{src: "loki", evs: []evidence.Evidence{
		metricEv("loki", "a", testNow.Add(-time.Minute)),
	}})

	store := newFakeStore()
	evStore := &fakeEvidenceStore{}
	targets := &fakeTargets{target: Target{Service: "api", StartedAt: testNow}}

	p := newTestProcessor(store, evStore, targets, reg, &fakeTimeline{}, Config{})

	const workers, perWorker = 8, 5
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				store.enqueue(collectTask(t, 1, time.Time{}))
				if _, err := p.RunOnce(context.Background()); err != nil {
					t.Errorf("并发 RunOnce 返回错误: %v", err)
				}
			}
		}()
	}
	wg.Wait()

	// 每条任务都该恰好被处理一次：要么成功，要么失败，不能凭空消失。
	got := len(store.completed) + len(store.failed)
	if got != workers*perWorker {
		t.Errorf("处理任务总数 = %d（成功 %d + 失败 %d），期望 %d —— 有任务被漏掉或重复处理",
			got, len(store.completed), len(store.failed), workers*perWorker)
	}
}
