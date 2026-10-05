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

## ADR-008：关联评分在出口统一量化到 6 位小数

- **Background**:
  五维加权评分的结果直接用于三档阈值判定（`score >= merge` → MERGE）。
  V1.0 §5.1 的权重是两位小数（0.25 / 0.25 / 0.20 / 0.20 / 0.10），
  默认 `merge` 阈值恰好是 0.80。
- **缺陷实测证据（M2 实现期）**:
  「时间 1 + 服务 1 + 标签 1 + 依赖 0 + 历史 1」这一组合的**数学结果是恰好 0.80**，
  按 §5.3 的 `>=` 语义应判 MERGE。但 IEEE 754 下
  `0.25 + 0.25 + 0.20 + 0.10` = `0.7999999999999999`（**比 0.80 少 1 ulp**），
  于是该组合被判成 REVIEW——即「完美匹配的告警仍需人工确认」。
  这不是假想：两条单测（合并档与历史加分）在真跑时立即转红，实测值
  `0.7999999999999999`、`score >= 0.8` 为 `False`。
- **Options**:
  1. 在 `Decide` 里加容差（`score >= merge - ε`）
  2. 把总分量化到 4 位小数（等于权重的精度）
  3. 把总分量化到 6 位小数
- **Decision**: 采用 **方案 3**，在 `CalculateScore` 出口做一次 `math.Round(score*1e6)/1e6`。
  方案 2 不可行——时间维是「重叠 / 时长」、标签维是 Jaccard，都是**比值（有理数）**，
  总分本身可以有远超 4 位的小数，量化到 4 位会丢弃真实信息（最大 5e-5）。
  方案 1 只治判定、不治数值：日志与 `DECIMAL(5,4)` 落库仍会显示 `0.7999…`，
  且容差值是拍脑袋的。方案 3 把误差压到 5e-7，远低于任何有意义的判定分辨率
  （阈值间隔 0.30），且**一处修改同时让日志、落库与判定三处自洽**。
- **Consequences**:
  - 正面：数学上恰好达阈的组合判定确定、可复现；分数字符串化后不再出现 `0.7999999999999999` 这类噪声。
  - 负面：引入了 5e-7 的量化误差，理论上会把 `0.7999996` 这样「本该 REVIEW」的值抬成 MERGE。
    评估结论是可接受——阈值本身由人以两位小数的精度设定，6 位量化的误差比设定本身的模糊度小 5 个数量级。
  - **回归护栏**：新增 `TestCalculateScoreThresholdBoundary`，同时断言
    「得分严格等于阈值」与「未量化的原始浮点值确实低于阈值」（后者防止将来有人删掉量化后又以为该用例多余）。
  - Replay 三场景的得分（0.675 / 0.57 / 0.775 / 0）均不落在阈值边界上，量化未改变任何既有结论。

---

## ADR-009：REVIEW 档以 `incident_alerts.needs_review` 列落库（关闭 OPEN-M2-2）

- **Background**:
  三档决策中，REVIEW 档（0.50 ≤ score < 0.80）在 M2 阶段**不建/不并事件**是不合适的
  ——它确实高度疑似同源，只是证据不足以自动确认。手册 §3.2 明确
  「人工确认队列 UI 与合并/拒绝操作属 M3；M2 只**落库标记** REVIEW」。
  因此需要一种方式把「这条关联边待人工确认」持久化下来，供 M3 消费。
- **Options**:
  1. 新增 `incident_alerts.needs_review BOOLEAN`
  2. 复用 `correlation_score`：小于阈值即视为待确认（不加列）
  3. 新增独立的 `pending_correlations` 表
- **Decision**: 采用 **方案 1**（迁移 0003）。
  - 方案 2 被否决：判定依赖阈值，而阈值是**可热改配置**（手册验收项）。
    一旦运维把 `merge` 从 0.80 调到 0.70，历史行的「是否待确认」会随当前配置**漂移**，
    人工队列的内容会在配置变更后莫名其妙地变化——这是不可接受的语义。
    显式落列把「当时的判定」固化下来，与后续配置解耦。
  - 方案 3 被否决：会与 `incident_alerts` 形成「同一事实两处存储」，
    需要额外的一致性维护；且 M3 真正需要的是「这条边待确认」这一属性，不是一张新表。
  - 配套：0003 另建**部分索引** `idx_incident_alerts_needs_review ... WHERE needs_review`。
    队列查询只关心待确认的行，部分索引比全列索引小得多。
- **Consequences**:
  - 正面：M3 可用一条 `WHERE needs_review` 查出队列；判定与配置解耦。
  - 负面：`incident_alerts` 增列——但 0001 已冻结、0003 走 `ADD COLUMN IF NOT EXISTS`，
    对既有数据是 `NOT NULL DEFAULT FALSE`，无需回填。
  - 幂等语义已锁定：`LinkAlertToIncident` 用 `ON CONFLICT DO NOTHING`，
    因此**首次判定优先**——同一条边被重复投递时不会把已决结果改写
    （`TestLinkAlertToIncidentIsIdempotentFirstWins` 钉死：先 MERGE(0.85, false) 再 REVIEW(0.60, true) 后，库里仍是 0.85 / false）。
  - 真库验证：`TestCorrelationM2Migration` 用 `Steps(-1)` 只回滚 0003，确认列与种子行均被删除、0002 的索引不受影响。

---

## ADR-010：关联配置 API 拒绝未知字段

- **Background**:
  `PUT /api/v1/correlation/config` 的实现是「以当前配置为底、用请求体覆盖所提供字段」
  （支持部分更新）。若直接 `json.Unmarshal`，JSON 中的**未知字段会被静默忽略**。
