-- ============================================================================
-- IIP M0 · scripts/verify_inserts.sql
-- T0.2 验收：6 项手工 INSERT 验证（合法值 + 非法值混合）
--
-- 用法（环境就绪后，PG 容器已起、迁移已 up）：
--   psql "postgres://postgres:dev@localhost:5432/iip?sslmode=disable" -v ON_ERROR_STOP=0 -f scripts/verify_inserts.sql
--
-- 期望：№1-№6 中「合法」语句成功，「非法」语句全部被 CHECK 约束拒绝。
-- 每一条非法语句都应报错：ERROR: new row for relation ... violates check constraint "chk_..."
-- 为让脚本一次性跑完（不因报错中断），未设 ON_ERROR_STOP=1；请逐条核对输出。
-- 脚本末尾的清理语句会删除本次插入的所有验证数据。
--
-- 注意：本文件是人工验收脚本，不是迁移文件，因此不放在 migrations/（避免被 embed 进二进制）。
-- ============================================================================

\echo '===== 准备：一个 incidents 行（供 evidence/timeline/feedback 外键使用）====='
-- 合法：incidents 合法插入，记录 id 供后续引用
INSERT INTO incidents (title, severity, status, service, started_at)
VALUES ('验证用事件', 'critical', 'open', 'checkout', '2026-09-25T00:00:00Z')
RETURNING id AS incident_id \gset

\echo '===== №1 alerts.status：合法 firing 通过，非法 bogus 被拒 ====='
-- 合法
INSERT INTO alerts (fingerprint, source, severity, status, starts_at, raw_payload)
VALUES ('fp-legal-001', 'alertmanager', 'critical', 'firing', '2026-09-25T00:00:00Z', '{}');
-- 非法（期望：chk_alerts_status 拒绝）
INSERT INTO alerts (fingerprint, source, severity, status, starts_at, raw_payload)
VALUES ('fp-illegal-001', 'alertmanager', 'critical', 'bogus', '2026-09-25T00:00:00Z', '{}');

\echo '===== №2 alerts.severity：合法 warning 通过，非法 fatal 被拒 ====='
-- 合法
INSERT INTO alerts (fingerprint, source, severity, status, starts_at, raw_payload)
VALUES ('fp-legal-002', 'prometheus', 'warning', 'firing', '2026-09-25T00:00:00Z', '{}');
-- 非法（期望：chk_alerts_severity 拒绝）
INSERT INTO alerts (fingerprint, source, severity, status, starts_at, raw_payload)
VALUES ('fp-illegal-002', 'prometheus', 'fatal', 'firing', '2026-09-25T00:00:00Z', '{}');

\echo '===== №3 incidents.status：合法 investigating 通过，非法 pending 被拒 ====='
-- 合法
INSERT INTO incidents (title, severity, status) VALUES ('验证用事件2', 'warning', 'investigating');
-- 非法（期望：chk_incidents_status 拒绝）
INSERT INTO incidents (title, severity, status) VALUES ('验证用事件3', 'warning', 'pending');

\echo '===== №4 evidence.type：合法 log 通过，非法 alert 被拒 ====='
-- 合法（引用上面创建的 incident_id）
INSERT INTO evidence (incident_id, type, source, timestamp)
VALUES (:incident_id, 'log', 'loki', '2026-09-25T00:00:00Z');
-- 非法（期望：chk_evidence_type 拒绝）
INSERT INTO evidence (incident_id, type, source, timestamp)
VALUES (:incident_id, 'alert', 'loki', '2026-09-25T00:00:00Z');

\echo '===== №5 timeline.event_type：合法 status_change 通过，非法 foo 被拒 ====='
-- 合法
INSERT INTO timeline (incident_id, event_type, description)
VALUES (:incident_id, 'status_change', '从 open 变为 investigating');
-- 非法（期望：chk_timeline_event_type 拒绝）
INSERT INTO timeline (incident_id, event_type, description)
VALUES (:incident_id, 'foo', '非法事件类型');

\echo '===== №6 correlation_feedback.decision：合法 merge 通过，非法 maybe 被拒 ====='
-- 合法
INSERT INTO correlation_feedback (incident_id, alert_id_pair, decision, actor)
VALUES (:incident_id, '1,2', 'merge', 'oncall@example.com');
-- 非法（期望：chk_correlation_feedback_decision 拒绝）
INSERT INTO correlation_feedback (incident_id, alert_id_pair, decision, actor)
VALUES (:incident_id, '1,2', 'maybe', 'oncall@example.com');

\echo '===== 清理：删除本次验证数据 ====='
DELETE FROM correlation_feedback WHERE incident_id = :incident_id;
DELETE FROM timeline            WHERE incident_id = :incident_id;
DELETE FROM evidence            WHERE incident_id = :incident_id;
DELETE FROM alerts              WHERE fingerprint IN ('fp-legal-001', 'fp-illegal-001', 'fp-legal-002', 'fp-illegal-002');
DELETE FROM incidents           WHERE id = :incident_id;
DELETE FROM incidents           WHERE title IN ('验证用事件2', '验证用事件3');

\echo '===== 验证完毕 ====='
