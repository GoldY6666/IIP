-- ============================================================================
-- IIP M4 · 0005_evidence_tasks_m4.up.sql
-- 规格来源：docs/IIP-M4详细开发手册.md §4.1 / §五
--
-- 本迁移为 M4「Evidence 自动收集」铺路，只做两件事：
--   1) tasks 表：worker 的任务队列（DB polling 模式，V1.0 §2.2 允许替代 Redis）。
--      SKIP LOCKED 领取 + available_at 指数退避 + 死信（手册 §4.1 的五点设计）。
--   2) idx_evidence_incident_type：T4.6 的证据查询按 (incident_id, type) 读。
--
-- **不改 evidence 表结构** —— 该表 0001 已建好且约束完整
-- （chk_evidence_type 6 值、chk_evidence_confidence 0–1、3 个索引），
-- 手册 §二 E1 已确认。也不改 0001–0004（AGENTS.md 约束 10 冻结）。
-- ============================================================================

-- 1) 任务队列
CREATE TABLE IF NOT EXISTS tasks (
    id            BIGSERIAL PRIMARY KEY,
    kind          VARCHAR(32)  NOT NULL,          -- 任务类型；M4 只有 collect_evidence
    payload       JSONB        NOT NULL DEFAULT '{}',  -- 参数，如 {"incident_id": 12}
    status        VARCHAR(16)  NOT NULL DEFAULT 'pending',
                   -- pending（可领）/ in_flight（已领）/ done（成功）/ dead（死信）
    attempts      INT          NOT NULL DEFAULT 0,     -- 每次领取 +1；>=3 即判死信（手册 §4.1）
    available_at  TIMESTAMPTZ  NOT NULL DEFAULT NOW(), -- 可被领取的最早时刻（退避靠它）
    claimed_at    TIMESTAMPTZ,                       -- 最近一次被领取的时刻（判超时回收用）
    claimed_by    VARCHAR(64),                       -- 领取者标识（hostname-pid）
    last_error    TEXT,                              -- 最近一次失败原因
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW(),

    -- status 用 CHECK 而非枚举类型：枚举加值要 ALTER TYPE，重建 CHECK 只需改这一处，
    -- 且与 0001/0004 既有表全部用 CHECK 的风格一致。
    CONSTRAINT chk_tasks_status  CHECK (status IN ('pending', 'in_flight', 'done', 'dead')),
    -- attempts 不允许为负：那只会来自程序 bug，而负数会让「已尝试次数」的
    -- 退避计算得出比第 1 次更短的延迟。
    CONSTRAINT chk_tasks_attempts CHECK (attempts >= 0)
);

-- 领取查询的走表路径：WHERE status='pending' AND available_at <= NOW() ORDER BY available_at
-- 这个组合条件必须走索引，否则任务积压时每轮轮询都是全表扫。
CREATE INDEX IF NOT EXISTS idx_tasks_claim
    ON tasks(status, available_at);

-- 超时回收的扫描路径：WHERE status='in_flight' AND claimed_at < NOW() - interval。
-- 与 idx_tasks_claim 分开是因为它查的是另一个 status 值，
-- 复合索引的前导列是 status，两者互不覆盖。
CREATE INDEX IF NOT EXISTS idx_tasks_stale
    ON tasks(claimed_at) WHERE status = 'in_flight';

-- 2) 证据查询索引（T4.6：按事件聚合 + 按类型过滤）
--
-- 0001 已有 idx_evidence_incident_id(incident_id) 与 idx_evidence_type(type)，
-- 但「某事件的某一类证据」这个组合查询用不上其中任何一个：
-- 单列索引只能二选一。证据 Tab 会同时带 incident_id 与 type 过滤，
-- 因此建复合索引。
CREATE INDEX IF NOT EXISTS idx_evidence_incident_type
    ON evidence(incident_id, type);