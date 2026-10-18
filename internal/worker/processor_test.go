package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/yu/iip/internal/evidence"
	"github.com/yu/iip/internal/incident"
	"github.com/yu/iip/internal/task"
)

// 本文件用**内存替身**测执行层的编排规则，不碰数据库。
//
// 手册 §七 要求「三采集器中一个失败仍落库另两个」用假 Collector 注入 ——
// 那正是本文件的核心。真库集成（事件创建 → 任务入队 → worker 采到 →
// 证据落库 → API 读出）在 internal/repository 的集成用例里测。

// ---- 替身 ----

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

// TestRunOnce_OneCollectorFailsOthersStillSaved 是手册 §七 的核心断言：
// 「三采集器中一个失败仍落库另两个」。
//
// 只断言「有证据落库」是不够的 —— 一个「全部失败就什么都不做」的实现
// 也会让证据数为 0，而那恰好是需要被抓住的行为。必须同时断言
// 任务被标记为**成功**（而不是失败重试）。
func TestRunOnce_OneCollectorFailsOthersStillSaved(t *testing.T) {
	good := &fakeCollector{src: "loki", evs: []evidence.Evidence{
		metricEv("loki", "a", testNow.Add(-time.Minute)),
		metricEv("loki", "b", testNow.Add(-2*time.Minute)),
	}}
	bad := &fakeCollector{src: "prometheus", err: errors.New("连接被拒绝")}

	reg := evidence.NewRegistry()
	reg.MustRegister(good)
	reg.MustRegister(bad)

	store := newFakeStore()
	evStore := &fakeEvidenceStore{}
	targets := &fakeTargets{target: Target{Service: "api", StartedAt: testNow.Add(-20 * time.Minute)}}
	store.enqueue(collectTask(t, 7, time.Time{}))

	p := newTestProcessor(store, evStore, targets, reg, nil, Config{})
	n, err := p.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce 返回错误 = %v，期望 nil（单个采集器失败不该让任务失败）", err)
	}
	if n != 1 {
		t.Fatalf("处理任务数 = %d，期望 1", n)
	}
	if len(evStore.saved) != 2 {
		t.Errorf("落库证据 %d 条，期望 2（失败的采集器不影响其余）", len(evStore.saved))
	}
	if len(store.completed) != 1 {
		t.Errorf("标记成功 %d 次，期望 1；失败数 = %d —— 单个采集器失败不该让任务重试",
			len(store.completed), len(store.failed))
	}
}

// TestRunOnce_EmptyResultIsSuccess 「采到了但没内容」必须是成功。
//
// 判它失败会让任务重试三次再进死信，而重试一百次结果也一样 ——
// 数据源确实没有异常，这是**正常结论**。
func TestRunOnce_EmptyResultIsSuccess(t *testing.T) {
	reg := evidence.NewRegistry()
	reg.MustRegister(&fakeCollector{src: "loki"}) // 无 err、无证据

	store := newFakeStore()
	evStore := &fakeEvidenceStore{}
	targets := &fakeTargets{target: Target{Service: "api", StartedAt: testNow.Add(-time.Minute)}}
	store.enqueue(collectTask(t, 1, time.Time{}))

	p := newTestProcessor(store, evStore, targets, reg, nil, Config{})
	if _, err := p.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce 返回错误 = %v，期望 nil（采到 0 条是正常结果）", err)
	}
	if len(store.failed) != 0 {
		t.Errorf("失败 %d 次，期望 0 —— 采到 0 条证据不该触发重试", len(store.failed))
	}
	// 但**游标必须推进**：否则空窗口会被下一轮重新采一遍，
	// 每轮白白消耗一次 attempts 直到进死信。
	if len(store.cursorSet) != 1 {
		t.Errorf("写入游标 %d 次，期望 1 —— 采到 0 条时游标也必须推进", len(store.cursorSet))
	}
}

// TestRunOnce_AllCollectorsFailIsFailure 与上一条互为反面。
//
// 「什么都没采到」与「采到了 0 条」是两回事：前者该重试（数据源可能恢复），
// 后者不该。不区分的话，两种情形下页面都显示「没有证据」，运维无法判断
// 该去查数据源还是查服务。
func TestRunOnce_AllCollectorsFailIsFailure(t *testing.T) {
	reg := evidence.NewRegistry()
	reg.MustRegister(&fakeCollector{src: "loki", err: errors.New("超时")})
	reg.MustRegister(&fakeCollector{src: "prometheus", err: errors.New("500")})

	store := newFakeStore()
	evStore := &fakeEvidenceStore{}
	targets := &fakeTargets{target: Target{Service: "api", StartedAt: testNow}}
	store.enqueue(collectTask(t, 1, time.Time{}))

	p := newTestProcessor(store, evStore, targets, reg, nil, Config{})
	if _, err := p.RunOnce(context.Background()); err == nil {
		t.Fatal("全部采集器失败时 RunOnce 应返回错误")
	}
	if len(store.failed) != 1 {
		t.Errorf("失败 %d 次，期望 1", len(store.failed))
	}
	if len(store.completed) != 0 {
		t.Errorf("标记成功 %d 次，期望 0", len(store.completed))
	}
	// 失败时**不能**推进游标：那会把没采到的区间标记成已采。
	if len(store.cursorSet) != 0 {
		t.Errorf("失败时仍写入了游标 %v —— 会让未采集的区间被永久跳过", store.cursorSet)
	}
}

