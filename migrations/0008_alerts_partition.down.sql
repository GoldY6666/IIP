-- ============================================================================
-- IIP T7.1 · 0008_alerts_partition.down.sql
--
-- 回滚 0008：分区表 → 0001 形态单表，恢复 ADR-002 的「V1.0 单表 + 索引」。
--
-- 破坏性：**是**
--   * 分区布局丢失（全部月度分区合并回一张表）；
--   * incident_alerts.alert_starts_at 列丢失（复合 FK 退化为单列 FK）；
--   * 三个维护函数被删除（保留期清理 / 分区补建 / 重复扫描能力消失）；
--   * 数据不丢：全量搬回单表，id 保持不变。
--
-- 回滚后如需保留期清理，只能回到「应用层定期 DELETE」——
-- 那正是 ADR-002 负面后果里写的「M7 需做在线迁移」的反向重演，
-- 故本 down 只用于紧急回退，不应作为常规运维手段。
--
-- 顺序说明（与 up 严格镜像）：
--   摘复合 FK → 建单表 → 搬回 → 删分区表 → 改名 → 重建索引 → 还原单列 FK
--   → 删分区键列 → 删函数。
-- ============================================================================

-- 1) 摘复合 FK（引用 alerts 的分区父表，必须先摘才能删表）。
ALTER TABLE incident_alerts DROP CONSTRAINT IF EXISTS incident_alerts_alert_fk;

-- 2) 建回 0001 形态的单表（列与 CHECK 逐字一致）。
CREATE TABLE alerts_v1_plain (
    id              BIGSERIAL PRIMARY KEY,
    fingerprint     VARCHAR(128) NOT NULL,
    source          VARCHAR(64) NOT NULL,
    severity        VARCHAR(16) NOT NULL,
    status          VARCHAR(16) NOT NULL DEFAULT 'firing',
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

-- 3) 数据搬回：分区父表上的 SELECT 读全部分区；id 显式带入保持引用稳定。
INSERT INTO alerts_v1_plain (id, fingerprint, source, severity, status, labels, annotations,
                             starts_at, ends_at, raw_payload, incident_id, created_at, updated_at)
SELECT id, fingerprint, source, severity, status, labels, annotations,
       starts_at, ends_at, raw_payload, incident_id, created_at, updated_at
FROM alerts;

-- 4) 旧序列归位（从 max(id)+1 继续）。
SELECT setval(pg_get_serial_sequence('alerts_v1_plain', 'id'),
              COALESCE((SELECT MAX(id) FROM alerts_v1_plain), 0) + 1, false);

-- 5) 删分区父表：连带全部分区、它们的索引与新序列。
DROP TABLE alerts;

-- 6) 改名回 alerts（序列 alerts_v1_plain_id_seq → alerts_id_seq 随表改名）。
ALTER TABLE alerts_v1_plain RENAME TO alerts;

-- 7) 索引重建为 0001/0002 原样：5 个普通索引 + 单列部分唯一索引。
CREATE INDEX idx_alerts_fingerprint ON alerts(fingerprint);
CREATE INDEX idx_alerts_incident_id ON alerts(incident_id);
CREATE INDEX idx_alerts_status      ON alerts(status);
CREATE INDEX idx_alerts_starts_at   ON alerts(starts_at);
CREATE INDEX idx_alerts_labels      ON alerts USING GIN(labels);
CREATE UNIQUE INDEX uq_alerts_fingerprint_firing ON alerts(fingerprint) WHERE status = 'firing';

-- 8) 还原单列 FK（0001 原名，ON DELETE CASCADE 语义不变）。
ALTER TABLE incident_alerts
    ADD CONSTRAINT incident_alerts_alert_id_fkey
    FOREIGN KEY (alert_id) REFERENCES alerts(id) ON DELETE CASCADE;

-- 9) 删分区键列（FK 已还原为单列，不再需要它）。
ALTER TABLE incident_alerts DROP COLUMN alert_starts_at;

-- 10) 删维护函数（参数类型签名必须与 up 完全一致）。
DROP FUNCTION IF EXISTS iip_ensure_alert_partitions(timestamptz, int);
DROP FUNCTION IF EXISTS iip_drop_expired_alert_partitions(int);
