// Package partition 负责 alerts 表月度分区的运维面（T7.1 / ADR-022）。
//
// 边界划分：
//   - 数据库侧（migrations/0008）拥有分区布局与两个维护函数
//     （补建 / 过期清理），本包**不手写 SQL**；
//   - 本包只做三件事：错误分类（SQLSTATE 23514）、维护编排
//     （ensure → drop → scan）、以及与 worker 维护 tick 的接线。
//
// 读路径（重复 firing 扫描）不走本包：它在 internal/db/queries/partition.sql
// 里是纯 sqlc 查询（ScanFiringDuplicates），本包只是转发。
package partition

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/yu/iip/internal/db"
)

// DefaultAheadMonths 是默认预创建的未来月分区数（不含当月，共 ahead+1 个）。
//
// 三处必须保持一致，但它们互相引用不到，改一处必须同步另两处：
//  1. 本常量（插入路径 23514 重试、NewMaintainer 的负值收敛）；
//  2. migrations/0008 建初始分区的 "+2 months"（SQL，引用不了 Go 常量）；
//  3. iip_ensure_alert_partitions 函数里 ahead_months < 0 时的兜底 2（在库里）。
const DefaultAheadMonths = 2

// SQLStateNoPartition 是「没有为行找到关系的分区」的 SQLSTATE。
//
// 判据用错误码而非错误文本：文本随数据库 locale 变（中文库是
// 「没有为行找到关系的分区」），且 PostgreSQL 不保证文案跨版本稳定。
// 同码的 CHECK 违反（23514）不会走到 alerts 插入路径——它由各自列的
// CHECK 在更早的阶段拦截，故此处不需要再细分。
const SQLStateNoPartition = "23514"

// IsNoPartition 报告错误是否为「插入行找不到分区」。
//
// errors.As 而不是类型断言：pgx 可能把 *pgconn.PgError 包在多层
// fmt.Errorf 里（repository 各处的 %w 包装），类型断言会漏判。
func IsNoPartition(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == SQLStateNoPartition
}

// Querier 是分区维护所需的最小查询面。
//
// 窄接口而非直接依赖 *db.Queries：单测可注入假实现，精确控制
// 「补建了几个分区 / 删了多少行 / 扫出几组重复」并断言调用顺序，
// 不必连真库。*db.Queries 天然满足本接口（编译期断言见下）。
type Querier interface {
	EnsureAlertPartitions(ctx context.Context, arg db.EnsureAlertPartitionsParams) ([]string, error)
	DropExpiredAlertPartitions(ctx context.Context, retentionDays int32) (int64, error)
	ScanFiringDuplicates(ctx context.Context) ([]db.ScanFiringDuplicatesRow, error)
}

// 编译期断言：sqlc 生成的 *db.Queries 必须满足窄接口。
var _ Querier = (*db.Queries)(nil)

// Maintainer 执行分区维护：补建未来分区 + 过期清理 + 重复 firing 曝光。
//
// aheadMonths / retentionDays 是**运维口径**参数，由 internal/config 注入
// （IIP_PARTITION_AHEAD_MONTHS / IIP_ALERT_RETENTION_DAYS）：
// 前者决定预建多远，后者决定留多久。
type Maintainer struct {
	q             Querier
	logger        *slog.Logger
	aheadMonths   int
	retentionDays int
}

// NewMaintainer 构造维护器。logger 为 nil 时退化为 slog.Default()。
//
// 两个参数的取值约束在这里成对出现是有意的：
//   - aheadMonths < 0 收敛为 DefaultAheadMonths（0 合法 = 只建当月）；
//     负数本身无业务含义，但数据库函数对负值的行为是「静默按默认 2」，
//     在构造器里显式收敛，避免「配了 -1 实际建了 3 个月」的静默偏差。
//   - retentionDays <= 0 直接报错：保留期配 0/负数会让维护 tick 的
//     DELETE 删掉**全表**。那必须是启动期错误而不是运行期惊喜——
//     「保留 0 天」在任何运维语境下都是误配，要清库应是显式运维动作。
func NewMaintainer(q Querier, logger *slog.Logger, aheadMonths, retentionDays int) (*Maintainer, error) {
	if q == nil {
		return nil, errors.New("partition: NewMaintainer 的 Querier 不能为 nil")
	}
	if logger == nil {
		logger = slog.Default()
	}
	if aheadMonths < 0 {
		aheadMonths = DefaultAheadMonths
	}
	if retentionDays <= 0 {
		return nil, fmt.Errorf("partition: retentionDays 必须为正数，实际 %d"+
			"（0 或负数会让维护 tick 删除全部告警）", retentionDays)
	}
	return &Maintainer{q: q, logger: logger, aheadMonths: aheadMonths, retentionDays: retentionDays}, nil
}

