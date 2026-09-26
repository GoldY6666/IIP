# IIP M0 设计决策记录（ADR 草稿）

> 状态说明：本文件由 AI 起草，每条决策标注「待人工确认」。
> 确认后由技术负责人将状态改为「已确认」，并可升格为正式 ADR。
> 规格依据：`docs/IIP-M0详细开发计划.md` T0.2 节。

---

## ADR-001：状态/类型列使用 VARCHAR + CHECK 约束，而非 PostgreSQL ENUM

- **Status**: 待人工确认（AI 起草）
- **Date**: 2026-09-25
- **来源**: IIP-M0详细开发计划 §T0.2 决策 ①
- **Background**:
  `alerts.status`、`incidents.status`、`evidence.type` 三个列取值为有限枚举集合。
  PostgreSQL 原生 ENUM 类型提供强类型保证，但**新增枚举值需要 `ALTER TYPE ... ADD VALUE`**，
  在事务中执行受限、且不可回滚，跨版本迁移成本高。
- **Options**:
  1. 使用 PG ENUM 类型（`CREATE TYPE alert_status AS ENUM (...)`）
  2. 使用 `VARCHAR(n)` + 命名 CHECK 约束
- **Decision**: 采用 **方案 2**（VARCHAR + CHECK）。
- **Consequences**:
  - 正面：新增取值只需 `ALTER TABLE ... DROP CONSTRAINT` + `ADD CONSTRAINT`，可回滚、可在事务中执行；值集合以 SQL 文本形式随迁移文件版本化，可 diff、可审查。
  - 负面：类型安全等级低于 ENUM（应用层仍需做常量校验）；列长度需人工维护（已按规格分别取 VARCHAR(16)/VARCHAR(32)）。
  - 约束命名统一采用 `chk_<表名>_<列名>` 形式，便于迁移脚本精确定位。

---

## ADR-002：`alerts` 表 V1.0 不做分区

- **Status**: 待人工确认（AI 起草）
- **Date**: 2026-09-25
- **来源**: IIP-M0详细开发计划 §T0.2 决策 ②
- **Background**:
  `alerts` 是写入量最大的表，长期看存在分区需求。
  但 PostgreSQL **分区表的唯一约束/主键必须包含分区键**，一旦现在分区，
  就等于此刻必须确定「fingerprint 全局唯一」还是「fingerprint 按分区唯一」的策略，
  而该策略需要在 M4-M7 关联引擎实际运行数据后才能定论。
- **Options**:
  1. V1.0 即建分区表（按时间 range 分区）
  2. V1.0 单表 + 索引，M7 再评估
- **Decision**: 采用 **方案 2**（暂不分区）。
- **Consequences**:
  - 正面：不在信息不足时锁定关键 schema 契约；单表 + 5 个索引足以支撑 V1.0 数据量。
  - 负面：若数据量超预期，M7 需做在线迁移（建分区表 + 数据搬迁）。
  - **缓解措施**：在 `0001_init.sql` 的 `alerts` 表上以列注释形式写明「V1.0 暂不分区，M7 评估」，
    确保后续维护者能看到该决策的上下文，而不是以为遗漏。

---

## ADR-003：时间列一律使用 TIMESTAMPTZ，全表补 created_at

- **Status**: 待人工确认（AI 起草）
- **Date**: 2026-09-25
- **来源**: IIP-M0详细开发计划 §T0.2 决策 ③
- **Background**:
  告警时间戳来自异构源（Alertmanager / Prometheus / K8s / Loki），
  若使用不带时区的 `TIMESTAMP`，跨时区或跨 DST 时会产生难以排查的偏移。
- **Options**:
  1. `TIMESTAMP`（无时区）
  2. `TIMESTAMPTZ`（带时区，PG 内部按 UTC 存储）
