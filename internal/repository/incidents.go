package repository

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yu/iip/internal/correlation"
	"github.com/yu/iip/internal/db"
)

// Incidents 是基于 PostgreSQL 的 correlation.IncidentStore 实现。
type Incidents struct {
	queries *db.Queries
}

// 编译期断言：必须满足关联引擎定义的仓储接口。
var _ correlation.IncidentStore = (*Incidents)(nil)

// NewIncidents 构造仓储。conn 取 db.DBTX，与 NewAlerts 一致（可接连接池或事务）。
func NewIncidents(conn db.DBTX) *Incidents {
	return &Incidents{queries: db.New(conn)}
}

// CreateIncident 新建 Incident（NEW 档）。
func (r *Incidents) CreateIncident(ctx context.Context, in correlation.Incident) (correlation.Incident, error) {
	row, err := r.queries.CreateIncident(ctx, db.CreateIncidentParams{
		Title:     in.Title,
		Severity:  in.Severity,
		Service:   textOrNull(in.Service),
		StartedAt: timestamp(in.StartedAt),
	})
	if err != nil {
		return correlation.Incident{}, fmt.Errorf("repository: 新建事件失败: %w", err)
	}
	return correlation.Incident{
		ID:        row.ID,
		Title:     row.Title,
		Severity:  row.Severity,
		Status:    row.Status,
		Service:   textOrEmpty(row.Service),
		StartedAt: row.StartedAt.Time,
	}, nil
}

// ListOpenIncidentAlerts 取所有 open / investigating Incident 的已关联告警。
//
// perIncidentLimit 限定每个 incident 最多返回多少条（<=0 不限额）：
// 无上限时候选集 = 全部 open incident 的成员总数，随告警量只增不减
// （实测 1 万条时 7555 条，把关联拖到 ~4.5 条/s）。传 correlation.CandidateLimitPerIncident。
func (r *Incidents) ListOpenIncidentAlerts(ctx context.Context, perIncidentLimit int32) ([]correlation.CandidateAlert, error) {
	rows, err := r.queries.ListOpenIncidentsWithAlerts(ctx, perIncidentLimit)
	if err != nil {
		return nil, fmt.Errorf("repository: 查询候选事件告警失败: %w", err)
	}
	out := make([]correlation.CandidateAlert, 0, len(rows))
	for i := range rows {
		row := &rows[i]
		labels := correlation.ParseLabels(row.AlertLabels)
		out = append(out, correlation.CandidateAlert{
			IncidentID: row.IncidentID,
			View: correlation.AlertView{
				ID:          row.AlertID,
				Fingerprint: row.AlertFingerprint,
				Severity:    row.AlertSeverity,
				Service:     labels["service"],
				Namespace:   labels["namespace"],
				Labels:      labels,
				StartsAt:    row.AlertStartsAt.Time,
				EndsAt:      fromNullableTimestamp(row.AlertEndsAt),
			},
		})
	}
	return out, nil
}

// LinkAlertToIncident 建立关联边（幂等）。score 为 nil 表示 NEW 档（无关联对象）。
//
// alertStartsAt 是分区键（T7.1 / ADR-022）：incident_alerts 对 alerts 的
// 外键是复合 FK (alert_id, alert_starts_at) → alerts(id, starts_at)，
// 调用方必须传告警自身的 starts_at；传错值 = 外键违反（23503），
// 不会静默错链（这正是复合 FK 相对「只按 alert_id 关联」多出来的保护）。
func (r *Incidents) LinkAlertToIncident(ctx context.Context, incidentID, alertID int64, alertStartsAt time.Time, score *float64, needsReview bool) error {
	err := r.queries.LinkAlertToIncident(ctx, db.LinkAlertToIncidentParams{
		IncidentID:    incidentID,
		AlertID:       alertID,
		AlertStartsAt: timestamp(alertStartsAt),
		NeedsReview:   needsReview,
		Score:         numericFromFloatPtr(score),
	})
	if err != nil {
		return fmt.Errorf("repository: 关联事件-告警失败: %w", err)
	}
	return nil
}

// SetAlertIncident 回写 alerts.incident_id。
func (r *Incidents) SetAlertIncident(ctx context.Context, alertID, incidentID int64) error {
	err := r.queries.SetAlertIncident(ctx, db.SetAlertIncidentParams{
		ID:         alertID,
		IncidentID: pgtype.Int8{Int64: incidentID, Valid: true},
	})
	if err != nil {
		return fmt.Errorf("repository: 回写告警 incident_id 失败: %w", err)
	}
	return nil
}

// IncidentCounts 返回某事件的关联告警数与证据数（T6.1 通知消息用，ADR-024）。
//
// 满足 notify.CountsProvider。事件不存在时两个计数均为 0（查询本身是标量子查询，
// 不报错）——通知侧按 0 展示，不让「事件刚好被删」炸掉派发循环。
func (r *Incidents) IncidentCounts(ctx context.Context, incidentID int64) (alertCount, evidenceCount int64, err error) {
	row, qErr := r.queries.IncidentNotifyCounts(ctx, incidentID)
	if qErr != nil {
		return 0, 0, fmt.Errorf("repository: 统计事件计数失败: %w", qErr)
	}
	return row.AlertCount, row.EvidenceCount, nil
}

// textOrNull 把空串映射为 SQL NULL。
func textOrNull(s string) pgtype.Text {
	if s == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: s, Valid: true}
}

// textOrEmpty 是 textOrNull 的逆运算。
func textOrEmpty(t pgtype.Text) string {
	if !t.Valid {
		return ""
	}
	return t.String
}

// numericFromFloatPtr 把 *float64 转成 pgtype.Numeric（nil → SQL NULL）。
//
// 走字符串路径而非直接 Scan(float64)：Numeric.Scan 对 string 的支持最稳妥，
// 避免不同 pgx 版本对 float64 处理的差异。保留 4 位小数，与列精度 DECIMAL(5,4) 对齐。
func numericFromFloatPtr(f *float64) pgtype.Numeric {
	if f == nil {
		return pgtype.Numeric{}
	}
	var n pgtype.Numeric
	if err := n.Scan(strconv.FormatFloat(*f, 'f', 4, 64)); err != nil {
		return pgtype.Numeric{}
	}
	return n
}
