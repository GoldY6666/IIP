package migrations_test

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	_ "github.com/golang-migrate/migrate/v4/database/postgres" // 注册 database/sql 的 postgres 迁移驱动
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// withPool 在一次性测试库上开一个短连接池执行断言，用后即关。
func withPool(t *testing.T, dsn string, fn func(ctx context.Context, pool *pgxpool.Pool)) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	defer pool.Close()
	fn(ctx, pool)
}

// assertIndexExists 断言单个索引存在（want=true）或不存在（want=false）。
func assertIndexExists(t *testing.T, dsn, indexName string, want bool) {
	t.Helper()
	withPool(t, dsn, func(ctx context.Context, pool *pgxpool.Pool) {
		var count int
		err := pool.QueryRow(ctx,
			`SELECT count(*) FROM pg_indexes WHERE schemaname = 'public' AND indexname = $1`,
			indexName).Scan(&count)
		if err != nil {
			t.Fatalf("查询索引 %s 存在性失败: %v", indexName, err)
		}
		if want && count != 1 {
			t.Errorf("期望索引 %s 存在，实际未找到", indexName)
		}
		if !want && count != 0 {
			t.Errorf("期望索引 %s 已被回滚删除，实际仍然存在", indexName)
		}
	})
}

// assertIndexesExist 对 expectedIndexes 逐个断言（up 后全在 / down-all 后全无）。
func assertIndexesExist(t *testing.T, dsn string, want bool) {
	t.Helper()
	for _, indexName := range expectedIndexes {
		assertIndexExists(t, dsn, indexName, want)
	}
}

