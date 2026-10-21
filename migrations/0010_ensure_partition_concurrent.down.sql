-- ============================================================================
-- IIP · 0010_ensure_partition_concurrent.down.sql
--
-- 回滚 0010：把 iip_ensure_alert_partitions() 还原成 0008 原版函数体
-- （无 duplicate_table 兜底），即恢复「并发补建同月分区时落败者报
-- 42P07」的行为。
--
-- 为什么 down 要完整重贴 0008 的函数体而不是「删掉函数」：
-- 0008 的 down（步骤 10）才是删函数的地方；0010 只重定义了 0008 建的
-- 函数体，它的回滚必须精确还原到 0008 那一刻的状态，否则「down 0010」
-- 之后库里既不是新行为也不是 0008 原行为，后续重新 up 0008 会撞
-- 「函数已存在」之外的第三种未知状态。
--
-- 注意（与 up 的头部注释同一条）：还原后并发补建竞争会重新变成
-- 「插入路径偶发 42P07」。故本 down 只用于紧急回退，不应作为常规
-- 运维手段——要退也应该连 0008 一起退（回到单表）。
-- ============================================================================

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