- **Decision**: 采用 **方案 2**。所有时间列（`starts_at`/`ends_at`/`started_at`/`resolved_at`/
  `timestamp`/`created_at`/`updated_at`/`correlated_at`）一律 `TIMESTAMPTZ`；
  每张表统一补 `created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()`。
- **Consequences**:
  - 正面：时间语义无歧义，跨时区查询与聚合结果确定；新增表有统一的时间锚点。
  - 负面：应用层序列化时需统一为 RFC3339；前端展示需显式做时区转换。
  - 规格中 `evidence.timestamp` 为 `NOT NULL` 但**无 DEFAULT**（证据时间来自采集源，不可用当前时刻兜底），
    此点已在建表语句中如实保留。

---

## ADR-004：M0 即建 `correlation_feedback` 预留表

- **Status**: 待人工确认（AI 起草）
- **Date**: 2026-09-25
- **来源**: IIP-M0详细开发计划 §T0.2 决策 ④
- **Background**:
  M2 的「反馈学习」需要记录人工对关联决策的修正（哪两个告警被人工判定应该/不应该关联）。
  该表不在 V1.0 文档 §3.2 的原始 DDL 中，但建表成本极低。
- **Options**:
  1. M2 需要时再加迁移
  2. M0 就建表（空表占位）
- **Decision**: 采用 **方案 2**，M0 建立 `correlation_feedback` 预留表。
- **Consequences**:
  - 正面：避免 V1.x 为一个已知需求补迁移；M2 启动即可用。
  - 负面：M0 存在一张暂时无写入方的表（可接受，已在迁移文件注释中标明用途与消费方为 M2）。
  - 表结构（按 T0.2 规格）：`incident_id`、`alert_id_pair`、`decision`、`actor`、`created_at`。

---

## ADR-005：应用镜像采用多阶段构建 + distroless 静态运行层

- **Status**: 待人工确认（AI 起草）
- **Date**: 2026-09-25
- **来源**: T0.6 扩展项（原计划只编排依赖服务，应用镜像为本次新增）
- **Background**:
  T0.6 原范围是「五服务依赖编排」，iip-server / iip-worker 走本机裸跑。
  本次要求把应用也容器化，需要决定：一个还是两个 Dockerfile、
  运行层用什么基础镜像、以及 distroless 无 shell 时健康检查怎么做。
- **Options**:
  1. 两个独立 Dockerfile，各写一遍构建流程
  2. **一个 Dockerfile 用 `--target` 出两个镜像**，builder 阶段共享模块缓存
  3. 运行层选 `distroless/static`（无 shell，约 20 MB）
  4. 运行层选 `alpine`（有 wget 可做 HTTP 探针，约 40 MB）
- **Decision**:
  - 采用 **方案 2 + 3**：单一 `Dockerfile`，`FINAL` 两个 target（`server` / `worker`），
    运行层 `gcr.io/distroless/static-debian12:nonroot`，`CGO_ENABLED=0` 静态编译。
  - 为弥补 distroless 无探针工具，**给 iip-server 增加 `--healthcheck` 模式**：
    请求本机 `/healthz` 后按结果退出 0/1，由 compose 的 `healthcheck.test` 调用。
- **Consequences**:
  - 正面：镜像体积小（约 20 MB）、攻击面小、以非 root 运行；一个文件维护两条构建路径，
    不会出现两个 Dockerfile 版本漂移；`--healthcheck` 不依赖任何外部工具，反而比 curl 探针更可靠。
  - 负面：**distroless 无法 `docker exec` 进 shell 排查问题**，容器内调试能力为零；
    `--healthcheck` 是为容器场景新增的代码路径，已配 15 个单元测试覆盖 flag 解析与地址规范化。
  - **缓解措施**：需要调试时改走本机裸跑（`scripts/run-local-nodocker.py`），
    或临时把运行层换成 `alpine`。该权衡已写入 `deploy/README.md`。
  - `iip-worker` 不监听端口、无端口可探，**显式不配 healthcheck**，
    避免制造一个恒为真的假探针（进程存活由 `restart: unless-stopped` 兜底）。

