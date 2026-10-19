package testsupport

import (
	"context"
	"fmt"
	"net/url"
	"sync"

	"github.com/jackc/pgx/v5"
)

// caseSchemaPrefix 是用例 schema 的命名前缀。
//
// 带上可识别前缀是为了让外部（门禁脚本）能在「没有任何测试在跑」的时刻
// 统一清理上次中断留下的残留 —— 清理责任刻意放在脚本侧而不是本包：
// go test 会并行跑多个包（不同进程），若由本包在启动时清理，
// 会删掉另一个包**正在使用**的 schema。
const (
	caseSchemaPrefix = "iip_case_"
	sessionDatabase  = "iip_testsession" // 跨次复用：第二次运行起连建库的 35s 也省掉
)

var (
	sessionOnce sync.Once
	sessionDSN  string
	sessionErr  error
)

// ensureSession 保证 session 库存在，返回其连接串（不含 search_path）。
//
// sync.Once 保证一个测试进程内只准备一次。跨进程安全靠「只建不清理」：
// 本函数从不删除任何 schema，因此不会干扰并发运行的其它测试包。
func ensureSession(adminDSN string) (string, error) {
	sessionOnce.Do(func() {
		sessionDSN, sessionErr = prepareSession(adminDSN)
	})
	return sessionDSN, sessionErr
}

func prepareSession(adminDSN string) (string, error) {
	if err := createDatabaseIfAbsent(adminDSN, sessionDatabase); err != nil {
		return "", err
	}
	return withDatabase(adminDSN, sessionDatabase), nil
}

// createDatabaseIfAbsent 建库；已存在则直接返回（复用，省掉 35s）。
func createDatabaseIfAbsent(adminDSN, name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), adminTimeout)
	defer cancel()

	conn, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		return fmt.Errorf("连接维护库失败: %w", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	var exists bool
	if err := conn.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, name).Scan(&exists); err != nil {
		return fmt.Errorf("检查库 %s 是否存在失败: %w", name, err)
	}
	if exists {
		return nil
	}
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		return fmt.Errorf("创建共享测试库 %s 失败: %w", name, err)
	}
	return nil
}

// execOnSession 在 session 库上执行一条管理语句（CREATE / DROP SCHEMA）。
func execOnSession(dsn, sql string) error {
	ctx, cancel := context.WithTimeout(context.Background(), adminTimeout)
	defer cancel()

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("连接共享测试库失败: %w", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	if _, err := conn.Exec(ctx, sql); err != nil {
		return err
	}
	return nil
}

// withDatabase 把连接串的库名换成指定值。
func withDatabase(dsn, db string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return dsn
	}
	u.Path = "/" + db
	return u.String()
}

// withSearchPath 给连接串加上 search_path 运行时参数。
//
// 两种消费方都认这个参数：pgx（放进 RuntimeParams）与 golang-migrate 底层的
// lib/pq（作为 startup 报文的运行时参数发给服务端）。这是「不改迁移文件」
// 就能让整库 DDL 落进指定 schema 的关键。
func withSearchPath(dsn, schema string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("解析连接串失败: %w", err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String(), nil
}
