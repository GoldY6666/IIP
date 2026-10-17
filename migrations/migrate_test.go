// Package migrations_test 以**外部测试包**形式编写。
//
// 必须用外部测试包：本用例要通过 testsupport 拿到一次性库，而 testsupport 依赖
// internal/store，store 又 import 本包（embed 迁移文件）——若写成包内测试
// （package migrations）就会构成 import cycle。外部测试包正是为切断此类环而存在。
package migrations_test

import (
	"context"
	"errors"
	"io/fs"
	"math"
	"path"
	"strconv"
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
	latest := latestVersion(t)
	if v, dirty := currentVersion(t, m); v != latest || dirty {
		t.Fatalf("up 后版本应为 %d 且非 dirty，实际 version=%d dirty=%v", latest, v, dirty)
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
	if v, dirty := currentVersion(t, m); v != latest || dirty {
		t.Fatalf("再次 up 后版本应为 %d 且非 dirty，实际 version=%d dirty=%v", latest, v, dirty)
	}
	assertIndexesExist(t, dsn, true)
}

// TestCorrelationM2Migration 覆盖 0003 的两项改动在**数据库层**确实生效且可完整回滚：
// incident_alerts.needs_review 列（REVIEW 档标记）与 correlation_rules 的默认配置行。
//
// 刻意**只回滚 0003**，而不是 Down() 全量回滚：
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
	// 用 mustDownTo(绝对版本) 而不是 Steps(-N)：后者「退几步」的语义随迁移总数
	// 漂移。M4 新增 0005/0006/0007 之后，Steps(-2) 落到的是 0005 而不是 0003，
	// 断言「版本应为 latestVersion-2」也跟着一起漂 —— 于是本用例转红，
	// 而被测的 0003 的 down 从头到尾没被测过。绝对版本不随库变化。
	mustDownTo(t, m, migrationCorrelation)
	assertNeedsReviewColumn(t, dsn, false)
	assertDefaultCorrelationRule(t, dsn, false)
	assertIndexExists(t, dsn, needsReviewIndex, false)
	assertIndexExists(t, dsn, dedupeIndex, true) // 0002 未被回滚，其索引必须还在

	// 3) 重新 up 到最新：验证 0003 可重复执行（幂等）。
	mustUp(t, m)
	want := latestVersion(t)
	if v, dirty := currentVersion(t, m); v != want || dirty {
		t.Fatalf("再次 up 后版本应为 %d 且非 dirty，实际 version=%d dirty=%v", want, v, dirty)
	}
	assertNeedsReviewColumn(t, dsn, true)
	assertDefaultCorrelationRule(t, dsn, true)
}

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

