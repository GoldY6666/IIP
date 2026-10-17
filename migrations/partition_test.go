// Package migrations_test 的外部测试（与 migrate_test.go 同包，复用其助手）。
//
// 本文件覆盖 T7.1（ADR-022）分区维护的**真实数据库行为**：
// 0008 的迁移结构（表形态 / 复合 PK / 复合 FK / 函数 / 当月分区）由
// migrate_test.go 的 TestAlertsPartitionMigration 断言；这里断言
// 「结构之上跑起来的样子」——
//   - 无分区插入的错误码（23514，精确到码）；
//   - 补建的覆盖范围与幂等；
//   - 过期清理的行级 CASCADE（复合 FK 的存在意义）；
//   - 重复 firing 的曝光（ADR-022 语义收敛后的已知缺口）；
//   - 非法保留期必须报错（0 = 删全表）。
//
// 全部走 internal/db 的 sqlc 生成查询（生产同一条路径），
// 不另写一份等价 SQL —— 那会让「生成物与库函数签名漂移」无从发现。
package migrations_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yu/iip/internal/db"
	"github.com/yu/iip/internal/testsupport"
)

// rawInsertAlert 裸 SQL 插一条告警（先补建 starts_at 所在月分区）。
//
// 与 internal/repository 的 insertAlertRow 同思路：本文件测的是数据库行为，
// 借道 repository.Insert 的 23514 重试会把「分区没补建」这件事藏掉。
func rawInsertAlert(t *testing.T, ctx context.Context, conn db.DBTX, fingerprint, status string, startsAt time.Time) int64 {
	t.Helper()
	q := db.New(conn)
	if _, err := q.EnsureAlertPartitions(ctx, db.EnsureAlertPartitionsParams{
		FromTs:      pgtype.Timestamptz{Time: startsAt, Valid: true},
		AheadMonths: 2,
	}); err != nil {
		t.Fatalf("补建 %s 所在月分区失败: %v", startsAt.Format("2006-01"), err)
	}
	var id int64
	err := conn.QueryRow(ctx, `
INSERT INTO alerts (fingerprint, source, severity, status, labels, raw_payload, starts_at)
VALUES ($1, 'alertmanager', 'critical', $2, '{}'::jsonb, '{}'::jsonb, $3)
RETURNING id`, fingerprint, status, startsAt).Scan(&id)
	if err != nil {
		t.Fatalf("插入告警 %s 失败: %v", fingerprint, err)
	}
	return id
}

// rawInsertIncident 裸 SQL 建一个事件，供关联边断言做外键用。
func rawInsertIncident(t *testing.T, ctx context.Context, conn db.DBTX) int64 {
	t.Helper()
	var id int64
	if err := conn.QueryRow(ctx,
		`INSERT INTO incidents (title, severity, status) VALUES ('分区用例事件', 'warning', 'open') RETURNING id`,
	).Scan(&id); err != nil {
		t.Fatalf("插入事件失败: %v", err)
	}
	return id
}

// rawLink 裸 SQL 建一条关联边（带分区键 alert_starts_at）。
func rawLink(t *testing.T, ctx context.Context, conn db.DBTX, incidentID, alertID int64, alertStartsAt time.Time) {
	t.Helper()
	if _, err := conn.Exec(ctx,
		`INSERT INTO incident_alerts (incident_id, alert_id, alert_starts_at) VALUES ($1, $2, $3)`,
		incidentID, alertID, alertStartsAt); err != nil {
		t.Fatalf("建立关联边失败: %v", err)
	}
}

