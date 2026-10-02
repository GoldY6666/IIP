// Package migrations_test 以**外部测试包**形式编写。
//
// 必须用外部测试包：本用例要通过 testsupport 拿到一次性库，而 testsupport 依赖
// internal/store，store 又 import 本包（embed 迁移文件）——若写成包内测试
// （package migrations）就会构成 import cycle。外部测试包正是为切断此类环而存在。
package migrations_test

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres" // 注册 database/sql 的 postgres 迁移驱动
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yu/iip/internal/testsupport"
	"github.com/yu/iip/migrations"
)

// latestVersion 是当前迁移文件到达的版本号（0004）。
//
// 新增迁移时必须同步修改此常量——它让「忘记把新迁移纳入 embed/schema」这类
// 沉默失败变成一条明确的测试失败。
const latestVersion = 4

// dedupeIndex 是 0002 建立的部分唯一索引名。
const dedupeIndex = "uq_alerts_fingerprint_firing"

// needsReviewIndex 是 0003 建立的部分索引名（只索引 needs_review 为真的关联边）。
const needsReviewIndex = "idx_incident_alerts_needs_review"

// timelineIncidentTSIndex / reviewSuppressionsIndex 是 0004 建立的两个索引名。
const (
	timelineIncidentTSIndex = "idx_timeline_incident_ts"
	reviewSuppressionsIndex = "idx_review_suppressions_fingerprint"
	reviewSuppressionsTable = "review_suppressions"
)

// newTimelineEventTypes 是 0004 为 timeline.event_type 新增的 7 个取值（手册 §4.2）。
//
// 刻意用切片而非 map：断言要按固定顺序逐个插入并报错，map 的随机迭代顺序
// 会让失败信息时左时右。
var newTimelineEventTypes = []string{
	"alert_linked", "acknowledged", "investigation_started",
	"mitigated", "resolved", "ignored", "reopened",
}

// expectedIndexes 列出「up 之后必须存在、down-all 之后必须消失」的索引。
//
// 逐个断言而非只查总数：索引被漏建或漏删时，错误信息能直接点出是哪一个。
var expectedIndexes = []string{
	dedupeIndex, needsReviewIndex,
	timelineIncidentTSIndex, reviewSuppressionsIndex, // 0004
}

// TestMigrateUpDownUp 是 M1 手册 §五 的验收：迁移可重复执行、可完整回滚。
//
// 走 up → down-all → up 三连，并在每个节点断言版本号与索引存在性——
// 只断言「命令没报错」会漏掉「down 没真的删掉」这类问题。
func TestMigrateUpDownUp(t *testing.T) {
	dsn := testsupport.NewEmptyDB(t)
	m := newMigrator(t, dsn)
	defer closeMigrator(t, m)

	// 1) up
	mustUp(t, m)
	if v, dirty := currentVersion(t, m); v != latestVersion || dirty {
		t.Fatalf("up 后版本应为 %d 且非 dirty，实际 version=%d dirty=%v", latestVersion, v, dirty)
	}
	assertIndexesExist(t, dsn, true)

	// 2) down-all
	if err := m.Down(); err != nil {
		t.Fatalf("down-all 失败: %v", err)
	}
	if v, dirty := currentVersion(t, m); v != 0 || dirty {
		t.Fatalf("down-all 后版本应为 0 且非 dirty，实际 version=%d dirty=%v", v, dirty)
	}
	assertIndexesExist(t, dsn, false)

	// 3) 再 up —— 证明可重复执行（"down 未真正回滚" 会在此暴露）
	mustUp(t, m)
	if v, dirty := currentVersion(t, m); v != latestVersion || dirty {
		t.Fatalf("再次 up 后版本应为 %d 且非 dirty，实际 version=%d dirty=%v", latestVersion, v, dirty)
	}
	assertIndexesExist(t, dsn, true)
}

