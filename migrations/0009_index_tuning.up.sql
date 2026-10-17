-- ============================================================================
-- IIP T7.2 · 0009_index_tuning.up.sql
-- 规格来源：docs/IIP开发计划.md §11 T7.2「JSONB 索引调优」+ docs/decisions.md ADR-023
--
-- 本迁移只做两件事，全部有 EXPLAIN 实测证据（2026-10-17，5 万 alerts /
-- 30 incidents / 2157 关联边 / 5 千 tasks 的代表性数据；取证方法、全部数字
-- 与原始输出索引见 docs/decisions.md ADR-023）：
--
-- 1) 建 idx_tasks_payload_incident：tasks((payload->>'incident_id'))
--    WHERE kind = 'collect_evidence'
--    证据（改前）：
--      * LastCollectWindowEnd（每轮采集一次）：Bitmap Index Scan on
--        idx_tasks_claim(status='done') 读 2144 行，再 Filter kind/payload/
--        window_end，1.25ms @ 5 千行；
--      * EnqueueTaskIfAbsent（每次入队一次）：NOT EXISTS 子查询 Seq Scan
--        全表；**插入路径（NOT EXISTS 为真）永远全表扫**——EXISTS 语义只让
--        「已有 pending」的跳过路径在首个匹配行处短路（实测最好情况 3 行 /
--        0.024ms，最坏 5000 行 / 1.4ms，两种都抓到过）。
--    为什么必须建：tasks 表**没有保留期**（done 行按设计永久保留供审计），
--    上面两条查询的成本随表量线性增长；索引把「按事件找任务」变成定长探测。
--    为什么谓词是 kind='collect_evidence'：两条消费查询都带这个条件
--    （LastCollectWindowEnd 硬编码、EnqueueTaskIfAbsent 显式传入），
--    部分索引顺带把 enqueue_evidence 等未来任务类型挡在索引外。
--    为什么不把 window_end 放进键序：Q5 要 (incident_id, window_end DESC)
--    定序，Q6 要可见 pending/in_flight —— 同一个部分索引 predicate 服务不了
--    两种 status 集；单索引 (payload->>'incident_id') WHERE kind 对两者都够用
--    （每事件任务数百行级，余下的过滤/排序可忽略）。
--
-- 2) 删 idx_alerts_labels（alerts(labels) 的 GIN 索引，0001 建，0008 重建为
--    分区索引）：**零读者**。
--    证据（双重）：
--      * 代码级：全部 44 条 sqlc 查询 + API 层，无一条对 labels 用 jsonb
--        谓词（@> / ? / ?| / ?& 全无命中）；唯一用法是 SELECT 投影与
--        应用层取值（ incidents.service 从 labels.service 取，在 Go 侧做）。
--        工具脚本层（verify-func/fault_drill/verify-images）确有
--        labels->>'x' 过滤，但 ->> 等式本来就用不了 GIN，不构成读者。
--      * 运行时：pg_stat_user_indexes 在 600+ 次语句执行（覆盖所有触达
--        alerts 的读路径：去重读/候选查询/重复扫描/事件列表）之后，
--        14 个分区副本 idx_scan **全为 0**。
--    代价（如实）：GIN 是所有索引类型里写入维护最贵的，且分区后每个分区
--    一份副本；删掉 = 每条告警插入少维护一份 GIN + 省存储。若将来要做
--    「按 label 搜索告警」，需要重新加回（新建迁移即可，DDL 见 down）。
--
-- 破坏性：**否**（纯索引增删，不动数据与表结构）。down 完整还原两者。
-- ============================================================================

-- 1) tasks 按事件 ID 的部分表达式索引（T7.2 裁定①：建）
CREATE INDEX IF NOT EXISTS idx_tasks_payload_incident
    ON tasks ((payload->>'incident_id'))
    WHERE kind = 'collect_evidence';

-- 2) alerts labels GIN：零读者，删（T7.2 裁定②：删）
--    父表上的分区索引，DROP 会连带删除全部分区副本。
DROP INDEX idx_alerts_labels;
