package repository

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yu/iip/internal/incident"
	"github.com/yu/iip/internal/testsupport"
)

// 本文件覆盖 M3 生命周期仓储的**真库**行为：乐观并发条件、resolved_at 三态副作用、
// timeline 写入与排序，以及「状态更新 + 留痕」必须同事务（失败整体回滚）。
//
// 领域层规则（谁能转到谁）由 internal/incident/status_test.go 覆盖，此处不重复。

// insertIncident 直接插一条指定状态的事件，返回其 id。
func insertIncident(t *testing.T, ctx context.Context, pool *pgxpool.Pool, status string) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(ctx,
		`INSERT INTO incidents (title, severity, status, service)
		 VALUES ($1, 'warning', $2, 'checkout') RETURNING id`,
		"lifecycle-"+status, status).Scan(&id)
	if err != nil {
		t.Fatalf("插入事件失败: %v", err)
	}
	return id
}

// currentStatus 读回事件当前状态，用于断言「有没有被改」。
func currentStatus(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id int64) incident.Status {
	t.Helper()
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM incidents WHERE id = $1`, id).Scan(&status); err != nil {
		t.Fatalf("读取事件状态失败: %v", err)
	}
	return incident.Status(status)
}

// TestLifecycleGetByID 覆盖命中与未命中两条路径。
func TestLifecycleGetByID(t *testing.T) {
	pool := testsupport.NewDB(t)
	repo := NewLifecycle(pool)
	ctx := context.Background()

	id := insertIncident(t, ctx, pool, "investigating")
	snap, err := repo.GetByID(ctx, id)
	if err != nil {
		t.Fatalf("GetByID 失败: %v", err)
	}
	if snap.ID != id || snap.Status != incident.StatusInvestigating {
		t.Errorf("快照 = {ID:%d Status:%s}，期望 {ID:%d Status:investigating}", snap.ID, snap.Status, id)
	}
	if snap.ResolvedAt != nil {
		t.Errorf("未解决的事件 resolved_at 应为 nil，实际 %v", snap.ResolvedAt)
	}

	// 未命中必须是领域层哨兵错误，而不是零值快照 —— 否则 API 会返回 200 空对象。
	if _, err = repo.GetByID(ctx, id+99999); !errors.Is(err, incident.ErrIncidentNotFound) {
		t.Errorf("未命中应返回 ErrIncidentNotFound，实际: %v", err)
	}
}

// TestLifecycleUpdateStatusOptimistic 断言 `status = from` 的乐观条件真的生效。
func TestLifecycleUpdateStatusOptimistic(t *testing.T) {
	pool := testsupport.NewDB(t)
	repo := NewLifecycle(pool)
	ctx := context.Background()

	id := insertIncident(t, ctx, pool, "open")

	ok, err := repo.UpdateStatus(ctx, incident.UpdateStatusArg{
		ID: id, From: incident.StatusOpen, To: incident.StatusInvestigating,
	})
	if err != nil {
		t.Fatalf("UpdateStatus 失败: %v", err)
	}
	if !ok {
		t.Fatal("匹配当前状态时更新应命中")
	}
	if got := currentStatus(t, ctx, pool, id); got != incident.StatusInvestigating {
		t.Errorf("状态 = %s，期望 investigating", got)
	}

	// 再拿过期的 from=open 提交一次：必须不命中（这就是并发冲突信号）。
	ok, err = repo.UpdateStatus(ctx, incident.UpdateStatusArg{
		ID: id, From: incident.StatusOpen, To: incident.StatusResolved,
	})
	if err != nil {
		t.Fatalf("第二次 UpdateStatus 不应报错: %v", err)
	}
	if ok {
		t.Error("from 与当前状态不符时更新不应命中")
	}
	if got := currentStatus(t, ctx, pool, id); got != incident.StatusInvestigating {
		t.Errorf("未命中的更新不应改变状态，实际 %s", got)
	}
}

// TestLifecycleResolveAtEffect 在真库上验证 resolved_at 的三态副作用（手册 §4.1 D1）。
func TestLifecycleResolveAtEffect(t *testing.T) {
	pool := testsupport.NewDB(t)
	repo := NewLifecycle(pool)
	ctx := context.Background()

	t.Run("进入 resolved 写入时间戳", func(t *testing.T) {
		id := insertIncident(t, ctx, pool, "investigating")
		_, err := repo.UpdateStatus(ctx, incident.UpdateStatusArg{
			ID: id, From: incident.StatusInvestigating, To: incident.StatusResolved,
			Effect: incident.ResolveAtEffect(incident.StatusInvestigating, incident.StatusResolved),
		})
		if err != nil {
			t.Fatalf("UpdateStatus 失败: %v", err)
		}
		snap, err := repo.GetByID(ctx, id)
		if err != nil {
			t.Fatalf("GetByID 失败: %v", err)
		}
		if snap.ResolvedAt == nil {
			t.Fatal("进入 resolved 后 resolved_at 应非空")
		}
		if snap.ResolvedAt.Before(time.Now().Add(-time.Minute)) {
			t.Errorf("resolved_at = %v，应接近当前时刻", snap.ResolvedAt)
		}
	})

	t.Run("进入 ignored 不写时间戳", func(t *testing.T) {
		id := insertIncident(t, ctx, pool, "open")
		_, err := repo.UpdateStatus(ctx, incident.UpdateStatusArg{
			ID: id, From: incident.StatusOpen, To: incident.StatusIgnored,
			Effect: incident.ResolveAtEffect(incident.StatusOpen, incident.StatusIgnored),
		})
		if err != nil {
			t.Fatalf("UpdateStatus 失败: %v", err)
		}
		snap, err := repo.GetByID(ctx, id)
		if err != nil {
			t.Fatalf("GetByID 失败: %v", err)
		}
		// 误报关闭不该被算进 MTTR，故该列必须保持 NULL。
		if snap.ResolvedAt != nil {
			t.Errorf("进入 ignored 后 resolved_at 应为 nil，实际 %v", snap.ResolvedAt)
		}
	})

	t.Run("从 resolved 重开清空时间戳", func(t *testing.T) {
		id := insertIncident(t, ctx, pool, "resolved")
		if _, err := pool.Exec(ctx,
			`UPDATE incidents SET resolved_at = NOW() WHERE id = $1`, id); err != nil {
			t.Fatalf("预置 resolved_at 失败: %v", err)
		}
		_, err := repo.UpdateStatus(ctx, incident.UpdateStatusArg{
			ID: id, From: incident.StatusResolved, To: incident.StatusOpen,
			Effect: incident.ResolveAtEffect(incident.StatusResolved, incident.StatusOpen),
		})
		if err != nil {
			t.Fatalf("UpdateStatus 失败: %v", err)
		}
		snap, err := repo.GetByID(ctx, id)
		if err != nil {
			t.Fatalf("GetByID 失败: %v", err)
		}
		if snap.ResolvedAt != nil {
			t.Errorf("重开后 resolved_at 应为 nil，实际 %v", snap.ResolvedAt)
		}
	})
}

// TestLifecycleAppendTimelineEntry 断言留痕落库的字段与默认值。
func TestLifecycleAppendTimelineEntry(t *testing.T) {
	pool := testsupport.NewDB(t)
	repo := NewLifecycle(pool)
	ctx := context.Background()

	id := insertIncident(t, ctx, pool, "open")
	ts := time.Date(2026, 10, 2, 10, 30, 0, 0, time.UTC)
	entry := incident.Entry{
		IncidentID:  id,
		EventType:   incident.EventAcknowledged,
		Description: "状态变更 open → acknowledged",
		Actor:       "yu",
		Metadata:    map[string]string{"from": "open", "to": "acknowledged"},
		Timestamp:   ts,
	}
	if err := repo.AppendTimelineEntry(ctx, entry); err != nil {
		t.Fatalf("AppendTimelineEntry 失败: %v", err)
	}

	var eventType, description string
	var actor *string
	var metaFrom, metaTo *string
	// 逐字段取而不是比对 metadata::text：jsonb 不保证键顺序，
	// 直接比对整个 JSON 文本会得到「内容一样但顺序不同」的假失败。
	err := pool.QueryRow(ctx,
		`SELECT event_type, description, actor, metadata->>'from', metadata->>'to'
		 FROM timeline WHERE incident_id = $1`, id).
		Scan(&eventType, &description, &actor, &metaFrom, &metaTo)
	if err != nil {
		t.Fatalf("查询 timeline 失败: %v", err)
	}
	if eventType != string(incident.EventAcknowledged) {
		t.Errorf("event_type = %s，期望 acknowledged", eventType)
	}
	if description != entry.Description {
		t.Errorf("description = %q，期望 %q", description, entry.Description)
	}
	if actor == nil || *actor != "yu" {
		t.Errorf("actor = %v，期望 yu", actor)
	}
	if metaFrom == nil || *metaFrom != "open" {
		t.Errorf("metadata.from = %v，期望 open", metaFrom)
	}
	if metaTo == nil || *metaTo != "acknowledged" {
		t.Errorf("metadata.to = %v，期望 acknowledged", metaTo)
	}

	// 无 actor / 无 metadata 的两条：分别落 NULL 与 {}。
	bare := incident.Entry{
		IncidentID:  id,
		EventType:   incident.EventReopened,
		Description: "状态变更 resolved → open",
		Timestamp:   ts.Add(time.Minute),
	}
	if err = repo.AppendTimelineEntry(ctx, bare); err != nil {
		t.Fatalf("写入无 actor/metadata 的留痕失败: %v", err)
	}
	var nullActor *string
	var bareMetadata string
	err = pool.QueryRow(ctx,
		`SELECT actor, metadata::text FROM timeline WHERE incident_id = $1 AND event_type = 'reopened'`, id).
		Scan(&nullActor, &bareMetadata)
	if err != nil {
		t.Fatalf("查询 bare 留痕失败: %v", err)
	}
	if nullActor != nil {
		t.Errorf("未指定 actor 时应落 NULL，实际 %v", *nullActor)
	}
	if bareMetadata != "{}" {
		t.Errorf("未指定 metadata 时应落 {}，实际 %s", bareMetadata)
	}
}

// TestLifecycleTransitionEndToEnd 走完整生命周期：
// open → acknowledged → investigating → mitigated → resolved → 重开。
//
// 这条用例同时验证「每一次流转都留痕」与「resolved_at 只在 resolved 期间有值」，
// 是手册 §4.1 / §4.2 在真库上的落地证明。
func TestLifecycleTransitionEndToEnd(t *testing.T) {
	pool := testsupport.NewDB(t)
	repo := NewLifecycle(pool)
	svc := incident.NewService(repo)
	ctx := context.Background()

	id := insertIncident(t, ctx, pool, "open")

	steps := []incident.Status{
		incident.StatusAcknowledged,
		incident.StatusInvestigating,
		incident.StatusMitigated,
		incident.StatusResolved,
	}
	for _, to := range steps {
		snap, err := svc.Transition(ctx, id, to, "yu")
		if err != nil {
			t.Fatalf("流转到 %s 失败: %v", to, err)
		}
		if snap.Status != to {
			t.Errorf("返回状态 = %s，期望 %s", snap.Status, to)
		}
	}

	// 终态后 resolved_at 必须有值。
	snap, err := repo.GetByID(ctx, id)
	if err != nil {
		t.Fatalf("GetByID 失败: %v", err)
	}
	if snap.ResolvedAt == nil {
		t.Fatal("resolved 之后 resolved_at 不应为 nil")
	}

	// 重开：状态回 open、resolved_at 清空、并多一条 reopened 留痕。
	after, err := svc.Transition(ctx, id, incident.StatusOpen, "yu")
	if err != nil {
		t.Fatalf("重开失败: %v", err)
	}
	if after.Status != incident.StatusOpen {
		t.Errorf("重开后状态 = %s，期望 open", after.Status)
	}
	if after.ResolvedAt != nil {
		t.Errorf("重开后 resolved_at 应为 nil，实际 %v", after.ResolvedAt)
	}

	// 留痕序列：4 次流转 + 1 次重开，按时间升序应为手册 §4.2 的事件类型。
	rows, err := pool.Query(ctx,
		`SELECT event_type FROM timeline WHERE incident_id = $1 ORDER BY timestamp ASC, id ASC`, id)
	if err != nil {
		t.Fatalf("查询 timeline 失败: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var et string
		if err := rows.Scan(&et); err != nil {
			t.Fatalf("扫描失败: %v", err)
		}
		got = append(got, et)
	}
	if rows.Err() != nil {
		t.Fatalf("遍历 timeline 失败: %v", rows.Err())
	}
	want := []string{
		"acknowledged", "investigation_started", "mitigated", "resolved", "reopened",
	}
	if len(got) != len(want) {
		t.Fatalf("留痕条数 = %d（%v），期望 %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 条留痕 = %s，期望 %s", i+1, got[i], want[i])
		}
	}
}

// TestLifecycleTransitionRejectsIllegal 断言非法流转在真库上不产生任何副作用。
func TestLifecycleTransitionRejectsIllegal(t *testing.T) {
	pool := testsupport.NewDB(t)
	repo := NewLifecycle(pool)
	svc := incident.NewService(repo)
	ctx := context.Background()

	id := insertIncident(t, ctx, pool, "investigating")
	if _, err := svc.Transition(ctx, id, incident.StatusAcknowledged, "yu"); err == nil {
		t.Fatal("回退流转应被拒绝，实际通过")
	}

	if got := currentStatus(t, ctx, pool, id); got != incident.StatusInvestigating {
		t.Errorf("被拒绝的流转不应改变状态，实际 %s", got)
	}
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM timeline WHERE incident_id = $1`, id).Scan(&n); err != nil {
		t.Fatalf("统计留痕失败: %v", err)
	}
	if n != 0 {
		t.Errorf("被拒绝的流转不应写留痕，实际 %d 条", n)
	}
}

