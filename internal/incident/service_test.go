package incident

import (
	"context"
	"errors"
	"testing"
	"time"
)

// 本文件用假存储覆盖 Service.Transition 的编排逻辑：事务边界、合法性前置、
// resolved_at 副作用的计算结果、乐观并发冲突的判定，以及「写失败就不留痕」。
// 真库行为由 internal/repository/lifecycle_test.go 覆盖，两者不重复。

// fakeStore 是 Store 的内存替身，记录每一次写调用以便断言。
type fakeStore struct {
	snap       Snapshot
	after      *Snapshot // 第二次及以后的 GetByID 返回它（模拟「更新后回读」）
	getErr     error
	getCalls   int
	updateOK   bool
	updateErr  error
	appendErr  error
	statusArgs []UpdateStatusArg
	entries    []Entry
}

func (f *fakeStore) GetByID(_ context.Context, _ int64) (Snapshot, error) {
	f.getCalls++
	if f.getErr != nil {
		return Snapshot{}, f.getErr
	}
	if f.getCalls > 1 && f.after != nil {
		return *f.after, nil
	}
	return f.snap, nil
}

func (f *fakeStore) UpdateStatus(_ context.Context, arg UpdateStatusArg) (bool, error) {
	f.statusArgs = append(f.statusArgs, arg)
	if f.updateErr != nil {
		return false, f.updateErr
	}
	return f.updateOK, nil
}

func (f *fakeStore) AppendTimelineEntry(_ context.Context, e Entry) error {
	if f.appendErr != nil {
		return f.appendErr
	}
	f.entries = append(f.entries, e)
	return nil
}

// fakeTransactor 给 fakeStore 套上事务边界，并记录事务被开了几次。
type fakeTransactor struct {
	store     *fakeStore
	inTxCalls int
	txErr     error
}

func (f *fakeTransactor) InTx(_ context.Context, fn func(Store) error) error {
	f.inTxCalls++
	if f.txErr != nil {
		return f.txErr
	}
	return fn(f.store)
}

// 委托给内嵌 store，让 fakeTransactor 自身也是完整 Store（接口要求）。
func (f *fakeTransactor) GetByID(ctx context.Context, id int64) (Snapshot, error) {
	return f.store.GetByID(ctx, id)
}

func (f *fakeTransactor) UpdateStatus(ctx context.Context, arg UpdateStatusArg) (bool, error) {
	return f.store.UpdateStatus(ctx, arg)
}

func (f *fakeTransactor) AppendTimelineEntry(ctx context.Context, e Entry) error {
	return f.store.AppendTimelineEntry(ctx, e)
}

// newTestService 组装一个「状态为 from、更新成功」的服务与它的假存储。
func newTestService(from Status) (*Service, *fakeTransactor) {
	store := &fakeStore{
		snap:     Snapshot{ID: 7, Title: "checkout 延迟升高", Status: from},
		updateOK: true,
	}
	tx := &fakeTransactor{store: store}
	svc := NewService(tx)
	svc.now = func() time.Time { return time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC) }
	return svc, tx
}

// TestTransitionHappyPath 断言一次合法流转的完整调用形状。
func TestTransitionHappyPath(t *testing.T) {
	svc, tx := newTestService(StatusOpen)
	after := Snapshot{ID: 7, Status: StatusInvestigating}
	tx.store.after = &after

	got, err := svc.Transition(context.Background(), 7, StatusInvestigating, "yu")
	if err != nil {
		t.Fatalf("合法流转不应报错: %v", err)
	}
	if got.Status != StatusInvestigating {
		t.Errorf("返回快照状态 = %q，期望 %q", got.Status, StatusInvestigating)
	}
	// 全流程必须在**一个**事务内完成（状态更新与留痕是同一件事的两半）。
	if tx.inTxCalls != 1 {
		t.Errorf("InTx 调用次数 = %d，期望 1", tx.inTxCalls)
	}
	if len(tx.store.statusArgs) != 1 {
		t.Fatalf("状态更新调用次数 = %d，期望 1", len(tx.store.statusArgs))
	}
	arg := tx.store.statusArgs[0]
	if arg.ID != 7 || arg.From != StatusOpen || arg.To != StatusInvestigating {
		t.Errorf("更新入参 = %+v，期望 ID=7 from=open to=investigating", arg)
	}
	if len(tx.store.entries) != 1 {
		t.Fatalf("留痕写入次数 = %d，期望 1", len(tx.store.entries))
	}
	entry := tx.store.entries[0]
	if entry.EventType != EventInvestigationStarted {
		t.Errorf("事件类型 = %q，期望 %q", entry.EventType, EventInvestigationStarted)
	}
	if entry.Actor != "yu" {
		t.Errorf("actor = %q，期望 yu", entry.Actor)
	}
	if entry.Metadata["from"] != "open" || entry.Metadata["to"] != "investigating" {
		t.Errorf("metadata = %v，期望含 from=open / to=investigating", entry.Metadata)
	}
	// 时钟注入：留痕时间与 resolved_at 副作用都应当可精确断言。
	want := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	if !entry.Timestamp.Equal(want) {
		t.Errorf("留痕时间 = %v，期望 %v", entry.Timestamp, want)
	}
}