- **缺陷分析（M2 实现期，编码阶段发现而非线上）**:
  配置键名打错（`{"threshods":{"merge":0.9}}`、`{"weights":{"servcie":0.25}}`）
  会得到 **HTTP 200 + 配置实际未变**。对配置类接口而言这是最难排查的一类故障：
  调用方看到成功，行为却毫无变化，且没有任何日志或错误可供追查。
  既有的 `Validate()` 拦不住它——权重压根没被解析，校验自然通过。
- **Options**:
  1. 保持静默忽略（JSON 默认行为）
  2. `Decoder.DisallowUnknownFields()`，未知字段 → 400
  3. 只对顶层未知字段告警，嵌套层忽略
- **Decision**: 采用 **方案 2**。未知字段属于「请求体不成形」，语义上是 400 而非 422
  （422 留给「成形但语义非法」，例如权重和不为 1）。
  方案 3 被否决：恰恰是嵌套层（`weights` / `thresholds` 内部）的键名最容易打错。
- **Consequences**:
  - 正面：拼错键名立刻得到可读的 400 与具体字段名；配置接口不再有「静默无事发生」这一状态。
  - 负面：客户端无法再顺手携带额外字段（例如自定义元数据）。评估为可接受——
    该端点的契约是固定的一组权重/阈值，没有扩展字段的正当用途。
  - **兼容性护栏**：新增 `TestCorrelationConfigPutC11_GetThenPutRoundTrip`，
    断言「GET 的输出原样 PUT 回去必须 200」。前端最常见的用法就是读-改-写，
    这条用例专门防「GET 多输出一个字段导致 PUT 400」这类由本决策引入的回归。
  - `TestCorrelationConfigPutC07_UnknownFieldIs400` 覆盖三组拼写错误（顶层与嵌套）。

---

## ADR-011：告警接入层的五个兜底策略裁定（关闭 OPEN-3/4/5/6/7）

- **Background**:
  M1 手册 §十 的 OPEN-3～OPEN-7 五项，代码里都已有「暂定」实现，但都未正式裁定。
  其中 OPEN-5 的复核触发条件写明是「M2 关联引擎上线后复核」，**该条件现已满足**——
  继续挂着就变成没人认领的待办。故本轮一并裁定。
  两个能实测的（OPEN-5 / OPEN-6）不以推理下结论，改在**真库 + 真容器**上做对照实验取证。
- **Decision**（逐项）:

  **OPEN-3：未知 `severity` 维持兜底 `info` + WARN**
  - 依据：未知取值的真实严重度**不可知**。兜底 `warning` 会把「上游新增了一个我们没映射的
    标签值」这类**命名漂移**渲染成需要关注的告警，让噪声挤占注意力；`info` 是唯一的
    「不升级」选择。**可发现性不靠兜底值承担**——`MapSeverity` 同时返回
    `recognized=false`，由调用方打 WARN 日志。
  - 别名表已覆盖 `critical/page/emergency/fatal/error → critical`、
    `warn → warning`、`notice/none/debug → info`，未命中才走兜底。
  - 重估触发条件（**改为可判定形式**）：出现一例「未命中别名表、但语义确为严重」的真实告警。
    原表述「有真实上游数据后」太含糊，无法判定何时算满足，等于永远不会重估。

  **OPEN-4：fingerprint 排除表维持写死，不做成可配置**
  - 依据：指纹是**去重与关联的主键**。若排除表可按环境配置，同一份告警在不同环境会算出
    不同指纹，后果是 ①历史数据无法跨环境比对；②关联的「历史相似度」维度失去意义；
    ③迁移与回放的可复现性被破坏。配置化换来的「适配个别环境」收益远小于此代价。
  - 四键取值（`instance`/`pod`/`container`/`node`）来自 V1.0 §4.3，属规格锁定值。
  - 这是**架构级**决策（影响数据模型的可移植性），故单独成 ADR，而非只在代码注释里说明。

  **OPEN-5：孤立 `resolved` 维持 skip + WARN，不插孤儿行**
  - 依据（**已实测**，见下方证据）：插入孤儿行会让 M2 关联引擎把它**当作一条新告警**参与
    候选集评分，可能在**没有任何 firing 起点**的情况下凭空建出 Incident 或并链到无关事件；
    而无 firing 起点的 resolved 对 RCA 不提供信息。
  - 代价（如实记录）：该条 resolved 在库中不留任何痕迹，仅 WARN 日志可见。

  **OPEN-6：重复 firing 维持「不回写」，保留首次现场**
  - 依据三条：① **Evidence-First**——首次现场是最有价值的一手证据，覆盖即永久丢失；
    ② 同一指纹蕴含非动态标签集合相同（指纹正是它们算出来的），故回写实际只能覆盖
    `annotations` 与 `raw_payload` 两者；③ Alertmanager 在**每个评估周期**都会重推仍在起火的
    告警，回写等于**持续写放大**，收益仅为「注解显示最新值」。
  - 边界（**已实测**）：`firing → resolved → 再 firing` 必须**插新行**，绝不能走「不回写」路径，
    否则「故障恢复后再次起火」的历史会被抹掉。实测确认走 `accepted=1` 插新行。
  - 将来若确需「最新值」视图：**新增列**（如 `latest_annotations`）或改由 `timeline` 记录
    演进，**不得**覆盖首次现场。

  **OPEN-7：`raw_payload` 维持只存单条 alert**
  - 依据：批次上下文（`groupKey` / `receiver` / `commonLabels` / `externalURL`）对同一批的
    **每一行都是相同的**。复制进 N 行是 N 倍写放大，且批次语义一变就要批量改写。
  - 将来若排查确需批次上下文：**加标量列**（`group_key`、`receiver`）或建独立 payload 表，
    **不要**把整批 JSON 塞进每行。

