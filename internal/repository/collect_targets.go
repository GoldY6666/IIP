package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/yu/iip/internal/db"
	"github.com/yu/iip/internal/incident"
	"github.com/yu/iip/internal/worker"
)

// CollectTargets 是 worker.IncidentReader 的 PostgreSQL 实现。
//
// 独立成类型而不给 Incidents / Lifecycle 加方法：
// worker 包的依赖方向是 worker → 领域接口，实现放在 repository，
// 与 task.Store / evidence.Store 完全同构。加方法到 Lifecycle 上会让
// 「事件生命周期」这个类型同时承担「采集锚点读取」，而两者毫无关系。
type CollectTargets struct {
	queries *db.Queries
}

// 编译期断言：必须满足 worker 包的契约。
var _ worker.IncidentReader = (*CollectTargets)(nil)

// NewCollectTargets 构造采集锚点读取器。
func NewCollectTargets(conn db.DBTX) *CollectTargets {
	return &CollectTargets{queries: db.New(conn)}
}

// Target 读取事件的 service 与 started_at。
//
// 复用 GetIncidentByID：这条查询已经存在，另写一条只取两列的
// 会让「事件详情读到的 service」与「采集用的 service」有可能是两套 SQL ——
// 一旦其中一条改了列名或加了过滤，两边就会不一致，
// 而这种不一致只在真库跑采集时才显形，排查成本极高。
func (r *CollectTargets) Target(ctx context.Context, incidentID int64) (worker.Target, error) {
	if incidentID <= 0 {
		return worker.Target{}, fmt.Errorf("repository: incident_id 必须为正整数，当前为 %d", incidentID)
	}

	row, err := r.queries.GetIncidentByID(ctx, incidentID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// 事件不存在 → 采集无从谈起。这是**业务失败**（重试有意义：
			// 事件可能在事务提交后才被别处看到），故返回哨兵错误
			// 而不是零值 Target —— 零值会让调用方推导出退化窗口，
			// 把「事件不存在」伪装成「采集不到数据」。
			return worker.Target{}, fmt.Errorf("repository: 事件 %d 不存在: %w", incidentID, incident.ErrIncidentNotFound)
		}
		return worker.Target{}, fmt.Errorf("repository: 读取事件 %d 的采集锚点失败: %w", incidentID, err)
	}

	return worker.Target{
		Service:   textOrEmpty(row.Service),
		StartedAt: row.StartedAt.Time.UTC(),
	}, nil
}