---

## ADR-006：应用组服务用 compose `profiles` 隔离

- **Status**: 待人工确认（AI 起草）
- **Date**: 2026-09-25
- **来源**: T0.6 兼容性要求
- **Background**:
  T0.6 的验收标准是「`docker compose up -d` → `docker compose ps` 五服务全 healthy」。
  若把 iip-server / iip-worker 加进默认服务集，`up -d` 会连带起应用，
  而应用依赖镜像先构建完成，会让原验收命令突然多出一个前置条件。
- **Options**:
  1. 应用与依赖平铺，`up -d` 起七个服务（破坏 T0.6 原验收语义）
  2. 应用组打 `profiles: ["app"]`，默认只起依赖五服务
- **Decision**: 采用 **方案 2**。
- **Consequences**:
  - 正面：T0.6 原验收命令与标准完全不变；需要全栈时显式 `--profile app up -d`，
    意图清晰，不会误起。
  - 负面：每次起全栈都要记得带 `--profile app`（已在 `Makefile` 的 `app-up` 目标里固化）。

---

## ADR-007：去重判定查询的排序必须以 `id DESC` 收尾

- **Status**: 已实施（2026-09-25，M1 端到端实测中发现缺陷后修复）
- **Date**: 2026-09-25
- **来源**: M1 端到端验证（**非预先设计**，是实测暴露的缺陷）
- **Background**:
  `GetAlertByFingerprint` 用于回答「该指纹最新一条是 firing 还是 resolved」，
  直接决定去重与 resolved 生命周期的走向。原实现排序为 `ORDER BY starts_at DESC LIMIT 1`。
  但 **Alertmanager 重发同一告警时 `startsAt` 与首次完全相同**——这是上游的真实行为，不是异常。
  于是「已 resolved 的旧行」与「重新起火的 firing 新行」在 `starts_at` 上并列。
- **缺陷实测证据**:
  端到端投递同一告警六步（firing → 重复 firing → resolved → 重复 resolved → 重新起火 → 再次 resolved），
  步骤 6 期望 `updated=1` 却得到 `skipped=1`。根因是步骤 3 与步骤 6 对同一个查询
  分别取回了不同的行——`starts_at` 并列时返回哪一行由执行计划决定，**未定义**。
  后果严重：告警被误判为「重复 resolved」而跳过，**永远停在 firing 状态**，
  即真实故障恢复后 IIP 永远不会记录恢复。
- **Options**:
  1. 保持 `ORDER BY starts_at DESC`（依赖未定义行为）
  2. `ORDER BY starts_at DESC, id DESC`（用主键做 tie-breaker）
  3. 改用 `created_at DESC`（新增行插入时刻更晚）
- **Decision**: 采用 **方案 2**。同一指纹内 `id` 单调递增，最大者即最近写入的那条，
  语义上正是「最新一条」。方案 3 亦可，但 `created_at` 的时间精度与时钟回拨风险
  使其不如单调主键可靠；且 `id` 已在 0001 中建索引，无需新增索引。
- **Consequences**:
  - 正面：去重判定从「碰巧正确」变为「确定正确」；`starts_at` 并列这一真实场景被覆盖。
  - 负面：无（排序键增加一个已索引列，代价可忽略）。
  - **回归护栏**：新增 `TestRefireThenResolveUsesLatestRow`（I-06）专门钉死该场景，
    两个 payload 的 `startsAt` 刻意取相同值。已做反向验证——把 `id DESC` 去掉后
    该用例立即转红（失败信息与线上观察到的完全一致：`skipped=1` 而非 `updated=1`）。
  - 测试辅助函数的同类排序也已同步为 `starts_at DESC, id DESC`，
    否则断言会「碰巧通过」。

---

## 待人工确认汇总

