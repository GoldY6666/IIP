package migrations_test

import (
	"context"
	"testing"

	_ "github.com/golang-migrate/migrate/v4/database/postgres" // 注册 database/sql 的 postgres 迁移驱动
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yu/iip/internal/testsupport"
)

// TestIncidentLifecycleM3Migration 覆盖 0004 的三项改动在**数据库层**确实生效且可单步回滚：
// incidents.status 放宽到 6 值、timeline.event_type 放宽到 12 值、review_suppressions 建表。
//
// 与 TestCorrelationM2Migration 同理，刻意**只回滚 0004**：
// 全量回滚会把两个 CHECK 所在的表一并删掉，届时「插入被拒」是必然的，
// 根本测不出 0004 的 down 是否真的把约束还原了。隔离到单步才有判别力。
//
// 反向断言同样重要：只测「acknowledged 能插进去」会被「有人把 CHECK 整个删掉」
// 蒙混过关，故每处放宽都配一条非法值必须仍被拒的用例（SQLSTATE 23514）。
func TestIncidentLifecycleM3Migration(t *testing.T) {
	dsn := testsupport.NewEmptyDB(t)
	m := newMigrator(t, dsn)
	defer closeMigrator(t, m)

	// 1) 全量 up
	mustUp(t, m)
	assertTableExists(t, dsn, reviewSuppressionsTable, true)

	withPool(t, dsn, func(ctx context.Context, pool *pgxpool.Pool) {
		// ① status='acknowledged' 可插入 —— 0001 限定的 5 值里没有它（偏差 A1）。
		incidentID := insertIncident(t, ctx, pool, "acknowledged")

		// ② 7 个新 event_type 均可插入（偏差 A2）。
		for _, eventType := range newTimelineEventTypes {
			if _, err := pool.Exec(ctx,
				`INSERT INTO timeline (incident_id, event_type, description, actor)
				 VALUES ($1, $2, $3, 'tester')`, incidentID, eventType, "m3 "+eventType); err != nil {
				t.Errorf("插入 event_type=%q 应成功，实际失败: %v", eventType, err)
			}
		}

		// ③ 非法值仍必须被拒 —— 证明是「放宽」而不是「删掉约束」。
		assertRejected(t, ctx, pool, "23514",
			`INSERT INTO incidents (title, severity, status) VALUES ('bad-status', 'warning', 'triage')`)
		assertRejected(t, ctx, pool, "23514",
			`INSERT INTO timeline (incident_id, event_type, description) VALUES ($1, 'foo', 'bad')`, incidentID)

		// ④ review_suppressions 唯一约束 (fingerprint, incident_id) 生效。
		const insertSuppression = `INSERT INTO review_suppressions (fingerprint, incident_id, actor)
			VALUES ($1, $2, 'tester')`
		if _, err := pool.Exec(ctx, insertSuppression, "fp-m3-suppress", incidentID); err != nil {
			t.Fatalf("插入抑制行应成功，实际失败: %v", err)
		}
		assertRejected(t, ctx, pool, "23505", insertSuppression, "fp-m3-suppress", incidentID)
	})

	// 2) 只回滚 0004：0001/0002/0003 保持不动。
	//
	// 绝对版本而非 Steps(-1)：理由同 0003 那条——相对步数会随迁移总数漂移，
	// 漂移后本用例测的不再是 0004 的 down，而断言却仍可能自洽。
	mustDownTo(t, m, migrationIncidentLifecycle)
	assertTableExists(t, dsn, reviewSuppressionsTable, false)
	assertIndexExists(t, dsn, timelineIncidentTSIndex, false)
	assertIndexExists(t, dsn, reviewSuppressionsIndex, false)
	assertIndexExists(t, dsn, needsReviewIndex, true) // 0003 未被回滚，其索引必须还在

	withPool(t, dsn, func(ctx context.Context, pool *pgxpool.Pool) {
		// ⑤ down 的破坏性 ① 真实发生：7 个新类型的 timeline 行已被删除。
		//    不删这些行的话，下面的 CHECK 还原会因违反约束而失败（SQLSTATE 23514），
		//    所以这条断言同时是「down 能跑完」的前置条件证明。
		var remain int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM timeline`).Scan(&remain); err != nil {
			t.Fatalf("统计 timeline 行数失败: %v", err)
		}
		if remain != 0 {
			t.Errorf("回滚 0004 后新增类型的 timeline 行应已被删除，实际残留 %d 行", remain)
		}

		// 破坏性 ②：acknowledged 事件被回写为 open，故库里不应再有 acknowledged。
		var acked int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM incidents WHERE status = 'acknowledged'`).Scan(&acked); err != nil {
			t.Fatalf("统计 acknowledged 事件数失败: %v", err)
		}
		if acked != 0 {
			t.Errorf("回滚 0004 后不应仍有 status='acknowledged' 的行，实际 %d 行", acked)
		}

		// 两个 CHECK 均已还原：新增取值重新被拒。
		assertRejected(t, ctx, pool, "23514",
			`INSERT INTO incidents (title, severity, status) VALUES ('rolled-back', 'warning', 'acknowledged')`)
		incidentID := insertIncident(t, ctx, pool, "open")
		for _, eventType := range newTimelineEventTypes {
			assertRejected(t, ctx, pool, "23514",
				`INSERT INTO timeline (incident_id, event_type, description) VALUES ($1, $2, 'rolled back')`,
				incidentID, eventType)
		}
	})

	// 3) 重新 up 到最新：验证 0004 可重复执行（幂等）。
	mustUp(t, m)
	want := latestVersion(t)
	if v, dirty := currentVersion(t, m); v != want || dirty {
		t.Fatalf("再次 up 后版本应为 %d 且非 dirty，实际 version=%d dirty=%v", want, v, dirty)
	}
	assertTableExists(t, dsn, reviewSuppressionsTable, true)
	withPool(t, dsn, func(ctx context.Context, pool *pgxpool.Pool) {
		// 二次 up 后 acknowledged 重新可用（三连可重复的最终证明）。
		insertIncident(t, ctx, pool, "acknowledged")
	})
}
