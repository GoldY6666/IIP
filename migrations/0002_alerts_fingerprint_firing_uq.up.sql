-- ============================================================================
-- IIP M1 · 0002_alerts_fingerprint_firing_uq.up.sql
-- 规格来源：docs/IIP-M1详细开发手册.md §五「迁移变更」
--            + docs/IIP-V1.0-技术设计文档.md §4.2 Deduplicate
--
-- 目的：为 M1 的去重提供**数据库层兜底**。
--   V1.0 §4.2 规定「相同 fingerprint + firing 状态 → 跳过」。应用层的
--   「先查后写」防不住并发投递，故必须由唯一索引在数据库层保证。
--
-- 前置：0001_init.up.sql 已冻结（AGENTS.md 约束 10），故本变更只能新增迁移文件。
--
-- 变更清单（AGENTS.md 约束 4：表/列/索引/约束）：
--   * 表     ：无新增
--   * 列     ：无新增、无修改
--   * 索引   ：新增 1 个 —— uq_alerts_fingerprint_firing
--   * 约束   ：无新增 CHECK（复用 0001 的 chk_alerts_status / chk_alerts_severity）
--   * 数据   ：无数据迁移
-- ============================================================================

-- 同一 fingerprint 最多只能有一条处于 firing 的行。
--
-- 关键：这是**部分索引**（WHERE status = 'firing'），不是整列唯一。
--   若做成整列唯一，「起火 → 恢复 → 再起火」的历史行就会被卡死；
--   而 0001 的 alerts 表按设计允许同一 fingerprint 有多条历史行
--   （见 internal/db/queries/alerts.sql 中 GetAlertByFingerprint 取「最新一条」的用法）。
--
-- 与 0001 的 idx_alerts_fingerprint（非唯一）并存：
--   后者服务普通查询与排序，本索引服务唯一性，职责不重叠。
--
-- 写入侧配套写法（见 internal/db/queries/alerts.sql 的 InsertAlert）：
--   ON CONFLICT (fingerprint) WHERE status = 'firing' DO NOTHING
--   注意：部分唯一索引作为冲突目标时，「必须重复索引谓词」，否则会报
--   「there is no unique or exclusion constraint matching the ON CONFLICT specification」。
CREATE UNIQUE INDEX uq_alerts_fingerprint_firing
    ON alerts (fingerprint)
    WHERE status = 'firing';
