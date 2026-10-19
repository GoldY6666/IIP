// Package testsupport 为集成测试提供一次性 PostgreSQL 数据库。
//
// 目标：让「必须连真库」的用例（迁移三连、唯一约束、并发去重）能像普通单测一样跑
// ——每个用例拿到一个全新的空库，跑完自动 DROP，彼此完全隔离。
//
// 前置：环境变量 IIP_TEST_PG_URL 指向一个具备 CREATE DATABASE 权限的实例。
// 未设置时 SkipIfNoPG 会 t.Skip，从而让 `go test ./...` 在无库环境下仍然整体通过
// （不把「没库」伪装成「测试通过」，而是显式跳过）。
//
// 库名形如 iip_test_<8hex>，与 scripts/verify-runtime.py 的随机建库做法一致。
package testsupport

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yu/iip/internal/store"
)

// EnvPGURL 是集成测试读取的连接串环境变量名。
const EnvPGURL = "IIP_TEST_PG_URL"

// adminDatabase 是用来执行 CREATE / DROP DATABASE 的维护库。
const adminDatabase = "postgres"

// adminTimeout 是建库/删库这类管理操作的超时。
//
// 取 120s 而非 30s：一个用例的生命周期里要 CREATE DATABASE 再跑完全部迁移，
// 本机在**连续**创建多个一次性库时（go test 串行跑十几条真库用例），
// CREATE DATABASE 实测可达数十秒——那不是用例失败，而是环境慢。
// 超时给窄了会把「机器慢」伪装成「断言失败」，白白浪费一次排查。
const adminTimeout = 120 * time.Second

// PGURL 返回集成测试用连接串；未配置时返回空串。
func PGURL() string {
	return strings.TrimSpace(os.Getenv(EnvPGURL))
}

// SkipIfNoPG 在未配置 IIP_TEST_PG_URL 时跳过当前用例，返回该连接串。
func SkipIfNoPG(t testing.TB) string {
	t.Helper()
	dsn := PGURL()
	if dsn == "" {
		t.Skipf("未设置 %s，跳过需要真实 PostgreSQL 的用例", EnvPGURL)
	}
	return dsn
}

// NewEmptyDB 新建一个一次性**空**数据库，返回其连接串，并注册用例结束后的清理。
//
// 供需要自行控制迁移方向的用例使用（如 up → down-all → up 三连验证）。
func NewEmptyDB(t testing.TB) string {
	t.Helper()
	adminDSN, testDSN, name, err := derive(SkipIfNoPG(t))
	if err != nil {
		t.Fatalf("解析 %s 失败: %v", EnvPGURL, err)
	}
	if err := createDatabase(adminDSN, name); err != nil {
		t.Fatalf("创建测试库 %s 失败: %v", name, err)
	}
	t.Cleanup(func() {
		if err := dropDatabase(adminDSN, name); err != nil {
			t.Logf("清理测试库 %s 未成功（不影响用例结论）: %v", name, err)
		}
	})
	return testDSN
}

// NewDB 返回一个「已迁移好、且与其它用例完全隔离」的连接池。
//
// 隔离粒度是 **schema** 而不是 database：一个 session 库由整轮共享，
// 每个用例在其中独占一个 schema（iip_case_<hex>），用例结束即 DROP SCHEMA CASCADE。
//
// 为什么不再「每个用例建一个库」——实测（2026-10-02，本机 PG16 / 5433）：
//
//	CREATE DATABASE  35.5s   ← 恒定，fsync=off 完全无效（不是刷盘配置问题）
//	DROP DATABASE    15.2s
//	跑 4 个迁移      10.4s
//	合计            61.2s/条 → 一个包 30 条就要 31 分钟，直接撞破 go test 默认 10m
//
//	改为 schema 后：CREATE SCHEMA 0.6s / DROP SCHEMA 2.1s / 迁移 10.3s = 13.1s/条（快 4.7 倍）
//
// 瓶颈在 Windows 上逐文件复制 template1 的成本（关掉 fsync 毫无变化可佐证），
// 属环境固有开销，只能靠「减少建库次数」绕开，不能靠调参消除。
// 证据：D:\_iip_setup\_diag_pg.txt（耗时拆解）、_pg_tune.txt（调参无效）、_pg_schema.txt（本方案验证）。
//
// 隔离性等价的依据：PostgreSQL 的表 / 序列 / 索引 / 约束全部落在 schema 命名空间内，
// 两个用例各占一个 schema 时互不可见，与「各占一个库」的隔离强度一致
// （由本包 TestSchemaIsolationBetweenCases 用真库断言，不是靠推理）。
//
// 约束：不要对本函数返回的连接池使用 t.Parallel()。
// 现有用例均未并行（同一用例内串行执行），故不额外加锁；
// 若将来要并行，需改为每个子测试各自调 NewDB。
func NewDB(t testing.TB) *pgxpool.Pool {
	t.Helper()
	adminDSN := SkipIfNoPG(t)

	sessDSN, err := ensureSession(adminDSN)
	if err != nil {
		t.Fatalf("准备共享测试库失败: %v", err)
	}

	schema := caseSchemaPrefix + randomSuffix()
	ident := pgx.Identifier{schema}.Sanitize()
	if createErr := execOnSession(sessDSN, "CREATE SCHEMA "+ident); createErr != nil {
		t.Fatalf("创建用例 schema %s 失败: %v", schema, createErr)
	}

	// 迁移与业务连接都靠连接串上的 search_path 落到本用例的 schema，
	// 因此 4 个迁移文件**一行都不用改**（它们本来就不带 schema 限定）。
	dsn, err := withSearchPath(sessDSN, schema)
	if err != nil {
		t.Fatalf("拼装带 search_path 的连接串失败: %v", err)
	}

	ctx := context.Background()
	if migrateErr := store.Migrate(ctx, dsn); migrateErr != nil {
		t.Fatalf("对用例 schema %s 执行迁移失败: %v", schema, migrateErr)
	}
	pool, err := store.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("连接用例 schema %s 失败: %v", schema, err)
	}

	// Cleanup 是 LIFO：先注册的 DROP SCHEMA 最后执行，故「先关池、再删 schema」。
	t.Cleanup(func() {
		if err := execOnSession(sessDSN, "DROP SCHEMA IF EXISTS "+ident+" CASCADE"); err != nil {
			t.Logf("清理用例 schema %s 未成功（不影响用例结论）: %v", schema, err)
		}
	})
	t.Cleanup(pool.Close)
	return pool
}

// --- 共享 session 库 ---