// TestCorrelationM2Migration 覆盖 0003 的两项改动在**数据库层**确实生效且可完整回滚：
// incident_alerts.needs_review 列（REVIEW 档标记）与 correlation_rules 的默认配置行。
//
// 刻意用 Steps(-1) **只回滚 0003**，而不是 Down() 全量回滚：
// 全量回滚会把 0001 建的表一并删掉，届时"默认配置行不存在"是必然的，
// 根本测不出 0003 的 down 是否漏删了种子行。隔离到单步才有判别力。
func TestCorrelationM2Migration(t *testing.T) {
	dsn := testsupport.NewEmptyDB(t)
	m := newMigrator(t, dsn)
	defer closeMigrator(t, m)

	// 1) 全量 up
	mustUp(t, m)
	assertNeedsReviewColumn(t, dsn, true)
	assertDefaultCorrelationRule(t, dsn, true)

	// 2) 只回滚 0003：0001/0002 保持不动，correlation_rules 表仍在。
	//
	// 用 Steps(-2) 而不是 Steps(-1)：0004 落地后「回退一步」落到的已是 0004 自己，
	// 那样测的是 0004 的 down 而非 0003 的。写成 latestVersion-2 的相对形式，
	// 后续再新增迁移也不会把本用例的语义带偏。
	if err := m.Steps(-2); err != nil {
		t.Fatalf("回滚 0004+0003 失败: %v", err)
	}
	if v, dirty := currentVersion(t, m); v != latestVersion-2 || dirty {
		t.Fatalf("回滚两步后版本应为 %d 且非 dirty，实际 version=%d dirty=%v", latestVersion-2, v, dirty)
	}
	assertNeedsReviewColumn(t, dsn, false)
	assertDefaultCorrelationRule(t, dsn, false)
	assertIndexExists(t, dsn, needsReviewIndex, false)
	assertIndexExists(t, dsn, dedupeIndex, true) // 0002 未被回滚，其索引必须还在

	// 3) 重新 up 到最新：验证 0003 可重复执行（幂等）。
	mustUp(t, m)
	if v, dirty := currentVersion(t, m); v != latestVersion || dirty {
		t.Fatalf("再次 up 后版本应为 %d 且非 dirty，实际 version=%d dirty=%v", latestVersion, v, dirty)
	}
	assertNeedsReviewColumn(t, dsn, true)
	assertDefaultCorrelationRule(t, dsn, true)
}

// TestIncidentLifecycleM3Migration 覆盖 0004 的三项改动在**数据库层**确实生效且可单步回滚：
// incidents.status 放宽到 6 值、timeline.event_type 放宽到 12 值、review_suppressions 建表。
//
// 与 TestCorrelationM2Migration 同理，刻意用 Steps(-1) **只回滚 0004**：
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
	// 写成 latestVersion-1：将来再新增迁移时，这里会立刻转红提醒「本用例测的
	// 已不是 0004」，而不是静默改测别的迁移。
	if err := m.Steps(-1); err != nil {
		t.Fatalf("回滚 0004 失败: %v", err)
	}
	if v, dirty := currentVersion(t, m); v != latestVersion-1 || dirty {
		t.Fatalf("回滚一步后版本应为 %d 且非 dirty，实际 version=%d dirty=%v", latestVersion-1, v, dirty)
	}
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
	if v, dirty := currentVersion(t, m); v != latestVersion || dirty {
		t.Fatalf("再次 up 后版本应为 %d 且非 dirty，实际 version=%d dirty=%v", latestVersion, v, dirty)
	}
	assertTableExists(t, dsn, reviewSuppressionsTable, true)
	withPool(t, dsn, func(ctx context.Context, pool *pgxpool.Pool) {
		// 二次 up 后 acknowledged 重新可用（三连可重复的最终证明）。
		insertIncident(t, ctx, pool, "acknowledged")
	})
}