- **Consequences**:
  - 正面：五项不再是没人认领的待办；两个可测项拿到了真库证据而非推理；OPEN-4 的架构理由
    被显式记录，后人不会「顺手把它做成配置项」。
  - **如实记录**：本轮**未改任何产品代码**——五项结论都是「维持现状」。
    这本身是个信号：M1 的暂定值经 M2 上线后的复核仍然成立。
  - **重要如实说明**：`timeline` 与 `evidence` 两张表虽在 0001 就已建好，但**截至本 ADR
    没有任何 Go 代码写入它们**（`internal/db/queries/` 只有 alerts / correlation_rules /
    incidents 三组查询）。因此「信息丢弃由 timeline 承接」**目前不成立**，它只是 M3 的设计
    意图。OPEN-5 / OPEN-6 的「敢丢」依据必须落在上面写的前两条（不污染关联、保留首次现场），
    **不能**写成「timeline 会记下来」。
  - 证据：
    - OPEN-5 / OPEN-6 真库实验：`D:\_iip_setup\_open56_report.txt`（**14 项断言全过**）。
      关键实测值——孤立 resolved → `skipped=1`，且 `alerts 8→8`、`incident_alerts 8→8`、
      `incidents 4→4`；重复 firing → `skipped=1`、同指纹仍 1 行、`annotations` 与
      `raw_payload` 均保持首次值（`999` 未渗入）；`resolved` → `updated=1`；
      再 firing → `accepted=1` 且该指纹累计 2 行。
    - 代码位置：`internal/alert/normalize.go`（severity 别名与兜底）、
      `internal/alert/fingerprint.go`（`DynamicLabelKeys`）、
      `internal/alert/service.go`（`persistWithoutHistory` / `persistAgainstHistory`）、
      `internal/db/queries/alerts.sql`（`ON CONFLICT ... DO NOTHING`）。
- **Related ADRs**: ADR-007（去重判定排序必须以 `id DESC` 收尾，OPEN-6 的正确性依赖它）；
  ADR-009（REVIEW 档落库）。

---

## ADR-012：gocritic `hugeParam` 阈值收敛到 128 字节，真正超重的入参改指针

- **Background**:
  M2 交付后补做 lint 门禁（因仓库无 remote，CI 从未运行），`golangci-lint v1.61.0`
  报 **rc=1 / 17 项**，其中 gocritic `hugeParam` 占 **13 项**。该检查默认阈值 80 字节，
  而本项目三个高频入参都超：`correlation.Incident` 96 B、`correlation.AlertView` 112 B、
  `alert.StoredAlert` 184 B。
  13 项里绝大多数是 `AlertView` 的按值传递——而 `AlertView` 是**刻意设计的值语义对象**
  （纯函数入参、不可变、无别名风险、便于表驱动单测）。为满足一个字节数门槛而全改指针，
  等于牺牲设计意图换指标。
- **Options**:
  (a) 全部改指针 —— 满足默认阈值，但把值语义对象改成指针，扩大 nil 风险与调用面。
  (b) 用 `exclude-rules` 排除 gocritic 的 `hugeParam` —— 等于**关闭该项检查**，
      违反本项目「逐项修复而非加排除规则」的纪律，也让未来真正超重的入参失去告警。
  (c) 收敛阈值到 128 B，**同时**把真正超重的 `StoredAlert`（184 B）改成指针。
- **Decision**: **(c)**。
  - `.golangci.yml` 的 `linters-settings.gocritic.settings.hugeParam.sizeThreshold` 设为 `128`；
    该键名经 `golangci-lint config verify` 校验通过（rc=0）。
  - `correlation.NewAlertView` 的入参由 `alert.StoredAlert`（值）改为 `*alert.StoredAlert`，
    4 处调用点同步改（含 2 处测试构造点：函数返回值不可取址，须先落变量再取址）。
  - 阈值是**收敛**不是**排除**：不 exclude 任何文件，不写任何 `nolint`，
    任何 ≥128 B 的值语义传参仍会被报出。
  - 同批修掉另外 4 项（同属 L1）：`rangeValCopy`（改成索引取址）、`ifElseChain`（改成 switch）、
    `govet shadow` ×2（改用 `errPut` / `errLink` 避免遮蔽）。
- **Consequences**:
  - 正面：hugeParam 13 → 1（只剩 184 B 的 `StoredAlert`），改指针后归 0；
    lint 门禁自 M1 起**首次真正转绿**（rc=0）。
  - 代价（如实记录）：128 B 是**项目自定**阈值，与 gocritic 默认值不再一致，
    后人需要知道「为什么不是 80」——这正是本条 ADR 存在的理由。
  - 残余风险（未掩盖）：若将来出现 90–128 B 的**真·重**结构按值传递，本配置不会报出。
    判断可接受，理由是该区间的按值拷贝成本在纳秒级，而该检查的价值主要在数百字节量级。
- **Related ADRs**: 与 ADR-011 同属「M2 交付后的补账」；ADR-011 记录了
  `timeline`/`evidence` 零写入这一事实，本条记录 lint 门禁此前从未真正执行过。
- **Evidence**:
  - 修复前基线（17 项）：`D:\_iip_setup\_ci_lint_raw.txt`
  - 阈值收敛后的中间态（5 项）：`D:\_iip_setup\_lint_after_threshold.txt`
  - 最终态（rc=0，无输出）：`D:\_iip_setup\_l1_gate.txt`（`golangci-lint: rc=0`）
    与 `D:\_iip_setup\_lint_m3_final.txt`

---

## ADR-013：服务拓扑走 compose 挂载，不烧进镜像

- **Background**:
  M2 报告 §10.2 记了一项未闭合：**容器内 `config/topology.yaml` 缺失**。根因已查明——
  `Dockerfile` 的 builder 阶段只 `COPY cmd/ internal/ migrations/`，`config/` **从未进入镜像**；
  而运行层是 distroless（无 shell、无 `WORKDIR`，默认工作目录为 `/`）。
  危险不在缺文件本身，而在**它静默降级**：`LoadTopology` 对「文件不存在/为空」按设计
  返回空拓扑且**不报错**（M2 手册 §4.5 的要求），于是容器内依赖维恒为 0，
  而所有既有断言**仍然全绿** —— 异常被伪装成正常。
  量化差异：跨服务直连边（checkout↔payment）的关联分在**有**拓扑时为 **0.7133**，
  **无**拓扑时上限只有 **0.5333**（后者直接掉进 REVIEW 档），原先的断言集合区分不出来。
