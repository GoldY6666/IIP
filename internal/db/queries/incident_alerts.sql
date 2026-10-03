-- M3 详情页：关联告警与 REVIEW 队列（手册 §4.4 / §4.5 / §4.7）。

-- name: ListIncidentAlertsByIncident :many
-- 详情页的「关联告警」列表。逐行读 incident_alerts，再 JOIN alerts 补齐展示字段。
-- correlation_score 为 NULL 表示 NEW 档（该告警是事件的起因），前端显示「—」而不是 0。
SELECT ia.alert_id,
       ia.correlation_score,
       ia.needs_review,
       ia.correlated_at,
       a.fingerprint,
       a.severity,
       a.labels,
       a.starts_at,
       a.ends_at
FROM incident_alerts ia
JOIN alerts a ON a.id = ia.alert_id
WHERE ia.incident_id = sqlc.arg('incident_id')
ORDER BY ia.correlated_at DESC, ia.alert_id DESC
LIMIT sqlc.arg('row_limit')::int;

-- name: ListReviewEdges :many
-- 待确认队列：needs_review 为真的关联边（0003 已建部分索引，直接可用）。
-- 与列表接口同样用 limit+1 探测分页，故 limit 由调用方按 limit+1 传入。
SELECT ia.incident_id,
       ia.alert_id,
       ia.correlation_score,
       ia.needs_review,
       ia.correlated_at,
       a.fingerprint,
       a.severity,
       a.labels,
       a.starts_at,
       a.ends_at
FROM incident_alerts ia
JOIN alerts a ON a.id = ia.alert_id
WHERE ia.needs_review
ORDER BY ia.correlated_at DESC, ia.alert_id DESC
LIMIT sqlc.arg('row_limit')::int OFFSET sqlc.arg('row_offset')::int;

-- name: GetReviewEdge :one
-- 判定前确认「这条待确认边还在」。
--
-- 带上 needs_review 条件：已经确认过的边（needs_review=FALSE）不应再被操作一次，
-- 否则重复 reject 会往抑制表写第二次、重复 confirm 会产生重复留痕。
SELECT ia.incident_id,
       ia.alert_id,
       ia.correlation_score,
       ia.needs_review,
       ia.correlated_at,
       a.fingerprint,
       a.severity,
       a.labels,
       a.starts_at,
       a.ends_at
FROM incident_alerts ia
JOIN alerts a ON a.id = ia.alert_id
WHERE ia.incident_id = sqlc.arg('incident_id')
  AND ia.alert_id    = sqlc.arg('alert_id')
  AND ia.needs_review;

-- name: ConfirmReviewEdge :execrows
-- 确认合并：清掉待确认标记，**保留**关联边（确认的含义是「这条关联是对的」）。
UPDATE incident_alerts
SET needs_review = FALSE
WHERE incident_id = sqlc.arg('incident_id')
  AND alert_id    = sqlc.arg('alert_id')
  AND needs_review;

-- name: DeleteIncidentAlert :execrows
-- 拒绝合并：删除该关联边。
DELETE FROM incident_alerts
WHERE incident_id = sqlc.arg('incident_id')
  AND alert_id    = sqlc.arg('alert_id');

-- name: CountIncidentAlertsByAlert :one
-- 拒绝后判断该告警是否还有别的归属：没有才把 alerts.incident_id 回写为 NULL。
SELECT count(*)
FROM incident_alerts
WHERE alert_id = sqlc.arg('alert_id');

-- name: ClearAlertIncident :execrows
-- 把告警的 incident_id 置 NULL（该告警已不属于任何事件）。
UPDATE alerts
SET incident_id = NULL, updated_at = NOW()
WHERE id = sqlc.arg('alert_id')
  AND incident_id IS NOT NULL;
