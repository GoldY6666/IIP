package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yu/iip/internal/evidence"
)

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