// TestFingerprintFiringUniqueness 验证 0002 的部分唯一索引在**数据库层**真的拦得住
// 重复的 firing，同时不阻断 resolved 的历史行（部分索引的关键语义）。
func TestFingerprintFiringUniqueness(t *testing.T) {
	pool := testsupport.NewDB(t)
	ctx := context.Background()

	const insertPlain = `
INSERT INTO alerts (fingerprint, source, severity, status, labels, raw_payload, starts_at)
VALUES ($1, 'alertmanager', 'warning', $2, '{}'::jsonb, '{}'::jsonb, NOW())`

	const fingerprint = "fp-test-firing-unique"

	// 第一条 firing：应当成功。
	if _, err := pool.Exec(ctx, insertPlain, fingerprint, "firing"); err != nil {
		t.Fatalf("首条 firing 插入失败: %v", err)
	}

	// 第二条同指纹 firing：必须被唯一索引拒绝（SQLSTATE 23505）。
	_, err := pool.Exec(ctx, insertPlain, fingerprint, "firing")
	if err == nil {
		t.Fatal("同指纹的第二条 firing 应被唯一索引拒绝，实际插入成功")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("期望 SQLSTATE 23505（唯一性冲突），实际: %v", err)
	}

	// 生产代码走的是 ON CONFLICT DO NOTHING：应当静默跳过，影响 0 行。
	const insertConflict = `
INSERT INTO alerts (fingerprint, source, severity, status, labels, raw_payload, starts_at)
VALUES ($1, 'alertmanager', 'warning', 'firing', '{}'::jsonb, '{}'::jsonb, NOW())
ON CONFLICT (fingerprint) WHERE status = 'firing' DO NOTHING`

	tag, err := pool.Exec(ctx, insertConflict, fingerprint)
	if err != nil {
		t.Fatalf("ON CONFLICT DO NOTHING 不应报错: %v", err)
	}
	if tag.RowsAffected() != 0 {
		t.Errorf("ON CONFLICT 命中时应影响 0 行，实际 %d 行", tag.RowsAffected())
	}

	// resolved 行不受该索引约束 —— 必须能有多条历史，否则「起火→恢复→再起火」会被卡死。
	if _, err := pool.Exec(ctx,
		`UPDATE alerts SET status='resolved', ends_at=NOW() WHERE fingerprint=$1`, fingerprint); err != nil {
		t.Fatalf("置为 resolved 失败: %v", err)
	}
	if _, err := pool.Exec(ctx, insertPlain, fingerprint, "resolved"); err != nil {
		t.Fatalf("resolved 行不应受 firing 唯一索引约束，插入失败: %v", err)
	}
	if _, err := pool.Exec(ctx, insertPlain, fingerprint, "firing"); err != nil {
		t.Fatalf("原 firing 行已转 resolved，新的 firing 应可插入，失败: %v", err)
	}

	var total int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM alerts WHERE fingerprint=$1`, fingerprint).Scan(&total); err != nil {
		t.Fatalf("统计行数失败: %v", err)
	}
	if total != 3 {
		t.Errorf("期望 3 条历史行（firing → resolved → resolved/firing），实际 %d 条", total)
	}
}

// --- 测试内小工具 ---

func newMigrator(t *testing.T, dsn string) *migrate.Migrate {
	t.Helper()
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		t.Fatalf("读取嵌入迁移失败: %v", err)
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, dsn)
	if err != nil {
		t.Fatalf("初始化迁移器失败: %v", err)
	}
	return m
}

func closeMigrator(t *testing.T, m *migrate.Migrate) {
	t.Helper()
	// migrate.Close 返回 (sourceErr, databaseErr)，此处忽略（用例已结束）。
	_, _ = m.Close()
}

func mustUp(t *testing.T, m *migrate.Migrate) {
	t.Helper()
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate up 失败: %v", err)
	}
}

// currentVersion 读取当前版本；已全部回滚（NilVersion）时归一化为 0。
func currentVersion(t *testing.T, m *migrate.Migrate) (version uint, dirty bool) {
	t.Helper()
	v, d, err := m.Version()
	if errors.Is(err, migrate.ErrNilVersion) {
		return 0, false
	}
	if err != nil {
		t.Fatalf("读取迁移版本失败: %v", err)
	}
	return v, d
}

// withPool 在一次性测试库上开一个短连接池执行断言，用后即关。
func withPool(t *testing.T, dsn string, fn func(ctx context.Context, pool *pgxpool.Pool)) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	defer pool.Close()
	fn(ctx, pool)
}

// assertIndexExists 断言单个索引存在（want=true）或不存在（want=false）。
func assertIndexExists(t *testing.T, dsn, indexName string, want bool) {
	t.Helper()
	withPool(t, dsn, func(ctx context.Context, pool *pgxpool.Pool) {
		var count int
		err := pool.QueryRow(ctx,
			`SELECT count(*) FROM pg_indexes WHERE schemaname = 'public' AND indexname = $1`,
			indexName).Scan(&count)
		if err != nil {
			t.Fatalf("查询索引 %s 存在性失败: %v", indexName, err)
		}
		if want && count != 1 {
			t.Errorf("期望索引 %s 存在，实际未找到", indexName)
		}
		if !want && count != 0 {
			t.Errorf("期望索引 %s 已被回滚删除，实际仍然存在", indexName)
		}
	})
}

// assertIndexesExist 对 expectedIndexes 逐个断言（up 后全在 / down-all 后全无）。
func assertIndexesExist(t *testing.T, dsn string, want bool) {
	t.Helper()
	for _, indexName := range expectedIndexes {
		assertIndexExists(t, dsn, indexName, want)
	}
}

// assertTableExists 断言单张表存在（want=true）或不存在（want=false）。
//
// 只认 public schema：information_schema.tables 会带上系统表，
// 不限定 schema 会让「表被删掉」这类断言被同名系统表蒙混过关。
func assertTableExists(t *testing.T, dsn, tableName string, want bool) {
	t.Helper()
	withPool(t, dsn, func(ctx context.Context, pool *pgxpool.Pool) {
		var exists bool
		err := pool.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1 FROM information_schema.tables
    WHERE table_schema = 'public' AND table_name = $1
)`, tableName).Scan(&exists)
		if err != nil {
			t.Fatalf("查询表 %s 存在性失败: %v", tableName, err)
		}
		if want && !exists {
			t.Errorf("期望表 %s 存在，实际未找到", tableName)
		}
		if !want && exists {
			t.Errorf("期望表 %s 已被回滚删除，实际仍然存在", tableName)
		}
	})
}