// TestRunOnce_NoCollectorsRegisteredIsNotFailure 一个采集器都没注册时
// **不判失败**。
//
// 那是配置问题（没配地址），不是采集失败。若判失败，任务会重试三次进死信，
// 而重试一万次也还是零个采集器 —— 队列被无意义的失败填满。
func TestRunOnce_NoCollectorsRegisteredIsNotFailure(t *testing.T) {
	store := newFakeStore()
	targets := &fakeTargets{target: Target{Service: "api", StartedAt: testNow}}
	store.enqueue(collectTask(t, 1, time.Time{}))

	p := newTestProcessor(store, &fakeEvidenceStore{}, targets,
		evidence.NewRegistry(), nil, Config{})
	if _, err := p.RunOnce(context.Background()); err != nil {
		t.Fatalf("无采集器时 RunOnce 返回错误 = %v，期望 nil", err)
	}
	if len(store.failed) != 0 {
		t.Errorf("失败 %d 次，期望 0 —— 没有采集器是配置问题，不该触发重试风暴", len(store.failed))
	}
}

// TestRunOnce_CursorAdvancesOnlyAfterSave 游标必须在**落库之后**才推进。
//
// 落库失败却推进游标，会让没存下来的区间被永久跳过 ——
// 表现是「任务都完成了，但证据缺了一段」，且没有任何错误提示。
func TestRunOnce_CursorAdvancesOnlyAfterSave(t *testing.T) {
	reg := evidence.NewRegistry()
	reg.MustRegister(&fakeCollector{src: "loki", evs: []evidence.Evidence{
		metricEv("loki", "x", testNow.Add(-time.Minute)),
	}})

	store := newFakeStore()
	evStore := &fakeEvidenceStore{saveErr: errors.New("连接中断")}
	targets := &fakeTargets{target: Target{Service: "api", StartedAt: testNow}}
	store.enqueue(collectTask(t, 1, time.Time{}))

	p := newTestProcessor(store, evStore, targets, reg, nil, Config{})
	if _, err := p.RunOnce(context.Background()); err == nil {
		t.Fatal("落库失败时 RunOnce 应返回错误")
	}
	if len(store.cursorSet) != 0 {
		t.Errorf("落库失败却写入了游标 %v —— 会跳过没存下来的区间", store.cursorSet)
	}
	if len(store.completed) != 0 {
		t.Errorf("落库失败却标记成功 %d 次", len(store.completed))
	}
}

// TestRunOnce_PassesResolvedWindowToCollectors 采集器收到的窗口必须
// 是 ResolveWindow 推导出的那个，而不是「最近一小时」。
func TestRunOnce_PassesResolvedWindowToCollectors(t *testing.T) {
	col := &fakeCollector{src: "loki", evs: []evidence.Evidence{
		metricEv("loki", "x", testNow.Add(-time.Minute)),
	}}
	reg := evidence.NewRegistry()
	reg.MustRegister(col)

	store := newFakeStore()
	incidentStart := testNow.Add(-30 * time.Minute)
	targets := &fakeTargets{target: Target{Service: "api", StartedAt: incidentStart}}
	store.enqueue(collectTask(t, 42, time.Time{}))

	p := newTestProcessor(store, &fakeEvidenceStore{}, targets, reg, nil, Config{})
	if _, err := p.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce 返回错误: %v", err)
	}
	if len(col.seen) != 1 {
		t.Fatalf("采集器被调用 %d 次，期望 1", len(col.seen))
	}
	got := col.seen[0]
	if got.IncidentID != 42 {
		t.Errorf("IncidentID = %d，期望 42", got.IncidentID)
	}
	if got.Service != "api" {
		t.Errorf("Service = %q，期望 %q", got.Service, "api")
	}
	if want := incidentStart.Add(-time.Hour); !got.Window.Start.Equal(want) {
		t.Errorf("窗口起点 = %v，期望 %v（事故起点前 1h）", got.Window.Start, want)
	}
	if !got.Window.End.Equal(testNow) {
		t.Errorf("窗口终点 = %v，期望 %v", got.Window.End, testNow)
	}
}

// TestRunOnce_UsesCursorFromPayload 任务带的游标必须被采信。
//
// 这是「只采增量」的核心：第二次采集的窗口起点是上次的终点，
// 而不是又回到事故起点 —— 否则同一批指标会落库两份。
func TestRunOnce_UsesCursorFromPayload(t *testing.T) {
	col := &fakeCollector{src: "loki", evs: []evidence.Evidence{
		metricEv("loki", "x", testNow.Add(-time.Minute)),
	}}
	reg := evidence.NewRegistry()
	reg.MustRegister(col)

	cursor := testNow.Add(-3 * time.Minute)
	store := newFakeStore()
	targets := &fakeTargets{target: Target{Service: "api", StartedAt: testNow.Add(-3 * time.Hour)}}
	store.enqueue(collectTask(t, 1, cursor))

	p := newTestProcessor(store, &fakeEvidenceStore{}, targets, reg, nil, Config{})
	if _, err := p.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce 返回错误: %v", err)
	}
	if len(col.seen) != 1 {
		t.Fatalf("采集器被调用 %d 次，期望 1", len(col.seen))
	}
	if !col.seen[0].Window.Start.Equal(cursor) {
		t.Errorf("窗口起点 = %v，期望游标 %v —— 第二次采集必须只覆盖新增区间",
			col.seen[0].Window.Start, cursor)
	}
	if w := col.seen[0].Window.Duration(); w != 3*time.Minute {
		t.Errorf("窗口宽 %v，期望 3m（= 游标到 now）", w)
	}
}

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
