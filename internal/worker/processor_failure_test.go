package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/yu/iip/internal/evidence"
	"github.com/yu/iip/internal/incident"
	"github.com/yu/iip/internal/task"
)

// TestRunOnce_AppliesBudget 证据条数超过上限时被截断。
func TestRunOnce_AppliesBudget(t *testing.T) {
	evs := make([]evidence.Evidence, 0, 60)
	for i := 0; i < 60; i++ {
		evs = append(evs, metricEv("loki", fmt.Sprintf("line-%d", i), testNow.Add(-time.Duration(i)*time.Second)))
	}
	reg := evidence.NewRegistry()
	reg.MustRegister(&fakeCollector{src: "loki", evs: evs})

	tl := &fakeTimeline{}
	store := newFakeStore()
	evStore := &fakeEvidenceStore{}
	targets := &fakeTargets{target: Target{Service: "api", StartedAt: testNow}}
	store.enqueue(collectTask(t, 1, time.Time{}))

	p := newTestProcessor(store, evStore, targets, reg, tl, Config{MaxPerIncident: 50})
	if _, err := p.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce 返回错误: %v", err)
	}
	if len(evStore.saved) != 50 {
		t.Errorf("落库 %d 条，期望 50（单事件上限）", len(evStore.saved))
	}
	// 截断必须在留痕里可见 —— 只保留 50 条却不说成「一共 50 条」，
	// 用户会以为没有更多证据。
	if len(tl.entries) != 1 {
		t.Fatalf("留痕 %d 条，期望 1", len(tl.entries))
	}
	md := tl.entries[0].Metadata
	if md["collected"] != "60" || md["kept"] != "50" || md["truncated"] != "true" {
		t.Errorf("留痕 metadata = %v，期望 collected=60 kept=50 truncated=true", md)
	}
}

// TestRunOnce_WritesEvidenceTimelineEntry 留痕必须带窗口与来源分布。
func TestRunOnce_WritesEvidenceTimelineEntry(t *testing.T) {
	reg := evidence.NewRegistry()
	reg.MustRegister(&fakeCollector{src: "loki", evs: []evidence.Evidence{
		metricEv("loki", "a", testNow.Add(-time.Minute)),
		metricEv("loki", "b", testNow.Add(-2*time.Minute)),
	}})
	reg.MustRegister(&fakeCollector{src: "prometheus", evs: []evidence.Evidence{
		metricEv("prometheus", "c", testNow.Add(-3*time.Minute)),
	}})

	tl := &fakeTimeline{}
	store := newFakeStore()
	targets := &fakeTargets{target: Target{Service: "api", StartedAt: testNow.Add(-time.Minute)}}
	store.enqueue(collectTask(t, 5, time.Time{}))

	p := newTestProcessor(store, &fakeEvidenceStore{}, targets, reg, tl, Config{})
	if _, err := p.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce 返回错误: %v", err)
	}
	if len(tl.entries) != 1 {
		t.Fatalf("留痕 %d 条，期望 1", len(tl.entries))
	}
	e := tl.entries[0]
	if e.EventType != incident.EventEvidenceFound {
		t.Errorf("event_type = %q，期望 %q", e.EventType, incident.EventEvidenceFound)
	}
	if e.Metadata["sources"] != "loki=2,prometheus=1" {
		t.Errorf("sources = %q，期望 %q（字典序）", e.Metadata["sources"], "loki=2,prometheus=1")
	}
	if e.Metadata["window"] == "" {
		t.Error("留痕缺 window —— 用户无从知道这批证据覆盖哪段时间")
	}
}

// TestRunOnce_TimelineFailureDoesNotFailTask 留痕失败不能判任务失败。
//
// 证据已经落库了，为一条给人看的记录把整轮判成失败，
// 用户看到的是「没有证据」，而证据就在库里。
func TestRunOnce_TimelineFailureDoesNotFailTask(t *testing.T) {
	reg := evidence.NewRegistry()
	reg.MustRegister(&fakeCollector{src: "loki", evs: []evidence.Evidence{
		metricEv("loki", "a", testNow.Add(-time.Minute)),
	}})

	store := newFakeStore()
	evStore := &fakeEvidenceStore{}
	targets := &fakeTargets{target: Target{Service: "api", StartedAt: testNow}}
	store.enqueue(collectTask(t, 1, time.Time{}))

	p := newTestProcessor(store, evStore, targets, reg,
		&fakeTimeline{err: errors.New("timeline 表被锁")}, Config{})
	if _, err := p.RunOnce(context.Background()); err != nil {
		t.Fatalf("留痕失败却让任务失败: %v", err)
	}
	if len(store.completed) != 1 {
		t.Errorf("标记成功 %d 次，期望 1", len(store.completed))
	}
	if len(evStore.saved) != 1 {
		t.Errorf("落库 %d 条，期望 1 —— 证据必须已经落库", len(evStore.saved))
	}
}

// TestRunOnce_IncidentNotFoundIsRetryable 事件读不到时判失败。
//
// 重试**有意义**：事件行可能刚由另一个事务写入，collector 读到的快照还没包含它。
// 但不能推进游标。
func TestRunOnce_IncidentNotFoundIsRetryable(t *testing.T) {
	reg := evidence.NewRegistry()
	reg.MustRegister(&fakeCollector{src: "loki", evs: []evidence.Evidence{
		metricEv("loki", "a", testNow.Add(-time.Minute)),
	}})

	store := newFakeStore()
	targets := &fakeTargets{err: errors.New("事件 1 不存在")}
	store.enqueue(collectTask(t, 1, time.Time{}))

	p := newTestProcessor(store, &fakeEvidenceStore{}, targets, reg, nil, Config{})
	if _, err := p.RunOnce(context.Background()); err == nil {
		t.Fatal("读不到事件时应返回错误")
	}
	if len(store.failed) != 1 {
		t.Errorf("失败 %d 次，期望 1", len(store.failed))
	}
	if len(store.cursorSet) != 0 {
		t.Errorf("读不到事件却写入了游标 %v", store.cursorSet)
	}
}

// TestRunOnce_UnknownKindGoesToDeadLetter 不认识的 kind 直接判失败。
//
// 静默跳过会让任务永远停在 in_flight 直到超时回收，循环往复。
func TestRunOnce_UnknownKindGoesToDeadLetter(t *testing.T) {
	store := newFakeStore()
	store.enqueue(&task.Task{
		Kind:    task.Kind("rebuild_index"),
		Payload: json.RawMessage(`{"x":1}`),
	})

	reg := evidence.NewRegistry()
	reg.MustRegister(&fakeCollector{src: "loki"})
	p := newTestProcessor(store, &fakeEvidenceStore{},
		&fakeTargets{target: Target{Service: "api", StartedAt: testNow}}, reg, nil, Config{})

	if _, err := p.RunOnce(context.Background()); err == nil {
		t.Fatal("不认识的任务类型应返回错误")
	}
	if len(store.failed) != 1 {
		t.Errorf("失败 %d 次，期望 1", len(store.failed))
	}
}
