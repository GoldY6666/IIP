package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yu/iip/internal/db"
	"github.com/yu/iip/internal/incident"
)

// Lifecycle 是 incident.Store / incident.Transactor 的 PostgreSQL 实现。
//
// 与 Incidents（M2 写侧）刻意分开：M2 的写侧接口服务于关联引擎，本类型服务于
// M3 的生命周期，两者若混在一个结构体里，改一处就会牵动另一处的编译面。
type Lifecycle struct {
	pool    *pgxpool.Pool
	queries *db.Queries

	lifecycleOps
}

// lifecycleOps 是三个查询的具体实现，与「是否在事务内」无关。
//
// 抽出来是为了让事务内外的实现共用同一份逻辑：InTx 里只需要换个 *db.Queries
// （指向 pgx.Tx），不必把三个方法再抄一遍——抄一遍就意味着将来只改一处会漏。
type lifecycleOps struct {
	queries *db.Queries
}

// 编译期断言：事务内外两种形态都必须满足领域层定义的接口。
var (
	_ incident.Store      = (*lifecycleOps)(nil)
	_ incident.Transactor = (*Lifecycle)(nil)
)

// NewLifecycle 构造生命周期仓储。
//
// 参数是 *pgxpool.Pool 而不是更宽的 db.DBTX：开事务需要 Begin，而 DBTX 接口里
// 没有它。这里明确要「能开事务的连接源」，宁可把类型收窄，也不要在运行期才发现
// 传进来的是个 Tx。
func NewLifecycle(pool *pgxpool.Pool) *Lifecycle {
	return &Lifecycle{
		pool:         pool,
		queries:      db.New(pool),
		lifecycleOps: lifecycleOps{queries: db.New(pool)},
	}
}

// GetByID 读取事件当前状态。
func (o *lifecycleOps) GetByID(ctx context.Context, id int64) (incident.Snapshot, error) {
	row, err := o.queries.GetIncidentByID(ctx, id)
	if err != nil {
		// 「查不到」不是错误而是状态，交给领域层决定映射成 404；
		// 其余（连不上、权限不足）必须原样抛出，不能被当成 404 吞掉。
		if errors.Is(err, pgx.ErrNoRows) {
			return incident.Snapshot{}, incident.ErrIncidentNotFound
		}
		return incident.Snapshot{}, fmt.Errorf("repository: 查询事件 %d 失败: %w", id, err)
	}
	return incident.Snapshot{
		ID:         row.ID,
		Title:      row.Title,
		Severity:   row.Severity,
		Status:     incident.Status(row.Status),
		Service:    textOrEmpty(row.Service),
		StartedAt:  row.StartedAt.Time,
		ResolvedAt: fromNullableTimestamp(row.ResolvedAt),
		UpdatedAt:  row.UpdatedAt.Time,
	}, nil
}

// UpdateStatus 带乐观条件地更新状态，返回是否命中。
func (o *lifecycleOps) UpdateStatus(ctx context.Context, arg incident.UpdateStatusArg) (bool, error) {
	affected, err := o.queries.UpdateIncidentStatus(ctx, db.UpdateIncidentStatusParams{
		IncidentID:     arg.ID,
		FromStatus:     string(arg.From),
		ToStatus:       string(arg.To),
		ClearResolved:  arg.Effect.ClearResolved,
		SetResolvedNow: arg.Effect.SetResolvedNow,
	})
	if err != nil {
		return false, fmt.Errorf("repository: 更新事件 %d 状态失败: %w", arg.ID, err)
	}
	return affected > 0, nil
}

// AppendTimelineEntry 写入一条时间线留痕。
func (o *lifecycleOps) AppendTimelineEntry(ctx context.Context, e incident.Entry) error {
	metadata, err := marshalMetadata(e.Metadata)
	if err != nil {
		return err
	}
	_, err = o.queries.InsertTimelineEntry(ctx, db.InsertTimelineEntryParams{
		IncidentID:  e.IncidentID,
		EventType:   string(e.EventType),
		Description: e.Description,
		Actor:       textOrNull(e.Actor),
		// 空 metadata 传 nil，由 SQL 的 COALESCE 兜成 '{}'，保持与列默认值一致。
		Metadata:  metadata,
		Timestamp: pgtype.Timestamptz{Time: e.Timestamp, Valid: true},
	})
	if err != nil {
		return fmt.Errorf("repository: 写入事件 %d 的时间线失败: %w", e.IncidentID, err)
	}
	return nil
}