// TestTransitionResolveAtEffect 断言 resolved_at 副作用在四种情形下的计算结果。
//
// 这里测的是「算出来什么」，不是「SQL 写对没有」——后者由 repository 的真库用例覆盖。
func TestTransitionResolveAtEffect(t *testing.T) {
	cases := []struct {
		name      string
		from      Status
		to        Status
		wantSet   bool
		wantClear bool
	}{
		{"进入 resolved 写 NOW", StatusInvestigating, StatusResolved, true, false},
		{"从 resolved 重开清 NULL", StatusResolved, StatusOpen, false, true},
		{"进入 ignored 不动", StatusOpen, StatusIgnored, false, false},
		{"普通前向不动", StatusOpen, StatusInvestigating, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, tx := newTestService(tc.from)
			if _, err := svc.Transition(context.Background(), 7, tc.to, "yu"); err != nil {
				t.Fatalf("流转不应报错: %v", err)
			}
			if len(tx.store.statusArgs) != 1 {
				t.Fatalf("状态更新调用次数 = %d，期望 1", len(tx.store.statusArgs))
			}
			eff := tx.store.statusArgs[0].Effect
			if eff.SetResolvedNow != tc.wantSet || eff.ClearResolved != tc.wantClear {
				t.Errorf("副作用 = %+v，期望 set=%v clear=%v", eff, tc.wantSet, tc.wantClear)
			}
		})
	}
}

// TestTransitionIllegal 断言非法流转被拦在写库之前：状态没改、留痕没写。
func TestTransitionIllegal(t *testing.T) {
	svc, tx := newTestService(StatusInvestigating)

	_, err := svc.Transition(context.Background(), 7, StatusAcknowledged, "yu")
	if err == nil {
		t.Fatal("回退流转应报错，实际通过")
	}
	var invalid *InvalidTransitionError
	if !errors.As(err, &invalid) {
		t.Fatalf("期望 *InvalidTransitionError，实际 %T: %v", err, invalid)
	}
	if invalid.Reason != ReasonBackward {
		t.Errorf("原因 = %q，期望 %q", invalid.Reason, ReasonBackward)
	}
	if len(tx.store.statusArgs) != 0 {
		t.Errorf("非法流转不应更新状态，实际调用了 %d 次", len(tx.store.statusArgs))
	}
	if len(tx.store.entries) != 0 {
		t.Errorf("非法流转不应写留痕，实际写了 %d 条", len(tx.store.entries))
	}
}

// TestTransitionSameStatus 断言同状态转换既不改状态也不留痕（手册 §4.3 幂等要求）。
func TestTransitionSameStatus(t *testing.T) {
	svc, tx := newTestService(StatusOpen)
	_, err := svc.Transition(context.Background(), 7, StatusOpen, "yu")
	if err == nil {
		t.Fatal("同状态转换应报错，实际通过")
	}
	var invalid *InvalidTransitionError
	if !errors.As(err, &invalid) || invalid.Reason != ReasonSameStatus {
		t.Fatalf("期望 same_status 的 InvalidTransitionError，实际: %v", err)
	}
	if len(tx.store.statusArgs) != 0 || len(tx.store.entries) != 0 {
		t.Error("同状态转换不应产生任何写入")
	}
}

// TestTransitionConflict 断言乐观并发冲突：更新未命中时返回 ConflictError 且不写留痕。
func TestTransitionConflict(t *testing.T) {
	svc, tx := newTestService(StatusOpen)
	tx.store.updateOK = false // 模拟「状态已被别的请求改走」

	_, err := svc.Transition(context.Background(), 7, StatusResolved, "yu")
	if err == nil {
		t.Fatal("并发冲突应报错，实际通过")
	}
	var conflict *ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("期望 *ConflictError，实际 %T: %v", err, conflict)
	}
	if conflict.Expected != StatusOpen {
		t.Errorf("冲突错误里的期望状态 = %q，应为提交时依据的旧状态 open", conflict.Expected)
	}
	if len(tx.store.entries) != 0 {
		t.Errorf("冲突时不应写留痕，实际写了 %d 条", len(tx.store.entries))
	}
}

// TestTransitionNotFound 断言「事件不存在」原样上抛，不被当成冲突或非法流转。
func TestTransitionNotFound(t *testing.T) {
	svc, tx := newTestService(StatusOpen)
	tx.store.getErr = ErrIncidentNotFound

	_, err := svc.Transition(context.Background(), 7, StatusResolved, "yu")
	if !errors.Is(err, ErrIncidentNotFound) {
		t.Fatalf("期望 ErrIncidentNotFound，实际: %v", err)
	}
	if len(tx.store.statusArgs) != 0 {
		t.Error("事件不存在时不应更新状态")
	}
}

// TestTransitionTimelineFailure 断言留痕写失败会向上抛出（由事务负责整体回滚）。
//
// 这条是「同事务」的可观测证明之一：若两者不在同一事务，留痕失败时状态已被改走，
// 页面上会出现「状态变了但没有留痕」的幽灵流转。
func TestTransitionTimelineFailure(t *testing.T) {
	svc, tx := newTestService(StatusOpen)
	tx.store.appendErr = errors.New("timeline 写入失败")

	_, err := svc.Transition(context.Background(), 7, StatusResolved, "yu")
	if err == nil {
		t.Fatal("留痕失败应上抛，实际通过")
	}
	if len(tx.store.statusArgs) != 1 {
		t.Errorf("留痕失败前应先尝试更新状态（随后由事务回滚），实际调用 %d 次", len(tx.store.statusArgs))
	}
}

// TestTransitionTxError 断言开事务失败时不会退化成「无事务写入」。
func TestTransitionTxError(t *testing.T) {
	svc, tx := newTestService(StatusOpen)
	tx.txErr = errors.New("begin 失败")

	if _, err := svc.Transition(context.Background(), 7, StatusResolved, "yu"); err == nil {
		t.Fatal("开事务失败应上抛，实际通过")
	}
	if len(tx.store.statusArgs) != 0 {
		t.Error("开事务失败后不应有任何写入")
	}
}