- **Options**:
  (a) `Dockerfile` 增加 `COPY config/ ./config/` —— 把示例文件烧进镜像。缺点：该文件自述为
      「示例，请按真实环境替换」，进镜像会让人误以为已配置真实拓扑；改拓扑须重建镜像。
  (b) compose 挂载宿主 `config/` 到容器 `/config:ro` + 用 `IIP_TOPOLOGY_FILE` 指定 ——
      与 prometheus / alertmanager / loki 三个服务的配置供给方式一致（它们都挂载配置文件），
      改文件后重启容器即生效。
  (c) 不做，接受依赖维恒 0 —— 关联质量实际下降却无人察觉。
- **Decision**: **(b)**。`deploy/docker-compose.yml` 的 `iip-server` 增加
  `volumes: [../config:/config:ro]`，并用 YAML 合并键在共享锚点之上**追加**拓扑路径：

  ```yaml
  environment:
    <<: *app-env
    IIP_TOPOLOGY_FILE: /config/topology.yaml
  ```

  只往 `iip-server` 追加，**不写进 `*app-env`** —— 拓扑只在 `newRouter` 里被 server 读取，
  给 worker 安一个它根本不读的变量是噪声，也会掩盖「worker 不依赖拓扑」这一约束。
- **Consequences**:
  - 正面：容器内依赖维恢复；跨服务直连边分数由 ≈0.5333 回到 **0.7133**；
    同服务对因共享邻居 payment，由 0.6667 升到 **0.7667**（两条均已写进验收断言）。
  - 代价（如实记录）：**容器不再自包含**——部署时必须提供 `config/` 目录。缺失时行为与修复前
    完全相同（静默降级），所以「分数变高」不足以作为唯一判据，必须加**正向断言**。
  - 因此 `scripts/verify-images.py` 新增 4.5.10 段：① 容器日志出现「服务拓扑已加载」且
    `services` 计数 ≥4；② 投递跨服务直连边告警对，断言分数落在 **[0.70, 0.73)**。
    该区间**能把「拓扑已加载」与「静默降级」区分开**（后者上限 0.5333），
    因此是可判定的判据，而不是「看起来更高」。
  - 如实记录：被挂载的仍是**示例拓扑**（4 个服务），不是任何真实环境的拓扑；生产部署须替换。
- **Related ADRs**: ADR-005（镜像多阶段 + distroless 运行层——正因运行层无 shell、
  无 `WORKDIR`，才有「默认工作目录 `/`」这一细节）；ADR-006（compose profiles 隔离应用组）。
- **Evidence**: `D:\_iip_setup\_verify_images_l2.txt`（32 项断言 0 失败）、
  `D:\_iip_setup\_l1l2_regression_report.txt`（六道门禁汇总）。
  > 口径说明：32 项为 **2026-10-01** 当时的真实值。2026-10-02 补 4.5.0「干净起点」
  > 后容器链路为 **34 项断言 0 失败**，且该次是在**未清空 named volume** 的脏数据环境下
  > 跑出来的（正是本 ADR 所加依赖维断言暴露了「重跑必假 FAIL」的脚本缺陷）。
  > 此处保留原始证据数字，当前值以 `docs/M3-交付详细报告.md` §6 为准。

---

## ADR-014：M3 的状态与事件类型用 0004 重建 CHECK，不改 0001

- **Background**:
  M3 状态机需要 `acknowledged`（手册偏差 A1），时间线需要 7 个专用事件类型（偏差 A2），
  但 `0001_init.up.sql:58 / :115` 的 `chk_incidents_status` / `chk_timeline_event_type`
  分别只认 5 个取值——`acknowledged` 不在其中，M3 计划的 7 个类型**一个也不在**。
  直接插入会被 PG 以 23514 拒绝。
- **Options**:
  (a) 改 `0001` —— 违反 `AGENTS.md` 约束 10（迁移文件冻结），且会让已部署环境无法对齐版本。
  (b) 改用 PG ENUM —— 违反 ADR-001（VARCHAR + 命名 CHECK），且加值要 `ALTER TYPE`，
      回滚比 CHECK 更麻烦（ENUM 不能删单个值）。
  (c) 新增 `0004_incident_lifecycle_m3`：DROP CONSTRAINT → 重建为 6 值 / 12 值。
- **Decision**: **(c)**，与 ADR-001 的口径保持一致（仍是可回滚的命名 CHECK）。
  - `incidents.status` → 6 值；`timeline.event_type` → 12 值（既有 5 个**全部保留**，
    其中 `status_change` 虽零存量行仍保留在 CHECK 里，是否删除见 OPEN-M3-3）。
  - 新增表 `review_suppressions`（REVIEW 拒绝后的跨次抑制键，见手册 §4.5）与两个索引。
  - **down 三步破坏性已写进 SQL 文件头并在此明示**：① 删除 7 个新类型的 timeline 行；
    ② `UPDATE incidents SET status='open' WHERE status='acknowledged'`；③ `DROP TABLE review_suppressions`。
    这三步会**真实丢数据**——不回写就无法把 CHECK 还原（PG 会因既有行违反而拒绝重建），
    不是「忘了写补偿」，而是**回滚语义上无法两全**。生产回滚前必须备份。
