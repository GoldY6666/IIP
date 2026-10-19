package migrations_test

import (
	"context"
	"errors"
	"testing"

	_ "github.com/golang-migrate/migrate/v4/database/postgres" // 注册 database/sql 的 postgres 迁移驱动
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yu/iip/internal/testsupport"
)

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