// partitionExists 报告 ts 所在月的分区是否存在。
func partitionExists(t *testing.T, ctx context.Context, conn db.DBTX, ts time.Time) bool {
	t.Helper()
	var exists bool
	if err := conn.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1 FROM pg_inherits i
    JOIN pg_class c ON c.oid = i.inhrelid
    WHERE i.inhparent = 'alerts'::regclass
      AND c.relname = 'alerts_' || to_char(date_trunc('month', $1::timestamptz AT TIME ZONE 'UTC'), 'YYYY_MM')
)`, ts).Scan(&exists); err != nil {
		t.Fatalf("查询分区存在性失败: %v", err)
	}
	return exists
}

// TestInsertWithoutPartitionRejectedWith23514 钉住 T7.1 插入路径的判据：
// 目标月无分区时 PostgreSQL 报的是 **SQLSTATE 23514**，而不是普通错误。
//
// 为什么必须精确到码：repository.Insert 的重试分支靠 IsNoPartition 分类，
// 而它只认 23514。若这里只断言「有错」，那么「错误码变了但测试仍绿」
// 会让重试静默失效 —— 表现是跨月/漂移的告警永久插不进去。
func TestInsertWithoutPartitionRejectedWith23514(t *testing.T) {
	pool := testsupport.NewDB(t)
	ctx := context.Background()

	// now+6 个月：超出迁移预建的 [当月, 当月+2]，必然无分区。
	future := time.Now().UTC().AddDate(0, 6, 3)
	assertRejected(t, ctx, pool, "23514",
		`INSERT INTO alerts (fingerprint, source, severity, status, labels, raw_payload, starts_at)
		 VALUES ($1, 'alertmanager', 'warning', 'firing', '{}'::jsonb, '{}'::jsonb, $2)`,
		"fp-no-part", future)

	// 补建目标月后，同一插入必须成功 ——「码判据」与「补建生效」各占一半。
	q := db.New(pool)
	created, err := q.EnsureAlertPartitions(ctx, db.EnsureAlertPartitionsParams{
		FromTs:      pgtype.Timestamptz{Time: future, Valid: true},
		AheadMonths: 2,
	})
	if err != nil {
		t.Fatalf("补建分区失败: %v", err)
	}
	if len(created) == 0 {
		t.Fatal("目标月无分区，补建应至少新建 1 个")
	}
	if !partitionExists(t, ctx, pool, future) {
		t.Errorf("补建后 %s 月分区应存在，实际未找到（created=%v）", future.Format("2006-01"), created)
	}
	id := rawInsertAlert(t, ctx, pool, "fp-no-part", "firing", future)
	if id == 0 {
		t.Error("补建后插入应返回自增主键")
	}
}

// TestEnsurePartitionsCoversRangeAndIsIdempotent 验证补建的覆盖范围与幂等。
//
// 覆盖范围是硬约束：from_ts 在预建范围之外时，ensure 必须把 [当月, from 月]
// 全部补上（漏一个月，插入就 23514）；重复调用不得新建（不幂等会让
// 维护 tick 每次都可能撞「分区已存在」错误）。
func TestEnsurePartitionsCoversRangeAndIsIdempotent(t *testing.T) {
	pool := testsupport.NewDB(t)
	ctx := context.Background()
	q := db.New(pool)

	ensure := func(from *time.Time) []string {
		t.Helper()
		arg := db.EnsureAlertPartitionsParams{AheadMonths: 2}
		if from != nil {
			arg.FromTs = pgtype.Timestamptz{Time: *from, Valid: true}
		}
		names, err := q.EnsureAlertPartitions(ctx, arg)
		if err != nil {
			t.Fatalf("补建分区失败: %v", err)
		}
		return names
	}

	// 不变量（而非计数）：ensure(NULL, 2) 之后 [当月, 当月+2] 必须全部存在。
	// 刻意不断言「新建了几个」——那取决于迁移的预建范围，是脆弱判据；
	// 跨用例运行时刻的月界漂移会让它假红。
	ensure(nil)
	for i := 0; i <= 2; i++ {
		m := time.Now().UTC().AddDate(0, i, 0)
		if !partitionExists(t, ctx, pool, m) {
			t.Errorf("ensure 后 %s 月分区应存在", m.Format("2006-01"))
		}
	}

	// from=+5 个月：覆盖范围必须扩到 +5 月（含中间每个月）。
	far := time.Now().UTC().AddDate(0, 5, 0)
	ensure(&far)
	for i := 3; i <= 5; i++ {
		m := time.Now().UTC().AddDate(0, i, 0)
		if !partitionExists(t, ctx, pool, m) {
			t.Errorf("from=+5 个月后 %s 月分区应存在", m.Format("2006-01"))
		}
	}

	// 幂等：重复同参不得新建（返回空 = 无需补建）。
	if again := ensure(&far); len(again) != 0 {
		t.Errorf("重复 ensure 不应新建分区，实际 %v", again)
	}
}

// TestDropExpiredCascadesIncidentAlerts 验证过期清理的**行级**语义：
// DELETE 父表行 + 复合 FK 的 ON DELETE CASCADE 清掉关联边；
// 保留期内的行与边原样保留；非法保留期（0）必须报错。
//
// 这条是「复合 FK」的存在意义所在：若 incident_alerts 仍是单列 FK，
// 每个被引用过的旧分区都会挡住 DELETE（对象级依赖，见 0008 头部实证）。
func TestDropExpiredCascadesIncidentAlerts(t *testing.T) {
	pool := testsupport.NewDB(t)
	ctx := context.Background()
	q := db.New(pool)

	old := time.Now().UTC().AddDate(0, 0, -200)
	recent := time.Now().UTC().AddDate(0, 0, -10)

	oldAlert := rawInsertAlert(t, ctx, pool, "fp-exp-old", "firing", old)
	recentAlert := rawInsertAlert(t, ctx, pool, "fp-exp-recent", "firing", recent)
	inc := rawInsertIncident(t, ctx, pool)
	rawLink(t, ctx, pool, inc, oldAlert, old)
	rawLink(t, ctx, pool, inc, recentAlert, recent)

	// 保留 90 天：200 天前的删，10 天前的留。
	deleted, err := q.DropExpiredAlertPartitions(ctx, 90)
	if err != nil {
		t.Fatalf("过期清理失败: %v", err)
	}
	if deleted != 1 {
		t.Errorf("应删除 1 行过期告警，实际 %d", deleted)
	}

	var oldLeft, recentLeft int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM alerts WHERE id = $1`, oldAlert).Scan(&oldLeft); err != nil {
		t.Fatalf("查询旧告警失败: %v", err)
	}
	if oldLeft != 0 {
		t.Error("过期告警行应被删除")
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM alerts WHERE id = $1`, recentAlert).Scan(&recentLeft); err != nil {
		t.Fatalf("查询近期告警失败: %v", err)
	}
	if recentLeft != 1 {
		t.Error("保留期内的告警行不应被删除")
	}

	// 级联：旧告警的关联边必须随之消失；近期告警的边必须保留。
	var oldLinks, recentLinks int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM incident_alerts WHERE alert_id = $1`, oldAlert).Scan(&oldLinks); err != nil {
		t.Fatalf("查询旧关联边失败: %v", err)
	}
	if oldLinks != 0 {
		t.Errorf("过期告警的关联边应被级联删除，实际残留 %d 条", oldLinks)
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM incident_alerts WHERE alert_id = $1`, recentAlert).Scan(&recentLinks); err != nil {
		t.Fatalf("查询近期关联边失败: %v", err)
	}
	if recentLinks != 1 {
		t.Error("保留期内告警的关联边不应被删除")
	}

	// 非法保留期：0 = 删全表，必须是错误而不是静默执行。
	if _, err := pool.Exec(ctx, `SELECT iip_drop_expired_alert_partitions(0)`); err == nil {
		t.Error("retention_days=0 必须报错（那会删掉全部告警）")
	}
	if _, err := pool.Exec(ctx, `SELECT iip_drop_expired_alert_partitions(-1)`); err == nil {
		t.Error("retention_days<0 必须报错")
	}
}

// TestScanFiringDuplicatesExposesCrossStartsAt 验证重复 firing 扫描：
// 同指纹、相邻两个 starts_at 的 firing 并存（ADR-022 收敛后的已知缺口）
// 必须被扫出来；不同指纹不构成重复。
//
// 走 db.Queries.ScanFiringDuplicates（生成物）而非另写 SQL ——
// 聚合 CAST（bigint[] / timestamptz）写错时这里会直接红。
func TestScanFiringDuplicatesExposesCrossStartsAt(t *testing.T) {
	pool := testsupport.NewDB(t)
	ctx := context.Background()

	now := time.Now().UTC()
	m1 := time.Date(now.Year(), now.Month(), 10, 8, 0, 0, 0, time.UTC)
	m2 := m1.AddDate(0, 1, 0)
	id1 := rawInsertAlert(t, ctx, pool, "fp-dup", "firing", m1)
	id2 := rawInsertAlert(t, ctx, pool, "fp-dup", "firing", m2)
	rawInsertAlert(t, ctx, pool, "fp-solo", "firing", m1)

	dups, err := db.New(pool).ScanFiringDuplicates(ctx)
	if err != nil {
		t.Fatalf("扫描重复 firing 失败: %v", err)
	}
	if len(dups) != 1 {
		t.Fatalf("应恰好扫出 1 组重复（fp-dup），实际 %d 组: %+v", len(dups), dups)
	}
	d := dups[0]
	if d.Fingerprint != "fp-dup" {
		t.Errorf("重复组指纹 = %q，期望 fp-dup", d.Fingerprint)
	}
	if d.FiringCount != 2 {
		t.Errorf("重复组 firing 数 = %d，期望 2", d.FiringCount)
	}
	wantIDs := []int64{id1, id2}
	if id1 > id2 {
		wantIDs = []int64{id2, id1} // array_agg 按 id 升序
	}
	if len(d.AlertIds) != 2 || d.AlertIds[0] != wantIDs[0] || d.AlertIds[1] != wantIDs[1] {
		t.Errorf("重复组 alert_ids = %v，期望 %v（按 id 升序）", d.AlertIds, wantIDs)
	}
}

// TestPartitionBoundsAreUtcMonthRegardlessOfSessionTz 钉住分区边界的
// **时区无关性**：无论会话时区是什么，分区边界都必须是 UTC 月界，
// 行严格按 starts_at 的 UTC 月份归属分区。
//
// 为什么不能只靠默认会话时区顺带覆盖（本项目的默认测试会话是 +08）：
//   - +08 会话下，边界 bug 的形态是「补建即重叠报错」——响，但换成 UTC
//     会话的机器就静默了，测试全绿而边界全错；
//   - 负偏移会话（本用例显式 SET America/New_York）下，bug 不报错而是
//     **静默错位**：UTC 月初落进上一月分区。只在前一种环境跑测试，
//     永远发现不了后一种。
//
// 故本用例用「行实际落进哪个分区」（tableoid）直接钉语义，而不是只断言
// 「没报错」。
//
// 历史根因（2026-10-17 实证，scripts/_probe_tz.py）：`date AT TIME ZONE
// 'UTC'` 的 date 操作数先被隐式转成 timestamptz（会话时区午夜）再按 UTC
// 输出墙钟，边界随会话时区漂移；正确写法是先 ::timestamp 再 AT TIME ZONE。
func TestPartitionBoundsAreUtcMonthRegardlessOfSessionTz(t *testing.T) {
	pool := testsupport.NewDB(t)
	ctx := context.Background()

	// SET TIME ZONE 是**会话级**的，而 pgxpool 每次查询可能落在不同连接上
	// ⇒ 必须借一条专用连接，把「设时区 → 补建 → 插入 → 查归属」全钉在一起。
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("获取专用连接失败: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SET TIME ZONE 'America/New_York'`); err != nil {
		t.Fatalf("设置会话时区失败: %v", err)
	}

	// +8 个月的人工月：避开迁移预建范围 [当月, 当月+2]，
	// 保证被测分区是本用例现场补建的（补建路径与迁移预建路径共用函数）。
	base := time.Now().UTC().AddDate(0, 8, 0)
	mStart := time.Date(base.Year(), base.Month(), 1, 0, 0, 0, 0, time.UTC)
	mEnd := mStart.AddDate(0, 1, 0)

	if _, err := db.New(conn).EnsureAlertPartitions(ctx, db.EnsureAlertPartitionsParams{
		FromTs:      pgtype.Timestamptz{Time: mStart, Valid: true},
		AheadMonths: 0,
	}); err != nil {
		t.Fatalf("补建 %s 月分区失败: %v", mStart.Format("2006-01"), err)
	}

	cases := []struct {
		name string
		ts   time.Time
		want string
	}{
		{"UTC 月初整点", mStart, "alerts_" + mStart.Format("2006_01")},
		{"UTC 月末最后一微秒", mEnd.Add(-time.Microsecond), "alerts_" + mStart.Format("2006_01")},
		{"UTC 下月月初整点", mEnd, "alerts_" + mEnd.Format("2006_01")},
	}
	for i, c := range cases {
		id := rawInsertAlert(t, ctx, conn, fmt.Sprintf("fp-tz-bound-%d", i), "firing", c.ts)
		var got string
		if err := conn.QueryRow(ctx,
			`SELECT tableoid::regclass::text FROM alerts WHERE id = $1`, id).Scan(&got); err != nil {
			t.Fatalf("%s：查询行所在分区失败: %v", c.name, err)
		}
		if got != c.want {
			t.Errorf("%s（ts=%s）：行落进 %s，期望 %s —— 分区边界必须按 UTC 月界，与会话时区无关",
				c.name, c.ts.Format(time.RFC3339), got, c.want)
		}
	}
}
