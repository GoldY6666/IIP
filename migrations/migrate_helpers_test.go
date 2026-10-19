// Package migrations_test 以**外部测试包**形式编写。
//
// 必须用外部测试包：本用例要通过 testsupport 拿到一次性库，而 testsupport 依赖
// internal/store，store 又 import 本包（embed 迁移文件）——若写成包内测试
// （package migrations）就会构成 import cycle。外部测试包正是为切断此类环而存在。
package migrations_test

import (
	"errors"
	"io/fs"
	"path"
	"strconv"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres" // 注册 database/sql 的 postgres 迁移驱动
	"github.com/golang-migrate/migrate/v4/source/iofs"

	"github.com/yu/iip/migrations"
)

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
