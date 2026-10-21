-- ============================================================================
-- IIP · 0010_ensure_partition_concurrent.up.sql
-- 背景：CI 首跑（2026-10-19，run#4）暴露的**真产品缺陷**——
--   iip_ensure_alert_partitions()（0008）的「查 pg_class → CREATE TABLE」
--   跨会话不原子：两个事务同时补建同一个月分区时，落败者的存在性检查
--   看不到对方**未提交**的建表（快照隔离），它的 CREATE 会阻塞到对方
--   提交，然后撞 42P07 duplicate_table，被调用方当成「补建失败」——
--   插入路径的 23514 重试随之失败，整个 Insert 报错。
--   CI 的 -race 拖慢时序放大了竞争窗口；本机 Windows 无 cgo 跑不了
--   -race，故本地全量测试复现不出（这正是它能活到 CI 才暴露的原因）。
--
-- 修法：把 CREATE 包进子事务，捕获 duplicate_table 后静默跳过——
--   「分区已存在」= 目标达成，不是错误。幂等语义不变，只是消掉
--   并发下的假失败。
--
-- 为什么不是「删掉手动存在性检查、只留 CREATE TABLE IF NOT EXISTS」：
--   IF NOT EXISTS 同样看不见未提交的建表（同一个快照隔离问题），
--   竞争窗口原样存在；而且丢了检查就丢了「本次是否真新建」的记账
--   （created 返回值）。
--
-- 为什么不是 Go 侧重试：错误发生在单个 SQL 调用内，重试要等对方提交，
--   退避时长不可控；且库侧消解竞争是唯一能让「N 个并发插入全部成功」
--   的办法——Go 侧重试第 N 次仍然要与第 1 个抢同一把目录锁。
--
-- 残余窗口（如实记录）：存在性检查通过后、CREATE 执行前，若对方恰好
--   提交，CREATE IF NOT EXISTS 会以 NOTICE 跳过而本事务仍把该分区记入
--   created —— 日志会把它说成本事务新建（实际是对方）。窗口为微秒级，
--   且 created 只被日志消费（repository 的 23514 重试路径直接丢弃返回
--   值，见 internal/repository/alerts.go ensurePartitionFor），不影响
--   任何正确性判断。
--
-- 破坏性：**否**（只重定义函数体，签名与返回类型不变；不碰数据与表结构）。
-- down 完整还原 0008 原版函数体与注释。
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
            -- 子事务：并发补建竞争中落败时，CREATE 阻塞到对方提交后必撞
            -- 42P07（对方未提交的建表本会话不可见，存在性检查漏掉了它）。
            -- 分区存在 = 目标达成 ⇒ 吞掉错误继续处理后续月份；
            -- 不记入 created（不是本事务建的）。
            BEGIN
                EXECUTE format(
                    'CREATE TABLE IF NOT EXISTS %I PARTITION OF alerts FOR VALUES FROM (%L) TO (%L)',
                    pname,
                    (m::timestamp) AT TIME ZONE 'UTC',
                    ((m + interval '1 month')::timestamp) AT TIME ZONE 'UTC'
                );
                created := array_append(created, pname);
            EXCEPTION
                WHEN duplicate_table THEN
                    NULL;
            END;
        END IF;
        m := (m + interval '1 month')::date;
    END LOOP;
    RETURN QUERY SELECT unnest(created);
END $$;

COMMENT ON FUNCTION iip_ensure_alert_partitions(timestamptz, int) IS
    '补建 alerts 月度分区：覆盖 [min(from_ts 月, 当月), max(from_ts 月, 当月+ahead)]。'
    '返回本次新建的分区名。from_ts=NULL 只保未来；插入路径传告警 starts_at。'
    '并发补建同月分区时静默跳过（duplicate_table，0010）：对方未提交的建表'
    '本会话不可见，CREATE 阻塞到对方提交后必撞 42P07，而分区存在即目标达成。';
