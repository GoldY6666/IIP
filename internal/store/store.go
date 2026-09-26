// Package store 提供 PostgreSQL 连接池与迁移能力。
//
// 规格依据：docs/IIP-M0详细开发计划.md §T0.3 / §T0.4。
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres" // 注册 database/sql 的 postgres 迁移驱动
	"github.com/golang-migrate/migrate/v4/source/iofs"         // 从 embed.FS 读取迁移
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/lib/pq" // database/sql 的 postgres 驱动（被 migrate postgres 驱动依赖）

	iipmigrations "github.com/yu/iip/migrations"
)

// 连接池公共参数（M0 使用保守默认值，后续可按需外提为配置）。
const (
	connectTimeout = 5 * time.Second
	maxConns       = 10
)

// Connect 建立 pgxpool 连接并做一次 Ping 校验（快速失败，不带病启动）。
func Connect(ctx context.Context, pgURL string) (*pgxpool.Pool, error) {
	if pgURL == "" {
		return nil, errors.New("store.Connect: pgURL 不能为空")
	}

	cfg, err := pgxpool.ParseConfig(pgURL)
	if err != nil {
		return nil, fmt.Errorf("store.Connect: 解析连接串失败: %w", err)
	}
	cfg.MaxConns = maxConns
	cfg.ConnConfig.ConnectTimeout = connectTimeout

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("store.Connect: 创建连接池失败: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store.Connect: Ping 失败: %w", err)
	}
	return pool, nil
}

// Migrate 对目标库执行嵌入的迁移文件（幂等）。
//
// 使用 golang-migrate 的 iofs source（读取嵌入的 migrations/*.sql）
// 与 postgres database 驱动（通过 database/sql）。
// 库已是最新版本时返回 nil（不把 ErrNoChange 当错误）。
func Migrate(ctx context.Context, pgURL string) error {
	if pgURL == "" {
		return errors.New("store.Migrate: pgURL 不能为空")
	}

	src, err := iofs.New(iipmigrations.FS, ".")
	if err != nil {
		return fmt.Errorf("store.Migrate: 读取嵌入迁移失败: %w", err)
	}
	defer func() { _ = src.Close() }()

	m, err := migrate.NewWithSourceInstance("iofs", src, pgURL)
	if err != nil {
		return fmt.Errorf("store.Migrate: 初始化迁移器失败: %w", err)
	}
	defer func() {
		// migrate.Close 返回 (sourceErr, databaseErr)，此处忽略（迁移已完成）。
		_, _ = m.Close()
	}()

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("store.Migrate: 执行迁移失败: %w", err)
	}
	return nil
}

// HealthCheck 用 pgxpool 真查库（SELECT 1）。供 server 的 /healthz 使用。
func HealthCheck(ctx context.Context, pool *pgxpool.Pool) error {
	if pool == nil {
		return errors.New("store.HealthCheck: pool 为 nil")
	}
	var one int
	return pool.QueryRow(ctx, "SELECT 1").Scan(&one)
}
