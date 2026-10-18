package notify

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeSink 记录收到的事件；可配置报错与延迟，用于验证派发语义。
type fakeSink struct {
	name  string
	err   error
	delay time.Duration

	mu      sync.Mutex
	events  []Event
	started chan struct{} // 收到第一条时关闭（测试同步用）
	once    sync.Once
}

func newFakeSink(name string) *fakeSink {
	return &fakeSink{name: name, started: make(chan struct{})}
}

func (f *fakeSink) Name() string { return f.name }

func (f *fakeSink) Send(_ context.Context, ev *Event) error {
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	f.mu.Lock()
	f.events = append(f.events, *ev)
	f.mu.Unlock()
	f.once.Do(func() { close(f.started) })
	return f.err
}

func (f *fakeSink) received() []Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Event, len(f.events))
	copy(out, f.events)
	return out
}

// fakeCounts 是 CountsProvider 的假实现；记录调用次数以验「每条通知只查一次」。
type fakeCounts struct {
	alert    int64
	evidence int64
	err      error

	calls atomic.Int32
}

func (f *fakeCounts) IncidentCounts(_ context.Context, _ int64) (int64, int64, error) {
	f.calls.Add(1)
	return f.alert, f.evidence, f.err
}

func testRef() IncidentRef {
	return IncidentRef{
		ID:        42,
		Title:     "HighLatency",
		Severity:  "critical",
		Service:   "checkout",
		Status:    "open",
		StartedAt: time.Date(2026, 10, 17, 19, 30, 0, 0, time.UTC),
	}
}

// waitFor 带超时地等一个信号，失败时直接 t.Fatal（不让用例挂死）。
func waitFor(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatalf("等待超时: %s", what)
	}
}

// TestServiceDispatchesAsync 钉住「Notify 不等待发送」：
// sink 第一条故意阻塞，Notify 必须立即返回。
//
// 这条是 T6.1 的立身之本——同步发送等于把 Slack 的故障接到 webhook
// 请求路径上（R7 的教训：主路径等外部系统，吞吐从 1785 条/s 崩到 ~6 条/s）。
func TestServiceDispatchesAsync(t *testing.T) {
	gate := make(chan struct{})
	sink := &gateSink{gate: gate, started: make(chan struct{})}
	svc := New([]Sink{sink}, nil, nil)
	defer func() { _ = svc.Close(context.Background()) }()

	done := make(chan bool, 1)
	go func() { done <- svc.NotifyIncidentCreated(testRef()) }()

	select {
	case ok := <-done:
		if !ok {
			t.Fatal("Notify 应入队成功")
		}
	case <-time.After(time.Second):
		t.Fatal("Notify 阻塞超过 1s——派发不是异步的")
	}

	// 放行 sink，让 Close 能排空。
	close(gate)
}

// gateSink 收到第一条后阻塞在 gate 上（测试异步性用）。
type gateSink struct {
	gate    chan struct{}
	started chan struct{}
	once    sync.Once
}

func (g *gateSink) Name() string { return "gate" }
func (g *gateSink) Send(_ context.Context, _ *Event) error {
	g.once.Do(func() { close(g.started) })
	<-g.gate
	return nil
}