- **Consequences**:
  - 正面：up → down → up 三连可重复（真库实测 4 用例全 PASS）；`0001`–`0003` 一字未动。
  - 连带动作（容易漏）：**新增迁移必须同步追加到 `sqlc.yaml` 的 `schema` 列表**，
    否则新表在 sqlc 里不存在、查询无法生成。本次已追加。
  - 另一处连带：`migrations/migrate_test.go` 的 `latestVersion` 3 → 4 会让既有用例的
    `Steps(-1)` **改回滚别的迁移**（本次 `TestCorrelationM2Migration` 因此转红）。
    已改为相对写法 `Steps(-2)` / `latestVersion-1`，避免下次重演。
- **Related ADRs**: ADR-001（VARCHAR + CHECK 而非 ENUM）、ADR-009（REVIEW 标记落在关联边上）。
- **Evidence**: `D:\_iip_setup\_m3_s1_mig.txt`（无库组 SKIP=4 / 真库组 PASS=4 FAIL=0 SKIP=0）。

---

## ADR-015：状态流转走「乐观条件 + 同事务」，resolved_at 三态由领域层计算

- **Background**:
  一次状态流转要同时做两件事：改 `incidents.status`、写一条 timeline 留痕。
  分两次写会出现「状态变了但没留痕」或反之的**幽灵流转**；并发下两个请求可能
  同时读到 `open` 再各自写走（后写覆盖先写）；`resolved_at` 还有三态语义要处理。
- **Options**:
  (a) 两次独立写入，先状态后留痕 —— 简单，但留痕失败时状态已改，事后无法追溯。
  (b) 同事务 + `WHERE status = from` 乐观条件，影响 0 行即判冲突。
  (c) 同事务 + `SELECT ... FOR UPDATE` 悲观锁 —— 一致性强，但会把读也堵住，
      且本产品的写冲突概率低、事务极短，用锁换不来收益。
- **Decision**: **(b)**。
  - 事务边界由 `incident.Transactor.InTx` 表达：领域层只认接口，仓储层（`repository.Lifecycle`）
    用 `pgx.Tx` 实现，状态更新与留痕共用同一连接。
  - 乐观条件 `status = from` 落在 SQL 的 WHERE 里（`UpdateIncidentStatus :execrows`），
    返回影响行数；0 行 → `*ConflictError`（409）。**不自动重试**：重试等于替调用方
    猜一个新状态，而新状态该由人决定。
  - `resolved_at` 三态由领域层 `ResolveAtEffect(from, to)` 算出两个开关，SQL 只负责执行：
    进入 `resolved` → 写 `NOW()`；从 `resolved` 重开 → 置 `NULL`；**进入 `ignored` 不写**。
    把规则留在 Go 侧，是为了让它能脱离数据库被单测覆盖。
- **Consequences**:
  - 正面：留痕写失败 → 整个事务回滚，实测「状态保持原值且无残留留痕」
    （`TestLifecycleTxRollback`）。并发冲突变成可判定的 409，而不是静默覆盖。
  - 代价（如实记录）：`ignored` 不写 `resolved_at` 意味着**「忽略时刻」只能从 timeline 取**；
    这是有意为之——误报关闭计入 MTTR 会把指标做脏。
  - 另一处代价：每次流转在事务内多一次回读（为了返回真实落库的 `resolved_at`），
    多一个 round-trip，换取返回值与库内一致。
- **Related ADRs**: ADR-003（时间列一律 TIMESTAMPTZ）、手册 §4.1 决策 D1。
- **Evidence**: `D:\_iip_setup\_m3_s1_lc.txt`（真库组 PASS=46 FAIL=0 SKIP=0）。

---

## ADR-016：M3 前端的三项裁定（批次 / 视觉基调 / 自动化测试）

- **Background**:
  M3 手册把前端排在 D8–D10，但开工前没人定过三件事：
  ① 前端是做成一批还是先搭视觉壳再填内容；② 视觉基调是沿用 antd 默认还是做行业形态；
  ③ 手册 §4.8 要求的「前端单测覆盖边界用例」在 M3 做不做。
  这三条若不定，S3 一开工就会在「先写页面还是先定规范」上来回返工。

  实测现状（2026-10-02）：`web/src` 下仅 5 个源文件共 235 行 —— `App.tsx` 全文 7 行
  （直接 `return <Alerts />`）、`pages/Alerts.tsx` 127 行（M1 告警列表）、`api/alerts.ts` 79 行。
  **没有路由、没有布局壳、没有导航、没有主题定制**（`react-router-dom` 与 `antd`
  已装在依赖里但路由未启用）。App.tsx 注释里那句「后续里程碑可在左侧加导航后挂更多页面」
  至今仍是待办。**因此准确表述是「UI 还没搭」而非「UI 需要翻新」。**

- **Options**:
  ① 批次：(a) S3 一次性交付含视觉；(b) 拆 S2.5 先做「布局+导航+配色+通用组件」的视觉壳；
     (c) 先翻新既有的 M1 告警页。
  ② 视觉：(a) 深色专业控制台；(b) 浅色 + antd 默认微调；(c) 先出静态稿再定。
  ③ 测试：(a) S3 起引入 vitest；(b) 延后到 M5。

- **Decision**（由禹于 2026-10-02 裁定）:
  ① **(a) S3 一次性做完** —— 布局壳、导航、事件列表、详情双 Tab、处置交互连同视觉一并交付。
  ② **(a) 深色专业控制台** —— 运维/监控值守场景的常见形态，长时间盯屏不刺眼，
     告警语义色在深底上对比更强。
  ③ **(b) 前端自动化测试延到 M5** —— M3 期间不引入 vitest。

