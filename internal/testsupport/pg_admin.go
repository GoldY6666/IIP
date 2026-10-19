package testsupport

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5"
)

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
	return "iip_test_" + randomSuffix()
}

// randomSuffix 生成 8 位十六进制随机串。
func randomSuffix() string {
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand 失败极罕见；退化为时间戳低位，唯一性依然足够。
		return fmt.Sprintf("%08x", time.Now().UnixNano()&0xffffffff)
	}
	return hex.EncodeToString(buf)
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
