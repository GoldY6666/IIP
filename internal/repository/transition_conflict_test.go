package repository

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yu/iip/internal/incident"
	"github.com/yu/iip/internal/testsupport"
)

// 本文件覆盖 M3 验收清单第 4 条的后半句：**并发冲突回 409 且不产生 timeline 行**。
//
// 「不产生 timeline 行」这半句此前只在 handler 层用替身验过（断言返回 409），
// 从未在真库上验过。而它恰恰是「状态流转与 timeline 写入同事务」这条设计的
// **唯一可观测证据**：若两件事不在一个事务里，冲突时状态没变、留痕却多一行，
// 页面会显示一条并不存在的流转，且**没有任何报错**指向它。
//
// 必须真库：内存假存储天然「同事务」，它证明不了数据库层的回滚。

// countTimelineRows 统计某事件的 timeline 行数。
func countTimelineRows(t *testing.T, pool *pgxpool.Pool, incidentID int64) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM timeline WHERE incident_id = $1`, incidentID).Scan(&n); err != nil {
		t.Fatalf("统计 timeline 行数失败: %v", err)
	}
	return n
}

// forceStatus 绕过服务层直接改 incidents.status，用来预置「另一个请求抢先改掉了状态」。
//
// 不能用 UpdateStatus：它带 `WHERE status = from` 的乐观条件，
// 预置时得先把 from 写成旧值，那样反而模拟不出「服务层读到的状态与更新条件不符」。
func forceStatus(t *testing.T, pool *pgxpool.Pool, incidentID int64, status string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE incidents SET status = $2 WHERE id = $1`, incidentID, status); err != nil {
		t.Fatalf("预置状态为 %s 失败: %v", status, err)
	}
}

// TestTransitionConflictLeavesNoTimelineRow 是本文件的核心用例：
// 过期 from 的 UpdateStatus 必须命中 0 行，且库里的状态与 timeline 都不变。
func TestTransitionConflictLeavesNoTimelineRow(t *testing.T) {
	pool := testsupport.NewDB(t)
	repo := NewLifecycle(pool)
	ctx := context.Background()

	id := insertIncident(t, ctx, pool, "open")
	before := countTimelineRows(t, pool, id)

	// 模拟并发：另一个连接抢先把状态改成了 investigating。
	forceStatus(t, pool, id, "investigating")

	// 本次请求却按「当前状态是 open」去流转 —— 与服务层在真并发下的行为等价。
	ok, err := repo.UpdateStatus(ctx, incident.UpdateStatusArg{
		ID:     id,
		From:   incident.StatusOpen,
		To:     incident.StatusAcknowledged,
		Effect: incident.ResolveEffect{},
	})
	if err != nil {
		t.Fatalf("UpdateStatus 返回错误 = %v，期望 nil（冲突应以 ok=false 表达）", err)
	}
	if ok {
		t.Fatal("过期 from 的 UpdateStatus 应命中 0 行，实际返回 true —— " +
			"乐观条件没生效，并发保护形同虚设")
	}

	if after := countTimelineRows(t, pool, id); after != before {
		t.Errorf("冲突后 timeline 行数 = %d，流转前 = %d —— 留下了幽灵留痕", after, before)
	}
	cur, err := repo.GetByID(ctx, id)
	if err != nil {
		t.Fatalf("GetByID 失败: %v", err)
	}
	if cur.Status != incident.StatusInvestigating {
		t.Errorf("冲突后状态 = %s，期望仍为 investigating（不该被这次流转改掉）", cur.Status)
	}
}

