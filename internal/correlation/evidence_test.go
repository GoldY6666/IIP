package correlation

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/yu/iip/internal/alert"
)

// 本文件只测 M4 挂在关联路径上的**入队钩子**。
// 关联本身的评分/决策由 service_test.go 覆盖，这里不重复 ——
// 只验证「入队被调用了几次、指向哪个事件、失败是否影响关联」。
//
// 复用 service_test.go 已有的替身（fakeIncidentStore / fakeConfigStore /
// corrAlert / newCorrService），不另造一套：两套替身会让同一个契约
// 在两处漂移，而漂移的一处永远不会被测到。

// recordingEnqueuer 记录入队调用。
type recordingEnqueuer struct {
	mu    sync.Mutex
	calls []int64
	err   error
}

func (e *recordingEnqueuer) EnqueueEvidence(_ context.Context, incidentID int64) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, incidentID)
	return e.err
}

func (e *recordingEnqueuer) seen() []int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]int64, len(e.calls))
	copy(out, e.calls)
	return out
}

// 编译期断言：钩子实现必须满足接口。
var _ EvidenceEnqueuer = (*recordingEnqueuer)(nil)

// TestEvidenceTasks_EnqueuesOnNewIncident 事件创建后必须入队。
//
// 这是 DoD 第 4 条（「2 分钟内≥5 条证据」）的起点：入队发生在 webhook
// 请求内，是最早可能的时机。
func TestEvidenceTasks_EnqueuesOnNewIncident(t *testing.T) {
	store := &fakeIncidentStore{}
	cfgStore := &fakeConfigStore{cfg: DefaultConfig()}
	enq := &recordingEnqueuer{}

	svc := newCorrService(store, cfgStore, &Topology{}).WithEvidenceTasks(enq)

	alerts := []alert.StoredAlert{
		corrAlert(1, "checkout", "LatencyHigh", alert.StatusFiring, "pod-a", corrStart, corrEnd, false),
	}
	if err := svc.CorrelateAlerts(context.Background(), alerts); err != nil {
		t.Fatalf("关联返回错误: %v", err)
	}

	got := enq.seen()
	if len(got) != 1 {
		t.Fatalf("入队 %d 次，期望 1 次（事件创建后必须入队）", len(got))
	}
	if got[0] != store.created[0].ID {
		t.Errorf("入队的 incident_id = %d，期望 %d（必须是刚建的那个事件）",
			got[0], store.created[0].ID)
	}
}

// TestEvidenceTasks_EnqueuesOnMerge 合入已有事件后也必须入队。
//
// 只断言创建会漏掉 MERGE —— 而合入恰恰是「范围扩大、该重新看指标」的时刻。
// 漏掉它的后果是证据永远停在事故发生的那一刻。
func TestEvidenceTasks_EnqueuesOnMerge(t *testing.T) {
	store := &fakeIncidentStore{
		candidates: []CandidateAlert{candidateOnCheckout(77, "pod-old")},
	}
	// co/total = 1/1 把分数顶进 MERGE 档 —— 与 service_test.go 的
	// TestCorrelateAlerts_MergeOnHighScore 用同一组数据口径。
	// 不给这两个值时分数落在 REVIEW 档，测的就不是 MERGE 路径了。
	cfgStore := &fakeConfigStore{cfg: DefaultConfig(), co: 1, total: 1}
	enq := &recordingEnqueuer{}

	svc := newCorrService(store, cfgStore, &Topology{}).WithEvidenceTasks(enq)

	alerts := []alert.StoredAlert{
		corrAlert(2, "checkout", "LatencyHigh", alert.StatusFiring, "pod-new", corrStart, corrEnd, false),
	}
	if err := svc.CorrelateAlerts(context.Background(), alerts); err != nil {
		t.Fatalf("关联返回错误: %v", err)
	}
	if len(store.created) != 0 {
		t.Fatalf("应合入既有事件却新建了 %d 个 —— 测试数据没进 MERGE 档", len(store.created))
	}

	got := enq.seen()
	if len(got) != 1 {
		t.Fatalf("入队 %d 次，期望 1 次（合入后也必须入队）", len(got))
	}
	if got[0] != 77 {
		t.Errorf("入队的 incident_id = %d，期望 77（应入队到被合入的那个事件）", got[0])
	}
}

