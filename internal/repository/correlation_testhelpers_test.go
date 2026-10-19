package repository

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// corrBase 是关联用例共用的基准时刻。
var corrBase = time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)

// insertAlertRow 直插一条告警并返回主键。
//
// 刻意用裸 SQL 而非 NewAlerts：本文件的被测对象是 M2 仓储，
// 借道 M1 的 Insert 会引入额外的去重语义，让"插不进去"这类失败难以归因。
func insertAlertRow(t *testing.T, pool *pgxpool.Pool, fingerprint, status, labels string, startsAt time.Time, endsAt *time.Time) int64 {
	t.Helper()
	// T7.1 起 alerts 按 starts_at 月度分区，迁移只预建 [当月, 当月+2]：
	// corrBase（2026-09）等历史月份不在范围内，裸插会以 23514 失败。
	// 先补建再插——本文件测的是 M2/M3 仓储语义，「分区缺失」不是它们的
	// 职责（补建路径由 internal/partition 维护 tick 负责，另有真库用例覆盖）。
	if _, err := pool.Exec(context.Background(),
		`SELECT iip_ensure_alert_partitions($1, 2)`, startsAt); err != nil {
		t.Fatalf("补建 %s 所在月分区失败: %v", startsAt.Format("2006-01"), err)
	}
	var id int64
	err := pool.QueryRow(context.Background(), `
INSERT INTO alerts (fingerprint, source, severity, status, labels, annotations, raw_payload, starts_at, ends_at)
VALUES ($1, 'alertmanager', 'critical', $2, $3::jsonb, '{}'::jsonb, '{}'::jsonb, $4, $5)
RETURNING id`, fingerprint, status, labels, startsAt, endsAt).Scan(&id)
	if err != nil {
		t.Fatalf("插入告警 %s 失败: %v", fingerprint, err)
	}
	return id
}

// countRows 执行一次计数查询，用于断言库内行数。
func countRows(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("计数查询失败 (%s): %v", sql, err)
	}
	return n
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// ---------------------------------------------------------------------------
// CorrelationConfig（配置读写 + 历史共现）
// ---------------------------------------------------------------------------
