// Package migrations_test 以**外部测试包**形式编写。
//
// 必须用外部测试包：本用例要通过 testsupport 拿到一次性库，而 testsupport 依赖
// internal/store，store 又 import 本包（embed 迁移文件）——若写成包内测试
// （package migrations）就会构成 import cycle。外部测试包正是为切断此类环而存在。
package migrations_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres" // 注册 database/sql 的 postgres 迁移驱动
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yu/iip/internal/testsupport"
	"github.com/yu/iip/migrations"
)

// latestVersion 是当前迁移文件到达的版本号（0002）。
//
// 新增迁移时必须同步修改此常量——它让「忘记把新迁移纳入 embed/schema」这类
// 沉默失败变成一条明确的测试失败。
const latestVersion = 2

// dedupeIndex 是 0002 建立的部分唯一索引名。
const dedupeIndex = "uq_alerts_fingerprint_firing"

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
	assertIndexExists(t, dsn, dedupeIndex, true)

	// 2) down-all
	if err := m.Down(); err != nil {
		t.Fatalf("down-all 失败: %v", err)
	}
	if v, dirty := currentVersion(t, m); v != 0 || dirty {
		t.Fatalf("down-all 后版本应为 0 且非 dirty，实际 version=%d dirty=%v", v, dirty)
	}
	assertIndexExists(t, dsn, dedupeIndex, false)

	// 3) 再 up —— 证明可重复执行（"down 未真正回滚" 会在此暴露）
	mustUp(t, m)
	if v, dirty := currentVersion(t, m); v != latestVersion || dirty {
		t.Fatalf("再次 up 后版本应为 %d 且非 dirty，实际 version=%d dirty=%v", latestVersion, v, dirty)
	}
	assertIndexExists(t, dsn, dedupeIndex, true)
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

// assertIndexExists 断言索引存在/不存在。
func assertIndexExists(t *testing.T, dsn, indexName string, want bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	defer pool.Close()

	var count int
	err = pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_indexes WHERE schemaname = 'public' AND indexname = $1`,
		indexName).Scan(&count)
	if err != nil {
		t.Fatalf("查询索引存在性失败: %v", err)
	}

	if want && count != 1 {
		t.Errorf("期望索引 %s 存在，实际未找到", indexName)
	}
	if !want && count != 0 {
		t.Errorf("期望索引 %s 已被回滚删除，实际仍然存在", indexName)
	}
}