// TestFingerprintFiringUniqueness 验证 firing 唯一性在**数据库层**真的拦得住
// 重复投递，同时不阻断 resolved 的历史行（部分索引的关键语义）。
//
// **T7.1（ADR-022）起语义收敛**：alerts 按 starts_at 月度分区，唯一索引从
// (fingerprint) 扩为 (fingerprint, starts_at) —— 「同指纹同时最多一条 firing」
// 收敛为「同指纹 + 同 starts_at 最多一条 firing」。Alertmanager 重发不重置
// startsAt，故「并发投递同一条告警」仍被兜底拦截；「同指纹不同 startsAt 的
// 重火并存」成为已知缺口（由 ScanFiringDuplicates 曝光），本用例把该边界
// 如实钉住 —— 写它不是为「允许」它，而是让「有人无意中改窄/改宽索引」时
// 有断言可依（改回单列 fingerprint 会让下面的边界用例以唯一冲突失败）。
//
// starts_at 必须在**运行当月**内取值：分区只覆盖迁移预建的 [当月, 当月+2]，
// 写死历史月份会让插入以 23514 失败——那是分区缺失，不是唯一性失效。
func TestFingerprintFiringUniqueness(t *testing.T) {
	pool := testsupport.NewDB(t)
	ctx := context.Background()

	// 当月 10 号 12:00：固定值（同指纹两条必须 starts_at 完全相同才冲突），
	// 又必定落在当月分区内（10 号不跨月，与运行日期无关）。
	now := time.Now().UTC()
	sameMonth := time.Date(now.Year(), now.Month(), 10, 12, 0, 0, 0, time.UTC)
	nextMonth := sameMonth.AddDate(0, 1, 0)

	const insertPlain = `
INSERT INTO alerts (fingerprint, source, severity, status, labels, raw_payload, starts_at)
VALUES ($1, 'alertmanager', 'warning', $2, '{}'::jsonb, '{}'::jsonb, $3)`

	const fingerprint = "fp-test-firing-unique"

	// 第一条 firing：应当成功。
	if _, err := pool.Exec(ctx, insertPlain, fingerprint, "firing", sameMonth); err != nil {
		t.Fatalf("首条 firing 插入失败: %v", err)
	}

	// 第二条同指纹 firing（starts_at 相同）：必须被唯一索引拒绝（SQLSTATE 23505）。
	_, err := pool.Exec(ctx, insertPlain, fingerprint, "firing", sameMonth)
	if err == nil {
		t.Fatal("同指纹同 starts_at 的第二条 firing 应被唯一索引拒绝，实际插入成功")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("期望 SQLSTATE 23505（唯一性冲突），实际: %v", err)
	}

	// 生产代码走的是 ON CONFLICT (fingerprint, starts_at) DO NOTHING：
	// 应当静默跳过，影响 0 行。
	// 注意参数序号：status 是字面量 'firing'（不再占一个参数位），starts_at
	// 因此是 $2 而不是 $3 —— 曾写成 $3 配 2 个实参，pgx 重编号后 PostgreSQL
	// 报 42P18「无法确定参数 $2 的数据类型」。显式 ::varchar / ::timestamptz
	// 让推断与序号脱钩，防同一类坑复发。
	const insertConflict = `
INSERT INTO alerts (fingerprint, source, severity, status, labels, raw_payload, starts_at)
VALUES ($1::varchar, 'alertmanager', 'warning', 'firing', '{}'::jsonb, '{}'::jsonb, $2::timestamptz)
ON CONFLICT (fingerprint, starts_at) WHERE status = 'firing' DO NOTHING`

	tag, err := pool.Exec(ctx, insertConflict, fingerprint, sameMonth)
	if err != nil {
		t.Fatalf("ON CONFLICT DO NOTHING 不应报错: %v", err)
	}
	if tag.RowsAffected() != 0 {
		t.Errorf("ON CONFLICT 命中时应影响 0 行，实际 %d 行", tag.RowsAffected())
	}

	// resolved 行不受该索引约束 —— 必须能有多条历史，否则「起火→恢复→再起火」会被卡死。
	if _, err := pool.Exec(ctx,
		`UPDATE alerts SET status='resolved', ends_at=NOW() WHERE fingerprint=$1 AND starts_at=$2`,
		fingerprint, sameMonth); err != nil {
		t.Fatalf("置为 resolved 失败: %v", err)
	}
	if _, err := pool.Exec(ctx, insertPlain, fingerprint, "resolved", sameMonth); err != nil {
		t.Fatalf("resolved 行不应受 firing 唯一索引约束，插入失败: %v", err)
	}
	if _, err := pool.Exec(ctx, insertPlain, fingerprint, "firing", sameMonth); err != nil {
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

	// **T7.1 新边界（ADR-022 的已知缺口）**：同指纹、不同 starts_at 的 firing
	// 可以并存 —— 分区键加入唯一索引后的语义收敛，配套缓解是应用层
	// LatestByFingerprint 去重 + ScanFiringDuplicates 定期曝光。
	if _, err := pool.Exec(ctx, insertPlain, fingerprint, "firing", nextMonth); err != nil {
		t.Fatalf("同指纹次月 starts_at 的 firing 应可并存（分区后语义收敛），失败: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM alerts WHERE fingerprint=$1 AND status='firing'`, fingerprint).Scan(&total); err != nil {
		t.Fatalf("统计 firing 行数失败: %v", err)
	}
	if total != 2 {
		t.Errorf("同指纹跨 starts_at 应并存 2 条 firing（ADR-022 收敛后语义），实际 %d 条", total)
	}
}

// migrationAlertsPartition 是 0008（alerts 月度分区，T7.1 / ADR-022）。
//
// 「回滚到 0008 之后」= 版本 7，因为 migrate 的版本号是**已应用的最大编号**。
// 写成 7 而不是 8 是这一行的全部含义（理由同 migrationIncidentLifecycle）。
const migrationAlertsPartition = 7

// TestAlertsPartitionMigration 覆盖 0008 的五项改动在**数据库层**确实生效
// 且可单步回滚：分区表形态、复合 PK、alert_starts_at 列 + 复合 FK、
// 两个维护函数、当月分区。
//
// 与 0003 / 0004 的单步回滚用例同理：刻意只回滚 0008 —— 全量回滚会把
// 0001 建的表一并删掉，届时「列不存在」是必然的，根本测不出 0008 的 down
// 是否真的把分区形态还原了。隔离到单步才有判别力。
func TestAlertsPartitionMigration(t *testing.T) {
	dsn := testsupport.NewEmptyDB(t)
	m := newMigrator(t, dsn)
	defer closeMigrator(t, m)

	// 1) 全量 up
	mustUp(t, m)
	if v, dirty := currentVersion(t, m); v != latestVersion(t) || dirty {
		t.Fatalf("up 后版本应为 %d 且非 dirty，实际 version=%d dirty=%v", latestVersion(t), v, dirty)
	}
	assertAlertsPartitioned(t, dsn, true)

	// 2) 只回滚 0008：0001-0007 保持不动，alerts 回到单表形态。
	mustDownTo(t, m, migrationAlertsPartition)
	assertAlertsPartitioned(t, dsn, false)

	// 3) 重新 up：证明可重复执行（幂等）。
	mustUp(t, m)
	if v, dirty := currentVersion(t, m); v != latestVersion(t) || dirty {
		t.Fatalf("再次 up 后版本应为 %d 且非 dirty，实际 version=%d dirty=%v", latestVersion(t), v, dirty)
	}
	assertAlertsPartitioned(t, dsn, true)
}

// assertAlertsPartitioned 断言 0008 的分区形态（want=true）或回滚后的单表形态。
//
// 六项逐一断言而非只查「表在不在」：0008 的 down 任何一项漏还原
// （函数没删、列没删、FK 没退回）都会让后续 up 撞「已存在」错误，
// 而那在「只断言表存在」的用例里全是绿的。
func assertAlertsPartitioned(t *testing.T, dsn string, want bool) {
	t.Helper()
	withPool(t, dsn, func(ctx context.Context, pool *pgxpool.Pool) {
		// ① alerts 表形态：分区父表 relkind='p'，普通表 relkind='r'。
		var relkind string
		if err := pool.QueryRow(ctx,
			`SELECT relkind::text FROM pg_class WHERE oid = 'alerts'::regclass`).Scan(&relkind); err != nil {
			t.Fatalf("查询 alerts 表形态失败: %v", err)
		}
		if want && relkind != "p" {
			t.Errorf("up 后 alerts 应为分区父表（relkind=p），实际 %q", relkind)
		}
		if !want && relkind != "r" {
			t.Errorf("回滚后 alerts 应为普通表（relkind=r），实际 %q", relkind)
		}

		// ② 分区键 starts_at 在 PK 里（分区表硬性要求；回滚后 PK 退回单列 id）。
		var pkHasPartitionKey int
		if err := pool.QueryRow(ctx, `
SELECT count(*)
FROM pg_index i
JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = ANY(i.indkey)
WHERE i.indrelid = 'alerts'::regclass AND i.indisprimary AND a.attname = 'starts_at'`,
		).Scan(&pkHasPartitionKey); err != nil {
			t.Fatalf("查询 alerts 主键列失败: %v", err)
		}
		if want && pkHasPartitionKey != 1 {
			t.Error("up 后 alerts 主键应含分区键 starts_at")
		}
		if !want && pkHasPartitionKey != 0 {
			t.Error("回滚后 alerts 主键不应再含 starts_at")
		}

		// ③ incident_alerts.alert_starts_at 列存在性 + NOT NULL。
		var isNullable string
		err := pool.QueryRow(ctx, `
SELECT is_nullable FROM information_schema.columns
WHERE table_schema = 'public' AND table_name = 'incident_alerts' AND column_name = 'alert_starts_at'`,
		).Scan(&isNullable)
		if errors.Is(err, pgx.ErrNoRows) {
			if want {
				t.Error("up 后 incident_alerts.alert_starts_at 列应存在")
			}
		} else if err != nil {
			t.Fatalf("查询 alert_starts_at 列失败: %v", err)
		} else if !want {
			t.Error("回滚后 alert_starts_at 列应被删除")
		} else if isNullable != "NO" {
			t.Errorf("alert_starts_at 应为 NOT NULL，实际 is_nullable=%s", isNullable)
		}

		// ④ incident_alerts → alerts 的外键列数：复合 FK 两列，回滚后单列。
		var fkCols int
		if err := pool.QueryRow(ctx, `
SELECT COALESCE(array_length(conkey, 1), 0)
FROM pg_constraint
WHERE conrelid = 'incident_alerts'::regclass AND contype = 'f'
  AND confrelid = 'alerts'::regclass`).Scan(&fkCols); err != nil {
			t.Fatalf("查询 incident_alerts 外键失败: %v", err)
		}
		if want && fkCols != 2 {
			t.Errorf("up 后 incident_alerts 对 alerts 应为两列复合 FK，实际 %d 列", fkCols)
		}
		if !want && fkCols != 1 {
			t.Errorf("回滚后应退回单列 FK，实际 %d 列", fkCols)
		}

		// ⑤ 两个维护函数存在性（补建 / 过期清理）。
		for _, fn := range []string{"iip_ensure_alert_partitions", "iip_drop_expired_alert_partitions"} {
			var exists bool
			if err := pool.QueryRow(ctx,
				`SELECT EXISTS (SELECT 1 FROM pg_proc WHERE proname = $1)`, fn).Scan(&exists); err != nil {
				t.Fatalf("查询函数 %s 存在性失败: %v", fn, err)
			}
			if want && !exists {
				t.Errorf("up 后函数 %s 应存在", fn)
			}
			if !want && exists {
				t.Errorf("回滚后函数 %s 应被删除", fn)
			}
		}

		// ⑥ 当月分区存在性（仅 up 后断言；回滚后无分区可言）。
		if want {
			var partExists bool
			if err := pool.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1 FROM pg_inherits i
    JOIN pg_class c ON c.oid = i.inhrelid
    WHERE i.inhparent = 'alerts'::regclass
      AND c.relname = 'alerts_' || to_char(date_trunc('month', NOW() AT TIME ZONE 'UTC'), 'YYYY_MM')
)`).Scan(&partExists); err != nil {
				t.Fatalf("查询当月分区存在性失败: %v", err)
			}
			if !partExists {
				t.Error("up 后当月分区应存在")
			}
		}
	})
}

// 被单步回滚用例引用的迁移版本号。
//
// 写常量而非从文件名推导是刻意的：用例要断言的是「回滚到**某个具体迁移之后**」，
// 那个「之后」必须由用例作者明确指定。latestVersion(t) 是「库现在到哪」，
// 两者语义不同：前者是本用例的测试目标，后者是仓库现状。
const (
	// migrationCorrelation 是 0003（correlation：needs_review 列 + 种子行）。
	migrationCorrelation = 2
	// migrationIncidentLifecycle 是 0004（incident 生命周期：status/timeline 放宽 + 抑制表）。
	//
	// 「回滚到 0004 之后」= 版本 3，因为 migrate 的版本号是**已应用的最大编号**。
	// 写成 3 而不是 4 是这一行的全部含义，写错会让回滚多退或少退一个迁移，
	// 而断言仍可能通过——测的就已经不是 0004 的 down 了。
	migrationIncidentLifecycle = 3
)

// migrationIndexTuning 是 0009（T7.2 索引调优：tasks 表达式索引 + 删 labels GIN）。
// 「回滚到 0009 之后」= 版本 8，理由同 migrationIncidentLifecycle。
const migrationIndexTuning = 8

// tasksPayloadIncidentIndex / alertsLabelsGINIndex 是 0009 直接操作的两个索引名。
const (
	tasksPayloadIncidentIndex = "idx_tasks_payload_incident"
	alertsLabelsGINIndex      = "idx_alerts_labels"
)

// TestIndexTuningMigration 覆盖 0009 的两项改动在**数据库层**确实生效且可单步回滚：
// 建 tasks 的部分表达式索引、删 alerts labels 的 GIN（父表 + 全部分区副本）。
//
// 与 0003 / 0004 / 0008 的单步回滚用例同理：刻意只回滚 0009 —— 全量回滚会把
// 0001 建的表一并删掉，届时「索引不在」是必然的，测不出 0009 的 down 是否真的
// 把 GIN 还原了。隔离到单步才有判别力。
func TestIndexTuningMigration(t *testing.T) {
	dsn := testsupport.NewEmptyDB(t)
	m := newMigrator(t, dsn)
	defer closeMigrator(t, m)

	// 1) 全量 up：新表达式索引在、GIN 没（父表与分区副本都没）。
	mustUp(t, m)
	if v, dirty := currentVersion(t, m); v != latestVersion(t) || dirty {
		t.Fatalf("up 后版本应为 %d 且非 dirty，实际 version=%d dirty=%v", latestVersion(t), v, dirty)
	}
	assertTasksPayloadIncidentIndex(t, dsn, true)
	assertLabelsGIN(t, dsn, false)

	// 2) 只回滚 0009：0001-0008 保持不动，tasks 索引消失、GIN 还原。
	mustDownTo(t, m, migrationIndexTuning)
	assertTasksPayloadIncidentIndex(t, dsn, false)
	assertLabelsGIN(t, dsn, true)

	// 3) 重新 up：证明可重复执行（down 没真还原会在此撞「索引已存在」）。
	mustUp(t, m)
	if v, dirty := currentVersion(t, m); v != latestVersion(t) || dirty {
		t.Fatalf("再次 up 后版本应为 %d 且非 dirty，实际 version=%d dirty=%v", latestVersion(t), v, dirty)
	}
	assertTasksPayloadIncidentIndex(t, dsn, true)
	assertLabelsGIN(t, dsn, false)
}

// assertTasksPayloadIncidentIndex 断言 idx_tasks_payload_incident 的存在性与定义。
//
// 为什么断言**定义**而不只查存在性：0009 的裁定全部来自 EXPLAIN 实测（ADR-023）——
// 键列或谓词写错（运算符用 -> 而非 ->>、漏掉 WHERE kind、键名拼错）都会让索引
// 建出来却罩不住目标查询，而「索引存在」的断言对此全绿。定义里必须同时含
// 表达式键与部分谓词，缺一即红。
//
// 用子串包含而非整串相等：pg_get_indexdef 的确切格式随 PG 小版本有差异
// （括号层数、::text 渲染位置），而不变量是「运算符 + 键名 + 谓词」这三样。
func assertTasksPayloadIncidentIndex(t *testing.T, dsn string, want bool) {
	t.Helper()
	withPool(t, dsn, func(ctx context.Context, pool *pgxpool.Pool) {
		var def string
		err := pool.QueryRow(ctx, `
SELECT pg_get_indexdef(i.indexrelid)
FROM pg_index i
WHERE i.indexrelid = to_regclass($1)`, tasksPayloadIncidentIndex).Scan(&def)
		if errors.Is(err, pgx.ErrNoRows) {
			if want {
				t.Errorf("up 后 %s 应存在", tasksPayloadIncidentIndex)
			}
			return
		}
		if err != nil {
			t.Fatalf("查询 %s 定义失败: %v", tasksPayloadIncidentIndex, err)
		}
		if !want {
			t.Errorf("回滚后 %s 应被删除，实际仍在: %s", tasksPayloadIncidentIndex, def)
			return
		}
		// 键列必须是 ->>（取 text）：用 -> 会得到 jsonb，与消费查询传入的
		// incident_id 参数类型对不上，索引建了也走不得。
		if !strings.Contains(def, "payload ->> 'incident_id'") {
			t.Errorf("索引键应为表达式 (payload->>'incident_id')，实际定义: %s", def)
		}
		// 谓词必须落在 kind 上且值为 collect_evidence：两条消费查询
		// （LastCollectWindowEnd / EnqueueTaskIfAbsent）都带该条件，
		// 漏掉它会把其他任务类型吸进索引。
		// 判法刻意不绑完整子串：pg_get_indexdef 把谓词渲染为
		// `((kind)::text = 'collect_evidence'::text)`（列名带括号与 ::text 投射），
		// 且括号层数随 PG 小版本有差异 —— 绑死完整串就是假红判据
		// （2026-10-17 实测踩过：产品正确、断言先红）。
		// 不变量取「WHERE 存在 + 谓词同时提到 kind 与 collect_evidence」：
		// 定义里 kind 只可能出现在谓词中（键是 payload 表达式），三者齐备即命中。
		if !strings.Contains(def, "WHERE") ||
			!strings.Contains(def, "kind") ||
			!strings.Contains(def, "'collect_evidence'") {
			t.Errorf("索引应带部分谓词 kind='collect_evidence'，实际定义: %s", def)
		}
	})
}

// assertLabelsGIN 断言 idx_alerts_labels（alerts 分区父表上的 GIN）的存在性。
//
// 父表与分区副本一起数，且要求副本数 == 分区数：0008 重建的 GIN 是**分区索引**，
// 每个分区各有一份副本。只查父表名会漏掉「副本残留」；不比分区数则会漏掉
// 「还原出来的其实不是分区索引」。down 重建 → up 再删的往返必须两头都归零/齐全。
func assertLabelsGIN(t *testing.T, dsn string, want bool) {
	t.Helper()
	withPool(t, dsn, func(ctx context.Context, pool *pgxpool.Pool) {
		var parentExists bool
		var childCount, partitionCount int
		err := pool.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM pg_class WHERE oid = to_regclass($1)),
       (SELECT count(*) FROM pg_inherits i
          JOIN pg_class c ON c.oid = i.inhrelid
         WHERE i.inhparent = to_regclass($1)),
       (SELECT count(*) FROM pg_inherits i
          JOIN pg_class c ON c.oid = i.inhrelid
         WHERE i.inhparent = 'alerts'::regclass)`,
			alertsLabelsGINIndex).Scan(&parentExists, &childCount, &partitionCount)
		if err != nil {
			t.Fatalf("查询 %s 存在性失败: %v", alertsLabelsGINIndex, err)
		}
		if want {
			if !parentExists {
				t.Errorf("回滚后 %s 应被还原", alertsLabelsGINIndex)
			}
			if childCount != partitionCount || partitionCount == 0 {
				t.Errorf("还原后的 %s 应为覆盖全部分区的分区索引，"+
					"实际分区副本 %d 个 / 分区 %d 个", alertsLabelsGINIndex, childCount, partitionCount)
			}
			return
		}
		if parentExists {
			t.Errorf("up 后 %s（零读者 GIN）应被删除，实际仍在父表上", alertsLabelsGINIndex)
		}
		if childCount != 0 {
			t.Errorf("up 后 %s 的全部分区副本应随父索引一并删除，实际残留 %d 个",
				alertsLabelsGINIndex, childCount)
		}
	})
}

// latestVersion 是当前迁移文件到达的版本号。
//
// **从 embed 里数出来，不是手写常量** —— 原先是 `const latestVersion = 4`，
// 于是 M4 新增 0005/0006/0007 之后它没跟着改，而依赖它的断言全部失真：
// 「回滚一步后版本应为 latestVersion-1」实际落到的已不是本用例要测的迁移。
// 表现是两条历史用例（0003 / 0004 的单步回滚）转红，而产品行为完全正确。
//
// 这正是手写版本号的固有风险：它要求每个写迁移的人记得同步改一个数字，
// 而「记得」不是可验证的。数出来则把同步成本降到零。
func latestVersion(t *testing.T) uint {
	t.Helper()
	entries, err := fs.Glob(migrations.FS, "*.up.sql")
	if err != nil {
		t.Fatalf("遍历嵌入迁移失败: %v", err)
	}
	var max uint
	for _, name := range entries {
		base, _, ok := strings.Cut(path.Base(name), "_")
		if !ok {
			t.Fatalf("迁移文件名不规范（应为 NNNN_xxx.up.sql）: %s", name)
		}
		n, err := strconv.ParseUint(base, 10, 32)
		if err != nil {
			t.Fatalf("迁移文件名的版本号不是数字: %s", name)
		}
		if uint(n) > max {
			max = uint(n)
		}
	}
	if max == 0 {
		t.Fatal("未在 embed 中找到任何 .up.sql 迁移")
	}
	return max
}

// mustDownTo 把库回滚到指定版本（**向下迁移，不允许 dirty**）。
//
// 用 Migrate(目标版本) 而不是 Steps(-N)：
//   - Steps(-N) 的语义随迁移总数漂移。新增一个迁移后，「回退一步」
//     落到的就不再是被测的那个，回滚测的全是别的东西——而且**不报错**，
//     只会让「0003 的 down 漏了种子行」这类缺陷蒙混过关。
//   - Migrate(版本) 把「我要退到哪」写死在用例里，与库里有几个迁移无关。
func mustDownTo(t *testing.T, m *migrate.Migrate, target uint) {
	t.Helper()
	if err := m.Migrate(target); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("回滚到版本 %d 失败: %v", target, err)
	}
	if v, dirty := currentVersion(t, m); v != target || dirty {
		t.Fatalf("回滚后版本应为 %d 且非 dirty，实际 version=%d dirty=%v",
			target, v, dirty)
	}
}

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