// TestTransitionConflictLeavesResolvedAtUntouched 断言冲突不会误写 resolved_at。
//
// 这条单列是因为 resolved_at 有一个「只在特定转换时写」的契约
// （手册 §4.1 决策 D1）：冲突时连状态都没改成，就更不该碰这一列 ——
// 否则一次失败的流转就能给事件盖上一个「已解决」的时间戳，污染 MTTR。
func TestTransitionConflictLeavesResolvedAtUntouched(t *testing.T) {
	pool := testsupport.NewDB(t)
	repo := NewLifecycle(pool)
	ctx := context.Background()

	id := insertIncident(t, ctx, pool, "open")
	forceStatus(t, pool, id, "investigating")

	// 试图流转到 resolved（带写 resolved_at 的副作用），但 from 是过期的 open。
	ok, err := repo.UpdateStatus(ctx, incident.UpdateStatusArg{
		ID:     id,
		From:   incident.StatusOpen,
		To:     incident.StatusResolved,
		Effect: incident.ResolveEffect{SetResolvedNow: true},
	})
	if err != nil {
		t.Fatalf("UpdateStatus 返回错误 = %v，期望 nil", err)
	}
	if ok {
		t.Fatal("过期 from 应命中 0 行")
	}

	cur, err := repo.GetByID(ctx, id)
	if err != nil {
		t.Fatalf("GetByID 失败: %v", err)
	}
	if cur.ResolvedAt != nil {
		t.Errorf("冲突失败后 resolved_at = %v，期望仍为 nil —— "+
			"一次未成功的流转给事件盖上了解决时间戳，会污染 MTTR", cur.ResolvedAt)
	}
}

// TestTransitionWithCurrentFromSucceeds 是冲突用例的反面：
// from 与库中实际一致时必须命中，否则整个乐观保护就是误伤。
func TestTransitionWithCurrentFromSucceeds(t *testing.T) {
	pool := testsupport.NewDB(t)
	repo := NewLifecycle(pool)
	ctx := context.Background()

	id := insertIncident(t, ctx, pool, "open")

	ok, err := repo.UpdateStatus(ctx, incident.UpdateStatusArg{
		ID:     id,
		From:   incident.StatusOpen,
		To:     incident.StatusAcknowledged,
		Effect: incident.ResolveEffect{},
	})
	if err != nil {
		t.Fatalf("UpdateStatus 失败: %v", err)
	}
	if !ok {
		t.Fatal("from 与库中实际一致时应命中 1 行")
	}
	cur, err := repo.GetByID(ctx, id)
	if err != nil {
		t.Fatalf("GetByID 失败: %v", err)
	}
	if cur.Status != incident.StatusAcknowledged {
		t.Errorf("状态 = %s，期望 acknowledged", cur.Status)
	}
}

// TestTransitionRollsBackWhenTimelineWriteFails 断言「留痕写失败要回滚状态」。
//
// 这是同事务的另一半证据：前两个用例证明「状态没变时留痕也不多」，
// 这条证明「留痕写失败时状态也没变」。两个方向都验，才算证明真在一个事务里。
func TestTransitionRollsBackWhenTimelineWriteFails(t *testing.T) {
	pool := testsupport.NewDB(t)
	repo := NewLifecycle(pool)
	ctx := context.Background()

	id := insertIncident(t, ctx, pool, "open")

	// 用 trigger 制造确定性的 timeline 写失败：比构造违反 CHECK 的数据更直接，
	// 也不会受 0004 迁移里列约束变动的影响。
	const trgFn = "test_fail_timeline"
	if _, err := pool.Exec(ctx, `
		CREATE OR REPLACE FUNCTION `+trgFn+`() RETURNS trigger AS $$
		BEGIN
			RAISE EXCEPTION '测试用：强制 timeline 写入失败';
		END $$ LANGUAGE plpgsql;`); err != nil {
		t.Fatalf("创建测试用 trigger 函数失败: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		CREATE TRIGGER test_block_timeline BEFORE INSERT ON timeline
		FOR EACH ROW EXECUTE FUNCTION `+trgFn+`();`); err != nil {
		t.Fatalf("创建测试用 trigger 失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DROP TRIGGER IF EXISTS test_block_timeline ON timeline;`)
		_, _ = pool.Exec(ctx, `DROP FUNCTION IF EXISTS `+trgFn+`();`)
	})

	svc := incident.NewService(repo)
	if _, err := svc.Transition(ctx, id, incident.StatusAcknowledged, "yu"); err == nil {
		t.Fatal("留痕写失败时 Transition 应返回错误")
	}

	cur, err := repo.GetByID(ctx, id)
	if err != nil {
		t.Fatalf("GetByID 失败: %v", err)
	}
	if cur.Status != incident.StatusOpen {
		t.Errorf("留痕失败后状态 = %s，期望回滚为 open —— "+
			"状态更新与留痕不在同一事务里，会留下「状态变了却没留痕」的幽灵流转",
			cur.Status)
	}
	if n := countTimelineRows(t, pool, id); n != 0 {
		t.Errorf("留痕失败后 timeline 应为 0 行，实际 %d", n)
	}
}
