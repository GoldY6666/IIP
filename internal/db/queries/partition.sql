-- ============================================================================
-- IIP T7.1 · 分区维护函数查询封装
-- 规格来源：migrations/0008_alerts_partition.up.sql 步骤 10 的三个维护函数。
--
-- 这些查询**只封装函数调用**，业务逻辑全在数据库侧（与全项目「SQL 走 sqlc」
-- 的纪律一致）；Go 侧只做编排与错误分类（见 internal/partition）。
--
-- 消费方：
--   * EnsureAlertPartitions      —— worker 维护 tick（from_ts=NULL）+ 插入路径
--                                   23514 重试（from_ts=告警 starts_at）
--   * DropExpiredAlertPartitions —— worker 维护 tick（retention_days 来自配置）
--   * ScanFiringDuplicates       —— worker 维护 tick（曝光跨 starts_at 重火）
-- ============================================================================

-- name: EnsureAlertPartitions :many
-- 补建 alerts 月度分区，返回本次新建的分区名（空结果 = 无需补建）。
--
-- from_ts 传 NULL = 只保未来（维护 tick 形态）；传告警自身 starts_at =
-- 保证目标月必被覆盖（插入路径 23514 重试形态）。覆盖范围是
-- [min(from_ts 月, 当月), max(from_ts 月, 当月 + ahead_months)]，幂等。
SELECT iip_ensure_alert_partitions(sqlc.arg('from_ts')::timestamptz, sqlc.arg('ahead_months')::int);

-- name: DropExpiredAlertPartitions :one
-- 删除 starts_at 早于 retention_days 的告警行，返回删除行数。
-- 行级 DELETE + ON DELETE CASCADE 会连带清掉 incident_alerts 的过期链接
-- （复合 FK (alert_id, alert_starts_at) 见 0008 步骤 9）。
SELECT iip_drop_expired_alert_partitions(sqlc.arg('retention_days')::int);

-- name: ScanFiringDuplicates :many
-- 列出同一 fingerprint 存在多条 firing 的分组（ADR-022 唯一性收敛后的残余缺口）。
--
-- 为什么是纯 SQL 而不是库函数：sqlc 不展开 RETURNS TABLE 函数的列
-- （SELECT * FROM func() 会退化成 []interface{}，无类型安全），
-- 读路径按项目纪律住在 queries/ 里由 sqlc 生成结构体；
-- 运维要手工看时执行同一段 SQL 即可（与 generated 代码零漂移）。
--
-- 全表扫 firing 行，维护 tick 月级频次下可接受；量级上来后需改窄窗口
-- （如只扫最近 N 天，或由应用层 LatestByFingerprint 顺带记录）。
-- 显式 CAST 不可省：sqlc 推不出 array_agg / min / max 的返回类型
-- （会退化成 interface{}，Scan 时直接报错），聚合表达式的类型必须写死。
SELECT fingerprint,
       count(*)                          AS firing_count,
       array_agg(id ORDER BY id)::bigint[] AS alert_ids,
       min(starts_at)::timestamptz       AS min_starts_at,
       max(starts_at)::timestamptz       AS max_starts_at
FROM alerts
WHERE status = 'firing'
GROUP BY fingerprint
HAVING count(*) > 1;