// List 按过滤条件取事件列表。
//
// 过滤值由调用方保证非空（服务层会把「不过滤」展开成全部合法取值），
// 因此这里不需要处理「空切片」的语义分支。
func (o *lifecycleOps) List(ctx context.Context, f incident.ListFilter) ([]incident.Summary, error) {
	rows, err := o.queries.ListIncidents(ctx, db.ListIncidentsParams{
		Statuses:   statusesToStrings(f.Statuses),
		Severities: f.Severities,
		Service:    textOrNull(f.Service),
		FromTs:     timestampPtr(f.From),
		ToTs:       timestampPtr(f.To),
		RowLimit:   int32(f.Limit),
		RowOffset:  int32(f.Offset),
	})
	if err != nil {
		return nil, fmt.Errorf("repository: 查询事件列表失败: %w", err)
	}
	out := make([]incident.Summary, 0, len(rows))
	for i := range rows {
		row := &rows[i]
		out = append(out, incident.Summary{
			ID:               row.ID,
			Title:            row.Title,
			Severity:         row.Severity,
			Status:           incident.Status(row.Status),
			Service:          textOrEmpty(row.Service),
			StartedAt:        row.StartedAt.Time,
			ResolvedAt:       fromNullableTimestamp(row.ResolvedAt),
			UpdatedAt:        row.UpdatedAt.Time,
			AlertCount:       int(row.AlertCount),
			NeedsReviewCount: int(row.NeedsReviewCount),
		})
	}
	return out, nil
}

// Timeline 取事件的时间线（时间倒序）。
func (o *lifecycleOps) Timeline(ctx context.Context, incidentID int64, limit int) ([]incident.Entry, error) {
	rows, err := o.queries.ListTimelineByIncident(ctx, db.ListTimelineByIncidentParams{
		IncidentID: incidentID,
		RowLimit:   int32(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("repository: 查询事件 %d 的时间线失败: %w", incidentID, err)
	}
	out := make([]incident.Entry, 0, len(rows))
	for i := range rows {
		row := &rows[i]
		out = append(out, incident.Entry{
			IncidentID:  row.IncidentID,
			EventType:   incident.EventType(row.EventType),
			Description: row.Description,
			Actor:       textOrEmpty(row.Actor),
			Metadata:    parseStringMap(row.Metadata),
			Timestamp:   row.Timestamp.Time,
		})
	}
	return out, nil
}

// Alerts 取事件的关联告警。
func (o *lifecycleOps) Alerts(ctx context.Context, incidentID int64, limit int) ([]incident.AlertLink, error) {
	rows, err := o.queries.ListIncidentAlertsByIncident(ctx, db.ListIncidentAlertsByIncidentParams{
		IncidentID: incidentID,
		RowLimit:   int32(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("repository: 查询事件 %d 的关联告警失败: %w", incidentID, err)
	}
	out := make([]incident.AlertLink, 0, len(rows))
	for i := range rows {
		row := &rows[i]
		out = append(out, incident.AlertLink{
			AlertID:          row.AlertID,
			Fingerprint:      row.Fingerprint,
			Severity:         row.Severity,
			Labels:           parseStringMap(row.Labels),
			StartsAt:         row.StartsAt.Time,
			EndsAt:           fromNullableTimestamp(row.EndsAt),
			CorrelationScore: numericToFloatPtr(row.CorrelationScore),
			NeedsReview:      row.NeedsReview,
			CorrelatedAt:     row.CorrelatedAt.Time,
		})
	}
	return out, nil
}

// CountAlerts 统计事件的关联告警总数（不受详情页 200 条内联上限影响）。
func (o *lifecycleOps) CountAlerts(ctx context.Context, incidentID int64) (int, error) {
	n, err := o.queries.CountIncidentAlertsByIncident(ctx, incidentID)
	if err != nil {
		return 0, fmt.Errorf("repository: 统计事件 %d 的关联告警数失败: %w", incidentID, err)
	}
	return int(n), nil
}

// CountNeedsReviewAlerts 统计该事件下仍待确认的关联边总数。
func (o *lifecycleOps) CountNeedsReviewAlerts(ctx context.Context, incidentID int64) (int, error) {
	n, err := o.queries.CountNeedsReviewAlertsByIncident(ctx, incidentID)
	if err != nil {
		return 0, fmt.Errorf("repository: 统计事件 %d 的待确认关联边失败: %w", incidentID, err)
	}
	return int(n), nil
}