// TestLifecycleTxRollback 是「状态更新与留痕同事务」的真库证明：
// 留痕写入失败（外键值不存在）时，同一事务里已成功的状态更新必须一起回滚。
//
// 若两者不在同一事务，这里会留下「状态变了但没留痕」的幽灵流转——
// 正是手册 §4.3 要求同事务的原因。
func TestLifecycleTxRollback(t *testing.T) {
	pool := testsupport.NewDB(t)
	repo := NewLifecycle(pool)
	ctx := context.Background()

	id := insertIncident(t, ctx, pool, "open")

	err := repo.InTx(ctx, func(st incident.Store) error {
		ok, errUpdate := st.UpdateStatus(ctx, incident.UpdateStatusArg{
			ID: id, From: incident.StatusOpen, To: incident.StatusInvestigating,
		})
		if errUpdate != nil {
			return errUpdate
		}
		if !ok {
			return errors.New("更新未命中")
		}
		// 指向不存在的事件：外键约束会拒绝这条留痕。
		return st.AppendTimelineEntry(ctx, incident.Entry{
			IncidentID:  id + 999999,
			EventType:   incident.EventInvestigationStarted,
			Description: "应被回滚",
			Timestamp:   time.Now(),
		})
	})
	if err == nil {
		t.Fatal("留痕写入失败时事务应返回错误，实际返回 nil")
	}

	if got := currentStatus(t, ctx, pool, id); got != incident.StatusOpen {
		t.Errorf("事务回滚后状态应仍为 open，实际 %s", got)
	}
	var n int
	if err = pool.QueryRow(ctx,
		`SELECT count(*) FROM timeline WHERE incident_id = $1`, id).Scan(&n); err != nil {
		t.Fatalf("统计留痕失败: %v", err)
	}
	if n != 0 {
		t.Errorf("回滚后不应残留任何留痕，实际 %d 条", n)
	}
}

