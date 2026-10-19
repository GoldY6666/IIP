package worker

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/yu/iip/internal/evidence"
	"github.com/yu/iip/internal/incident"
	"github.com/yu/iip/internal/task"
)

// fakeCollector 是一个可控的采集器。
type fakeCollector struct {
	src  string
	evs  []evidence.Evidence
	err  error
	seen []evidence.Input // 记录收到的输入，供断言窗口/服务名

	// delay 让 Collect 真的耗时，用于验证采集耗时真的进了指标（T6.3）。
	delay time.Duration
}

func (f *fakeCollector) Source() evidence.Source { return f.src }

func (f *fakeCollector) Collect(_ context.Context, in evidence.Input) ([]evidence.Evidence, error) {
	f.seen = append(f.seen, in)
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	return f.evs, f.err
}

// fakeStore 是 task.Store 的内存实现。
//
// 用互斥锁而非裸 map：Processor 的契约明说「同一实例会被多个 goroutine
// 同时调用」，替身不满足这个契约的话，并发用例测的是替身的 bug。
type fakeStore struct {
	mu      sync.Mutex
	queue   []*task.Task
	seq     int64
	claimEr error

	completed []int64             // 记录被标记成功的任务 id
	cursorSet map[int64]time.Time // 记录写入的游标
	failed    []int64

	// reclaimed 是 ReclaimStale 的返回值，用于验证回收条数被累加进指标（T6.3）。
	// 默认 0，故不影响既有用例。
	reclaimed int64
}

func newFakeStore() *fakeStore {
	return &fakeStore{cursorSet: map[int64]time.Time{}}
}

// enqueue 放一个任务进队列。
func (f *fakeStore) enqueue(t *task.Task) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	t.ID = f.seq
	f.queue = append(f.queue, t)
}

func (f *fakeStore) Enqueue(_ context.Context, kind task.Kind, payload json.RawMessage) (task.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	tk := task.Task{ID: f.seq, Kind: kind, Payload: payload, Status: task.StatusDone}
	return tk, nil
}

func (f *fakeStore) EnqueueIfAbsent(ctx context.Context, kind task.Kind, payload json.RawMessage) (task.Task, error) {
	return f.Enqueue(ctx, kind, payload)
}

func (f *fakeStore) Claim(_ context.Context, batch int, claimedBy string) ([]task.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.claimEr != nil {
		return nil, f.claimEr
	}
	if batch <= 0 || len(f.queue) == 0 {
		return nil, nil
	}
	n := batch
	if n > len(f.queue) {
		n = len(f.queue)
	}
	out := make([]task.Task, 0, n)
	for _, tk := range f.queue[:n] {
		tk.Status = task.StatusInFlight
		tk.Attempts++
		tk.ClaimedBy = claimedBy
		out = append(out, *tk)
	}
	f.queue = f.queue[n:]
	return out, nil
}

func (f *fakeStore) Complete(_ context.Context, id int64, windowEnd time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.completed = append(f.completed, id)
	if !windowEnd.IsZero() {
		f.cursorSet[id] = windowEnd
	}
	return nil
}

func (f *fakeStore) Fail(_ context.Context, id int64, _ time.Duration, maxAttempts int, lastErr string) (task.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failed = append(f.failed, id)
	status := task.StatusPending
	if maxAttempts <= 1 {
		status = task.StatusDead
	}
	return task.Task{ID: id, Status: status, Attempts: maxAttempts, LastError: lastErr}, nil
}

func (f *fakeStore) ReclaimStale(context.Context, time.Duration) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reclaimed, nil
}

func (f *fakeStore) LastWindowEnd(context.Context, int64) (time.Time, error) { return time.Time{}, nil }

// fakeEvidenceStore 记录落库的证据。
type fakeEvidenceStore struct {
	mu      sync.Mutex
	saved   []evidence.Evidence
	saveErr error
	calls   int
}

func (f *fakeEvidenceStore) Save(_ context.Context, _ int64, list []evidence.Evidence) ([]evidence.StoredEvidence, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.saveErr != nil {
		return nil, f.saveErr
	}
	f.saved = append(f.saved, list...)
	out := make([]evidence.StoredEvidence, 0, len(list))
	for i, e := range list {
		out = append(out, evidence.StoredEvidence{ID: int64(i + 1), Type: e.Type, Source: e.Source})
	}
	return out, nil
}

func (f *fakeEvidenceStore) ListByIncident(context.Context, int64, evidence.Type, int) (evidence.EvidenceList, error) {
	return evidence.EvidenceList{}, nil
}

func (f *fakeEvidenceStore) CountByType(context.Context, int64) (map[evidence.Type]int, error) {
	return nil, nil
}

// fakeTargets 是 IncidentReader 的内存实现。
type fakeTargets struct {
	mu      sync.Mutex
	target  Target
	err     error
	service string
}

func (f *fakeTargets) Target(_ context.Context, incidentID int64) (Target, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return Target{}, f.err
	}
	t := f.target
	if t.Service == "" {
		t.Service = f.service
	}
	return t, nil
}

// fakeTimeline 记录留痕。
type fakeTimeline struct {
	mu      sync.Mutex
	entries []incident.Entry
	err     error
}

func (f *fakeTimeline) AppendTimelineEntry(_ context.Context, e incident.Entry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.entries = append(f.entries, e)
	return nil
}

// ---- 辅助 ----

var testNow = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func metricEv(source, value string, ts time.Time) evidence.Evidence {
	return evidence.Evidence{
		Type:      evidence.TypeMetric,
		Source:    source,
		Query:     "up",
		Value:     value,
		Timestamp: ts,
	}
}

func collectTask(t *testing.T, incidentID int64, cursor time.Time) *task.Task {
	t.Helper()
	payload, err := task.NewCollectEvidencePayload(incidentID, cursor)
	if err != nil {
		t.Fatalf("构造任务参数失败: %v", err)
	}
	return &task.Task{Kind: task.KindCollectEvidence, Payload: payload}
}

func newTestProcessor(
	store *fakeStore, evStore *fakeEvidenceStore, targets *fakeTargets,
	reg *evidence.Registry, tl TimelineWriter, cfg Config,
) *Processor {
	p := NewProcessor(store, evStore, targets, reg, tl, cfg, discardLogger())
	p.SetClock(func() time.Time { return testNow })
	return p
}

// ---- 用例 ----
