package incident

import (
	"context"
	"sync"
	"testing"

	"github.com/yu/iip/internal/notify"
)

// fakeNotifier 记录被派发的状态变更通知。
type fakeNotifier struct {
	mu    sync.Mutex
	calls []notifyCall
}

type notifyCall struct {
	ref   notify.IncidentRef
	from  string
	to    string
	actor string
}

func (f *fakeNotifier) NotifyStatusChanged(ref notify.IncidentRef, from, to, actor string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, notifyCall{ref: ref, from: from, to: to, actor: actor})
	return true
}

func (f *fakeNotifier) received() []notifyCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]notifyCall(nil), f.calls...)
}

// TestNotifyOnSuccessfulTransition 钉住合法流转后恰好派发一次通知，
// from/to/actor 与落库一致，且引用的是流转后的快照。
func TestNotifyOnSuccessfulTransition(t *testing.T) {
	svc, tx := newTestService(StatusOpen)
	after := Snapshot{ID: 7, Title: "checkout 延迟升高", Severity: "critical", Status: StatusInvestigating}
	tx.store.after = &after
	n := &fakeNotifier{}
	svc = svc.WithNotifier(n)

	if _, err := svc.Transition(context.Background(), 7, StatusInvestigating, "yu"); err != nil {
		t.Fatalf("合法流转不应报错: %v", err)
	}

	got := n.received()
	if len(got) != 1 {
		t.Fatalf("应派发 1 条状态变更通知，实得 %d", len(got))
	}
	c := got[0]
	if c.ref.ID != 7 || c.ref.Status != string(StatusInvestigating) {
		t.Errorf("通知事件快照不符: %+v", c.ref)
	}
	if c.from != string(StatusOpen) || c.to != string(StatusInvestigating) {
		t.Errorf("通知流转 %s→%s，期望 open→investigating", c.from, c.to)
	}
	if c.actor != "yu" {
		t.Errorf("操作人 = %q，期望 yu", c.actor)
	}
}

// TestNotifySilentOnInvalidTransition：非法流转必须不发通知——
// 否则用户会收到一条并未发生的流转（与 TaskUpdate 同一类「谎报」）。
func TestNotifySilentOnInvalidTransition(t *testing.T) {
	svc, _ := newTestService(StatusResolved)
	n := &fakeNotifier{}
	svc = svc.WithNotifier(n)

	if _, err := svc.Transition(context.Background(), 7, StatusAcknowledged, "yu"); err == nil {
		t.Fatal("终态到 acknowledged 的流转应被拒绝")
	}
	if got := n.received(); len(got) != 0 {
		t.Errorf("非法流转不应派发通知，实得 %d 条", len(got))
	}
}

// TestNotifySilentOnConflict：并发冲突（影响 0 行）同样不发通知——
// 状态没有改变，通知发了就是假的。
func TestNotifySilentOnConflict(t *testing.T) {
	svc, tx := newTestService(StatusOpen)
	tx.store.updateOK = false // 模拟被别的请求抢先改状态
	n := &fakeNotifier{}
	svc = svc.WithNotifier(n)

	if _, err := svc.Transition(context.Background(), 7, StatusInvestigating, "yu"); err == nil {
		t.Fatal("影响 0 行应返回冲突错误")
	}
	if got := n.received(); len(got) != 0 {
		t.Errorf("冲突时不应派发通知，实得 %d 条", len(got))
	}
}

// TestNotifyNilByDefault：不挂 notifier 时流转行为不受影响（M3 形态）。
func TestNotifyNilByDefault(t *testing.T) {
	svc, tx := newTestService(StatusOpen)
	after := Snapshot{ID: 7, Status: StatusInvestigating}
	tx.store.after = &after

	if _, err := svc.Transition(context.Background(), 7, StatusInvestigating, "yu"); err != nil {
		t.Fatalf("未挂 notifier 时不应报错: %v", err)
	}
}
