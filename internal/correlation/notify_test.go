package correlation

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/yu/iip/internal/alert"
	"github.com/yu/iip/internal/notify"
)

// fakeNotifier 记录被派发的事件创建通知。
type fakeNotifier struct {
	mu      sync.Mutex
	created []notify.IncidentRef
}

func (f *fakeNotifier) NotifyIncidentCreated(inc notify.IncidentRef) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = append(f.created, inc)
	return true
}

func (f *fakeNotifier) calls() []notify.IncidentRef {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]notify.IncidentRef(nil), f.created...)
}

// TestNotifierFiresOnNewIncident 钉住 NEW 档建档成功后恰好派发一次通知，
// 且通知里的字段与落库值一致（标题/严重度/服务/状态/开始时间）。
func TestNotifierFiresOnNewIncident(t *testing.T) {
	store := &fakeIncidentStore{}
	n := &fakeNotifier{}
	svc := newCorrService(store, &fakeConfigStore{cfg: DefaultConfig()}, nil).WithNotifier(n)

	if err := svc.CorrelateAlerts(context.Background(), []alert.StoredAlert{newFiringOnCheckout(101, "pod-a")}); err != nil {
		t.Fatalf("CorrelateAlerts: %v", err)
	}

	got := n.calls()
	if len(got) != 1 {
		t.Fatalf("NEW 档应派发 1 条通知，实得 %d", len(got))
	}
	ref := got[0]
	if ref.ID != store.created[0].ID {
		t.Errorf("通知事件 ID 应等于落库 ID %d，实得 %d", store.created[0].ID, ref.ID)
	}
	if ref.Title == "" || ref.Severity != "critical" || ref.Service != "checkout" {
		t.Errorf("通知字段与落库值不一致: %+v", ref)
	}
	if ref.Status != "open" {
		t.Errorf("新建事件状态应为 open，实得 %q", ref.Status)
	}
	if !ref.StartedAt.Equal(corrStart) {
		t.Errorf("开始时间应为 %v，实得 %v", corrStart, ref.StartedAt)
	}
}

// TestNotifierSilentOnMerge：合入既有事件不是「新事件」，不应发创建通知。
func TestNotifierSilentOnMerge(t *testing.T) {
	topo := &Topology{}
	topo.AddEdges(map[string][]string{"checkout": {"payment"}})
	store := &fakeIncidentStore{candidates: []CandidateAlert{candidateOnCheckout(7, "pod-old")}}
	n := &fakeNotifier{}
	svc := newCorrService(store, &fakeConfigStore{cfg: DefaultConfig()}, topo).WithNotifier(n)

	if err := svc.CorrelateAlerts(context.Background(), []alert.StoredAlert{newFiringOnCheckout(101, "pod-new")}); err != nil {
		t.Fatalf("CorrelateAlerts: %v", err)
	}
	if len(n.calls()) != 0 {
		t.Errorf("MERGE 档不应派发创建通知，实得 %d 条", len(n.calls()))
	}
}

// TestNotifierSilentWhenLinkFails 钉住触发时机在 link 之后：
// 事件建好但告警没关联上时，通知里的「关联告警 N 条」会与事实不符。
func TestNotifierSilentWhenLinkFails(t *testing.T) {
	store := &fakeIncidentStore{linkErr: errors.New("link 失败")}
	n := &fakeNotifier{}
	svc := newCorrService(store, &fakeConfigStore{cfg: DefaultConfig()}, nil).WithNotifier(n)

	if err := svc.CorrelateAlerts(context.Background(), []alert.StoredAlert{newFiringOnCheckout(101, "pod-a")}); err == nil {
		t.Fatal("link 失败应向上抛错")
	}
	if len(n.calls()) != 0 {
		t.Errorf("关联失败时不应派发通知，实得 %d 条", len(n.calls()))
	}
}

// TestNotifierNilByDefault：不挂 notifier 时关联行为不受影响（M2 形态）。
func TestNotifierNilByDefault(t *testing.T) {
	store := &fakeIncidentStore{}
	svc := newCorrService(store, &fakeConfigStore{cfg: DefaultConfig()}, nil)

	if err := svc.CorrelateAlerts(context.Background(), []alert.StoredAlert{newFiringOnCheckout(101, "pod-a")}); err != nil {
		t.Fatalf("未挂 notifier 时不应报错: %v", err)
	}
	if len(store.created) != 1 {
		t.Fatalf("未挂 notifier 时仍应新建事件，实得 %d", len(store.created))
	}
}