// assertTableExists 断言单张表存在（want=true）或不存在（want=false）。
//
// 只认 public schema：information_schema.tables 会带上系统表，
// 不限定 schema 会让「表被删掉」这类断言被同名系统表蒙混过关。
func assertTableExists(t *testing.T, dsn, tableName string, want bool) {
	t.Helper()
	withPool(t, dsn, func(ctx context.Context, pool *pgxpool.Pool) {
		var exists bool
		err := pool.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1 FROM information_schema.tables
    WHERE table_schema = 'public' AND table_name = $1
)`, tableName).Scan(&exists)
		if err != nil {
			t.Fatalf("查询表 %s 存在性失败: %v", tableName, err)
		}
		if want && !exists {
			t.Errorf("期望表 %s 存在，实际未找到", tableName)
		}
		if !want && exists {
			t.Errorf("期望表 %s 已被回滚删除，实际仍然存在", tableName)
		}
	})
}

// insertIncident 插入一条指定状态的事件并返回其 id，供 timeline 断言做外键用。
func insertIncident(t *testing.T, ctx context.Context, pool *pgxpool.Pool, status string) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(ctx,
		`INSERT INTO incidents (title, severity, status, service)
		 VALUES ($1, 'warning', $2, 'checkout') RETURNING id`,
		"m3-lifecycle-"+status, status).Scan(&id)
	if err != nil {
		t.Fatalf("插入 status=%q 的事件应成功，实际失败: %v", status, err)
	}
	return id
}

// assertRejected 断言一条 SQL 被数据库拒绝，且 SQLSTATE 恰为期望值。
//
// 必须比对 SQLSTATE 而不只是「有错」：CHECK 违反是 23514、唯一冲突是 23505，
// 只看「报错」会把「语法写错」和「约束生效」混为一谈，测试就失去判别力。
func assertRejected(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sqlState, stmt string, args ...any) {
	t.Helper()
	_, err := pool.Exec(ctx, stmt, args...)
	if err == nil {
		t.Fatalf("期望该 SQL 被拒绝（SQLSTATE %s），实际执行成功: %s", sqlState, stmt)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("期望 *pgconn.PgError，实际: %v", err)
	}
	if pgErr.Code != sqlState {
		t.Fatalf("期望 SQLSTATE %s，实际 %s（%v）", sqlState, pgErr.Code, err)
	}
}

// assertNeedsReviewColumn 断言 0003 新增的 incident_alerts.needs_review 列存在性及其约束。
func assertNeedsReviewColumn(t *testing.T, dsn string, want bool) {
	t.Helper()
	withPool(t, dsn, func(ctx context.Context, pool *pgxpool.Pool) {
		var isNullable, columnDefault string
		err := pool.QueryRow(ctx, `
SELECT is_nullable, COALESCE(column_default, '')
FROM information_schema.columns
WHERE table_schema = 'public' AND table_name = 'incident_alerts' AND column_name = 'needs_review'`,
		).Scan(&isNullable, &columnDefault)

		if errors.Is(err, pgx.ErrNoRows) {
			if want {
				t.Fatal("期望 incident_alerts.needs_review 列存在，实际未找到")
			}
			return
		}
		if err != nil {
			t.Fatalf("查询 needs_review 列失败: %v", err)
		}
		if !want {
			t.Fatal("期望 incident_alerts.needs_review 列已被回滚删除，实际仍然存在")
		}
		// NOT NULL + DEFAULT FALSE：读端无需处理 NULL，且既有行为 false（不误标待确认）。
		if isNullable != "NO" {
			t.Errorf("needs_review 应为 NOT NULL，实际 is_nullable=%s", isNullable)
		}
		if !strings.Contains(strings.ToLower(columnDefault), "false") {
			t.Errorf("needs_review 默认值应为 false，实际 column_default=%q", columnDefault)
		}
	})
}

// assertDefaultCorrelationRule 断言 0003 种子的默认关联配置行存在性与其内容。
//
// 这里把 V1.0 §5 的五维权重与三档阈值**硬编码**进断言：默认配置是对外承诺的一部分，
// 迁移里的种子值被改动时必须让测试失败，而不是被悄悄带过。
func assertDefaultCorrelationRule(t *testing.T, dsn string, want bool) {
	t.Helper()
	withPool(t, dsn, func(ctx context.Context, pool *pgxpool.Pool) {
		// 先判断表本身是否存在：若调用方刚做过全量 down，0001 建的表已全被删除，
		// 此时"配置行不存在"是必然的，直接查询会报 42P01 而不是给出结论。
		var tableExists bool
		if err := pool.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1 FROM information_schema.tables
    WHERE table_schema = 'public' AND table_name = 'correlation_rules'
)`).Scan(&tableExists); err != nil {
			t.Fatalf("查询 correlation_rules 表存在性失败: %v", err)
		}
		if !tableExists {
			if want {
				t.Fatal("期望 correlation_rules 表存在，实际未找到")
			}
			return
		}

		var count int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM correlation_rules WHERE name = 'default'`).Scan(&count); err != nil {
			t.Fatalf("统计默认关联配置行失败: %v", err)
		}
		if !want {
			if count != 0 {
				t.Fatalf("默认关联配置行应已被回滚删除，实际仍有 %d 行", count)
			}
			return
		}
		if count != 1 {
			t.Fatalf("默认关联配置应恰好 1 行，实际 %d 行", count)
		}

		var enabled bool
		var wTime, wService, wLabel, wDependency, wHistorical, merge, review float64
		err := pool.QueryRow(ctx, `
SELECT enabled,
       (rule_config->'weights'->>'time')::double precision,
       (rule_config->'weights'->>'service')::double precision,
       (rule_config->'weights'->>'label')::double precision,
       (rule_config->'weights'->>'dependency')::double precision,
       (rule_config->'weights'->>'historical')::double precision,
       (rule_config->'thresholds'->>'merge')::double precision,
       (rule_config->'thresholds'->>'review')::double precision
FROM correlation_rules WHERE name = 'default'`).
			Scan(&enabled, &wTime, &wService, &wLabel, &wDependency, &wHistorical, &merge, &review)
		if err != nil {
			t.Fatalf("读取默认关联配置失败: %v", err)
		}

		if !enabled {
			t.Error("默认关联配置应 enabled = TRUE")
		}

		wantWeights := []struct {
			name string
			got  float64
			want float64
		}{
			{"time", wTime, 0.25},
			{"service", wService, 0.25},
			{"label", wLabel, 0.20},
			{"dependency", wDependency, 0.20},
			{"historical", wHistorical, 0.10},
		}
		sum := 0.0
		// 刻意用切片而非 map：map 的迭代顺序在 Go 里是随机的，
		// 而浮点求和对顺序敏感，会让这条断言时红时绿。
		for _, w := range wantWeights {
			if w.got != w.want {
				t.Errorf("权重 %s = %v，期望 %v", w.name, w.got, w.want)
			}
			sum += w.got
		}
		// 逐项比对可以精确；但求和必须留容差：
		// IEEE 754 下 0.25+0.25+0.20+0.20+0.10 = 0.9999999999999999，
		// 与产品侧 Config.Validate 的 weightSumEpsilon 口径保持一致。
		if math.Abs(sum-1.0) > 0.001 {
			t.Errorf("五维权重之和 = %v，期望 1.0", sum)
		}

		if merge != 0.80 {
			t.Errorf("merge 阈值 = %v，期望 0.80", merge)
		}
		if review != 0.50 {
			t.Errorf("review 阈值 = %v，期望 0.50", review)
		}
	})
}
