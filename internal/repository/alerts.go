// Package repository 是领域层持久化接口在数据库上的实现。
//
// 它把 sqlc 生成的 internal/db（不手写 SQL、不手拼字符串）适配成 internal/alert
// 定义的 Repository 接口。连接池由 internal/store 建立后注入 —— **不另建连接池**，
// 这是 IIP-M1详细开发手册 §4.5 的硬约束。
package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yu/iip/internal/alert"
	"github.com/yu/iip/internal/db"
)

// Alerts 是基于 PostgreSQL 的 alert.Repository 实现。
type Alerts struct {
	queries *db.Queries
}

// 编译期断言：Alerts 必须满足领域层的 Repository 接口。
var _ alert.Repository = (*Alerts)(nil)

// NewAlerts 构造仓储。
//
// 参数取 db.DBTX 而非 *pgxpool.Pool：既能接连接池，也能接 pgx.Tx
// （为后续「一个事务内多步写入」留出口），同时把依赖面收到最小。
// store.Connect 返回的 *pgxpool.Pool 天然满足该接口。
func NewAlerts(conn db.DBTX) *Alerts {
	return &Alerts{queries: db.New(conn)}
}

// LatestByFingerprint 取该指纹最新一条告警（ORDER BY starts_at DESC LIMIT 1）。
func (a *Alerts) LatestByFingerprint(ctx context.Context, fingerprint string) (alert.StoredAlert, error) {
	row, err := a.queries.GetAlertByFingerprint(ctx, fingerprint)
	if errors.Is(err, pgx.ErrNoRows) {
		// 转成领域层语义：调用方据此区分「从未出现」与「查到记录」。
		return alert.StoredAlert{}, alert.ErrNotFound
	}
	if err != nil {
		return alert.StoredAlert{}, fmt.Errorf("repository: 按指纹查询最新告警失败: %w", err)
	}
	return toStoredAlert(&row), nil
}

// Insert 落库一条告警。
//
// sqlc 生成的语句带 ON CONFLICT (fingerprint) WHERE status='firing' DO NOTHING，
// 命中冲突时**不返回行**（pgx.ErrNoRows）。这在语义上是「已存在同指纹的 firing」，
// 属正常业务分支，因此翻译为 (zero, false, nil) 而不是错误。
func (a *Alerts) Insert(ctx context.Context, in *alert.Alert) (alert.StoredAlert, bool, error) {
	if in == nil {
		return alert.StoredAlert{}, false, errors.New("repository: 待插入的告警为 nil")
	}

	row, err := a.queries.InsertAlert(ctx, db.InsertAlertParams{
		Fingerprint: in.Fingerprint,
		Source:      in.Source,
		Severity:    in.Severity,
		Status:      in.Status,
		Labels:      in.Labels,
		Annotations: in.Annotations,
		StartsAt:    timestamp(in.StartsAt),
		EndsAt:      nullableTimestamp(in.EndsAt),
		RawPayload:  in.RawPayload,
		IncidentID:  nullableInt8(in.IncidentID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return alert.StoredAlert{}, false, nil
	}
	if err != nil {
		return alert.StoredAlert{}, false, fmt.Errorf("repository: 插入告警失败: %w", err)
	}
	return toStoredAlert(&row), true, nil
}

// Resolve 把指定行置为 resolved 并写入结束时间。
func (a *Alerts) Resolve(ctx context.Context, id int64, endsAt *time.Time) error {
	err := a.queries.ResolveAlert(ctx, db.ResolveAlertParams{
		ID:     id,
		EndsAt: nullableTimestamp(endsAt),
	})
	if err != nil {
		return fmt.Errorf("repository: 将告警 %d 置为 resolved 失败: %w", id, err)
	}
	return nil
}

// List 取最新一批告警（starts_at DESC, id DESC），不做 status 过滤。
//
// 空结果返回 ([]StoredAlert{}, nil) 而非 (nil, nil)：保持「空切片」语义，
// 避免调用方对 nil 切片做 len() 时误以为出错，也便于前端直接 map。
func (a *Alerts) List(ctx context.Context, limit, offset int32) ([]alert.StoredAlert, error) {
	rows, err := a.queries.ListAlerts(ctx, db.ListAlertsParams{
		Limit:  limit,
		Offset: offset,
	})
	if err != nil {
		return nil, fmt.Errorf("repository: 查询告警列表失败: %w", err)
	}
	out := make([]alert.StoredAlert, 0, len(rows))
	for i := range rows {
		out = append(out, toStoredAlert(&rows[i]))
	}
	return out, nil
}

// toStoredAlert 把 sqlc 生成的行映射为领域对象。
//
// 参数取指针而非值：db.Alert 含若干 json.RawMessage 与 pgtype 字段，体积近 300 字节，
// 按值传参会整份复制（gocritic hugeParam）。只读语义由「不修改 row」保证。
func toStoredAlert(row *db.Alert) alert.StoredAlert {
	return alert.StoredAlert{
		ID: row.ID,
		Alert: alert.Alert{
			Fingerprint: row.Fingerprint,
			Source:      row.Source,
			Severity:    row.Severity,
			Status:      row.Status,
			Labels:      row.Labels,
			Annotations: row.Annotations,
			StartsAt:    row.StartsAt.Time,
			EndsAt:      fromNullableTimestamp(row.EndsAt),
			RawPayload:  row.RawPayload,
			IncidentID:  fromNullableInt8(row.IncidentID),
		},
	}
}

// timestamp 把必然有值的时间转成 pgtype.Timestamptz。
func timestamp(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: true}
}

// nullableTimestamp 把可空时间转成 pgtype.Timestamptz（nil → SQL NULL）。
func nullableTimestamp(t *time.Time) pgtype.Timestamptz {
	if t == nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: *t, Valid: true}
}

// fromNullableTimestamp 是 nullableTimestamp 的逆运算。
func fromNullableTimestamp(ts pgtype.Timestamptz) *time.Time {
	if !ts.Valid {
		return nil
	}
	t := ts.Time
	return &t
}

// nullableInt8 把可空 int64 转成 pgtype.Int8（nil → SQL NULL）。
func nullableInt8(v *int64) pgtype.Int8 {
	if v == nil {
		return pgtype.Int8{}
	}
	return pgtype.Int8{Int64: *v, Valid: true}
}

// fromNullableInt8 是 nullableInt8 的逆运算。
func fromNullableInt8(v pgtype.Int8) *int64 {
	if !v.Valid {
		return nil
	}
	n := v.Int64
	return &n
}