| ADR | 决策 | 状态 |
|-----|------|------|
| ADR-001 | VARCHAR + CHECK 替代 PG ENUM | 待人工确认 |
| ADR-002 | alerts 表暂不分区 | 待人工确认 |
| ADR-003 | 时间列一律 TIMESTAMPTZ | 待人工确认 |
| ADR-004 | M0 建 correlation_feedback 预留表 | 待人工确认 |
| ADR-005 | 多阶段构建 + distroless 运行层 + `--healthcheck` 自检 | 待人工确认 |
| ADR-006 | 应用组用 compose profiles 隔离 | 待人工确认 |
| ADR-007 | 去重查询排序以 `id DESC` 收尾 | 已实施（M1 实测修复，非待确认项） |

---

## 悬而未决项（OPEN）

> 出现「定不下来／先放一放／等外部条件」时在此登记。只追加 + 就地关闭。

| Date | Source | Open Item | Related Constraints | Current Leaning | Blocked By | Resolves When | Status | Resolution |
|------|--------|-----------|---------------------|-----------------|------------|---------------|--------|------------|
| 2026-09-25 | T0.6 扩展 | 镜像能否真实构建与容器能否真实启动 | Docker Desktop 已装 CLI（29.8.0 / compose v5.5.1 / buildx v0.37.1），但 daemon 不可用：`npipe:////./pipe/docker_engine` 不存在，`WslService`/`vmcompute`/`LxssManager`/`com.docker.service` 四服务均 NOT INSTALLED。**决定性证据**：① 安装日志 `install-log.txt` 中 `Selected backend mode:` 值为空 —— 安装时未检测到任何后端；② `com.docker.backend.exe.log` 显示 `State:stopped` 且 `StopReason:` 为空 —— 引擎**从未被启动**，不是崩溃；③ `C:\ProgramData\DockerDesktop\` 目录不存在（该目录仅在首次成功启动引擎时创建）；④ 当前进程 `IsAdmin = False`，无法 dism 启用功能；⑤ `wsl.exe` 文件存在但执行被本机沙箱 Program Blacklist 硬拦截，反证后端必须是 WSL2 | Dockerfile、.dockerignore、compose 应用组均已写好并通过 `compose config` 语法校验（默认 5 服务 / `--profile app` 7 服务，rc=0）。**镜像内容物已提前完成等价验证**：按 Dockerfile builder 阶段逐参数交叉编译，ELF 解析确认 `PT_INTERP` 缺失 + 无 libc/libpthread/libdl 引用 = 真静态链接；版本串注入生效；server 迁移 SQL 完整嵌入（worker 按预期被死代码消除剥离）。宿主机上应用链路 37 项端到端断言全通过 | 需管理员启用 `Microsoft-Windows-Subsystem-Linux` + `VirtualMachinePlatform` 并重启；`wsl.exe` 被本机沙箱安全策略黑名单拦截，无法由 AI 代为安装 | 用户完成 WSL 安装并启动 Docker Desktop 后 | **RESOLVED**（2026-09-25） | 用户以管理员身份安装 WSL 运行时本体（`wsl.2.7.14.0.x64.msi`，SHA256 校验通过）并手动启动 Docker Desktop。链路 C 实测通过：`docker build` 出 iip-server 13.8MB / iip-worker 11.3MB；`compose --profile app up` 起 7 服务全部收敛到终态；容器内 14 项断言全 PASS。过程中新发现并修复两个缺陷（alertmanager healthcheck 文案不匹配、验收脚本硬编码容器名）。详见 `docs/v1-测试报告.md` 五 / 七节 |
| 2026-09-25 | T0.6 扩展 | 容器内 `iip` 库与宿主机 5433 上的库数据不互通 | 应用组 `IIP_PG_URL` 指向 compose 服务名 `postgres:5432`（内部网络），非宿主机 `localhost:5433` | 倾向保持容器栈自包含；若需复用在 5433 上已验收的数据，可把 `IIP_PG_URL` 改为 `host.docker.internal:5433` | 取决于「容器栈是要独立可移植，还是要复用本地已有数据」 | 用户明确容器栈定位后 | OPEN | — |
| 2026-09-25 | T0.6 扩展 | Docker Desktop 下 `daemon.json` 的镜像源配置无法从外部修改 | Docker Desktop（WSL2 后端）以 GUI 内的配置为 `daemon.json` 权威来源，会回写覆盖外部手改。本次尝试写入三源（DaoCloud + 1Panel + 轩辕），`docker info` 始终只显示一个源 | 不依赖镜像源：改用「预拉取 + 重打标签」（`scripts/prepull-mirror.py`），构建全程零网络。若确需多源，由用户在 GUI 的 Docker Engine 编辑器里改 | 需要 GUI 操作，AI 无法代劳；且当前方案已不阻塞验收 | 出现「预拉取不可行、必须依赖在线多源」的场景时 | OPEN | — |
| 2026-09-25 | M1 启动 | 新迁移的版本号用 `0002_` 还是沿用文档写的 `0003_` | `AGENTS.md` 约束 10 与 `docs/migration-notes.md:19` 均写「后续只能新增 `0003_*.sql`」，但 **0002 从未存在**——`migration-notes.md:15-17` 说明 down 与 up 共用版本号 0001。该「0003」源自 M0 计划原文的「0001_init.sql + 0002_*.down.sql」假设，属**文档自身笔误** | 倾向 `0002_`（保持连续）。`golang-migrate` 不要求版本号连续，两者都能跑；差别只在可读性——不连续的号会让人反问「0002 去哪了」 | 仅缺裁定，无外部条件 | 技术负责人裁定后 | **RESOLVED**（2026-09-25） | 裁定采用 `0002_`。已建 `migrations/0002_alerts_fingerprint_firing_uq.up.sql` / `.down.sql`；up→down→up 三连在真库验证通过（`migrations/migrate_test.go` 的 `TestMigrateUpDownUp`，每步均断言版本号与索引存在性，非仅看命令返回码）。`docs/migration-notes.md:19` 与 `AGENTS.md` 约束 10 中的「0003」确认为文档笔误（0002 从未存在，down 与 up 曾共用 0001），已登记待改 |
| 2026-09-25 | M1 启动 | `sqlc generate` 的生成物（`internal/db/db.go` / `models.go` / `alerts.sql.go`）是否提交进仓库 | `sqlc.yaml:6` 明确「M0 只落配置与查询骨架，**不生成任何 Go 产物**」，故 `internal/db/` 目前只有 `queries/`。M1 起必须生成，但生成物是否入库需定 | 倾向**入库**：CI 与 `docker build` 就不必安装 sqlc CLI，构建可复现、无工具链漂移；代价是生成物需随查询变更同步提交 | 技术负责人裁定 | 裁定后 | **RESOLVED**（2026-09-25） | 裁定**入库**。`internal/db/{db,models,alerts.sql,querier}.go` 已由 sqlc v1.27.0 生成并提交。收益：CI 与 `docker build` 不必安装 sqlc CLI，构建可复现、无工具链版本漂移。代价（已如实记录）：查询变更后必须同步执行 `sqlc generate` 再提交，否则生成物与查询漂移；该纪律已写入手册 §4.6 与 `sqlc.yaml` 注释。另需注意——Windows 版 sqlc 为 MinGW 构建，需 `libwinpthread-1.dll` 与可执行文件同目录才能运行 |

> 确认方式：技术负责人在 `docs/IIP-M0-AI开发计划.md` §P2「你 review」栏的四个关注点
> （表数索引数对照、DECIMAL(5,4) 精度、ON DELETE CASCADE 范围、feedback 表字段是否够 M2 用）
> 逐行审查 `migrations/0001_init.sql` 后，将上表状态改为「已确认」。
