-- ============================================================================
-- IIP T7.1 · 0008_alerts_partition.up.sql
-- 规格来源：docs/IIP-V1.0-技术设计文档.md §3.2（alerts 分区策略）
--           + docs/decisions.md ADR-002（V1.0 暂不分区）→ ADR-022（T7.1 落地取代）
--
-- 背景（ADR-002 的延期条件已满足）
-- --------------------------------
-- ADR-002（2026-09-25）裁定 V1.0 不分区，理由是「分区表主键必须含分区键，
-- 会在信息不足时锁死 fingerprint 全局唯一策略」。T7.1 重新评估时该策略已有
-- M1-M6 的实测数据支撑，且分区本身已成为运维刚需（保留期清理、月度膨胀），
-- 故本次落地，ADR-002 由 ADR-022 取代。
--
-- 唯一性策略的收敛（本迁移最核心的语义变化）
-- ------------------------------------------
-- 0002 的部分唯一索引是 uq_alerts_fingerprint_firing(fingerprint) WHERE firing，
-- 语义 = 「同一指纹同时最多一条 firing」，**与 starts_at 无关**。
-- 分区表要求唯一约束含分区键，因此索引改为 (fingerprint, starts_at)，
-- 语义收敛为「同一指纹 + 同一 starts_at 最多一条 firing」。
--
-- 为什么这个收敛是可接受的（而不是缺陷）：
--   * 应用层去重（LatestByFingerprint → 已 firing 则跳过）在正常路径上
--     恒在数据库兜底之前生效，DB 层只拦「并发投递同一条告警」——
--     而并发投递的 startsAt 由 Alertmanager 保证相同（重发不重置 startsAt）。
--   - 残余缺口 = 「同指纹、不同 starts_at 的两条 firing 并存」，
--     只能由「起火→未收到 resolved→以新 startsAt 重火」造成。
--     该缺口由 ScanFiringDuplicates 查询（internal/db/queries/partition.sql）
--     在维护 tick 里定期曝光，属已知限制而非静默错误。
--
-- 为什么不能 DROP / DETACH / TRUNCATE 旧分区（保留期清理的真实约束）
-- ------------------------------------------------------------------
-- 2026-10-17 实测（一次性库，结论全文见 docs/decisions.md ADR-022）：
--   * DROP / DETACH / TRUNCATE 一个被外键引用的分区，**全部失败**
--     —— 即使引用行已删完（ref_left=0）也失败：FK 是**对象级依赖**，
--     与行数无关；DETACH 同理被拦。
--   * DELETE 父表行会**行级级联**删掉引用行（实测 ref_left=0）。
-- 因此保留期清理用「整表 DELETE starts_at < cutoff」+ 行级 ON DELETE CASCADE，
-- 由 iip_drop_expired_alert_partitions(retention_days) 封装。
-- 代价：分区只增不减（DROP 被 FK 挡死），空分区保留——
-- 这同时给了「历史月份回填/重放」留位，是有意取舍。
--
-- incident_alerts 必须带分区键（复合外键）
-- ----------------------------------------
-- 保留期清理走行级 DELETE 父表行 ⇒ 若 incident_alerts 仍是单列 FK
-- (alert_id) → alerts(id)，则每个旧分区都被 FK 挡死，连 DELETE 都无法进行。
-- 故本迁移给 incident_alerts 增加 alert_starts_at 列（从 alerts 行回填），
-- 外键改为 (alert_id, alert_starts_at) → alerts(id, starts_at) 复合 FK，
-- 行级 CASCADE 语义不变（DELETE 父行仍级联删边）。
--
-- down 的破坏性：**是** —— 分区布局（全部月度分区）与 alert_starts_at 列
-- 丢失，数据不丢（全量搬回单表）。回滚后保留期清理能力随之消失，
-- 需重新评估 ADR-002 的单表演进路径。
-- ============================================================================

-- ----------------------------------------------------------------------------
-- 1) 旧表改名腾名。索引名是 schema 级全局的，旧表的 5 个索引 + 部分唯一索引
--    必须先随旧表一起删掉（步骤 7），新索引才能在旧表删除后重建。
--    序列随表改名（alerts_id_seq → alerts_v1_plain_id_seq）。
-- ----------------------------------------------------------------------------
ALTER TABLE alerts RENAME TO alerts_v1_plain;

