package alert

import (
	"context"
	"errors"
	"testing"
)

// fakeCorrelator 记录被喂入的告警，并可注入错误。
// 复用同包的 newFakeRepo / makePayload / rawFiring 等既有夹具。
type fakeCorrelator struct {
	calls int
	got   []StoredAlert
	err   error
}

func (f *fakeCorrelator) CorrelateAlerts(_ context.Context, alerts []StoredAlert) error {
	f.calls++
	f.got = append(f.got, alerts...)
	return f.err
}

// 落库成功的新 firing 告警会被送给关联引擎（且带上了 ID）。
func TestIngestInvokesCorrelatorOnNewFiring(t *testing.T) {
	svc := newTestService(newFakeRepo())
	fc := &fakeCorrelator{}
	svc.SetCorrelator(fc)

	if _, err := svc.Ingest(context.Background(), makePayload("firing", rawFiring)); err != nil {
		t.Fatalf("ingest 失败: %v", err)
	}
	if fc.calls != 1 {
		t.Fatalf("期望关联被调用 1 次，实际 %d 次", fc.calls)
	}
	if len(fc.got) != 1 {
		t.Fatalf("期望关联收到 1 条告警，实际 %d 条", len(fc.got))
	}
	if got := fc.got[0]; got.ID == 0 || got.Status != StatusFiring {
		t.Fatalf("关联收到的告警不正确（应有 ID 且为 firing）: %+v", got)
	}
}

// 重复投递（skipped）不产生新行，因此不触发关联。
func TestIngestSkipsCorrelatorWhenDuplicate(t *testing.T) {
	svc := newTestService(newFakeRepo())
	if _, err := svc.Ingest(context.Background(), makePayload("firing", rawFiring)); err != nil {
		t.Fatal(err)
	}
	fc := &fakeCorrelator{}
	svc.SetCorrelator(fc)

	if _, err := svc.Ingest(context.Background(), makePayload("firing", rawFiring)); err != nil {
		t.Fatal(err)
	}
	if fc.calls != 0 {
		t.Fatalf("重复告警不应触发关联，实际调用 %d 次", fc.calls)
	}
}

// 关联失败**不得**冒泡为 Ingest 错误（否则 webhook 会 503，放大风暴）。
func TestIngestSwallowsCorrelatorError(t *testing.T) {
	svc := newTestService(newFakeRepo())
	svc.SetCorrelator(&fakeCorrelator{err: errors.New("关联引擎爆炸")})

	res, err := svc.Ingest(context.Background(), makePayload("firing", rawFiring))
	if err != nil {
		t.Fatalf("关联失败不应冒泡为 Ingest 错误，实得: %v", err)
	}
	if res.Accepted != 1 {
		t.Fatalf("期望 accepted=1，实际 %+v", res)
	}
}

// resolved 不插入新行，因此不触发关联（其生命周期归 M3）。
func TestIngestDoesNotCorrelateResolved(t *testing.T) {
	svc := newTestService(newFakeRepo())
	if _, err := svc.Ingest(context.Background(), makePayload("firing", rawFiring)); err != nil {
		t.Fatal(err)
	}
	fc := &fakeCorrelator{}
	svc.SetCorrelator(fc)

	if _, err := svc.Ingest(context.Background(), makePayload("resolved", rawResolved)); err != nil {
		t.Fatal(err)
	}
	if fc.calls != 0 {
		t.Fatalf("resolved 不应触发关联，实际调用 %d 次", fc.calls)
	}
}

// 未注入关联引擎时，行为与 M1 一致（不 panic、照常落库）。
func TestIngestWithoutCorrelator(t *testing.T) {
	svc := newTestService(newFakeRepo())
	res, err := svc.Ingest(context.Background(), makePayload("firing", rawFiring))
	if err != nil {
		t.Fatalf("未装配关联时不应报错: %v", err)
	}
	if res.Accepted != 1 {
		t.Fatalf("期望 accepted=1，实际 %+v", res)
	}
}