// TestLifecycleListTimelineOrder 断言时间线按时间倒序、同刻按 id 倒序。
func TestLifecycleListTimelineOrder(t *testing.T) {
	pool := testsupport.NewDB(t)
	repo := NewLifecycle(pool)
	ctx := context.Background()

	id := insertIncident(t, ctx, pool, "open")
	base := time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC)
	// 三条留痕：两条同一时刻（考察次级排序键），一条更早。
	for i, e := range []incident.Entry{
		{IncidentID: id, EventType: incident.EventAlertLinked, Description: "第一条", Timestamp: base},
		{IncidentID: id, EventType: incident.EventAcknowledged, Description: "第二条", Timestamp: base},
		{IncidentID: id, EventType: incident.EventMitigated, Description: "第三条", Timestamp: base.Add(-time.Hour)},
	} {
		if err := repo.AppendTimelineEntry(ctx, e); err != nil {
			t.Fatalf("写入第 %d 条留痕失败: %v", i+1, err)
		}
	}

	var desc string
	err := pool.QueryRow(ctx,
		`SELECT description FROM timeline WHERE incident_id = $1
		 ORDER BY timestamp DESC, id DESC LIMIT 1`, id).Scan(&desc)
	if err != nil {
		t.Fatalf("查询最新留痕失败: %v", err)
	}
	// 同一时刻的两条里，id 更大的（第二条）应排在最前。
	if desc != "第二条" {
		t.Errorf("最新的留痕 = %q，期望「第二条」（同刻按 id 倒序）", desc)
	}
}
