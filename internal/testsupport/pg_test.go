package testsupport

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestSchemaIsolationBetweenCases 用真库证明「每用例一个 schema」的隔离强度
// 等价于「每用例一个库」——这是 NewDB 敢把隔离粒度从 database 降到 schema 的依据。
//
// 断言三件事：
//  1. 两次连续 NewDB 拿到**不同**的 current_schema()；
//  2. 各自 schema 内都有迁移建出的 8 张表 —— 若 search_path 没生效，
//     表会落到 public，这条会红，所以它是「连接串带 search_path」这一手法的守门断言；
//  3. 在 A 里写入的数据，B 完全看不到。
func TestSchemaIsolationBetweenCases(t *testing.T) {
	a := NewDB(t)
	b := NewDB(t)
	ctx := context.Background()

	var schemaA, schemaB string
	if err := a.QueryRow(ctx, "SELECT current_schema()").Scan(&schemaA); err != nil {
		t.Fatalf("读取 A 的 current_schema 失败: %v", err)
	}
	if err := b.QueryRow(ctx, "SELECT current_schema()").Scan(&schemaB); err != nil {
		t.Fatalf("读取 B 的 current_schema 失败: %v", err)
	}
	if schemaA == schemaB {
		t.Fatalf("两个用例拿到了同一个 schema %q，隔离失效", schemaA)
	}
	t.Logf("A=%s B=%s", schemaA, schemaB)

	cases := []struct {
		name   string
		pool   *pgxpool.Pool
		schema string
	}{
		{"A", a, schemaA},
		{"B", b, schemaB},
	}
	// 断言「关键表都在本用例 schema 里」，而不是比对表的总张数：
	// 若 search_path 未生效，表会落到 public，这里会查到 0；
	// 用表名清单而非数字，是为了将来新增迁移加表时这条不会无谓变红。
	// review_suppressions 由 0004 新增，它出现即证明迁移跑到了最新版本。
	wantTables := []string{"alerts", "incidents", "timeline", "review_suppressions"}
	for _, c := range cases {
		var n int
		err := c.pool.QueryRow(ctx,
			`SELECT count(*) FROM information_schema.tables
			  WHERE table_schema = $1 AND table_name = ANY($2::text[])`,
			c.schema, wantTables).Scan(&n)
		if err != nil {
			t.Fatalf("%s: 统计表数失败: %v", c.name, err)
		}
		if n != len(wantTables) {
			t.Errorf("%s: schema %s 内只找到 %d/%d 张关键表（%v）——"+
				"表没落进用例 schema，说明 search_path 未生效（会串味到 public）",
				c.name, c.schema, n, len(wantTables), wantTables)
		}
	}

	// 跨用例不可见：往 A 插一条探针，B 必须一条都查不到。
	const insertProbe = `INSERT INTO incidents (title, severity, status) VALUES ('隔离探针', 'warning', 'open')`
	if _, err := a.Exec(ctx, insertProbe); err != nil {
		t.Fatalf("向 A 插入探针失败: %v", err)
	}
	var seen int
	if err := b.QueryRow(ctx,
		`SELECT count(*) FROM incidents WHERE title = '隔离探针'`).Scan(&seen); err != nil {
		t.Fatalf("在 B 中查询探针失败: %v", err)
	}
	if seen != 0 {
		t.Errorf("B 看到了 A 的数据（%d 行），schema 隔离失效", seen)
	}
}
