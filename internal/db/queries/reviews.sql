-- M3 REVIEW 抑制（手册 §4.5）。
--
-- 抑制键是 **(fingerprint, incident_id)**，不是 alert_id 对：新告警到来时 ID 必然不同，
-- 按 ID 记的抑制永远命中不了第二次。fingerprint 已剔除 instance/pod/container/node
-- 四个动态键，因此能跨实例、跨次复用。

-- name: IsSuppressed :one
-- 关联流程判定为 REVIEW 档时先查这里；命中则不建边、按 NEW 档处理。
-- 用 EXISTS 而不是 count(*)：只关心「有没有」，EXISTS 命中即可短路返回。
SELECT EXISTS (
    SELECT 1
    FROM review_suppressions
    WHERE fingerprint = sqlc.arg('fingerprint')
      AND incident_id = sqlc.arg('incident_id')
);

-- name: InsertReviewSuppression :exec
-- 写一条抑制行。同一 (fingerprint, incident_id) 重复写入静默跳过：
-- 唯一约束已在 0004 建好，这里用 ON CONFLICT DO NOTHING 让它成为幂等操作，
-- 而不是让「同一个人连点两次拒绝」变成 500。
INSERT INTO review_suppressions (fingerprint, incident_id, actor, reason)
VALUES (sqlc.arg('fingerprint'),
        sqlc.arg('incident_id'),
        sqlc.narg('actor'),
        sqlc.narg('reason'))
ON CONFLICT (fingerprint, incident_id) DO NOTHING;