// insertIncident 插入一条指定状态的事件并返回其 id，供 timeline 断言做外键用。
func insertIncident(t *testing.T, ctx context.Context, pool *pgxpool.Pool, status string) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(ctx,
		`INSERT INTO incidents (title, severity, status, service)
		 VALUES ($1, 'warning', $2, 'checkout') RETURNING id`,
		"m3-lifecycle-"+status, status).Scan(&id)
	if err != nil {
		t.Fatalf("插入 status=%q 的事件应成功，实际失败: %v", status, err)
	}
	return id
}

// assertRejected 断言一条 SQL 被数据库拒绝，且 SQLSTATE 恰为期望值。
//
// 必须比对 SQLSTATE 而不只是「有错」：CHECK 违反是 23514、唯一冲突是 23505，
// 只看「报错」会把「语法写错」和「约束生效」混为一谈，测试就失去判别力。
func assertRejected(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sqlState, stmt string, args ...any) {
	t.Helper()
	_, err := pool.Exec(ctx, stmt, args...)
	if err == nil {
		t.Fatalf("期望该 SQL 被拒绝（SQLSTATE %s），实际执行成功: %s", sqlState, stmt)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("期望 *pgconn.PgError，实际: %v", err)
	}
	if pgErr.Code != sqlState {
		t.Fatalf("期望 SQLSTATE %s，实际 %s（%v）", sqlState, pgErr.Code, err)
	}
}

// assertNeedsReviewColumn 断言 0003 新增的 incident_alerts.needs_review 列存在性及其约束。
func assertNeedsReviewColumn(t *testing.T, dsn string, want bool) {
	t.Helper()
	withPool(t, dsn, func(ctx context.Context, pool *pgxpool.Pool) {
		var isNullable, columnDefault string
		err := pool.QueryRow(ctx, `
SELECT is_nullable, COALESCE(column_default, '')
FROM information_schema.columns
WHERE table_schema = 'public' AND table_name = 'incident_alerts' AND column_name = 'needs_review'`,
		).Scan(&isNullable, &columnDefault)

		if errors.Is(err, pgx.ErrNoRows) {
			if want {
				t.Fatal("期望 incident_alerts.needs_review 列存在，实际未找到")
			}
			return
		}
		if err != nil {
			t.Fatalf("查询 needs_review 列失败: %v", err)
		}
		if !want {
			t.Fatal("期望 incident_alerts.needs_review 列已被回滚删除，实际仍然存在")
		}
		// NOT NULL + DEFAULT FALSE：读端无需处理 NULL，且既有行为 false（不误标待确认）。
		if isNullable != "NO" {
			t.Errorf("needs_review 应为 NOT NULL，实际 is_nullable=%s", isNullable)
		}
		if !strings.Contains(strings.ToLower(columnDefault), "false") {
			t.Errorf("needs_review 默认值应为 false，实际 column_default=%q", columnDefault)
		}
	})
}