-- ----------------------------------------------------------------------------
-- 2) 建新分区表：列 / CHECK 约束与 0001 逐字一致，主键加分区键 starts_at。
--    BIGSERIAL 保持（sqlc 生成的 Go 类型不变），新序列 alerts_id_seq 归位见步骤 5。
-- ----------------------------------------------------------------------------
CREATE TABLE alerts (
    id              BIGSERIAL,
    fingerprint     VARCHAR(128) NOT NULL,          -- ADR-022：分区表唯一索引改为 (fingerprint, starts_at)，见 uq_alerts_fingerprint_firing
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
    CONSTRAINT chk_alerts_severity CHECK (severity IN ('critical', 'warning', 'info')),
    CONSTRAINT pk_alerts PRIMARY KEY (id, starts_at)   -- 分区表硬性要求：PK 必须含分区键
) PARTITION BY RANGE (starts_at);

COMMENT ON TABLE alerts IS
    'alerts 表（T7.1 起按 starts_at 月度 RANGE 分区，ADR-022）。'
    '分区键 = starts_at；PK/唯一索引均含分区键；保留期清理走行级 DELETE（FK 对象级依赖使 DROP/DETACH 分区不可行）。';

COMMENT ON COLUMN alerts.fingerprint IS
    '由稳定 label 集合 SHA256 生成。T7.1 分区后 firing 唯一性收敛为'
    ' (fingerprint, starts_at)（分区表唯一约束必须含分区键，ADR-022）；'
    '跨 starts_at 的重火并存由 iip_scan_firing_duplicates() 定期曝光。';

-- ----------------------------------------------------------------------------
-- 3) 建分区：覆盖「存量数据月份 → max(当月+2, 存量最晚月份)」。
--    月份边界一律按 UTC 计算，与 iip_ensure_alert_partitions() 的口径一致
--    —— 否则不同会话时区下两次计算的月界不同，后建的会与前建的边界错位
--    甚至重叠报错。
--    hi 取 GREATEST 是给「存量数据里有时钟漂移到未来的行」留保险：
--    漏一个月，步骤 4 的搬迁就会以 23514 失败。
--
--    **为什么边界必须写 (m::timestamp) AT TIME ZONE 'UTC' 而不是 m AT TIME ZONE 'UTC'**
--    （2026-10-17 实证；永久守卫 = migrations/partition_test.go 的
--    TestPartitionBoundsAreUtcMonthRegardlessOfSessionTz，显式 SET 负偏移
--    会话时区后按 tableoid 钉行的月份归属）：
--      * `date AT TIME ZONE 'UTC'` 的 date 操作数先被隐式转成 **timestamptz**
--        （= 会话时区午夜），再按 UTC 输出墙钟 ⇒ 得 '2026-10-31 16:00:00'；
--        format('%L') 渲染它**不带偏移**，回解析时又按会话时区读 ⇒
--        实际边界 '2026-10-31 08:00+00'，落进上一分区范围 → 重叠 FAIL。
--      * 先显式 ::timestamp（纯午夜、无时区语义）再 AT TIME ZONE 'UTC'，
--        得到 timestamptz，时刻恰为 UTC 月界；%L 渲染**带偏移**
--        （'+08'），回解析时刻不变 ⇒ 与会话时区无关。
-- ----------------------------------------------------------------------------
DO $$
DECLARE
    m  date;
    lo date;
    hi date;
BEGIN
    SELECT date_trunc('month', COALESCE(MIN(starts_at), NOW()) AT TIME ZONE 'UTC')::date
      INTO lo FROM alerts_v1_plain;
    SELECT GREATEST(
               (date_trunc('month', NOW() AT TIME ZONE 'UTC') + interval '2 months')::date,
               date_trunc('month', COALESCE(MAX(starts_at), NOW()) AT TIME ZONE 'UTC')::date
           )
      INTO hi FROM alerts_v1_plain;
    m := lo;
    WHILE m <= hi LOOP
        EXECUTE format(
            'CREATE TABLE IF NOT EXISTS %I PARTITION OF alerts FOR VALUES FROM (%L) TO (%L)',
            'alerts_' || to_char(m, 'YYYY_MM'),
            (m::timestamp) AT TIME ZONE 'UTC',
            ((m + interval '1 month')::timestamp) AT TIME ZONE 'UTC'
        );
        m := (m + interval '1 month')::date;
    END LOOP;
