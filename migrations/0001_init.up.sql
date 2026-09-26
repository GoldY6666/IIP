-- ============================================================================
-- IIP M0 · 0001_init.up.sql
-- 规格来源：docs/IIP-V1.0-技术设计文档.md §3.2 + docs/IIP-M0详细开发计划.md T0.2
-- 工程决策：见 docs/decisions.md ADR-001 ~ ADR-004
--
-- 本文件一旦合并即冻结（AGENTS.md 约束 10）。
-- 后续 schema 变更只能新增 0003_*.sql / 0004_*.sql，禁止改本文件。
--
-- 说明：
--   * 状态/类型列一律 VARCHAR + 命名 CHECK 约束，不用 PG ENUM（ADR-001）
--   * 时间列一律 TIMESTAMPTZ；每张表均含 created_at（ADR-003）
--   * 仅从属表使用 ON DELETE CASCADE（incident_alerts / evidence / timeline / correlation_feedback）
--   * alerts 表 V1.0 不做分区，原因见该表 fingerprint 列注释（ADR-002）
-- ============================================================================

-- ============================================
-- Alerts Table
-- ============================================
CREATE TABLE alerts (
    id              BIGSERIAL PRIMARY KEY,
    fingerprint     VARCHAR(128) NOT NULL,          -- 由稳定 label 集合 SHA256 生成；V1.0 暂不分区：分区表主键必须含分区键，会在此刻锁死 fingerprint 全局唯一策略，M7 视数据量再评估（ADR-002）
    source          VARCHAR(64) NOT NULL,           -- alertmanager, prometheus, k8s, etc.
    severity        VARCHAR(16) NOT NULL,           -- critical, warning, info
    status          VARCHAR(16) NOT NULL DEFAULT 'firing',  -- firing, resolved, suppressed
    labels          JSONB NOT NULL DEFAULT '{}',
    annotations     JSONB NOT NULL DEFAULT '{}',
    starts_at       TIMESTAMPTZ NOT NULL,
    ends_at         TIMESTAMPTZ,
    raw_payload     JSONB NOT NULL,
    incident_id     BIGINT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT chk_alerts_status   CHECK (status   IN ('firing', 'resolved', 'suppressed')),
    CONSTRAINT chk_alerts_severity CHECK (severity IN ('critical', 'warning', 'info'))
);

CREATE INDEX idx_alerts_fingerprint ON alerts(fingerprint);
CREATE INDEX idx_alerts_incident_id ON alerts(incident_id);
CREATE INDEX idx_alerts_status      ON alerts(status);
CREATE INDEX idx_alerts_starts_at   ON alerts(starts_at);
CREATE INDEX idx_alerts_labels      ON alerts USING GIN(labels);

-- ============================================
-- Incidents Table
-- ============================================
CREATE TABLE incidents (
    id              BIGSERIAL PRIMARY KEY,
    title           VARCHAR(512) NOT NULL,
    severity        VARCHAR(16) NOT NULL,           -- critical, warning, info
    status          VARCHAR(16) NOT NULL DEFAULT 'open',  -- open, investigating, mitigated, resolved, ignored
    service         VARCHAR(256),
    started_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    resolved_at     TIMESTAMPTZ,
    root_causes     JSONB DEFAULT '[]',             -- [{ "hypothesis": "...", "confidence": 0.87 }]
    recommendations JSONB DEFAULT '[]',             -- [{ "action": "...", "priority": "high" }]
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT chk_incidents_status   CHECK (status   IN ('open', 'investigating', 'mitigated', 'resolved', 'ignored')),
    CONSTRAINT chk_incidents_severity CHECK (severity IN ('critical', 'warning', 'info'))
);

CREATE INDEX idx_incidents_status     ON incidents(status);
CREATE INDEX idx_incidents_severity   ON incidents(severity);
CREATE INDEX idx_incidents_service    ON incidents(service);
CREATE INDEX idx_incidents_started_at ON incidents(started_at);

-- ============================================
-- Alert-Incident Junction (M:N)
-- ============================================
CREATE TABLE incident_alerts (
    incident_id       BIGINT NOT NULL REFERENCES incidents(id) ON DELETE CASCADE,
    alert_id          BIGINT NOT NULL REFERENCES alerts(id)    ON DELETE CASCADE,
    correlated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    correlation_score DECIMAL(5,4),                 -- 关联评分 0.0000-1.0000
    PRIMARY KEY (incident_id, alert_id),
    CONSTRAINT chk_incident_alerts_score CHECK (correlation_score IS NULL OR (correlation_score >= 0 AND correlation_score <= 1))
);

