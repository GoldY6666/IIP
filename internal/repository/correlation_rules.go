package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/yu/iip/internal/correlation"
	"github.com/yu/iip/internal/db"
)

// CorrelationConfig 是基于 PostgreSQL 的 correlation.ConfigStore 实现。
//
// 配置存于 correlation_rules 的单行（name='default'），字段 rule_config 承载
// weights/thresholds。查询每轮读一次库，因此「改配置免重启生效」自然成立。
type CorrelationConfig struct {
	queries *db.Queries
}

// 编译期断言：必须满足关联引擎定义的配置仓储接口。
var _ correlation.ConfigStore = (*CorrelationConfig)(nil)

// NewCorrelationConfig 构造仓储。
func NewCorrelationConfig(conn db.DBTX) *CorrelationConfig {
	return &CorrelationConfig{queries: db.New(conn)}
}

// GetConfig 读单行配置。行不存在时回退到内置默认值（不报错），
// 使「库尚未种子」也能正常关联。
func (r *CorrelationConfig) GetConfig(ctx context.Context) (correlation.Config, error) {
	row, err := r.queries.GetCorrelationConfig(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return correlation.DefaultConfig(), nil
	}
	if err != nil {
		return correlation.Config{}, fmt.Errorf("repository: 读取关联配置失败: %w", err)
	}
	cfg, err := correlation.UnmarshalConfigJSON(row.RuleConfig)
	if err != nil {
		return correlation.Config{}, err
	}
	// enabled 列是总开关的权威来源（rule_config 内的同名值仅作冗余自洽）。
	cfg.Enabled = row.Enabled
	return cfg, nil
}

// PutConfig 写单行配置（幂等 upsert）。
func (r *CorrelationConfig) PutConfig(ctx context.Context, cfg correlation.Config) error {
	raw, err := correlation.MarshalConfigJSON(cfg)
	if err != nil {
		return fmt.Errorf("repository: 序列化关联配置失败: %w", err)
	}
	if _, err := r.queries.UpsertCorrelationConfig(ctx, db.UpsertCorrelationConfigParams{
		RuleConfig: raw,
		Enabled:    cfg.Enabled,
	}); err != nil {
		return fmt.Errorf("repository: 写入关联配置失败: %w", err)
	}
	return nil
}

// CoOccurrence 返回两个 fingerprint 的共现计数。
func (r *CorrelationConfig) CoOccurrence(ctx context.Context, fpA, fpB string) (co, total float64, err error) {
	row, err := r.queries.FingerprintCoOccurrence(ctx, db.FingerprintCoOccurrenceParams{
		Fingerprint:   fpA,
		Fingerprint_2: fpB,
	})
	if err != nil {
		return 0, 0, fmt.Errorf("repository: 查询历史共现失败: %w", err)
	}
	return row.Co, row.Total, nil
}
