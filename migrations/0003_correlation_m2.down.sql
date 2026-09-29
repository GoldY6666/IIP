-- ============================================================================
-- IIP M2 · 0003_correlation_m2.down.sql
-- 0003 的回滚：与 up 严格对称，保证 up → down → up 三连可重复。
-- ============================================================================

-- 2) 撤销 needs_review 标记
DROP INDEX IF EXISTS idx_incident_alerts_needs_review;
ALTER TABLE incident_alerts DROP COLUMN IF EXISTS needs_review;

-- 1) 删除种子配置行
DELETE FROM correlation_rules WHERE name = 'default';