- **Consequences**:
  - 正面：一次性交付可让禹看到完整产品形态，避免「先搭空壳再返工」；
    深色主题与 IIP 的值守定位一致。
  - **必须如实标注的代价**：手册 §4.8 的「前端自动化测试覆盖边界用例」这条验收
    在 M3 期间 **NOT DONE**，已登记 OPEN 并延到 M5。连带效果是——分页夹紧、
    错误码提示映射、截断提示这类纯前端逻辑在 M3 没有机器可验证的回归网，
    只能靠人工点。这与后端「每条边界分支都要有用例」的纪律不对等，
    **不得在报告里把这条写成通过**（AGENTS.md 约束 5 / 12）。
  - 另一处约束：选择「一次性交付」意味着 S3 的交付包显著大于 S1 / S2，
    review 成本高，需按稳健的 Batches 提交而非一次性大提交。

- **Related ADRs**: 手册 §4.8（前端测试要求）、ADR-015（后端同事务纪律；
  与「前端在 M3 暂时没有回归网」形成对照）。
- **Evidence**: 本 ADR 的「实测现状」由文件系统实查得出（非估算），
  行数按字节级 `\n` 统计。

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
| ADR-008 | 关联评分统一量化到 6 位小数 | 已实施（M2 实测修复：0.80 边界被浮点毛刺误判为 REVIEW） |
| ADR-009 | REVIEW 档落 `incident_alerts.needs_review` 列 | 已实施（M2；关闭 OPEN-M2-2） |
| ADR-010 | 配置 API 拒绝未知字段 | 已实施（M2；防「拼错键名回 200 却什么也没改」） |
| ADR-011 | 告警接入层五项兜底策略裁定（关闭 OPEN-3/4/5/6/7） | 已裁定（OPEN-5/6 有真库实验证据；五项均为「维持现状」，产品代码零改动） |
| ADR-012 | gocritic `hugeParam` 阈值收敛到 128 B + 真正超重入参改指针 | 已实施（M3 前置 L1：lint 17 项 → rc=0；是「阈值收敛」不是「排除规则」，无 nolint） |
| ADR-013 | 服务拓扑走 compose 挂载，不烧进镜像 | 已实施（M3 前置 L2：容器内依赖维恢复，跨服务分数 0.5333 → 0.7133；新增可判定正向断言） |
| ADR-014 | M3 的状态/事件类型用 0004 重建 CHECK，不改 0001 | 已实施（M3 S1：6 值 / 12 值；up→down→up 三连通过；down 三步破坏性已明示；sqlc.yaml 同步追加 0004） |
| ADR-015 | 状态流转走「乐观条件 + 同事务」，resolved_at 三态由领域层计算 | 已实施（M3 S1：`WHERE status=from` + `InTx`；ignored 不写 resolved_at） |
| ADR-016 | M3 前端三项裁定：S3 一次性交付 / 深色专业控制台 / 前端自动化测试延到 M5 | 已裁定（禹 2026-10-02；代价：手册 §4.8 的前端单测在 M3 为 NOT DONE，已登记 OPEN） |
| ADR-017 | K8s 采集器：默认关闭 + 开关打开必须真的注册 + 补 `IIP_K8S_NAMESPACE` | 已实施（M4 B6：24 个 mock server 单测；修正了「开关打开却不注册」的死配置；归属判定改为「先定主人再判归属」） |
| ADR-018 | 迁移单步回滚用例改用绝对版本，`latestVersion` 从 embed 推导 | 已实施（M4 收尾：消除「回退 N 步」随迁移总数漂移的断言；M4 新增 3 个迁移曾让 0003/0004 的 down 完全没被测到） |
| ADR-019 | compose 必须给 worker 注入 `IIP_PROM_URL` / `IIP_LOKI_URL`；容器链路的造数走 Loki push API | 已实施（M4 容器链路：原 compose 只给 `IIP_PG_URL`+`IIP_LOG_LEVEL`，两个采集器静默跳过注册，事件恒 0 条证据而 M1–M3 断言全绿） |
| ADR-020 | 验收断言禁止绑定会随里程碑漂移的总数：总数只记录不判定，硬契约改用「关键表逐张存在」，数量类只做 `>= N` 下界 | 已实施（M4 链路 B：`public 表数量 == 8` 因新增 tasks 表变 10，产品正确而测试红；与 M3「表数因 0004 变 9」是同一个坑，同一处踩了两次） |
| ADR-021 | M5 新增明暗主题切换：默认暗色 + localStorage 记忆，Header 图标切换，palette 动态解析保持 17 处调用点不变 | 已实施（推翻 M5 手册 §3.2 的 Out-of-Scope；**不推翻** ADR-016 —— 后者裁定的是默认基调而非「只有深色」。亮色语义色按白底重新校准，对比度须计算验证） |

---

ADR-021：M5 新增明暗主题切换（推翻 M5 手册 §3.2 的 Out-of-Scope 裁定）

- **Background**:
  M5 手册初稿 §3.2「明确不做」里列着「暗色/亮色主题切换 —— M3 已定深色控制台
  （ADR-016），本期不加切换」。**2026-10-04 禹在 M5 期间追加该需求**，
  并当场裁定四项：配色基准、默认主题与记忆方式、切换入口位置、是否记 ADR。

  先核了 ADR-016 原文：它的选型 (a) 写的是「**深色**专业控制台」，
  理由是「长时间盯屏不刺眼」—— 这是对**默认基调**的裁定，
  **并没有说「产品只有深色、禁止亮色」**。所以本次是**扩展**而非推翻 ADR-016，
  但确实推翻了 M5 手册 §3.2 的一条 Out-of-Scope —— 按「先改手册再改代码」
  的纪律，先改手册（§3.1 加 S5、§3.2 移出该条、新增 §4.5 设计），再动代码。

  实测的**真实技术约束**（决定了方案选择，不是配色偏好问题）：
  `web/src` 下 `palette.*` 共 **17 处**内联 style 引用，跨 **5 个文件**
  （Layout 6 / Settings 6 / WeightRow 2 / Reviews 2 / Incidents 1）。
  这些是字面颜色，**`ConfigProvider` 换 theme 不会让它们变** ——
  只改 `theme.ts` 的后果是「切到亮色后卡片变白、但文字仍是暗色 `#e6edf3`」，
  即亮色主题下白底白字。

  有利的事实：17 处引用的**全是 `text.*` / `surface.*` / `semantic.*` 语义键，
  零处硬编码十六进制**（实查）。这让「保持 17 处调用点不变」成为可能。

