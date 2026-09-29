-- M2 关联引擎：配置读写 + 历史共现查询。
-- 规格依据：docs/IIP-M2详细开发手册.md §4.4 / §4.7。

-- name: GetCorrelationConfig :one
-- 取单行配置（name='default'）。零行时调用方回退到内置默认值。
SELECT id, name, description, enabled, rule_config, weight, created_at, updated_at
FROM correlation_rules
WHERE name = 'default';

-- name: UpsertCorrelationConfig :one
-- 写单行配置：存在即更新 rule_config，不存在则插入（幂等）。
-- 允许通过 enabled 关闭整个关联引擎。
INSERT INTO correlation_rules (name, description, enabled, rule_config, weight)
VALUES ('default', 'M2 关联引擎默认权重与阈值（V1.0 §5）', $2, $1, 1.0)
ON CONFLICT (name) DO UPDATE
    SET rule_config = EXCLUDED.rule_config,
        enabled     = EXCLUDED.enabled,
        updated_at  = NOW()
RETURNING id, name, description, enabled, rule_config, weight, created_at, updated_at;

-- name: FingerprintCoOccurrence :one
-- 历史相似度的数据源：统计两个 fingerprint 在「同一 Incident」中的共现比例。
--   co    = 同时包含两者的 Incident 数
--   total = 包含任一者的 Incident 数（并集）
-- 零历史（total=0）时调用方返回 0.0。
WITH pair_incidents AS (
    SELECT DISTINCT ia.incident_id, a.fingerprint
    FROM incident_alerts ia
    JOIN alerts a ON a.id = ia.alert_id
    WHERE a.fingerprint IN ($1, $2)
),
per_incident AS (
    SELECT incident_id, COUNT(DISTINCT fingerprint)::int AS fp_count
    FROM pair_incidents
    GROUP BY incident_id
)
SELECT
    COUNT(*) FILTER (WHERE fp_count = 2)::float8 AS co,
    COUNT(*)::float8                            AS total
FROM per_incident;