CREATE INDEX idx_incident_alerts_alert_id ON incident_alerts(alert_id);

-- ============================================
-- Evidence Table
-- ============================================
CREATE TABLE evidence (
    id              BIGSERIAL PRIMARY KEY,
    incident_id     BIGINT NOT NULL REFERENCES incidents(id) ON DELETE CASCADE,
    type            VARCHAR(32) NOT NULL,           -- metric, log, trace, deployment, config_change, historical
    source          VARCHAR(64) NOT NULL,           -- prometheus, loki, k8s, git, etc.
    query           TEXT,                           -- 采集该证据所用的查询
    value           TEXT,                           -- 采集到的值 / 摘要
    timestamp       TIMESTAMPTZ NOT NULL,           -- 证据时间来自采集源，不用 NOW() 兜底（ADR-003）
    confidence      DECIMAL(5,4),                   -- 对假设的支撑强度 0.0000-1.0000
    metadata        JSONB DEFAULT '{}',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT chk_evidence_type       CHECK (type IN ('metric', 'log', 'trace', 'deployment', 'config_change', 'historical')),
    CONSTRAINT chk_evidence_confidence CHECK (confidence IS NULL OR (confidence >= 0 AND confidence <= 1))
);

CREATE INDEX idx_evidence_incident_id ON evidence(incident_id);
CREATE INDEX idx_evidence_type        ON evidence(type);
CREATE INDEX idx_evidence_timestamp   ON evidence(timestamp);

-- ============================================
-- Timeline Table
-- ============================================
CREATE TABLE timeline (
    id              BIGSERIAL PRIMARY KEY,
    incident_id     BIGINT NOT NULL REFERENCES incidents(id) ON DELETE CASCADE,
    event_type      VARCHAR(32) NOT NULL,           -- alert_fired, alert_resolved, status_change, evidence_found, user_comment
    description     TEXT NOT NULL,
    timestamp       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    actor           VARCHAR(128),                   -- system, user_id, webhook_source
    metadata        JSONB DEFAULT '{}',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT chk_timeline_event_type CHECK (event_type IN ('alert_fired', 'alert_resolved', 'status_change', 'evidence_found', 'user_comment'))
);

CREATE INDEX idx_timeline_incident_id ON timeline(incident_id);
CREATE INDEX idx_timeline_timestamp   ON timeline(timestamp);

-- ============================================
-- Correlation Rules Table (configurable)
-- ============================================
CREATE TABLE correlation_rules (
    id              BIGSERIAL PRIMARY KEY,
    name            VARCHAR(256) NOT NULL,
    description     TEXT,
    enabled         BOOLEAN NOT NULL DEFAULT TRUE,
    rule_config     JSONB NOT NULL,                 -- 规则私有配置
    weight          DECIMAL(5,4) NOT NULL DEFAULT 1.0,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT chk_correlation_rules_weight CHECK (weight >= 0)
);

CREATE INDEX idx_correlation_rules_enabled ON correlation_rules(enabled);
CREATE UNIQUE INDEX uq_correlation_rules_name ON correlation_rules(name);

-- ============================================
-- Correlation Feedback Table (M2 反馈学习预留) —— ADR-004
-- 用途：记录人工对关联决策的修正，供 M2 反馈学习消费。
-- 消费方：M2（本表在 M0 阶段无写入方，属预期）。
-- ============================================
CREATE TABLE correlation_feedback (
    id             BIGSERIAL PRIMARY KEY,
    incident_id    BIGINT NOT NULL REFERENCES incidents(id) ON DELETE CASCADE,
    alert_id_pair  TEXT NOT NULL,                   -- 被人工判定的一对告警，形如 "<alert_id_a>,<alert_id_b>"
    decision       VARCHAR(16) NOT NULL,            -- merge, split, ignore
    actor          VARCHAR(128),                    -- 做出该判定的人/系统
    metadata       JSONB DEFAULT '{}',              -- 可选扩展（如判定理由）
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT chk_correlation_feedback_decision CHECK (decision IN ('merge', 'split', 'ignore'))
);

CREATE INDEX idx_correlation_feedback_incident_id ON correlation_feedback(incident_id);
CREATE INDEX idx_correlation_feedback_created_at  ON correlation_feedback(created_at);