- **Options**:
  ① 配色基准：(a) GitHub Light 同源；(b) antd 默认亮色。
  ② 默认主题：(a) 默认暗色 + localStorage 记忆；(b) 跟随 `prefers-color-scheme`；
     (c) 始终暗色不记忆。
  ③ 切换入口：(a) Header 右侧图标按钮；(b) 设置页里加一项；(c) 两处都要。
  ④ 实现路径：(a) 逐处改 `useToken()`；(b) 让 `palette` 随主题动态解析；
     (c) CSS 变量。

- **Decision**（由禹于 2026-10-04 裁定）:
  ① **(a) GitHub Light 同源** —— 与现有暗色主题同源（GitHub Primer 的
     dark/light 两套），改色时不必重新挑色，语义色也能沿用已校准的那一档。
     不用 (b) 是因为它会让控制台从「行业形态」退回「通用后台」观感。
  ② **(a) 默认暗色 + localStorage 记忆** —— 首次访问（无记录）用暗色，
     保持 ADR-016 的值守定位不变；切换后写入 `localStorage`。
     **不采用 `prefers-color-scheme` 作为默认**：值守场景多数在夜间，
     跟随系统会让部分用户在白天看到为暗色重新校准过的亮色。
  ③ **(a) Header 右侧图标按钮** —— 主题属全局设置，顶栏是全局区；
     图标 + Tooltip（「切换到亮色」/「切换到暗色」），点击即切。
     不放设置页是因为切换要专门进 `/settings` 路径长了两步。
  ④ **(b) 让 `palette` 随主题动态解析** —— 保持 17 处调用点**一行不改**。
     逐处改 `useToken()` 要动 17 处，且 `useToken` 只能在组件内调用
     （模块级常量处用不了）；CSS 变量同样要改 17 处。
     详见手册 §4.5.2 的方案对照表。

- **Consequences**:
  - 正面：值守夜间的暗色体验不变，白天/明亮环境下多一个可用选项；
    且不改任何既有调用点，改动面被限制在 `theme.ts` + 一个 Provider + 一处按钮。
  - **必须如实标注的代价**：
    - **亮色语义色要按白底重新校准，不能直接抄暗底那一档** ——
      暗底的 `#f85149`（GitHub danger）对白底的对比度约 3.3:1，
      **不足 WCAG AA 的 4.5:1**。这一条**必须用计算验证，不能凭观感**。
    - **「内联色是否真的跟着变了」只能在真浏览器 / 截图里验** ——
      jsdom 读不到计算后的颜色。若只用组件测试，会出现
      「单测全绿但亮色下白底白字」，与记忆里「测试通过 ≠ 验过了」同一类。
    - 若将来再加第三套主题，`palette` 的动态解析要能扩展；
      但**不为此提前做插件化的抽象**（YAGNI）。
  - 本 ADR 与 ADR-016 **不冲突**：ADR-016 裁定的是默认基调，本次是增加可选的第二套。

- **Related ADRs**: ADR-016（M3 定「深色专业控制台」为默认基调）、
  手册 §4.5（本次的完整设计与四条必验判据）、手册 §3.1 的 S5 条目。