// TestEvidenceTasks_FailureDoesNotBreakCorrelation 入队失败不能让关联失败。
//
// 关联在 webhook 请求内，失败会让 Alertmanager 重投整批告警 → 放大成风暴。
// 而入队失败的代价只是「这次合入暂时没有证据」，下一轮会补上。
func TestEvidenceTasks_FailureDoesNotBreakCorrelation(t *testing.T) {
	store := &fakeIncidentStore{}
	cfgStore := &fakeConfigStore{cfg: DefaultConfig()}
	enq := &recordingEnqueuer{err: errors.New("tasks 表被锁")}

	svc := newCorrService(store, cfgStore, &Topology{}).WithEvidenceTasks(enq)

	alerts := []alert.StoredAlert{
		corrAlert(1, "checkout", "LatencyHigh", alert.StatusFiring, "pod-a", corrStart, corrEnd, false),
	}
	if err := svc.CorrelateAlerts(context.Background(), alerts); err != nil {
		t.Fatalf("入队失败却让关联失败: %v", err)
	}
	if len(enq.seen()) != 1 {
		t.Errorf("入队被调用 %d 次，期望 1（失败也要尝试过）", len(enq.seen()))
	}
	// 更重要：事件本身必须已经建好。
	if len(store.created) != 1 {
		t.Errorf("事件创建 %d 次，期望 1 —— 入队失败不能回滚事件创建", len(store.created))
	}
}

// TestEvidenceTasks_NilHookIsNoop 未装配时关联行为与 M2 完全一致。
//
// 这条保证 M2 的全部单测不需要改一行，也让「可选钩子」这个约定可验证。
func TestEvidenceTasks_NilHookIsNoop(t *testing.T) {
	store := &fakeIncidentStore{}
	cfgStore := &fakeConfigStore{cfg: DefaultConfig()}
	// 刻意不调 WithEvidenceTasks。
	svc := newCorrService(store, cfgStore, &Topology{})

	alerts := []alert.StoredAlert{
		corrAlert(1, "checkout", "LatencyHigh", alert.StatusFiring, "pod-a", corrStart, corrEnd, false),
	}
	if err := svc.CorrelateAlerts(context.Background(), alerts); err != nil {
		t.Fatalf("未装配钩子时关联失败: %v", err)
	}
	if len(store.created) != 1 {
		t.Errorf("事件创建 %d 次，期望 1", len(store.created))
	}
}

// TestEvidenceTasks_EnqueuesPerAlertInBatch 批次内每条告警各入队一次，
// 且指向同一个事件。
//
// 断言「指向同一个事件」比只数次数重要：若第二次合入时错误地新建了事件，
// 计数仍可能是 1，但事故就被拆成了两个 —— 而这正是 M2 要避免的。
//
// 队列不会因此膨胀：task 侧的 EnqueueIfAbsent 对同一事件的未完成任务幂等。
func TestEvidenceTasks_EnqueuesPerAlertInBatch(t *testing.T) {
	store := &fakeIncidentStore{}
	cfgStore := &fakeConfigStore{cfg: DefaultConfig()}
	enq := &recordingEnqueuer{}

	svc := newCorrService(store, cfgStore, &Topology{}).WithEvidenceTasks(enq)

	alerts := []alert.StoredAlert{
		corrAlert(1, "checkout", "LatencyHigh", alert.StatusFiring, "pod-a", corrStart, corrEnd, false),
		corrAlert(2, "checkout", "LatencyHigh", alert.StatusFiring, "pod-b", corrStart, corrEnd, false),
		corrAlert(3, "checkout", "LatencyHigh", alert.StatusFiring, "pod-c", corrStart, corrEnd, false),
	}
	if err := svc.CorrelateAlerts(context.Background(), alerts); err != nil {
		t.Fatalf("关联返回错误: %v", err)
	}

	seen := enq.seen()
	if len(seen) != 3 {
		t.Fatalf("入队 %d 次，期望 3（创建 1 + 合入 2）", len(seen))
	}
	if len(store.created) != 1 {
		t.Errorf("事件创建 %d 次，期望 1 —— 三条同源告警应聚成一个事件", len(store.created))
	}
	for i, id := range seen {
		if id != store.created[0].ID {
			t.Errorf("第 %d 次入队指向事件 %d，期望 %d", i, id, store.created[0].ID)
		}
	}
}

// TestEvidenceTasks_ResolvedAlertDoesNotEnqueue resolved 告警不建事件也不入队。
//
// 与 M2 的既有行为一致（resolved 的生命周期归状态机，不参与关联）。
func TestEvidenceTasks_ResolvedAlertDoesNotEnqueue(t *testing.T) {
	store := &fakeIncidentStore{}
	cfgStore := &fakeConfigStore{cfg: DefaultConfig()}
	enq := &recordingEnqueuer{}

	svc := newCorrService(store, cfgStore, &Topology{}).WithEvidenceTasks(enq)

	alerts := []alert.StoredAlert{
		corrAlert(1, "checkout", "LatencyHigh", alert.StatusResolved, "pod-a", corrStart, corrEnd, true),
	}
	if err := svc.CorrelateAlerts(context.Background(), alerts); err != nil {
		t.Fatalf("关联返回错误: %v", err)
	}
	if got := enq.seen(); len(got) != 0 {
		t.Errorf("resolved 告警触发了 %d 次入队，期望 0", len(got))
	}
}