// assertDefaultCorrelationRule 断言 0003 种子的默认关联配置行存在性与其内容。
//
// 这里把 V1.0 §5 的五维权重与三档阈值**硬编码**进断言：默认配置是对外承诺的一部分，
// 迁移里的种子值被改动时必须让测试失败，而不是被悄悄带过。
func assertDefaultCorrelationRule(t *testing.T, dsn string, want bool) {
	t.Helper()
	withPool(t, dsn, func(ctx context.Context, pool *pgxpool.Pool) {
		// 先判断表本身是否存在：若调用方刚做过全量 down，0001 建的表已全被删除，
		// 此时"配置行不存在"是必然的，直接查询会报 42P01 而不是给出结论。
		var tableExists bool
		if err := pool.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1 FROM information_schema.tables
    WHERE table_schema = 'public' AND table_name = 'correlation_rules'
)`).Scan(&tableExists); err != nil {
			t.Fatalf("查询 correlation_rules 表存在性失败: %v", err)
		}
		if !tableExists {
			if want {
				t.Fatal("期望 correlation_rules 表存在，实际未找到")
			}
			return
		}

		var count int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM correlation_rules WHERE name = 'default'`).Scan(&count); err != nil {
			t.Fatalf("统计默认关联配置行失败: %v", err)
		}
		if !want {
			if count != 0 {
				t.Fatalf("默认关联配置行应已被回滚删除，实际仍有 %d 行", count)
			}
			return
		}
		if count != 1 {
			t.Fatalf("默认关联配置应恰好 1 行，实际 %d 行", count)
		}

		var enabled bool
		var wTime, wService, wLabel, wDependency, wHistorical, merge, review float64
		err := pool.QueryRow(ctx, `
SELECT enabled,
       (rule_config->'weights'->>'time')::double precision,
       (rule_config->'weights'->>'service')::double precision,
       (rule_config->'weights'->>'label')::double precision,
       (rule_config->'weights'->>'dependency')::double precision,
       (rule_config->'weights'->>'historical')::double precision,
       (rule_config->'thresholds'->>'merge')::double precision,
       (rule_config->'thresholds'->>'review')::double precision
FROM correlation_rules WHERE name = 'default'`).
			Scan(&enabled, &wTime, &wService, &wLabel, &wDependency, &wHistorical, &merge, &review)
		if err != nil {
			t.Fatalf("读取默认关联配置失败: %v", err)
		}

		if !enabled {
			t.Error("默认关联配置应 enabled = TRUE")
		}

		wantWeights := []struct {
			name string
			got  float64
			want float64
		}{
			{"time", wTime, 0.25},
			{"service", wService, 0.25},
			{"label", wLabel, 0.20},
			{"dependency", wDependency, 0.20},
			{"historical", wHistorical, 0.10},
		}
		sum := 0.0
		// 刻意用切片而非 map：map 的迭代顺序在 Go 里是随机的，
		// 而浮点求和对顺序敏感，会让这条断言时红时绿。
		for _, w := range wantWeights {
			if w.got != w.want {
				t.Errorf("权重 %s = %v，期望 %v", w.name, w.got, w.want)
			}
			sum += w.got
		}
		// 逐项比对可以精确；但求和必须留容差：
		// IEEE 754 下 0.25+0.25+0.20+0.20+0.10 = 0.9999999999999999，
		// 与产品侧 Config.Validate 的 weightSumEpsilon 口径保持一致。
		if math.Abs(sum-1.0) > 0.001 {
			t.Errorf("五维权重之和 = %v，期望 1.0", sum)
		}

		if merge != 0.80 {
			t.Errorf("merge 阈值 = %v，期望 0.80", merge)
		}
		if review != 0.50 {
			t.Errorf("review 阈值 = %v，期望 0.50", review)
		}
	})
}
