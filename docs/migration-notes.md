# 数据库迁移决策与实现说明

> 本文件对应 T0.2 的 4 条工程决策与实现细节。
> 决策记录见 `docs/decisions.md`（ADR-001 ~ ADR-004）。

## 迁移文件

| 文件 | 作用 |
|------|------|
| `migrations/0001_init.up.sql` | up：建立 6 张核心表 + 1 张预留表、全部索引与 CHECK 约束 |
| `migrations/0001_init.down.sql` | down：按依赖倒序删除全部表（可完整回滚） |
| `migrations/embed.go` | 将上述 `.sql` 嵌入二进制（供 server 启动时自动迁移） |
| `scripts/verify_inserts.sql` | 人工验收脚本（非迁移文件，故置于 `scripts/` 避免被 embed） |

> 命名说明：golang-migrate 要求同版本的 up/down 成对（`<version>_<name>.up.sql` /
> `<version>_<name>.down.sql`），因此 down 与 up 共用版本号 `0001`，而非另起 `0002`。
> 这样 `migrate ... up` / `down-all` 才能正确配对执行。

**冻结约定**：`0001_init.*.sql` 合并后即冻结，后续 schema 变更只能新增 `0003_*.sql` 等新迁移。

## 4 条工程决策摘要

| # | 决策 | 理由 |
|---|------|------|
| ① | `status` / `type` 用 `VARCHAR + CHECK` 而非 PG ENUM | PG ENUM 加值需 ALTER 且不可回滚；CHECK 更灵活可 diff |
| ② | `alerts` 表 V1.0 不分区 | 分区表主键必须含分区键，会提前锁定 fingerprint 唯一策略；M7 再评估 |
| ③ | 时间列一律 `TIMESTAMPTZ`，全表补 `created_at` | 告警源跨时区，无时区时间会产生难排查偏移 |
| ④ | M0 建 `correlation_feedback` 预留表 | M2 反馈学习需要，建表成本 5 分钟，避免 V1.x 补迁移 |

## 规格外的主动追加（已获技术负责人确认保留，2026-09-25）

以下 4 项**不在 V1.0 文档 §3.2 原始 DDL 中**，属防御性增强。技术负责人已逐条核实原文并裁决**全部保留**：

| # | 追加项 | 裁决理由 |
|---|--------|----------|
| a | `timeline.created_at` | ADR-003 要求「所有表含 created_at」；V1.0 原文其余 5 表均有、唯 timeline 缺失，属**原文自身不一致**，补上使其自洽 |
| b | `correlation_rules` 的 `UNIQUE(name)` 索引 | 防规则重名。技术负责人已确认保留 |
| c | `correlation_rules(enabled)` 索引 | 规则启用状态是高频过滤条件 |
| d | `incident_alerts(alert_id)` 索引 + 各表数值区间 CHECK | 反查用索引；`correlation_score`/`confidence`/`weight` 做 `>= 0`、`<= 1` 防御 |

> 处置记录：2026-09-25 技术负责人裁决「全部保留」。此裁决已同步至本文档与 `docs/decisions.md`。

## 验证方式

环境就绪后执行「up → down-all → up」三连，确认可重复执行；
再跑手工 INSERT 验证脚本，确认非法值全部被 CHECK 拒绝。
详细命令见交付说明中的验收命令清单。