// Ensure 补建未来分区（FromTs 传零值 = NULL = 只保当月及未来）。
// 返回本次新建的分区名（空 = 无需补建）。
func (m *Maintainer) Ensure(ctx context.Context) ([]string, error) {
	names, err := m.q.EnsureAlertPartitions(ctx, db.EnsureAlertPartitionsParams{
		// FromTs 零值 → pgtype.Timestamptz{Valid:false} → SQL NULL，
		// 函数内 COALESCE 到当月，即「只保未来」形态。
		AheadMonths: int32(m.aheadMonths),
	})
	if err != nil {
		return nil, fmt.Errorf("partition: 补建 alerts 分区失败: %w", err)
	}
	return names, nil
}

// DropExpired 删除过期告警行（含级联的 incident_alerts 链接），返回删除行数。
func (m *Maintainer) DropExpired(ctx context.Context) (int64, error) {
	n, err := m.q.DropExpiredAlertPartitions(ctx, int32(m.retentionDays))
	if err != nil {
		return 0, fmt.Errorf("partition: 清理过期告警失败: %w", err)
	}
	return n, nil
}

// ScanFiringDuplicates 返回跨 starts_at 并存的 firing 分组（残余缺口曝光）。
func (m *Maintainer) ScanFiringDuplicates(ctx context.Context) ([]db.ScanFiringDuplicatesRow, error) {
	rows, err := m.q.ScanFiringDuplicates(ctx)
	if err != nil {
		return nil, fmt.Errorf("partition: 扫描重复 firing 失败: %w", err)
	}
	return rows, nil
}

// RunMaintenance 执行一轮完整维护：先 ensure，再按保留期清理，
// 最后扫重复 firing 并逐条 WARN。
//
// 顺序有依赖：ensure 必须先行——它是另两步的地基（删/扫都假设分区在位；
// 补建本身也保证后续插入不撞 23514）。
//
// 任一步失败即整体返回错误，由调用方（worker）决定只记日志：
// 维护是旁路职责，失败不该拖垮证据采集主循环；但绝不静默——
// 补建失败会让插入路径持续撞 23514，日志是唯一线索。
func (m *Maintainer) RunMaintenance(ctx context.Context) error {
	created, err := m.Ensure(ctx)
	if err != nil {
		return err
	}
	if len(created) > 0 {
		m.logger.Info("已补建 alerts 分区", slog.Any("partitions", created))
	}

	deleted, err := m.DropExpired(ctx)
	if err != nil {
		return err
	}
	if deleted > 0 {
		m.logger.Info("已清理过期告警",
			slog.Int64("deleted", deleted),
			slog.Int("retention_days", m.retentionDays))
	}

	dups, err := m.ScanFiringDuplicates(ctx)
	if err != nil {
		return err
	}
	for _, d := range dups {
		// 逐条 WARN 而非只报计数：每组重复都要能直接定位到 alert_id，
		// 运维需要知道「是哪几条」而不是「有几组」。
		m.logger.Warn("发现同指纹多条 firing 并存（ADR-022 唯一性收敛后的已知缺口）",
			slog.String("fingerprint", d.Fingerprint),
			slog.Int64("firing_count", d.FiringCount),
			slog.Any("alert_ids", d.AlertIds),
			slog.Time("min_starts_at", d.MinStartsAt.Time),
			slog.Time("max_starts_at", d.MaxStartsAt.Time))
	}
	return nil
}
