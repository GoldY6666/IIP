-- M3 事件生命周期：timeline 留痕。
-- 规格依据：docs/IIP-M3详细开发手册.md §4.2 / §4.7。

-- name: InsertTimelineEntry :one
-- 写入一条时间线条目并返回整行（含数据库生成的 id / timestamp）。
-- timestamp 由调用方显式给出（与 evidence 同口径：事件时间来自发生时刻，不用 NOW() 兜底，
-- 见 ADR-003）——状态流转是「刚刚发生」的，故 incident 服务层传 time.Now()。
-- actor 与 metadata 可空：系统自动写入的事件（如 alert_linked）可能没有操作人。
INSERT INTO timeline (incident_id, event_type, description, actor, metadata, timestamp)
VALUES (sqlc.arg('incident_id'),
        sqlc.arg('event_type'),
        sqlc.arg('description'),
        sqlc.narg('actor'),
        COALESCE(sqlc.narg('metadata'), '{}'::jsonb),
        sqlc.arg('timestamp'))
RETURNING id, incident_id, event_type, description, timestamp, actor, metadata, created_at;

-- name: ListTimelineByIncident :many
-- 详情页时间线：新近在上。id DESC 作为次级排序键，避免同一时刻的多条事件
-- 在翻页时顺序漂移（只按 timestamp 排序时，同秒事件的前后关系是未定义的）。
-- row_limit 由调用方传入（默认 200），不写死在 SQL 里。
SELECT id, incident_id, event_type, description, timestamp, actor, metadata, created_at
FROM timeline
WHERE incident_id = sqlc.arg('incident_id')
ORDER BY timestamp DESC, id DESC
LIMIT sqlc.arg('row_limit')::int;
