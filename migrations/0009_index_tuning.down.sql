-- ============================================================================
-- IIP T7.2 · 0009_index_tuning.down.sql
--
-- 回滚 0009：删 tasks 表达式索引 + 重建 alerts labels GIN（0001 原样）。
-- 与 up 严格镜像；无数据破坏（纯索引）。
--
-- 顺序说明：先删新建的，再还原删掉的——两者无依赖，顺序只为可读。
-- ============================================================================

-- 1) 删 T7.2 新建的 tasks 表达式索引
DROP INDEX IF EXISTS idx_tasks_payload_incident;

-- 2) 还原 0001 的 labels GIN（在分区父表上建 = 全部分区各建一份）
CREATE INDEX IF NOT EXISTS idx_alerts_labels ON alerts USING GIN(labels);
