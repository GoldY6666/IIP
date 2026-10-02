-- ============================================================================
-- IIP M3 · 0004_incident_lifecycle_m3.up.sql
-- 规格来源：docs/IIP-M3详细开发手册.md §4.1 / §4.2 / §4.5 / §五
--
-- 本迁移为 M3「事件生命周期（状态机 + Timeline）」铺路，改动面 = 2 个 CHECK
-- 放宽 + 1 张新表 + 2 个索引，**不新增/删除任何列**（resolved_at 复用既有列）：
--   1) incidents.status：5 值 → 6 值（新增 acknowledged，偏差 A1）。
--      0001 限定的是 open/investigating/mitigated/resolved/ignored，
--      状态机阶序里 acknowledged 排在 open 之后（手册 §4.1），缺它「认领」无法表达。
--   2) timeline.event_type：5 值 → 12 值（新增 7 个，偏差 A2）。
--      既有 5 值全部保留：status_change 手册 §4.2 明示保留（零存量行，是否删除
--      见 OPEN-M3-3），user_comment 还要被 REVIEW 的确认/拒绝复用。
--   3) review_suppressions：REVIEW 拒绝后的抑制表（手册 §4.5）。
--      不用既有 correlation_feedback：后者的键是具体 alert_id 对，新告警到来时
--      ID 必然不同，无法跨次复用；抑制需要按 fingerprint 复用。
--   4) idx_timeline_incident_ts：事件详情按 (incident_id, timestamp DESC) 读时间线。
--      idx_review_suppressions_fingerprint：抑制命中查询按 fingerprint 打。
--
-- 说明：
--   * 不改 0001 / 0002 / 0003（AGENTS.md 约束 10 冻结），本文件为唯一新增。
--   * 幂等：CREATE INDEX IF NOT EXISTS / DROP CONSTRAINT IF EXISTS /
--     ADD CONSTRAINT（约束名固定，重复执行同一文件不会叠加）。
--   * down 的三步破坏性（删 timeline 新类型行 / acknowledged 回写 open /
--     删 review_suppressions 表）见 .down.sql 头部，会真实丢数据。
-- ============================================================================

-- 1) 放宽 incidents.status：5 值 → 6 值（新增 acknowledged）
ALTER TABLE incidents
    DROP CONSTRAINT IF EXISTS chk_incidents_status;

ALTER TABLE incidents
    ADD CONSTRAINT chk_incidents_status
    CHECK (status IN ('open', 'acknowledged', 'investigating', 'mitigated', 'resolved', 'ignored'));

-- 2) 放宽 timeline.event_type：5 值 → 12 值（既有 5 + 新增 7）
ALTER TABLE timeline
    DROP CONSTRAINT IF EXISTS chk_timeline_event_type;

ALTER TABLE timeline
    ADD CONSTRAINT chk_timeline_event_type
    CHECK (event_type IN (
        -- 既有 5 个（保留不动）
        'alert_fired', 'alert_resolved', 'status_change', 'evidence_found', 'user_comment',
        -- M3 新增 7 个（手册 §4.2）
        'alert_linked', 'acknowledged', 'investigation_started',
        'mitigated', 'resolved', 'ignored', 'reopened'
    ));

-- 3) REVIEW 拒绝后的抑制表（手册 §4.5）
CREATE TABLE IF NOT EXISTS review_suppressions (
    id           BIGSERIAL PRIMARY KEY,
    fingerprint  VARCHAR(128) NOT NULL,        -- 与告警指纹同源：动态 label 已排除，可跨实例复用
    incident_id  BIGINT NOT NULL REFERENCES incidents(id) ON DELETE CASCADE,
    actor        VARCHAR(128),                 -- 做出该判定的人
    reason       TEXT,                         -- 拒绝理由（可选）
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_review_suppressions UNIQUE (fingerprint, incident_id)
);

-- 4) 索引：事件详情时间线读取 + 抑制命中查询
CREATE INDEX IF NOT EXISTS idx_timeline_incident_ts
    ON timeline(incident_id, timestamp DESC);

CREATE INDEX IF NOT EXISTS idx_review_suppressions_fingerprint
    ON review_suppressions(fingerprint);
