-- ============================================================================
-- IIP M3 · 0004_incident_lifecycle_m3.down.sql
-- 0004 的回滚：与 up 严格逆序，保证 up → down → up 三连可重复。
--
-- ⚠️ 破坏性（必须明示，手册 §五）：本文件会**真实丢数据**，三步如下——
--   ① DELETE FROM timeline WHERE event_type IN (<7 个新值>)
--      → M3 写入的全部状态流转留痕行被删除（这些行的语义无法回退到既有类型：
--        status_change 只是通用类型，把 acknowledged/mitigated 等折叠过去会丢精度）。
--   ② UPDATE incidents SET status='open' WHERE status='acknowledged'
--      → 「已认领未调查」的信息被降级为「刚创建」。acknowledged 是 0001 CHECK
--        里没有的值，不回写就无法把 CHECK 还原（PG 会拒绝重建约束）。
--   ③ DROP TABLE review_suppressions
--      → 全部抑制行丢失，回滚后再次 up 时队列会重新出现曾被拒绝的候选边。
--
-- 本 down 只为「三连可重复」与本地回滚服务；**生产回滚前必须先备份**这三处数据。
-- ============================================================================

-- 5) 撤销索引（逆序：先 review_suppressions 的，再 timeline 的）
DROP INDEX IF EXISTS idx_review_suppressions_fingerprint;
DROP INDEX IF EXISTS idx_timeline_incident_ts;

-- 4→3) 删除抑制表（破坏性 ③）
DROP TABLE IF EXISTS review_suppressions;

-- 2) 还原 timeline.event_type 到 5 值 —— 先删新增类型的行（破坏性 ①），
--    否则旧 CHECK 重建时会被既有行违反而报 23514。
DELETE FROM timeline
 WHERE event_type IN ('alert_linked', 'acknowledged', 'investigation_started',
                      'mitigated', 'resolved', 'ignored', 'reopened');

ALTER TABLE timeline
    DROP CONSTRAINT IF EXISTS chk_timeline_event_type;

ALTER TABLE timeline
    ADD CONSTRAINT chk_timeline_event_type
    CHECK (event_type IN ('alert_fired', 'alert_resolved', 'status_change', 'evidence_found', 'user_comment'));

-- 1) 还原 incidents.status 到 5 值 —— 先把 acknowledged 回写为 open（破坏性 ②），
--    否则旧 CHECK 重建时会被既有行违反而报 23514。
UPDATE incidents SET status = 'open' WHERE status = 'acknowledged';

ALTER TABLE incidents
    DROP CONSTRAINT IF EXISTS chk_incidents_status;

ALTER TABLE incidents
    ADD CONSTRAINT chk_incidents_status
    CHECK (status IN ('open', 'investigating', 'mitigated', 'resolved', 'ignored'));
