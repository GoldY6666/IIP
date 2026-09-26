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
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
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
const adminTimeout = 30 * time.Second

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

// NewDB 新建一次性数据库、执行全部迁移，返回可用连接池。
//
// 清理顺序由 t.Cleanup 的 LIFO 保证：先关池，再 DROP 库。
func NewDB(t testing.TB) *pgxpool.Pool {
	t.Helper()
	dsn := NewEmptyDB(t)

	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("对测试库执行迁移失败: %v", err)
	}
	pool, err := store.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// derive 把基准连接串拆成「维护库连接串」与「一次性测试库连接串」。
func derive(base string) (adminDSN, testDSN, dbName string, err error) {
	parsed, err := url.Parse(base)
	if err != nil {
		return "", "", "", fmt.Errorf("%s 不是合法 URL: %w", EnvPGURL, err)
	}
	if parsed.Host == "" {
		return "", "", "", fmt.Errorf("%s 缺少主机: %q", EnvPGURL, base)
	}

	name := randomDatabaseName()

	admin := *parsed
	admin.Path = "/" + adminDatabase

	test := *parsed
	test.Path = "/" + name

	return admin.String(), test.String(), name, nil
}

// randomDatabaseName 生成 iip_test_<8hex>。
func randomDatabaseName() string {
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand 失败极罕见；退化为时间戳低位，唯一性依然足够。
		return fmt.Sprintf("iip_test_%08x", time.Now().UnixNano()&0xffffffff)
	}
	return "iip_test_" + hex.EncodeToString(buf)
}

// createDatabase 在维护库上建库。
//
// 库名由本包生成（仅小写字母、数字、下划线），且经 pgx.Identifier 转义，
// 不存在标识符注入面。
func createDatabase(adminDSN, name string) error {
	return execAdmin(adminDSN, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize())
}

// dropDatabase 在维护库上删库。
//
// 用 WITH (FORCE)（PG 13+）强制断开残留会话：否则任何未及时关闭的连接
// 都会让 DROP 失败，进而在测试机上堆积垃圾库。
func dropDatabase(adminDSN, name string) error {
	sql := "DROP DATABASE IF EXISTS " + pgx.Identifier{name}.Sanitize() + " WITH (FORCE)"
	return execAdmin(adminDSN, sql)
}

// execAdmin 在维护库上执行一条管理语句。
func execAdmin(adminDSN, sql string) error {
	ctx, cancel := context.WithTimeout(context.Background(), adminTimeout)
	defer cancel()

	conn, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		return fmt.Errorf("连接维护库失败: %w", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	if _, err := conn.Exec(ctx, sql); err != nil {
		return err
	}
	return nil
}
