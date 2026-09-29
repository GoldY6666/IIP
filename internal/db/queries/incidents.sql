-- M2 关联引擎：Incident 创建、候选集查询、关联写入。
-- 规格依据：docs/IIP-M2详细开发手册.md §4.3 / §4.7。

-- name: CreateIncident :one
-- NEW 档：为一条无法与既有 Incident 关联的 firing 告警新建 Incident。
-- status 固定 'open'（M3 状态机负责后续流转）；service 取自告警 labels.service。
INSERT INTO incidents (title, severity, status, service, started_at)
VALUES ($1, $2, 'open', $3, $4)
RETURNING id, title, severity, status, service, started_at, resolved_at,
          root_causes, recommendations, created_at, updated_at;

-- name: ListOpenIncidentsWithAlerts :many
-- 取所有 open / investigating 的 Incident 及其已关联告警（供逐对评分）。
-- 每行是「一条已关联告警」；调用方按 incident_id 聚合。
-- ends_at 可能为 NULL（仍 firing），由 Go 侧按当前时刻兜底。
SELECT i.id              AS incident_id,
       i.status          AS incident_status,
       i.severity        AS incident_severity,
       a.id              AS alert_id,
       a.fingerprint     AS alert_fingerprint,
       a.severity        AS alert_severity,
       a.labels          AS alert_labels,
       a.starts_at       AS alert_starts_at,
       a.ends_at         AS alert_ends_at
FROM incidents i
JOIN incident_alerts ia ON ia.incident_id = i.id
JOIN alerts a           ON a.id = ia.alert_id
WHERE i.status IN ('open', 'investigating')
ORDER BY i.id, a.id;

-- name: LinkAlertToIncident :exec
-- 关联一条告警到 Incident。(incident_id, alert_id) 为主键，重复链接静默跳过（幂等）。
-- correlation_score 为 0.0000-1.0000；NEW 档传 NULL（无关联对象）。
-- needs_review = true 表示 REVIEW 档，待人工确认。
INSERT INTO incident_alerts (incident_id, alert_id, correlation_score, needs_review)
VALUES ($1, $2, sqlc.narg('score'), $3)
ON CONFLICT (incident_id, alert_id) DO NOTHING;

-- name: SetAlertIncident :exec
-- 回写 alerts.incident_id（反规范化指针），便于后续按 incident 直查告警。
UPDATE alerts
SET incident_id = $2, updated_at = NOW()
WHERE id = $1;