// TestServiceFillsCounts 钉住「计数由派发线程补，且每条通知只查一次」。
func TestServiceFillsCounts(t *testing.T) {
	sink := newFakeSink("slack")
	counts := &fakeCounts{alert: 3, evidence: 5}
	svc := New([]Sink{sink}, counts, nil)

	if !svc.NotifyIncidentCreated(testRef()) {
		t.Fatal("入队失败")
	}
	waitFor(t, sink.started, "sink 收到通知")

	got := sink.received()
	if len(got) != 1 {
		t.Fatalf("应收到 1 条，实际 %d 条", len(got))
	}
	if got[0].AlertCount != 3 || got[0].EvidenceCount != 5 {
		t.Errorf("计数应被填入 3/5，实际 %d/%d", got[0].AlertCount, got[0].EvidenceCount)
	}
	if n := counts.calls.Load(); n != 1 {
		t.Errorf("计数查询应恰好 1 次，实际 %d 次", n)
	}
	if err := svc.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestServiceCountsFailureSkipsSending 钉住 ADR-024 的取舍：
// 计数查不到就**不发**——写着「证据 0 条」的错消息比漏发更误导 on-call。
func TestServiceCountsFailureSkipsSending(t *testing.T) {
	sink := newFakeSink("slack")
	counts := &fakeCounts{err: errors.New("db down")}
	svc := New([]Sink{sink}, counts, nil)
	defer func() { _ = svc.Close(context.Background()) }()

	svc.NotifyIncidentCreated(testRef())

	// 没有 started 信号可等（本就不该发），给派发线程一个明确的时间窗。
	time.Sleep(100 * time.Millisecond)
	if got := sink.received(); len(got) != 0 {
		t.Errorf("计数失败时不应发送任何通知，实际收到 %d 条", len(got))
	}
}

// TestServiceDropsWhenFullAndLogs 钉住队列满时的丢弃语义。
//
// 同步手法：先让 worker 阻塞在第一条上（等 started 信号），
// 再逐个入队——此时队列空、worker 占住，装满 256 后第 257 条必被丢弃。
// 不先等 started 的话，「worker 是否已取走第一条」会成为随机变量。
func TestServiceDropsWhenFullAndLogs(t *testing.T) {
	gate := make(chan struct{})
	sink := &gateSink{gate: gate, started: make(chan struct{})}
	svc := New([]Sink{sink}, nil, nil)

	// 第一条：worker 取走后阻塞在 gate 上。
	if !svc.NotifyIncidentCreated(testRef()) {
		t.Fatal("第一条应入队")
	}
	waitFor(t, sink.started, "worker 取走第一条")

	// 队列容量 256：全部应入队成功。
	for i := 0; i < defaultQueueSize; i++ {
		if !svc.NotifyIncidentCreated(testRef()) {
			t.Fatalf("第 %d 条不应被丢弃（队列未满）", i+2)
		}
	}
	// 第 257 条起：满，丢弃。
	for i := 0; i < 3; i++ {
		if svc.NotifyIncidentCreated(testRef()) {
			t.Error("队列已满，应丢弃并返回 false")
		}
	}
	close(gate)
	if err := svc.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestServiceCloseDrains 钉住关闭时排空残留（优雅退出不丢已入队的通知）。
func TestServiceCloseDrains(t *testing.T) {
	const total = 5
	sink := newFakeSink("slack")
	sink.delay = 10 * time.Millisecond // 每条都慢一点，保证 Close 时队列非空
	svc := New([]Sink{sink}, &fakeCounts{}, nil)

	for i := 0; i < total; i++ {
		if !svc.NotifyIncidentCreated(testRef()) {
			t.Fatalf("第 %d 条入队失败", i+1)
		}
	}
	if err := svc.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := sink.received(); len(got) != total {
		t.Errorf("Close 应排空全部 %d 条，实际收到 %d 条", total, len(got))
	}
}

// TestServiceConcurrentCloseAndNotify 钉住「Close 与 Notify 并发时，
// Close 返回后入队必失败」。
//
// 背景：这是实测抓到的真缺陷——旧实现用 select 同时等 queue 与 stopped，
// worker 排空期间两者同时就绪，Go 随机选分支，约一半概率把事件塞进
// 已无人消费的队列并返回 true（10 次跑 5 次红）。断言的是
// 「Close 返回 ⇒ 通知必被拒」，不是某次调度的巧合。
func TestServiceConcurrentCloseAndNotify(t *testing.T) {
	sink := newFakeSink("slack")
	svc := New([]Sink{sink}, nil, nil)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				svc.NotifyIncidentCreated(testRef()) // 结果不关心，只要不 panic
			}
		}()
	}

	// 与入队并发地关闭。
	if err := svc.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	wg.Wait()

	// Close 已返回：此后任何入队都必须失败（事件已无人消费）。
	for i := 0; i < 20; i++ {
		if svc.NotifyIncidentCreated(testRef()) {
			t.Fatal("Close 返回后入队仍成功——通知会被静默丢弃")
		}
	}
}

// TestServiceNotifyAfterClose 钉住关闭后入队不 panic（关 channel 上发送
// 会直接 panic，这是实现细节也是主要坑）。
func TestServiceNotifyAfterClose(t *testing.T) {
	svc := New([]Sink{newFakeSink("slack")}, nil, nil)
	if err := svc.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if svc.NotifyIncidentCreated(testRef()) {
		t.Error("关闭后入队应返回 false")
	}
	// 幂等：再关一次不应 panic。
	if err := svc.Close(context.Background()); err != nil {
		t.Fatalf("二次 Close: %v", err)
	}
}

// TestServiceOneSinkFailureDoesNotBlockOthers 钉住「一个出口挂不影响另一个」。
func TestServiceOneSinkFailureDoesNotBlockOthers(t *testing.T) {
	bad := newFakeSink("bad")
	bad.err = errors.New("slack 500")
	good := newFakeSink("generic")
	svc := New([]Sink{bad, good}, &fakeCounts{alert: 1, evidence: 2}, nil)
	defer func() { _ = svc.Close(context.Background()) }()

	svc.NotifyStatusChanged(testRef(), "open", "acknowledged", "zhangsan")
	waitFor(t, good.started, "正常出口收到通知")

	got := good.received()
	if len(got) != 1 {
		t.Fatalf("正常出口应收到 1 条，实际 %d 条", len(got))
	}
	ev := got[0]
	if ev.Kind != KindStatusChanged || ev.FromStatus != "open" || ev.ToStatus != "acknowledged" || ev.Actor != "zhangsan" {
		t.Errorf("状态变更字段未原样传递: %+v", ev)
	}
	if ev.AlertCount != 1 || ev.EvidenceCount != 2 {
		t.Errorf("计数应填入 1/2，实际 %d/%d", ev.AlertCount, ev.EvidenceCount)
	}
}

// TestNilServiceSafe 钉住 nil *Service 全方法可调用：
// wiring 层「没配 webhook 就传 nil」因此不需要分支，两个消费包同理。
func TestNilServiceSafe(t *testing.T) {
	var svc *Service
	if svc.NotifyIncidentCreated(testRef()) {
		t.Error("nil Service 的 Notify 应返回 false")
	}
	if svc.NotifyStatusChanged(testRef(), "open", "resolved", "") {
		t.Error("nil Service 的 Notify 应返回 false")
	}
	if err := svc.Close(context.Background()); err != nil {
		t.Errorf("nil Service 的 Close 应返回 nil，实际 %v", err)
	}
}

// TestServiceNilCountsStillSends 钉住「没接计数源也能发」：
// 计数字段留零，消息形态降级（T6.2 的通用 webhook 可能不需要计数）。
func TestServiceNilCountsStillSends(t *testing.T) {
	sink := newFakeSink("slack")
	svc := New([]Sink{sink}, nil, nil)
	defer func() { _ = svc.Close(context.Background()) }()

	svc.NotifyIncidentCreated(testRef())
	waitFor(t, sink.started, "sink 收到通知")
	if got := sink.received(); len(got) != 1 {
		t.Fatalf("应收到 1 条，实际 %d 条", len(got))
	}
}
