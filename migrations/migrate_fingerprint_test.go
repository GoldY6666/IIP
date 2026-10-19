package migrations_test

import (
	"context"
	"errors"
	"testing"
	"time"

	_ "github.com/golang-migrate/migrate/v4/database/postgres" // 注册 database/sql 的 postgres 迁移驱动
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/yu/iip/internal/testsupport"
)

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