END $$;

-- ----------------------------------------------------------------------------
-- 4) 数据搬迁：显式带 id（引用稳定），分区已覆盖存量全部月份。
-- ----------------------------------------------------------------------------
INSERT INTO alerts (id, fingerprint, source, severity, status, labels, annotations,
                    starts_at, ends_at, raw_payload, incident_id, created_at, updated_at)
SELECT id, fingerprint, source, severity, status, labels, annotations,
       starts_at, ends_at, raw_payload, incident_id, created_at, updated_at
FROM alerts_v1_plain;

-- 5) 新序列归位：从 max(id)+1 继续（is_called=false，下一个即 max+1）。
SELECT setval(pg_get_serial_sequence('alerts', 'id'),
              COALESCE((SELECT MAX(id) FROM alerts), 0) + 1, false);

-- ----------------------------------------------------------------------------
-- 6) incident_alerts 增加分区键列并从旧表回填。
--    旧 FK (alert_id) → alerts(id) 保证每条边都能匹配到行；
--    回填后 NOT NULL —— 复合 FK 要求引用侧两列均非空。
--    若回填后有 NULL 残留（= 库里有 FK 罩不住的脏数据），SET NOT NULL 会
--    直接让本迁移失败，这是有意行为：脏数据必须在迁移期暴露，而不是
--    留到运行期变成「偶发外键违反」。
-- ----------------------------------------------------------------------------
ALTER TABLE incident_alerts ADD COLUMN alert_starts_at TIMESTAMPTZ;

UPDATE incident_alerts ia
SET alert_starts_at = a.starts_at
FROM alerts_v1_plain a
WHERE a.id = ia.alert_id;

ALTER TABLE incident_alerts ALTER COLUMN alert_starts_at SET NOT NULL;

-- ----------------------------------------------------------------------------
-- 7) 摘旧 FK、删旧表（连带旧索引与旧序列，索引名随之释放）。
-- ----------------------------------------------------------------------------
ALTER TABLE incident_alerts DROP CONSTRAINT incident_alerts_alert_id_fkey;

DROP TABLE alerts_v1_plain;

-- ----------------------------------------------------------------------------
-- 8) 索引重建。uq_alerts_fingerprint_firing 从 (fingerprint) 扩为
--    (fingerprint, starts_at) —— 分区表唯一约束必须含分区键。
--    ON CONFLICT 的谓词必须重复写一遍（部分唯一索引作为冲突目标的硬性要求），
--    消费方 internal/db/queries/alerts.sql 的 InsertAlert 已同步。
-- ----------------------------------------------------------------------------
CREATE INDEX idx_alerts_fingerprint ON alerts(fingerprint);
CREATE INDEX idx_alerts_incident_id ON alerts(incident_id);
CREATE INDEX idx_alerts_status      ON alerts(status);
CREATE INDEX idx_alerts_starts_at   ON alerts(starts_at);
CREATE INDEX idx_alerts_labels      ON alerts USING GIN(labels);
CREATE UNIQUE INDEX uq_alerts_fingerprint_firing ON alerts(fingerprint, starts_at) WHERE status = 'firing';

-- 9) 复合 FK：引用侧带分区键，行级 ON DELETE CASCADE 语义不变。
ALTER TABLE incident_alerts
    ADD CONSTRAINT incident_alerts_alert_fk
    FOREIGN KEY (alert_id, alert_starts_at) REFERENCES alerts(id, starts_at) ON DELETE CASCADE;

