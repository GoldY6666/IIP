-- ============================================================================
-- IIP M2 · 0003_correlation_m2.up.sql
-- 规格来源：docs/IIP-M2详细开发手册.md §4.4 / §4.8 / §五
--
-- 本迁移只做两件事（改动面尽量小，不加新表）：
--   1) 在既有 correlation_rules 中种子一行 name='default'，其 rule_config
--      存 M2 关联引擎的权重与阈值（V1.0 §5 默认值）。改配置即 UPDATE 此行，
--      无需重启——满足手册「配置可调且免重启生效」的验收。
--   2) 给 incident_alerts 增加 needs_review 标记：REVIEW 档（0.50 ≤ score < 0.80）
--      的告警仍链接到候选 Incident，但打标待人工确认，供 M3 的确认队列消费。
--
-- 说明：
--   * 不新增表；0001 / 0002 已冻结（AGENTS.md 约束 10），本文件为唯一新增。
--   * 幂等：全部语句可重复执行（ON CONFLICT / IF NOT EXISTS）。
-- ============================================================================

-- 1) 种子默认关联配置（单行）
INSERT INTO correlation_rules (name, description, enabled, rule_config, weight)
VALUES (
    'default',
    'M2 关联引擎默认权重与阈值（V1.0 §5）',
    TRUE,
    '{
       "weights":    {"time": 0.25, "service": 0.25, "label": 0.20, "dependency": 0.20, "historical": 0.10},
       "thresholds": {"merge": 0.80, "review": 0.50}
     }'::jsonb,
    1.0
)
ON CONFLICT (name) DO NOTHING;

-- 2) REVIEW 档落库所需：关联边增加「待人工确认」标记
ALTER TABLE incident_alerts
    ADD COLUMN IF NOT EXISTS needs_review BOOLEAN NOT NULL DEFAULT FALSE;

CREATE INDEX IF NOT EXISTS idx_incident_alerts_needs_review
    ON incident_alerts(needs_review) WHERE needs_review;
