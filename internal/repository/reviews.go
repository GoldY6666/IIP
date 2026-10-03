package repository

import (
	"context"
	"fmt"

	"github.com/yu/iip/internal/correlation"
	"github.com/yu/iip/internal/db"
)

// Reviews 是 correlation.SuppressionStore 的 PostgreSQL 实现。
//
// 与 Lifecycle 分开：抑制是「关联流程的判定输入」，生命周期是「事件的处置写入」，
// 两者职责不同、调用时机不同，合成一个类型只会让谁依赖谁变模糊。
type Reviews struct {
	queries *db.Queries
}

// 编译期断言：必须满足关联引擎定义的抑制接口。
var _ correlation.SuppressionStore = (*Reviews)(nil)

// NewReviews 构造抑制仓储。conn 取 db.DBTX（可接连接池或事务）。
func NewReviews(conn db.DBTX) *Reviews {
	return &Reviews{queries: db.New(conn)}
}

// IsSuppressed 报告该指纹是否已被该事件拒绝过。
func (r *Reviews) IsSuppressed(ctx context.Context, fingerprint string, incidentID int64) (bool, error) {
	hit, err := r.queries.IsSuppressed(ctx, db.IsSuppressedParams{
		Fingerprint: fingerprint,
		IncidentID:  incidentID,
	})
	if err != nil {
		return false, fmt.Errorf("repository: 查询抑制失败: %w", err)
	}
	return hit, nil
}

// AddSuppression 写一条抑制行；重复写入静默跳过（SQL 侧 ON CONFLICT DO NOTHING）。
func (r *Reviews) AddSuppression(ctx context.Context, fingerprint string, incidentID int64, actor, reason string) error {
	err := r.queries.InsertReviewSuppression(ctx, db.InsertReviewSuppressionParams{
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
