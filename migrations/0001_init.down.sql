-- ============================================================================
-- IIP M0 · 0001_init.down.sql
-- 回滚 0001_init.up.sql 的全部变更（DROP 全部表）。
-- 顺序：先删从属表（有外键），再删被引用表，避免外键依赖报错。
-- 依赖 `IF EXISTS` 保证可重复执行不报错。
-- ============================================================================

DROP TABLE IF EXISTS correlation_feedback;
DROP TABLE IF EXISTS correlation_rules;
DROP TABLE IF EXISTS timeline;
DROP TABLE IF EXISTS evidence;
DROP TABLE IF EXISTS incident_alerts;
DROP TABLE IF EXISTS incidents;
DROP TABLE IF EXISTS alerts;