-- ----------------------------------------------------------------------------
-- 10) 维护函数（worker 维护 tick 与插入路径 23514 重试共用）。
--
--  iip_ensure_alert_partitions(from_ts, ahead_months)
--    补建 [min(from_ts 月, 当月), max(from_ts 月, 当月+ahead)] 的分区，
--    返回本次新建的分区名。from_ts 传 NULL = 只保未来（维护 tick 形态）；
--    插入路径传告警自身 starts_at（23514 重试形态，保证目标月必被覆盖）。
--    幂等：已存在的分区跳过。月份边界按 UTC，与步骤 3 一致。
--
--  iip_drop_expired_alert_partitions(retention_days)
--    删除 starts_at 早于 retention_days 的告警行，返回删除行数。
--    用整表 DELETE 而非 DROP 分区：FK 对象级依赖（见头部注释实测）。
--    行级 CASCADE 会连带删 incident_alerts 链接。
--    代价：不回收磁盘（VACUUM 的职责），分区数只增不减（空分区保留，
--    为历史月回填/重放留位，有意取舍）。
--
--  iip_scan_firing_duplicates()
--    列出同一 fingerprint 存在多条 firing 的组 —— 分区键加入唯一索引后的
--    残余缺口（同指纹不同 starts_at 的重火并存）的曝光口。
--    全表扫 firing 行：维护 tick 月级频次下可接受；量级上来后应改为
--    只扫最近 N 天或改由应用层 LatestByFingerprint 顺带记录。
-- ----------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION iip_ensure_alert_partitions(from_ts timestamptz, ahead_months int)
RETURNS SETOF text
LANGUAGE plpgsql AS $$
DECLARE
    base    date;
    lo      date;
    hi      date;
    m       date;
    created text[];
    pname   text;
BEGIN
    IF ahead_months IS NULL OR ahead_months < 0 THEN
        ahead_months := 2;
    END IF;
    base := COALESCE(date_trunc('month', from_ts AT TIME ZONE 'UTC')::date,
                     date_trunc('month', NOW() AT TIME ZONE 'UTC')::date);
    lo := LEAST(base, date_trunc('month', NOW() AT TIME ZONE 'UTC')::date);
    hi := GREATEST(base, (date_trunc('month', NOW() AT TIME ZONE 'UTC')
                          + make_interval(months => ahead_months))::date);
    m := lo;
    WHILE m <= hi LOOP
        pname := 'alerts_' || to_char(m, 'YYYY_MM');
        IF NOT EXISTS (
            SELECT 1
            FROM pg_class c
            JOIN pg_inherits i ON i.inhrelid = c.oid
            WHERE i.inhparent = 'alerts'::regclass
              AND c.relname = pname
        ) THEN
            EXECUTE format(
                'CREATE TABLE IF NOT EXISTS %I PARTITION OF alerts FOR VALUES FROM (%L) TO (%L)',
                pname,
                (m::timestamp) AT TIME ZONE 'UTC',
                ((m + interval '1 month')::timestamp) AT TIME ZONE 'UTC'
            );
            created := array_append(created, pname);
        END IF;
        m := (m + interval '1 month')::date;
    END LOOP;
    RETURN QUERY SELECT unnest(created);
END $$;

COMMENT ON FUNCTION iip_ensure_alert_partitions(timestamptz, int) IS
    '补建 alerts 月度分区：覆盖 [min(from_ts 月, 当月), max(from_ts 月, 当月+ahead)]。'
    '返回本次新建的分区名。from_ts=NULL 只保未来；插入路径传告警 starts_at。';

CREATE OR REPLACE FUNCTION iip_drop_expired_alert_partitions(retention_days int)
RETURNS bigint
LANGUAGE plpgsql AS $$
DECLARE
    deleted bigint;
BEGIN
    IF retention_days IS NULL OR retention_days <= 0 THEN
        RAISE EXCEPTION 'iip_drop_expired_alert_partitions: retention_days 必须为正数，实际 %', retention_days;
    END IF;
    DELETE FROM alerts WHERE starts_at < NOW() - make_interval(days => retention_days);
    GET DIAGNOSTICS deleted = ROW_COUNT;
    RETURN deleted;
END $$;

COMMENT ON FUNCTION iip_drop_expired_alert_partitions(int) IS
    '删除 starts_at 早于 retention_days 的告警行（行级 DELETE + CASCADE 级联清理 incident_alerts）。'
    '不用 DROP 分区：被 FK 引用的分区 DROP/DETACH/TRUNCATE 全部失败（对象级依赖，2026-10-17 实测）。';
