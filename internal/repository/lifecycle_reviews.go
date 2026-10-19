package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/yu/iip/internal/db"
	"github.com/yu/iip/internal/incident"
)

// ListReviews 取待确认队列的一页。
func (o *lifecycleOps) ListReviews(ctx context.Context, limit, offset int) ([]incident.ReviewEdge, error) {
	rows, err := o.queries.ListReviewEdges(ctx, db.ListReviewEdgesParams{
		RowLimit:  int32(limit),
		RowOffset: int32(offset),
	})
	if err != nil {
		return nil, fmt.Errorf("repository: 查询待确认队列失败: %w", err)
	}
	out := make([]incident.ReviewEdge, 0, len(rows))
	for i := range rows {
		out = append(out, reviewEdgeFromRow(&rows[i]))
	}
	return out, nil
}

// GetReviewEdge 取一条仍在待确认的边；不存在时返回领域层哨兵错误。
func (o *lifecycleOps) GetReviewEdge(ctx context.Context, incidentID, alertID int64) (incident.ReviewEdge, error) {
	row, err := o.queries.GetReviewEdge(ctx, db.GetReviewEdgeParams{
		IncidentID: incidentID,
		AlertID:    alertID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return incident.ReviewEdge{}, incident.ErrReviewEdgeNotFound
		}
		return incident.ReviewEdge{}, fmt.Errorf("repository: 查询待确认边失败: %w", err)
	}
	// 与队列行字段一致，但 sqlc 为两个查询生成了不同的行类型，
	// 故在此就地构造（不值得为两个结构再抽一层转换）。
	return incident.ReviewEdge{
		IncidentID: row.IncidentID,
		Alert: incident.AlertLink{
			AlertID:          row.AlertID,
			Fingerprint:      row.Fingerprint,
			Severity:         row.Severity,
			Labels:           parseStringMap(row.Labels),
			StartsAt:         row.StartsAt.Time,
			EndsAt:           fromNullableTimestamp(row.EndsAt),
			CorrelationScore: numericToFloatPtr(row.CorrelationScore),
			NeedsReview:      row.NeedsReview,
			CorrelatedAt:     row.CorrelatedAt.Time,
		},
	}, nil
}

// ConfirmReviewEdge 清掉待确认标记并保留关联边。
func (o *lifecycleOps) ConfirmReviewEdge(ctx context.Context, incidentID, alertID int64) (bool, error) {
	affected, err := o.queries.ConfirmReviewEdge(ctx, db.ConfirmReviewEdgeParams{
		IncidentID: incidentID,
		AlertID:    alertID,
	})
	if err != nil {
		return false, fmt.Errorf("repository: 确认关联边失败: %w", err)
	}
	return affected > 0, nil
}

// DeleteAlertEdge 删除一条关联边。
func (o *lifecycleOps) DeleteAlertEdge(ctx context.Context, incidentID, alertID int64) (bool, error) {
	affected, err := o.queries.DeleteIncidentAlert(ctx, db.DeleteIncidentAlertParams{
		IncidentID: incidentID,
		AlertID:    alertID,
	})
	if err != nil {
		return false, fmt.Errorf("repository: 删除关联边失败: %w", err)
	}
	return affected > 0, nil
}

// CountAlertEdges 统计某告警还剩多少条关联边。
func (o *lifecycleOps) CountAlertEdges(ctx context.Context, alertID int64) (int, error) {
	n, err := o.queries.CountIncidentAlertsByAlert(ctx, alertID)
	if err != nil {
		return 0, fmt.Errorf("repository: 统计告警 %d 的关联边失败: %w", alertID, err)
	}
	return int(n), nil
}

// ClearAlertIncident 把告警的 incident_id 置 NULL。
func (o *lifecycleOps) ClearAlertIncident(ctx context.Context, alertID int64) error {
	if _, err := o.queries.ClearAlertIncident(ctx, alertID); err != nil {
		return fmt.Errorf("repository: 清除告警 %d 的 incident_id 失败: %w", alertID, err)
	}
	return nil
}

// AddSuppression 写一条抑制行。
//
// 与 repository.Reviews 的同名方法功能相同，但**必须各有一份**：
// 复核流程要求「删边 + 写抑制 + 写留痕」在同一事务里，
// 因此抑制写入必须能复用当时的 pgx.Tx，而不是另开一个仓储。
func (o *lifecycleOps) AddSuppression(ctx context.Context, fingerprint string, incidentID int64, actor, reason string) error {
	err := o.queries.InsertReviewSuppression(ctx, db.InsertReviewSuppressionParams{
		Fingerprint: fingerprint,
		IncidentID:  incidentID,
		Actor:       textOrNull(actor),
		Reason:      textOrNull(reason),
	})
	if err != nil {
		return fmt.Errorf("repository: 写入抑制失败: %w", err)
	}
	return nil
}

// reviewEdgeFromRow 把队列行转成领域对象。
func reviewEdgeFromRow(row *db.ListReviewEdgesRow) incident.ReviewEdge {
	return incident.ReviewEdge{
		IncidentID: row.IncidentID,
		Alert: incident.AlertLink{
			AlertID:          row.AlertID,
			Fingerprint:      row.Fingerprint,
			Severity:         row.Severity,
			Labels:           parseStringMap(row.Labels),
			StartsAt:         row.StartsAt.Time,
			EndsAt:           fromNullableTimestamp(row.EndsAt),
			CorrelationScore: numericToFloatPtr(row.CorrelationScore),
			NeedsReview:      row.NeedsReview,
			CorrelatedAt:     row.CorrelatedAt.Time,
		},
	}
}

// InTx 在一个事务内执行 fn。
func (r *Lifecycle) InTx(ctx context.Context, fn func(incident.Store) error) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("repository: 开启事务失败: %w", err)
	}
	// 提交成功后 Rollback 会返回 ErrTxClosed，属预期，忽略即可。
	defer func() { _ = tx.Rollback(ctx) }()

	// fn 内用事务版 Store：它写的所有语句都在本事务里，失败即整体回滚。
	if fnErr := fn(&lifecycleOps{queries: r.queries.WithTx(tx)}); fnErr != nil {
		return fnErr
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("repository: 提交事务失败: %w", err)
	}
	return nil
}

// marshalMetadata 把 metadata 序列化为可写入 jsonb 的值。
//
// 空 map 返回 nil（SQL 侧 COALESCE 兜底），避免写入 "null" 这种既不是对象
// 又不是空对象的值，给读侧添麻烦。
func marshalMetadata(m map[string]string) (any, error) {
	if len(m) == 0 {
		return nil, nil
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("repository: 序列化 timeline metadata 失败: %w", err)
	}
	return raw, nil
}