- **Evidence**: 「17 处 / 5 个文件 / 零处硬编码」由文件系统实查得出
  （`web/src` 全量 `.ts`/`.tsx` 正则扫描 `palette\.`），非估算。
  亮色对比度将由计算脚本给出实测值并记入手册 F5 段落，不凭观感描述。

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
| 2026-09-26 | M2 开工（手册 §10 OPEN-M2-1） | 拓扑解析需要 YAML 库，是否违反「`go.mod` 无新增依赖」的验收项 | M2 手册 §八 明确要求 `go.mod` 无新增依赖，但 §4.5 又要求用 YAML 承载服务拓扑 | 倾向引入 `gopkg.in/yaml.v3` 并评估 `go.sum` 是否新增模块 | 无（查证即可裁定） | 查清 yaml.v3 是否为既有间接依赖 | **RESOLVED**（2026-09-26） | `gopkg.in/yaml.v3 v3.0.1` **本就是 koanf 的间接依赖且已在 `go.mod` 中**，M2 使用后升级为直接依赖，`go.sum` 未新增任何模块。故验收项「无新增依赖」成立（严格表述：无新增**模块**；require 列表新增一行 yaml.v3 的直接引用）。另：`LoadTopology` 对缺文件返回空拓扑且**不报错**，使「没有拓扑也能跑」成为默认行为，与「拓扑是可选增强」的设计一致 |
| 2026-09-26 | M2 开工（手册 §10 OPEN-M2-2） | REVIEW 档（0.50 ≤ score < 0.80）在 M2 阶段如何持久化 | M2 手册 §3.2 规定「M2 只落库标记 REVIEW，确认队列 UI 属 M3」 | 倾向在 `incident_alerts` 增 `needs_review` 列 | 无 | 裁定后 | **RESOLVED**（2026-09-26） | 采用 `needs_review BOOLEAN NOT NULL DEFAULT FALSE` + 部分索引。曾考虑「不加列、用 score < 当前阈值 反推」，因阈值可热改会导致历史判定随配置漂移而否决。详见 ADR-009 |
| 2026-09-26 | M2 验收 | M2 镜像的**容器链路**实测（`docker build` → `compose up` → 健康收敛 → 库内 `incident_alerts` 核对） | 手册 §八 的镜像内容物已等价验证（交叉编译 + ELF 解析：真静态、版本串注入、0003 已嵌入）；宿主机真库链路已全绿（21 项断言，0 跳过） | 待 Docker daemon 可用后按 `scripts/verify-runtime.py` 既有流程补验 | 本机 Docker daemon 未运行（`npipe:////./pipe/dockerDesktopLinuxEngine` 不存在）。此前 M0/v1 阶段的容器链路曾实测通过（见上方 2026-09-25 已关闭项），本次是**同一链路的 M2 版本待复验**，非回归 | Docker Desktop 启动后 | **RESOLVED**（2026-10-01） | 用户手动启动 Docker Desktop（本机 CLI 在**非默认路径** `C:\Users\Yu\AppData\Local\Programs\DockerDesktop\`）。`scripts/verify-images.py` 对齐 M2 后**连跑两次**：首轮清空 named volume，27 项断言 0 失败；紧接重跑仍全绿（事件 2→4、关联边 4→8），同时证明脚本可重复执行。容器内的关键证据：迁移到版本 3 且非 dirty、8 张表、`correlation_rules` 种子权重逐位等于 V1.0 §5、PUT 非法权重 → 422 / 未知字段 → 400、阈值抬到 0.99 时本轮 2 条告警均判 NEW、阈值还原后本轮 2 条告警被并入**同一个** Incident，且全程**未重启任何容器**。报告：`D:\_iip_setup\_verify_images.txt`（M1 期同名证据归档为 `_verify_images_m1_20260925.txt`） |
| 2026-09-26 | M2 验收 | K8s 服务拓扑自动发现是否需在 M2 补做 | 手册 §3.2 的触发条件写明「若 Replay 中依赖维全 0 再补」 | 触发条件**未成立**，无需补做 | 无 | 触发条件成立时（依赖维在 Replay 中恒 0） | **RESOLVED**（2026-09-26） | Replay 的 scenario2（跨服务级联）与 scenario1（单服务风暴）均实际用到依赖维：前者靠 `checkout↔payment` 直接边拿到 0.9，后者靠 `CommonNeighbors(checkout, checkout)` 拿到 0.5。依赖维不是空转，§3.2 的触发条件明确未成立 |
| 2026-09-26 | M2 验收 | 事件（Incident）列表/详情 API 与前端页面何时做 | M2 手册 §3.2 已列为 Out-of-Scope（属 M3 T3.1） | README 中原写「事件列表属 M2，404 是预期行为」已随 M2 落地而**过期**，本次一并更正为 M3 | M3 排期 | M3 开工后 | OPEN（**后端部分已完成**） | 后端 API 已于 M3 S2（2026-10-02）交付：`GET /api/v1/incidents`、`GET /api/v1/incidents/{id}`、`POST /{id}/transition`、`POST /{id}/comments`、`GET /api/v1/reviews`、`POST /api/v1/reviews/{incidentID}/{alertID}`，六条路由全部装配并有 handler 用例。**剩余为前端页面**，按 ADR-016 排在 S3（D8–D10）一次性交付 |
| 2026-10-02 | M3 S2（禹裁定） | 前端自动化测试（手册 §4.8 要求「覆盖边界用例」）延到 M5，M3 期间不引入 vitest | AGENTS.md 约束 5 / 12：未做项必须如实标注，**不得写成通过** | M3 保持前端零测试；M5 引入 vitest 时一并覆盖分页夹紧、错误码 → 提示文案映射、截断提示等纯前端逻辑 | 无外部阻塞，纯排期取舍：禹选择把 M3 的时间投在功能交付上 | M5 开工 | OPEN | 已由 ADR-016 记录裁定理由与代价。代价（如实）：M3 内上述三类前端逻辑没有机器可验证的回归网，只能人工点验 |
| 2026-10-04 | M4 收尾补登记 | `evidence.confidence` 全期留空（M4 的 OPEN-M4-1/2/3 此前只写在 `M4-交付详细报告.md` 正文，**未进本登记册**） | AGENTS.md 约束 5：未做项必须如实标注且可追溯；开放项集中在本文才能被下一个读报告的人看到 | 无（纯补账，无需裁定） | M6 开工 | **已登记**：OPEN-M4-1/2/3 三条见下 |
| 2026-10-04 | M4 补登记 | **OPEN-M4-1**：`evidence.confidence` 全表为空 | M4 的三个采集器只做「采到/采不到」，**没有评估置信度的能力**。填 1.0 等于宣称「这条证据确凿」而毫无依据 | 需先设计**可复核**的置信度判据（规则命中 / 覆盖度 / 来源自带），给不出则继续留空 | M6 的 S1 | **OPEN**（延至 M6 S1） |
| 2026-10-04 | M4 补登记 | **OPEN-M4-2**：`type=trace` 无采集器 | T3 类型里已定义 trace，但没有任何采集器往这个值写 | 无（缺的是实现，不是裁定） | M6 的 S2 | **OPEN**（延至 M6 S2）。**2026-10-04 实测补充：`trace` 早已在 `0001` 的 `chk_evidence_type` CHECK 约束内、且被 `idx_evidence_type` 覆盖 ⇒ 实现 trace **零迁移**，「以为要改表」是错的 |
| 2026-10-04 | M4 补登记 | **OPEN-M4-3**：K8s 采集器未接真实集群（偏差 B1 的延续） | 仓库零 K8s 依赖：compose 无 k8s 服务、无 kubeconfig、无 RBAC 清单。只有 mock server 单测（24 个用例）证明解析与降级逻辑正确 | 需禹提供可访问的真实集群 | 拿到集群时 | **OPEN**（延至 M6，待禹提供集群；否则继续挂着） |

> 确认方式：技术负责人在 `docs/IIP-M0-AI开发计划.md` §P2「你 review」栏的四个关注点
> （表数索引数对照、DECIMAL(5,4) 精度、ON DELETE CASCADE 范围、feedback 表字段是否够 M2 用）
> 逐行审查 `migrations/0001_init.sql` 后，将上表状态改为「已确认」。
