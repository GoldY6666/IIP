# IIP 开发计划（V1.0 → 可发布 MVP）

**版本**：V1.0
**日期**：2026-09-25
**编制依据**：《IIP V1.0 — Incident Intelligence Platform 技术设计文档》
**适用模式**：单人全职开发 · 全栈自研 · 里程碑 + 任务清单制

---

## 1. 计划总览

### 1.1 总体目标

在约 **13-15 周（65-75 个工作日，含 20% 缓冲）** 内，按 V1.0 技术设计交付一个**可安装、可演示、可自验证**的 MVP：

> 接入 Alertmanager 告警 → 归一化去重 → 关联成 Incident → 自动收集证据（Prometheus/Loki/K8s）→ Web UI 查看与处置 → Slack 通知

### 1.2 里程碑路线图

| 里程碑 | 名称 | 工期 | 累计 | 核心产出 |
|--------|------|------|------|----------|
| M0 | 项目骨架与环境 | 5 天 | 第 1 周 | monorepo、DB 迁移、CI 流水线 |
| M1 | 告警接入与持久化 | 8 天 | 第 3 周 | webhook API + 处理流水线 1-5 步 |
| M2 | Correlation Engine | 10 天 | 第 5 周 | 五维关联评分 + Replay 评估集 |
| M3 | Incident 生命周期 | 5 天 | 第 6 周 | 状态机 + Timeline + 查询 API |
| M4 | Evidence 自动收集 | 8 天 | 第 8 周 | Worker 框架 + 三类采集器 |
| M5 | 最小 Web UI | 10 天 | 第 10 周 | Incident Console + Detail + 处置 |
| M6 | 通知与基础指标 | 5 天 | 第 11 周 | Slack 通知 + /metrics |
| M7 | 加固与发布 | 8 天 | 第 13 周 | Docker Compose、压测、v0.1.0 发版 |

### 1.3 范围声明

**本计划做（V1.0 范围内）**：

- Alert Ingest（Alertmanager webhook 协议）
- Normalize / Fingerprint / Dedup / Persist
- Correlation Engine（五维加权评分，阈值 0.80 合并 / 0.50 人工确认 / <0.50 新建）
- Incident 状态机（OPEN → INVESTIGATING → MITIGATED → RESOLVED / IGNORED）
- Evidence Collector（Prometheus 指标、Loki 日志、K8s 事件/部署）
- Timeline
- 最小 Web UI（列表、详情、处置操作）
- Slack 出站通知（HTTP API + 手动 Webhook URL）

**本计划明确不做（防止范围失控）**：

| 不做项 | 归属 | 决策点 |
|--------|------|--------|
| K8s 服务拓扑自动发现 | V1.x | M2 验收时评估：若 Replay 中 DependencySimilarity 全 0 则提前补简单拓扑 |
| 历史共现自学习 | V1.x | 同上，M2 先手动配权重 |
| 完整 On-Call 排班/升级 | V2.0 | 见 §11 风险决策点 D4 |
| AI/LLM RCA | V1.x 增强层 | 独立 spike，不占主线工期 |
| RBAC/SSO/多租户 | Enterprise | 不进 V1.0 |
| 独立通知中心/电话短信 | V2.0 | 不进 V1.0 |

---

## 2. 技术选型（开工前一次性定死）

| 层 | 选型 | 理由 |
|----|------|------|
| 语言 | Go 1.23+ | V1.0 伪代码即 Go；单体 + Worker 模式契合 |
| HTTP 框架 | chi v5 | 轻量、标准库兼容、中间件生态够用 |
| 数据库访问 | pgx v5 + sqlc | 类型安全、编译期校验 SQL，适合单人项目减少低级错误 |
| 迁移 | golang-migrate | 与 V1.0 的 DDL 直接对应 |
| 队列 | V1.0 初期用 DB polling（`SELECT ... FOR UPDATE SKIP LOCKED`），Redis 作为接口预留 | 降低部署依赖，架构文档已允许 |
| 配置 | koanf + 环境变量 | 适配 Docker Compose |
| 前端 | React + Vite + Ant Design | 个人项目出活最快，表格/时间线组件现成 |
| 容器 | Docker Compose（iip-server/iip-worker/postgres/redis/loki/prometheus） | V1.0 设计原则之一 |
| CI | GitHub Actions：lint(golangci-lint) + test + build + 前端 tsc 检查 | 每次 push 必跑，M7 加 E2E |
| 测试 | 单元测试 + testcontainers-go（集成测试起真 PG）+ Replay 数据集（M2） | 质量门禁见 §3.3 |

---

## 3. 工程约定

### 3.1 仓库结构

```text
iip/
├── cmd/
│   ├── iip-server/main.go        # HTTP API 进程
│   └── iip-worker/main.go        # 后台任务进程
├── internal/
│   ├── api/                      # 路由与 handler（chi）
│   ├── ingest/                   # validate / normalize / fingerprint / dedup / persist
│   ├── correlation/              # 五维评分 + 决策
│   ├── incident/                 # 状态机 + timeline
│   ├── evidence/                 # collector 注册表 + prom/loki/k8s 采集器
│   ├── notify/                   # Slack adapter
│   ├── store/                    # sqlc 生成的查询层 + 迁移
│   └── config/                   # 配置加载
├── migrations/                   # 0001_init.sql 起，V1.0 §3.2 的 6 张表
├── web/                          # React 前端
├── deploy/docker-compose.yml     # 一键部署
├── testdata/replay/              # M2 回放评估集（alert 样本 + 期望 incident 分组）
└── scripts/                      # 造数、回放、压测脚本
```

### 3.2 开发规范

- 每个里程碑结束打 tag：`v0.0-m1` … `v0.1.0`，保证"任何时刻都能演示上一个完成的状态"。
- 分支策略：main 常驻可发布；每任务一分支 `feat/M1-webhook`，PR 自合并（CI 绿才合）。
- 每个 handler/算法必须有表驱动单元测试；核心链路覆盖率 ≥ 70%，correlation 包 ≥ 90%。
- 代码注释与 commit message 中文英文均可，但 schema/API 注释用英文。

### 3.3 质量门禁（每次 CI 必过）

1. `golangci-lint` 零新增告警；
2. 全量单测通过；
3. 集成测试（testcontainers 起 PG）覆盖：告警写入→查询、关联合并、状态流转；
4. 前端 `tsc --noEmit` 通过。

---

## 4. M0：项目骨架与环境（5 天）

**目标**：一条命令拉起完整依赖环境，server/worker 空壳可运行，CI 生效。

### 任务清单

- [ ] T0.1 初始化 monorepo：Go module、web/ 脚手架（Vite + React + AntD）、`.gitignore`、`.editorconfig`（0.5d）
- [ ] T0.2 编写 `migrations/0001_init.sql`：V1.0 §3.2 全部 6 张表（alerts / incidents / incident_alerts / evidence / timeline / correlation_rules）及全部索引，GIN(labels) 一并建（1d）
- [ ] T0.3 配置加载（端口、DSN、Redis 开关、日志级别），启动时执行迁移（0.5d）
- [ ] T0.4 `cmd/iip-server` 空壳：chi 路由 + `/healthz` + 请求日志中间件（recovery/zap）（0.5d）
- [ ] T0.5 `cmd/iip-worker` 空壳：polling 循环骨架 + 优雅退出（SIGTERM → drain）（1d）
- [ ] T0.6 `deploy/docker-compose.yml`：postgres + redis + loki + prometheus + alertmanager（联调用）（1d）
- [ ] T0.7 GitHub Actions：lint + test + build；本地 pre-commit 钩子（0.5d）

### 验收标准

- `docker compose up` 后，`make run-server` 与 `make run-worker` 均正常启动，`/healthz` 返回 200；
- CI 三色通过；迁移在干净 PG 上可重复执行。

---

## 5. M1：告警接入与持久化（8 天）

**目标**：打通 V1.0 §4.2 流水线前 5 步，真实 Alertmanager 告警能落库。

### 任务清单

- [ ] T1.1 sqlc 定义并生成 `alerts` 表 CRUD 查询（1d）
- [ ] T1.2 `POST /api/v1/alerts/webhook` handler：接收 Alertmanager v4 payload，JSON 结构校验（version/status/alerts 必填字段）、批量 alerts 拆分（1.5d）
- [ ] T1.3 Normalizer：severity 映射表（critical/error/warning/info → 三级）、label 键名标准化、时区统一 UTC、`endsAt=0001-01-01` 语义处理（2d）
- [ ] T1.4 Fingerprint：V1.0 §4.3 算法（排除 instance/pod/container/node → 排序序列化 → SHA256）；**预留 `correlation_rules.fingerprint_exclude_labels` 可配置扩展**（1d）
- [ ] T1.5 Dedup：同 fingerprint + firing → 更新 starts_at 而非插入新行；resolved 状态匹配逻辑（1d）
- [ ] T1.6 Persist：raw_payload 原文入库；重复 webhook（同 fingerprint 且 startsAt 相同）幂等跳过（1d）
- [ ] T1.7 `GET /api/v1/alerts` 分页查询 API + 过滤（status/source/service）（0.5d）

### 验收标准

- Alertmanager 真实 fire 一条规则 → 3 秒内 `alerts` 表出现记录，labels/severity/fingerprint 正确；
- 同一告警持续 firing 1 小时（Alertmanager 会重复推送）→ 表中**只有 1 行**，starts_at 为首次时间；
- 并发重复 webhook 不产生脏数据（单测 + 集成测试覆盖）；
- 覆盖率：ingest 包 ≥ 80%。

---

## 6. M2：Correlation Engine（10 天）⭐ 技术核心

**目标**：实现 V1.0 §5 五维加权评分与三档决策，并用 Replay 评估集验证效果。**这是全项目技术含量最高、最需要数据迭代的环节，安排整两周。**

### 任务清单

- [ ] T2.1 `correlation_rules` 表 CRUD + 配置 API：权重（Wt/Ws/Wl/Wd/Wh）与阈值（0.80/0.50）改为**数据库可配置**，默认值用 V1.0 文档值（1d）
- [ ] T2.2 TimeSimilarity：窗口重叠/并集，处理 `ends_at=NULL`（仍 firing 按当前时刻算）（1d）
- [ ] T2.3 ServiceSimilarity：service 精确 1.0 → namespace 0.6 → 0.0（0.5d）
- [ ] T2.4 LabelSimilarity：Jaccard，明确集合元素为 `k=v` 对（非仅 key），排除动态 label（1d）
- [ ] T2.5 DependencySimilarity：V1 用**静态拓扑配置文件**（YAML：`checkout -> payment -> mysql` 式依赖边），HasEdge 0.9 / CommonNeighbors 0.5（1.5d）
- [ ] T2.6 HistoricalSimilarity：SQL 查询 fingerprint 对在 `incident_alerts` 的共现频率（1d）
- [ ] T2.7 Correlate 主流程：取 open+investigating 的 Incident → 逐对评分 → max 分 → 三档决策（MERGE / REVIEW 标记 / NEW）→ 事务写 `incidents` + `incident_alerts`（含 correlation_score）（2d）
- [ ] T2.8 **Replay 评估集**：手工构造 3 组场景数据集放入 `testdata/replay/`——①同服务风暴（200 告警期望聚 1-2 个 incident）②跨服务级联（A→B→C 依赖链告警）③不相关杂告警（期望互不合并）；评分脚本输出合并精确率/召回率（2d）
- [ ] T2.9 调权重：跑 Replay 迭代权重配置，把最优默认值固化进迁移脚本（随 T2.8）

### 验收标准

- Replay 三场景：①精确率 100%（不错并）②召回率 ≥ 90%（同根因告警不漏并）③杂告警组每事件独立、不产生合并；
- 权重与阈值修改后无需重启即生效（下条告警按新配置计算）；
- correlation 包测试覆盖率 ≥ 90%；
- 1000 条告警灌入回放耗时 < 60s（单线程可接受，M7 优化）。

### 降级预案（超期 2 周时启用）

砍 T2.5/T2.6（拓扑与历史维度），V1.0 发布先用三维模型（时间 0.35 / 服务 0.35 / 标签 0.30），拓扑与历史共现移至 V1.1。

---

## 7. M3：Incident 生命周期与 Timeline（5 天）

**目标**：Incident 可被人查询、认领、处置、关闭，全程留痕。

### 任务清单

- [ ] T3.1 Incident 查询 API：列表（按 status/severity/service/时间过滤+分页）、详情（含关联 alerts、evidence、timeline 聚合）（1.5d）
- [ ] T3.2 状态机服务：严格按 V1.0 §6.2 转换表实现；非法转换返回 422；**补全文档截断的转换**（RESOLVED→OPEN 重开、MITIGATED→RESOLVED、INVESTIGATING→IGNORED）（1.5d）
- [ ] T3.3 Timeline：状态变更、告警合入、用户操作自动写入 `timeline` 表（1d）
- [ ] T3.4 人工确认队列 API：REVIEW 档告警列表 + 「合并/拒绝」两个操作（拒绝则下次同类不再提示，写 suppression 规则表——新增 1 张轻量表）（1d）

### 验收标准

- 集成测试覆盖全部合法转换 + 每种非法转换的拒绝；
- 一次告警合并到 Incident 后，timeline 完整记录「10:35 关联告警 x / 10:37 张三认领 / 10:50 状态变更」链路；
- REVIEW 队列可处理且二次相同告警不再重复打扰。

---

## 8. M4：Evidence 自动收集（8 天）

**目标**：每个 Incident 创建/合并告警后，自动拉取 1 小时窗口内的三类证据。

### 任务清单

- [ ] T4.1 Worker 任务框架：`tasks` 表 + SKIP LOCKED 领取 + 重试（指数退避、最大 3 次）+ 死信记录（1.5d）
- [ ] T4.2 Collector 注册表接口：`Collect(incident, window) []Evidence`，按 source 可插拔（1d）
- [ ] T4.3 PrometheusCollector：按 service label 查该服务 RED 指标（错误率/延迟/饱和度）窗口前后对比，存 query + value + 变化幅度；超时 5s、失败降级不阻塞（2d）
- [ ] T4.4 LokiCollector：LogQL `label_match`，取错误级别日志 Top 10 + 突增统计（1.5d）
- [ ] T4.5 K8sCollector：list 相关 namespace 近 1h 的 Warning 事件 + Deployment rollout 记录（1.5d）
- [ ] T4.6 Evidence 查询 API（按 incident 聚合 + 按类型过滤）+ 采集预算控制（单 incident 最多 50 条 evidence，超限截断）（0.5d）

### 验收标准

- 人为触发一次测试故障（起 error-rate 规则 + mock deployment）→ Incident 创建后 2 分钟内自动出现 ≥ 5 条证据，`query` 字段可点击复现；
- 外部数据源宕机时：告警主链路零阻塞（webhook 202 延迟无变化）、worker 只记 error 日志、恢复后自动补采；
- 三类采集器均有 mock server 单测。

---

## 9. M5：最小 Web UI（10 天）

**目标**：浏览器完成一次事故处置闭环。

### 任务清单

- [ ] T5.1 前端骨架：布局（侧边导航）、路由、API 客户端、登录（V1 先单管理员 token，不做用户体系）（1.5d）
- [ ] T5.2 Incident Console：活跃事件列表（severity 色标、持续时间滚动、关联告警数、证据数、状态），支持筛选/搜索（2d）
- [ ] T5.3 Incident Detail 三 Tab：①Timeline（垂直时间线组件）②证据卡列表（类型图标 + 值摘要 + 原始 query 展开 + 指向外部面板的链接）③关联告警（含各条 correlation_score 可解释展示）（3d）
- [ ] T5.4 处置操作：认领（→INVESTIGATING）、标记缓解（→MITIGATED）、解决（→RESOLVED）、忽略（→IGNORED），带备注输入（1d）
- [ ] T5.5 REVIEW 确认队列页（0.5d）
- [ ] T5.6 Alertmanager 直跳链接：告警详情带 Grafana Explore/Prometheus 原查询 URL（0.5d）
- [ ] T5.7 配置页：关联权重/阈值编辑表单 + 保存生效（1d）
- [ ] T5.8 前端 E2E：Playwright 冒烟脚本（列表→详情→处置）（1d）

### 验收标准

- 新用户从 `docker compose up` 到在 UI 处置完一个 incident ≤ 30 分钟；
- Detail 页三要素齐：时间线、证据、关联告警及评分；
- 处置操作实时生效并回写 timeline。

---

## 10. M6：通知与基础指标（5 天）

**任务清单**：

- [x] T6.1 Slack Adapter：webhook URL 配置，新 Incident / 状态变更两类消息（Incident 摘要 + UI 深链）（1.5d）
  - 2026-10-18 实施完成：`internal/notify` 叶子包（异步派发 256 队列 + 单 worker；`SlackSink` 纯文本 `{"text":...}`；`formatText` 为可单测纯函数）+ 两处 hook（correlation 的 NEW 档 `link` 成功后、incident 的 `InTx` 提交后）+ 三个配置键（`IIP_SLACK_WEBHOOK_URL` / `IIP_SLACK_TIMEOUT` / `IIP_UI_BASE_URL`，默认关闭）。决策记录 **ADR-024**。
  - **失败语义是本次的实质裁定**：默认关闭、异步 best-effort、失败只记日志（不返回 error，避免 Alertmanager 重投放大故障）；**计数查询失败宁可漏发**也不发「证据 0 条」的假消息。关闭顺序 `srv.Shutdown → notifier.Close → pool.Close`。
  - **实测抓到两个真缺陷**：① 关闭竞态 —— `select` 同时等 queue 与 stopped 时两 case 同时就绪，Go 随机选分支，约一半概率把事件塞进已无人消费的队列并**返回 true**（10 次跑 5 次红）；修法用互斥量把「判 closed + 入队」做成原子段，新增 `TestServiceConcurrentCloseAndNotify` 并用变异证明必红。② 标题重复 —— `incidentTitle` 已含服务名，模板又拼一次（`LatencyHigh (checkout)（checkout）`）；模板改为只用 Title。
  - 判据：`internal/notify` 15 用例（20 连跑稳定，变异 5 处全红）；两处 hook 各 4 用例（变异 3 处全红，其中「触发提前」只被时序用例抓到）；真栈探针（真 PG + 真 server + 假 webhook）两条消息内容与计数全对、优雅退出码 0。
- [x] T6.2 通知去风暴：同 Incident 10 分钟内最多 2 条更新消息；通用出站 webhook（预留外部系统接收）（1d）
  - 2026-10-18 实施完成：`internal/notify/limits.go`（滑动窗口 2 条/10 分钟 + 终态/重开白名单 + 内存上界）+ `webhook.go`（结构化 JSON + 可选 Bearer token）+ 配置三键（`IIP_NOTIFY_WEBHOOK_URL` / `_TIMEOUT` / `_TOKEN`，默认关闭）。决策记录 **ADR-025**。
  - **两个裁定**：① 终态（resolved/ignored）与重开必发且不消耗配额（禹 2026-10-18 裁定，理由是静默丢失终态比多一条消息代价大）；② 档位用固定常量而非配置键（T6.3 出 metrics 后再议）。
  - **滑动窗口而非固定窗口**：固定窗口在边界会漏掉已发消息（t0 与 t0+9m59 各一条后，t0+10m 起算的新窗口可再发 2 条 ⇒ 10 分 2 秒内 4 条）；`TestLimiterWindowIsSlidingNotFixed` 专抓这条，变异成固定窗口后该用例红。
  - **两个 sink 拒绝重定向**：实测默认跟随会把请求体与 `Authorization` 头发给第三方（变异验证时「第三方」收到 2 次请求）。
  - 判据：限流 6 例 + 通用 webhook 5 例 + 配置用例；变异 5 处全红；真栈探针一条告警 + 四次流转，两出口均 4 条（mitigated 被抑制、resolved 必发）、日志无 token、退出码 0。
- [x] T6.3 自监控 `/metrics`：`alert_ingest_total`、`webhook_duration_seconds`、`dedup_hit_ratio`、`correlation_score_bucket`（直方图）、`incident_created_total`、`evidence_collect_duration_seconds`、`task_retry_total`（1.5d）
  - 2026-10-18 实施完成：`internal/metrics`（叶子包，7 个仪器 + 每实例**独立注册表**）+ 三处注入（`alert.Service` / `correlation.Service` / `worker.Processor`，`nil` 兜底为 `Noop`）+ server `/metrics` 端点 + worker 独立监听（默认 `:9091`）。决策记录 **ADR-026**。
  - **三个裁定**（禹 2026-10-18）：① 用官方 `prometheus/client_golang`（非手写 exposition）；② 比率**不落 Gauge**，改「计数器 + PromQL」；③ worker **加独立监听**。实施时另定：注册表**不用全局默认表**（单测会重复注册 panic 且数值互相污染）、server 指标**默认开**（与通知类开关方向相反：指标不出站、不含告警内容，默认关的代价是线上才发现没开）。
  - **与计划原文的三处偏差（如实记录）**：
    ① `alert_ingest_total` 等统一加 `iip_` 前缀（避免与业务系统同名指标撞车）；
    ② `dedup_hit_ratio` **拆为** `iip_alert_dedup_total{result=hit|miss|race}` —— 比率由 PromQL 算，且新增 `race`（判 miss 后插入撞唯一约束 = 并发去重兜底真的生效，它是唯一能证明「两个 webhook 同时到了」的信号）；
    ③ `correlation_score_bucket` 落地为 `iip_correlation_score{action}` 直方图，桶按**分数值域** 0.1 步长给，不用耗时默认桶。
  - **不变量**：`dedup{hit}+dedup{miss} == ingest{stored}+ingest{duplicate}`（`race` 是 miss 子集，不参与等式）。这条比分别断言四个数更能抓漏埋点。
  - **标签纪律**：任何标签都不取事件/告警/服务维度（无界基数），`TestLabelNamesAreBounded` 是正向白名单 + 反向黑名单的机器守卫。
  - **端点契约**：`/metrics` 与 `/healthz` 同级、**不受 `IIP_AUTH_TOKEN` 保护**（Prometheus 不带业务 token）；`IIP_METRICS_ENABLED=false` 时不注册路由（404 而非 403）。worker 端口被占用**只 WARN 不退出**（指标是旁路，不该拖死采集）；compose 给 worker 加 `expose: 9091`（不发布到宿主机）。
  - 判据：新包 6 例 + 埋点侧 29 例；**变异 11/11 全红**；真栈探针（真 PG + 真 server + 真 worker + 假 Prometheus）**6 阶段全 PASS**（含「worker 指标只在真采到证据后出现」与「两个注册表确实独立」）；全量真库 **1088 PASS / 1 SKIP / 0 FAIL**；lint `0 issues`。
  - **未做**：抓取配置示例与 Grafana 面板（T6.4）；抑制次数升级为指标（仍只有 INFO 日志，理由见 ADR-026 的 Related ADRs）。
- [x] T6.4 部署侧 Prometheus 抓取配置示例 + 基础面板 JSON（0.5d）
  - 2026-10-18 实施完成。性质是**兑现 M0 留的欠条**：`deploy/prometheus/prometheus.yml` 里
    iip-server 那段本来就是注释占位（原注释写着「M1 预留…M6 打开」），本次换成两个真 job。
  - 交付四件：抓取配置两个 job（`iip-server:8080` / `iip-worker:9091`）＋
    `rules/iip-self.yml`（4 条记录规则 + 2 条告警）＋ `grafana/iip-overview.json`（9 个 panel）＋
    `deploy/README.md` 接入说明。
  - **去重命中率定成记录规则**（`iip:dedup_hit_ratio:5m`）：它要两行 PromQL 拼，散落在多个面板里
    迟早各自漂移，而那种漂移没有任何报错，只会让两个面板给出不同数字。分母**只含 hit+miss**——
    race 是 miss 的子集（`service.go` 先记 miss，撞唯一约束时再额外记 race），加进去就是重复计数。
  - **两条告警**（禹裁定「最不易误报」两条）：`IIPTargetDown`（up==0 5m）、
    `IIPAlertIngestStalled`（15m 无入库 + 24h 有过入库 + up==1）。第二条两个刻意设计：
    ① `or vector(0)` —— 计数器惰性创建，一条告警都没进过时序列不存在，`sum()` 返回空，
    只写 `== 0` 的版本在「从没通过告警」的环境里**恒不触发**，而那正是最该报的场景；
    ② 24h 基线 —— 排除「这套环境本来就没接入过」。残留误报（白天有量、夜间归零）如实写进注释。
  - **自指问题**：`alertmanager.yml` 把所有告警都送给 iip-server，自监控告警若也走这条路由，
    「入库停滞」一旦送达 IIP 就自己消除条件（它自己就是一条入库）。故文档要求自监控告警走独立 receiver。
  - **顺手清理一个真危害**：`rules/highlatency.yml` 改名 `.yml.example`。两条规则是 M0 用来验链路的
    **恒真假规则**，接通真实指标后每 4h 往库里灌一条 checkout 假告警（落库 → 建事件 → 采证据），
    并在新面板上表现为无法解释的周期尖峰。
  - **不加 Grafana 服务进 compose**（禹裁定）：会破坏「七服务」计数，`verify-images`/`verify-e2e`
    的断言要跟着改一轮，收益不值得。面板以可导入 JSON 形式提供。
  - 判据：`scripts/verify-observability.py`（六类静态核对：引用闭包 / 覆盖 / 标签名与值 /
    记录规则引用与加载通配 / 端口漂移 / 面板结构），**变异 6/6 全红**（拼错指标名、删面板、
    改标签值、引用不存在的规则、端口写错、规则改名 .yaml），恢复后基线仍绿；接入 `tasks.ps1 verify-observability`。
  - **判据自己踩的坑（已修，留在脚本注释里）**：`balanced_block(src, m.start()+m.end()-1)` 把索引
    写成了两数相加，越界后返回空块 ⇒ 0 个 instrument ⇒ 模型为空 ⇒「覆盖检查」因集合差为空而**恒绿**。
    这是判据自证失效的典型形态，故补了「解析到 0 个 instrument 必须硬报」与「0 个选择器必须硬报」两条自守卫。
  - **做不到的（如实标注）**：本机无 Docker daemon / promtool / Grafana 实例 ⇒ PromQL 语法、
    面板渲染、抓取成功三者**均未经机器验证**。文档只声明「名字与定义一致」，不声明「面板可用」。
- [x] T6.5 操作者文档《5 分钟接入 Alertmanager》（0.5d）
  - 2026-10-18 实施完成：`docs/接入指南-Alertmanager.md`（操作侧：receiver → 手发验证 →
    三处确认 → payload 契约 → 排错表 → 通用 HTTP 源 → checklist）+
    `scripts/verify-onboarding.py`（把文档里的代码块**原文**在真栈上跑一遍）。
  - **为什么文档要配可执行验收**：「5 分钟」是可证伪的承诺，而文档最容易被自己毁掉 ——
    curl 少个引号、payload 缺个必填字段、路径错一个字符，都只在操作者那里暴露。
    故脚本按 `<!-- verify:NAME -->` 标记抽出代码块，交给 Git Bash 执行，断言文档声称的
    `accepted/skipped/updated/rejected` 与 5 个状态码。文档与实现一旦不一致就红。
  - 判据 13 项全 PASS（S1–S9）：receiver YAML 结构、curl 原文执行、`page→critical` 归一、
    指标计数、改 `instance` 重发仍 `skipped=1`（动态标签不参与指纹）、resolve 后库里只剩
    resolved、缺 version→400、缺 alertname→200 且 `rejected=1/errors[0].index=0`、
    `/metrics` 与 `/healthz` 免 token 而 `/api/v1/*` 401、通用源最小体。
    **变异 6/6 全红**（文档侧 4：删 version / 路径多 s / 鉴权头写成 Authorization /
    severity 写成未识别值；实现侧 2：指纹不再剔除 instance、不校验 version），恢复后基线仍绿。
  - **判据自己踩的两个坑（都修了，注释留在脚本里）**：
    ① `shutil.which("bash")` 在 Windows 上会拿到 `System32\bash.exe`（WSL 启动器）——
    从 Git Bash 调用能跑、从 PowerShell 调用全红，**判据结论取决于调用者的 PATH**。
    改为逐个候选用 `uname -s` 自证是 MINGW/MSYS/CYGWIN 才用；
    ② 报红只打 stdout 不打 stderr，导致「bash 起不来」看起来像「文档写错了」。
    报红不带真因等于没报。
  - **顺手发现一个真缺陷（T6.5 时未改，已由 T6.6 修）**：请求体里含**非 UTF-8 字节**时，
    `json.Unmarshal` 照收（Go 不校验字符串的 UTF-8 合法性），落库时 PG 报
    `无效的 "UTF8" 编码字节顺序 (SQLSTATE 22021)`，接口回 **503**。
    现象会误导：客户端编码问题被报成「IIP 存储故障」，且 Alertmanager 会一直重试。
    **2026-10-18 由 T6.6 落地为 400（ADR-027）**：三个 JSON 入口在解码前 `utf8.Valid` 校验，
    不合法回 400 并记 `first_invalid_offset`。文档侧仍保留「中文别内联在命令行」的规避说明。
  - 未验证（如实标注）：本机无 Alertmanager 二进制与 Grafana 实例 ⇒ receiver YAML 只证到
    「可解析且字段正确」，未证到「Alertmanager 能加载它」；`http_headers` 的实际发送行为同理。
- [x] T6.6（**计划外新增**）HTTP 入口加固：三个 JSON 入口收成一个 `readBody`（0.5d）
  - 2026-10-18 实施完成。不在 §10 原任务清单内，来源是 T6.5 的现场发现 + T6.6 立项时
    把三个入口并排审查。决策记录 **ADR-027**。
  - **修的是两个一直在的语义缺陷，不是加功能**：① `POST /api/v1/incidents/{id}/comments`
    把「请求体超限」报成 400「不是合法 JSON 或含未知字段」（`decodeStrict` 直接
    `json.NewDecoder(r.Body)`，`MaxBytesReader` 的错误被 `Decode` 一并返回）——
    另两个入口都有 413 用例，**唯独它没有**，从 M3 起没人发现；② 非 UTF-8 请求体
    一路走到 PG 才爆成 503（见 T6.5 那条）。
  - **不变量**：同一类坏输入在**任何** JSON 入口得到同一状态码与同一文案。
    判据写成矩阵（3 入口 × 2 类坏输入）+ 「合法 UTF-8 不能被误杀」的反面用例 ——
    只有矩阵没有反面用例时，「一律 400」也能全绿。
  - **同批两个「不做」的裁定**（禹 2026-10-18，理由写入 `deploy/README.md` 与 CHANGELOG）：
    不做限流（429 会把风暴中最关键的最新告警推到退避队列后面，限流的正确位置是反代/网关）、
    不做 CORS（UI 与 API 同源，默认开是安全倒退）。
  - **必须说清的边界**：本改动**不阻止 Alertmanager 重试**（它对任何非 2xx 都重试），
    价值是**正确诊断**；把它当抑制方案讲就是过度声称。
  - 判据：`internal/api/body_test.go` 4 个用例（413 矩阵 / UTF-8 矩阵 / 合法体反面 /
    偏移计算五组）；**变异 5/5 全红**（去掉 UTF-8 校验 / `readBody` 一律拒绝 /
    incidents 改回读流 Decode / 413 改 400 / 单入口跳过校验）。变异体构建失败不算判据有
    鉴别力 —— 打回退变异时必须同时删掉 `bytes` import，否则是编译红不是断言红。

**验收标准**：新 Incident 30 秒内 Slack 收到通知且消息含证据计数；/metrics 指标在压测中可观测（见 M7）。

---

## 11. M7：加固与发布（8 天）

**任务清单**：

- [x] T7.1 告警数据分区：`alerts` 按月分区 + retention 配置（默认 90 天）+ `pg_partman` 或定时任务（1d）
  - 2026-10-17 实施完成（迁移 0008 + `internal/partition/` + `partition.sql` 三维护函数），决策记录 **ADR-022**（取代 ADR-002）。分区键 `starts_at`，PK `(id, starts_at)`，唯一索引收敛为 `(fingerprint, starts_at) WHERE firing`（分区表硬性要求；语义收敛与残余缺口见 ADR-022，缺口由 `iip_scan_firing_duplicates()` 定期曝光）。
  - **保留期清理的可行形态是实测出来的，不是设计出来的**：被 FK 引用的分区 DROP / DETACH / TRUNCATE **全部失败**（对象级依赖，引用行删净也失败）⇒ `iip_drop_expired_alert_partitions()` 走整表 DELETE + 行级 CASCADE；为此 `incident_alerts` 增加 `alert_starts_at` 列、FK 改复合。代价如实记：分区只增不减、不回收磁盘（空分区保留，顺带为历史月回填留位）。
  - **修了一个时区相关的真 bug**：分区月界原先写 `m AT TIME ZONE 'UTC'`（date 操作数被隐式转 timestamptz = 会话时区午夜），+08 会话下月末界落到分区中段、迁移直接重叠 FAIL；负偏移会话下更糟——**静默把行分错月**。已改 `(m::timestamp) AT TIME ZONE 'UTC'`（绝对 UTC 午夜），并加永久守卫 `TestPartitionBoundsAreUtcMonthRegardlessOfSessionTz`（显式 SET 负偏移会话时区，按 tableoid 钉行的月份归属）——+08 会让 bug 响亮失败，守卫必须覆盖安静失败的时区。
  - 回归：真库全量 `go test -p 1` 0 FAIL；`TestAlertsPartitionMigration`（up → 单步回滚到 7 → up）证明 0008 可完整回滚。
- [x] T7.2 JSONB 索引调优：跑 Replay 慢查询采样，验证 GIN 索引命中，必要时加表达式索引（0.5d）
  - 2026-10-17 实施完成（迁移 0009），决策记录 **ADR-023**。先取证后裁定：5 万 alerts / 5 千 tasks 规模对 9 条热查询跑 `EXPLAIN (ANALYZE, BUFFERS)`，负载 COMMIT 后读 `pg_stat_user_indexes`（ROLLBACK 会把累计索引计数一并回滚——取证方法本身踩过的坑）。
  - **裁定①：删 `idx_alerts_labels` GIN（零读者）**。代码级 44 条 sqlc 查询无 labels jsonb 谓词；运行时 600+ 次语句后 14 个分区副本 `idx_scan` **全为 0**（同期 fingerprint_idx=101、FK 索引=22578，证明统计口径有效）。工具脚本的 `labels->>'x'` 过滤不构成读者——`->>` 等式本来就用不了 GIN。代价如实：GIN 写入维护最贵且分区后每分区一份副本。
  - **裁定②：建 `idx_tasks_payload_incident ON tasks((payload->>'incident_id')) WHERE kind='collect_evidence'`**。证据：Q5（LastCollectWindowEnd）bitmap 读 2144 行再过滤 1.25ms；Q6（EnqueueTaskIfAbsent）**插入路径永远全表扫**（EXISTS 只让跳过路径在首个匹配行短路——主探针里 0.024ms 是短路假象，专项探针三种形态对照证伪了「索引无用」的结论）；tasks 无保留期，成本随表量线性增长。
  - **记录一个被实测否决的方案**：Q3 join 补分区键 `a.starts_at = ia.alert_starts_at`——V0 14.6ms / V1 21.0ms（2.1k 边），V0 19.9ms / V1 20.8ms（7.5k 边），一组更慢一组持平；主导成本是 5 万 alerts 的 Append + 逐分区 Seq Scan 底价（10.6–11.6ms），分区键裁剪去不掉。写进 ADR 以防后来者重做。
  - 回归：`TestIndexTuningMigration`（up → 单步回滚到 8 → up，断言索引**定义**而非仅存在性）；变异测试两轮（注掉 DROP → 红；去掉 WHERE 谓词 → 红）证明判据有鉴别力；判据一度假红（`pg_get_indexdef` 渲染谓词带 `::text` 投射，初版子串绑太死），已改绑不变量。真库全量回归 0 FAIL（唯一 SKIP 为已知的 Windows 符号链接权限项）。
- [x] T7.3 压测：scripts/ingest_bench 灌 1 万告警，目标 p95 < 200ms、零丢失、webhook 不拒绝（1.5d）
  - 2026-10-12 实测：**ingest 链路 6/6 全 PASS**（1 万条、b=1、c=8、吞吐 1785 条/s、折算单条 p95=6.0ms、零丢失、指纹全异）。
  - **但开启 M2 关联时无法收口**：吞吐退化至 ~6 条/s（1 万条 10 分钟跑不完）。分诊证实瓶颈 100% 在关联而非 ingest（`--no-correlation` 下同样 1 万条 16 秒完成）。
  - 根因：`internal/correlation/service.go` 的 `correlateOne` 对每条新 firing 告警遍历所有 open incident 的**全部成员**打分，而 `scorer.Score` → `hist` → `coOccurrence` 每对是一次存储层查询（memo 仅批内有效，1 万个不同指纹几乎不命中）⇒ O(N²)。实测 8 个 incident / 6504 条告警时，每条新告警 ≈ 6400 次 DB 查询。
  - **定量退化曲线**（同为 b=1 / c=8，只翻关联开关）：

    | 条数 | 开关联吞吐 | 折算单条 p95 | 关关联吞吐 | 折算单条 p95 |
    |---|---|---|---|---|
    | 200 | 未测 | 未测 | 158 条/s | 49.9ms |
    | 500 | **96 条/s** | **177.1ms（贴 200ms 线）** | 未测 | 未测 |
    | ~6500 | ~6 条/s | 无法完成 | 未测 | 未测 |
    | 10000 | ~6 条/s（10 分钟未完成） | 无法完成 | **1785 条/s** | **6.0ms** |

    ⇒ **当前实现 N ≲ 500 可接受，N ≥ 1000 不可用**。告警风暴（真实生产常见形态）必然触发。
  - ⇒ 本项标记为「ingest 达标 / 关联阻塞」，缺陷升级为 **R7**，需在 M2 修复规模控制后才可整体收口。
  - **R7 修复进展（2026-10-12）**：方案②「共现批量预取」已实施并被**实证否决**——新告警尚未 link 到 incident，批量共现查询恒返回 0 行，把旧的 M 次白干查询变成更贵的 O(M²) 白干（prefetch 耗时随候选数 1.8ms→90.8ms 爆炸），1 万条吞吐无改善（仍 ~4.5 条/s）。已整体回退。
  - 现按修正方案实施：**候选限额 + 跳无用查询**。① `ListOpenIncidentsWithAlerts` 加 `ROW_NUMBER() OVER (PARTITION BY i.id ORDER BY a.id DESC)`，每 incident 只取最近 `CandidateLimitPerIncident=20` 条（候选集从 7555 条降到 ≤ incident 数×20）；② 历史维不查库——`CorrelateAlerts` 注入 `noHistory` 闭包（与逐对查询的**结果完全一致**：新告警查共现恒得 co=0，那些查询本就是纯浪费）。
  - 语义影响与收口验证（2026-10-12，任务 #25 已完成）：限额可能漏合并（第 21 条以后候选不参与评分），已用三层判据确认——① **M2 Replay 指标无退化**：三场景（风暴 202 条 / 级联 4 条 / 杂告警 5 条）精确率、召回率均 **1.0000 / 1.0000**（与 M2 基线逐位一致）；② **实时路径 A/B**（`replay_realtime_test.go`）：夹具逐条走 `CorrelateAlerts`，「执行限额」与「忽略限额」两遍分组逐组一致，且与回放期望一致（风暴场景限额真实截断 16470 个候选，非空跑）；③ **异质边界探针**（`TestCandidateLimitBoundaryOnHeterogeneousIncident`）：构造「新告警的最佳匹配是 20 条以前的老成员」场景，限额下确实分拆（不限额 1 组 / 限额 2 组）——既证明 A/B 机制有鉴别力（不是结构上恒绿），也把已知限制的准确边界锁定在代码里。
  - **已知限制的处置（2026-10-12 禹裁定：锚定种子成员；2026-10-14 实施完成）**：候选集改为「每 incident 最近 20 条 **+ 该事件的种子告警**」（SQL 加 `rn_asc` 窗口列，WHERE 增加 `OR rn_asc = 1`；sqlc 重新生成，签名未变）。种子 = 事件首条告警（`min(a.id)`，NEW 档建事件时 link 的那条），事件的标题/severity 由它派生，是事件身份的来源 ⇒「与事件同根因的新告警」绝不漏合并，成本每事件仅 +1 条候选。修复经异质探针**三跑对照**验证：不限额 1 组 / 锚定 1 组（修复生效）/ 不锚定 2 组（阴性对照，证明场景确实踩边界且机制可观测分歧）；真库用例 `TestListOpenIncidentAlertsPerIncidentLimit` 扩为四条语义（限额 / 取最近 / **种子恒在** / limit<=0 与小事件不误伤）。
  - **残留边界（比修复前窄得多，已记录待 V1.1 复查）**：种子锚定只保证「与事件首条告警同根因的新告警」不漏合并；若新告警的最佳匹配是**非种子**的老成员，仍可能漏合并（需要更复杂的异质构造，当前探针不覆盖）。标准回放夹具组内同质不受影响；压测中仅 2 个 incident 超 20 条且同为同质风暴，未观测到质量变化。
  - **压测复跑结果（2026-10-12，M2 关联开启，锚定前）**：**6/6 判据 PASS**——1 万条、b=1、c=8、wall=54.8s、吞吐 **182 条/s**（修复前 ~4.5 条/s，提升约 40 倍）、折算单条 **p95=76.3ms < 200ms**、零丢失（10000=10000）、rejected=0、指纹互异。报告见 `scripts/_verify_bench.txt`；以 `python scripts/ingest_bench.py -n 10000 -b 1 -c 8` 实际输出为准。
  - **种子锚定后压测复跑（2026-10-14）**：**6/6 判据 PASS**——同参 1 万条、b=1、c=8、wall=28.9s、吞吐 **346 条/s**、折算单条 **p95=45.6ms < 200ms**、零丢失、rejected=0、指纹互异。锚定每事件仅 +1 条候选，对吞吐无感（数值优于上轮主要来自机器状态更热，非锚定本身提速；结论以判据全过 + 无量级劣化为准）。
  - 仍存在的差距：开关联 1785 条/s vs 开关联 182~346 条/s（约 5~10 倍）——剩余成本是每条告警与 ≤(incident 数×21) 个候选评分 + 2~3 次写库往返，**不再是候选集爆炸**。若未来要更高吞吐，R7 应对③「关联异步化」是兜底。
- [x] T7.4 故障演练：kill worker → 重启恢复任务、PG 断连 → 自动重连补采、Redis 缺席 → DB polling 兜底（1d）
  - 2026-10-14 实测：**33/33 判据 PASS（FAIL=0、SKIP=0，总耗时 70.3s）**。一键脚本 `python scripts/fault_drill.py`——真实进程级故障注入（真 kill / 真停库），端口 PG 5436 + HTTP 8083 + 独立 PGDATA，与其余四条验证链路错开；报告逐行落盘 `scripts/_fault_drill.txt`（gitignore 覆盖，同 `_verify_*.txt` 惯例）。
  - **D1 worker 崩溃与任务回收**（崩溃现场真杀出来，非 SQL 预置）：假采集器挂起 60s 把任务按在 in_flight → 硬杀 worker1（claimed_by 后缀与 pid 对得上）→ 任务停在 in_flight（持有者已消失）→ 起 worker2（stale_after 注入 10s）→ **ReclaimStale 回收**（worker2 日志实证）→ 重领重采 → done。**attempts=2**（崩溃者领 1 次 + 新 worker 领 1 次；回收不 +1、领取才 +1）⇒ 队列不僵死；证据 5 条 + 游标 window_end 回填 + 零死信（崩溃一次不耗尽 3 次尝试）。
  - **D2 PG 断连与自动重连**（真停库 25s）：断连期 webhook **13/13 全 503**（存储不可用可重试语义）+ healthz 503 + server/worker 进程全存活 + worker 日志有逐轮重试记录（非静默停摆）；重启 PG 后**不重启 server**，healthz 自动回 200 ⇒ **pgxpool 自动重连实证**；恢复后新告警入队→采集→证据落库；断连期直插的任务（SQL 注入，payload 带游标；「断连期无法走 webhook 入队」已在判据 detail 标注）恢复后照常处理完 ⇒ **队列不丢**；全程零死信；基线事件证据 5→10（增量补采，不重采覆写）。
  - **D3 Redis 缺席**：`go list -deps ./cmd/iip-worker` 依赖闭包**零 Redis 客户端**（275 个包无命中）+ go.mod 无 redis require ⇒ 结构上不存在 Redis 代码路径；运行时本演练全程无 Redis 进程/环境变量而轮询采集闭环成立（D1/D2 的 PASS 即证据）；外部佐证：verify-func 的 M4 采集闭环 89 PASS 同样运行在默认 DB polling 配置（IIP_REDIS_DISABLED 默认 true）下。
  - 诚实声明：① D1 注入 `--evidence-timeout=30s`（生产默认 5s）只为拉宽崩溃观察窗，判据 detail 有标注；② D2 停库用 `pg_ctl stop -m fast`（计划性停库）模拟断连，不模拟磁盘损坏/脑裂等更深故障（属 PG 自身领域）；③ 三场景的恢复行为全部由产品自身完成，脚本只观测不干预。
- [x] T7.5 Docker Compose 生产化：健康检查、重启策略、资源限制、外部数据源 TLS 配置；Helm Chart 简化版（1d）
  - 2026-10-14 实施完成。范围澄清：摸底确认 compose 的「健康检查、重启策略、镜像钉死、密码扩展字段」早已达标（M6/M12 期建设），本轮真缺口只有三个——**资源限制与日志轮转**、**采集器 TLS**（零能力）、**Helm chart**（仓库完全没有）。
  - **compose 生产化**：7 服务全部加 `deploy.resources.limits.memory`（数据面 PG/Prom/Loki 512M，控制面 128–256M）+ json-file 日志轮转（`x-logging` 锚点，10m×3）。只限 memory 不限 cpu（cpu 限额在共享内核开发机上易把健康检查拖到随机 unhealthy）。补 `.env.example`（.gitignore 的 `!.env.example` 早引用它、文件却一直不存在）。镜像标签**故意不动**（0.0-m2），留给 T7.6 发版一次性改（compose / verify-images.py 的 IIP_TAG / chart 的 values.image.tag 三处联动）。
  - **采集器 TLS**（代码）：`internal/config` 加 `EvidenceTLSInsecure`(IIP_EVIDENCE_TLS_INSECURE，默认 false) + `EvidenceTLSCAFile`(IIP_EVIDENCE_TLS_CA_FILE)；CA 文件「非空但读不到/不是 PEM」在 **config.Validate 与 BuildTLSConfig 双道拦启动失败**，不静默降级。`internal/collectors/tls.go` 新增 `BuildTLSConfig`（MinVersion 钉 1.2；insecure 优先于 CA 且装配时打 WARN）；`collectors.go` 的 `options` 加 `tlsConfig` + `WithTLSConfig`，**4 个采集器各写一遍的 `&http.Client{Timeout:...}` 收敛到 `newHTTPClient(o)`**（无 TLS 配置时与旧行为逐字节一致：不套 Transport）。`cmd/iip-worker` 的 buildRegistry 改返回 error（TLS 配置错误 = 显式配置写错，与「采集器缺席打 INFO」是有意的不对称）。
  - **Helm chart**（`deploy/helm/iip`，简化版）：双轨数据库（内置单实例 PG StatefulSet+PVC / `database.embedded=false` 外置）；采集器地址留空即不注册（与 worker 运行时语义一致）；K8s/trace 开关打开时地址必填由 `required` 在 **helm install 期**拦下（比 worker 启动期更早）；刻意不做多副本选主/PG HA/Ingress/HAP（理由写在 values.yaml 顶部）。
  - **判据**：① 静态 12/12 PASS（verify-images.py 新增 `[0.0]` 段：compose 七服务 resources+logging+restart、TLS 三处接线、四采集器收敛——**放 daemon 检查之前，daemon 不在也能跑**）；② `helm lint` + 3 组 `helm template`（默认/外置+全采集器/漏配）全过，3 类 required 均 rc=1 且错误可读；③ TLS 单测：BuildTLSConfig 5 条 + httptest 自签三态 + 错 CA 被拒 + 零配置路径锁定；④ **变异验证两轮**：WithTLSConfig 改 no-op → 端到端三态两条红（判据有鉴别力）；prometheus.go 退回旧构造 → 收敛判据精确变红；⑤ 全量真库回归 `go test -p 1` exit=0（106.7s，repository 57s/migrations 16s = 真库真跑）；gofmt/vet/golangci-lint(0 issues) 全过。
  - **踩坑记录**：WrongCARejected 用例第一版**恒绿**——httptest 复用包级同一张内置证书，「另一把 CA」其实是同一把；改本地 ecdsa 自签生成异质证书后才真被拒（判据自证失效的又一实例：前提指错对象比断言写错更隐蔽）。
  - **后补门禁（环境阻塞，不冒充）**：**真栈 up + verify-images.py 全链路**——本机 docker daemon 不在跑（Docker Desktop 未启动），compose 仅验到 `config --quiet` rc=0 + python-yaml 静态断言；容器链路段（含 4.7.4d worker 二进制 --help 暴露 TLS flag 的 exec 判据）待 daemon 恢复后补跑。CI ci.yml 仍因 remote 空从未真正跑过。
- [x] T7.6 README（Quickstart/架构图/API 摘要）+ CHANGELOG + v0.1.0 发布流程（1d）
  - 2026-10-14 实施完成。**镜像标签四联动（比 T7.5 预留的三处更广）**：Makefile `IMAGE_TAG`（自查发现自 M2 起停滞在 `0.0-m1`，落后两代漂移，补漂移原因注释）、compose `x-app-image` 锚点 + 注释、Chart.yaml `version`+`appVersion`、values.yaml `tag`、verify-images.py `IIP_TAG` 默认值、check-binaries.py `VERSION`、tasks.ps1 `ImageTag` 参数默认值、deploy/README.md 原始构建示例——全部对齐 `0.1.0`；双向 grep 验证 `0.0-m*` 残留仅剩 docs/ 历史验收表格（按方案不动）+ Makefile 漂移注释 + 两处 `-ldflags version` 注释（代码注入约定说明，非标签值）。
  - **README 重构**：开头状态行从停更的「M3」改为 v0.1.0 实况（M0–M5 收口、手册 M6 S1–S5 收口、T7.3/4/5 收口），未交付项如实标注（计划 §10 的 M6 五项未开工、T7.1-2-7 未做、K8s/trace 未接真实后端、CI 因 remote 空从未跑过）；验证链路四条→五条（补 fault_drill.py + 5436/8083 端口说明）；新增「系统架构」mermaid 图 + 「API 摘要」表（14 路由从 `cmd/iip-server/main.go` 逐一提取，非手写）；末尾追加「M6 验收状态」「T7 与后续」「发布（v0.1.0）」三节。
  - **CHANGELOG.md 新建**（Keep a Changelog 格式，英文节标题 + 中文正文）：`## [0.1.0] - 2026-10-14` 聚合 M0–M6 + T7.3–T7.5 全部变更，Fixed 收录 R7 两轮（批量预取实证否决 + 候选限额/种子锚定），Known limitations 如实列出 11 项未交付/已知边界（含 §10 M6 未开工与 R7 残留边界），不粉饰。
  - **tasks.ps1 补齐**：此前只有 13 个基础函数，T7.3–T7.5 新增的验证能力在 PowerShell 侧完全缺失——补 `ImageTag` 参数 + 8 个函数（Invoke-ImageBuild/ImageLs/AppUp/AppDown/VerifyImages/VerifyRuntime/CheckBinaries/RunLocal）+ list 展示 + switch 分发 + default 错误提示；顺手修正 compose-up/down「五服务」→「七服务」陈旧描述（compose 自 M6 期就是 7 服务，描述漂移）。
  - **判据**：① `helm lint` rc=0（仅 icon INFO）；② `helm template` 默认组 6 文档 rc=0、外置 PG 组 3 文档 rc=0（外置组少 3 文档 = 内置 PG StatefulSet/PVC/Service 正确消失）；③ `go build ./...` rc=0；④ `tasks.ps1 list` 21 个任务全部正常列出（8 个新函数接线验证）；⑤ 标签一致性双向 grep（见上）。
  - **诚实声明**：① 本机 docker daemon 不在跑，镜像构建 / verify-images 全链路与 T7.5 一样只到静态校验，tag 未经过容器链路实证；② 仓库无 git remote，`git tag v0.1.0` 仅本地打好，push 与 GitHub Release 待禹本机执行；③ README 的「五服务」类描述漂移说明文档与代码的同步只能靠人肉 grep，已修的这处之外不担保全仓库无同类陈旧数字。
- [x] T7.7 完整 E2E 回归（M1-M6 验收项一键脚本化）（1d）
  - 2026-10-16 实施完成。核心交付 `scripts/verify-e2e.py`——**不重复实现任何判据**，只做编排：把已有验证资产（6 条链路 + 真库测试 + UI + 压测）编排为 S0–S7 八个阶段，按开发计划的 M1–M6 验收项与 MVP 总验收逐项出状态。阶段：S0 前置（工具链/端口/仓库状态）→ S1 静态（check-binaries + **标签四联动** + helm lint/template + compose config + **干净克隆构建**）→ S2 真库全量测试（`go test -p 1`，含 M2 Replay）→ S3 业务闭环（verify-func 89 判据）→ S4 UI（vitest 180 + playwright 9）→ S5 故障演练（33 判据）→ S6 压测（1 万条 6 判据）→ S7 容器链路。
  - **关键设计**：① 阶段 JSON 独立落 `scripts/_e2e/<id>.json`，支持 `--stages s2,s3` 分批跑 + `--summary` 汇总（沙箱单次调用 ~10 分钟上限，全量约 15 分钟必须分批）；② 5434 被三条链复用，编排器只串行 + 每阶段开跑前端口探测（占位=可读 FAIL，不让脚本跑到一半才炸）；③ BLOCKED 语义——daemon 缺席等环境问题记 BLOCKED 不判红但大字警告「MVP 未完整验收」，`--strict` 下判红（CI 用）；④ 汇总的 M1–M6 映射表内置 23 条验收项，无阶段覆盖的（M0 CI 三色 / M5 的 30 分钟人工 UX / 计划 §10 的 Slack+/metrics）如实标 N/A 并写明原因，不假装覆盖。
  - **标签四联动守卫判据**（治 T7.6 发现的漂移病）：13 个位置（Makefile / compose×3 / deploy README×2 / Chart×2 / values / verify-images / check-binaries / tasks.ps1）逐一「恰好匹配一次」且互等，再与 `git describe` 最新 tag 对齐（剥前导 v）——防「七处一起漂到一个没人发过的值」。
  - **本轮实测结果**（2026-10-16，分批 A=S0+S1 / B=S2+S3 / C1=S4 / C2=S5+S6 / D=S7+汇总）：**S0 PASS(4s) / S1 PASS(53s) / S2 PASS(17 包 ok) / S3 PASS(89/0/0) / S4 PASS(vitest 180/180 + playwright 9/9) / S5 PASS(33/0/0) / S6 PASS(6/6) / S7 BLOCKED(daemon 缺席，12 静态 PASS + 1 环境 FAIL)**。汇总：M1–M6 二十项 PASS + 三项 N/A（如实）；MVP 总验收「Replay+故障演练」「v0.1.0 已发布」PASS，「干净机器 clone+compose up ≤15min」BLOCKED（只实证了 clone+构建半句）。退出码宽松 0 / strict 1，符合设计。
  - **判据（自证不恒绿，六条全过）**：① 用法——`--dry-run` rc=0、未知阶段 rc=2 且列可用、空汇总 rc=2 不装 PASS；② **变异 B（标签判据）**——values.yaml 改 0.9.9 → 红（13 处 2 值）；删 Makefile IMAGE_TAG 行 → 红（0 匹配）；恢复 → 绿；③ **变异 A（失败传导）**——把 S5 命令换成必然失败替身 → 阶段 FAIL、批次 rc=1、汇总结论「FAIL（1 个阶段红）」rc=1；还原后复跑恢复绿；④ BLOCKED 实证——daemon 缺席 S7 报 BLOCKED 非 PASS/FAIL，`--strict` rc=1；⑤ 端口占位——占住 5434 跑 S2 → FAIL「端口 5434 被占用」+ netstat hint + rc=1；⑥ 快 PASS 取证——vitest/playwright 的 34 秒快过被独立复核（落盘日志：Test Files 14 passed / 9 passed + 真 server http_request 痕迹），排除空跑。
  - **踩坑记录（5 个，都已修）**：① **`script_stage` 闭包把 `cmd[0]`（python 绝对路径）当 stage id 传参**——`os.path.join` 遇绝对路径吞掉前缀，JSON 写到 `python.exe.json`，阶段跑过了但汇总 MISSING；是「汇总不装 PASS」的机制抓到的（若只跑批次不看汇总就会漏）。② pnpm 在 Windows 是 `.CMD`，`subprocess` 裸名字调用 rc=127——改 `cmd /c <绝对路径>` 包一层（bash 里直接跑 pnpm 还会被 corepack 垫片搞出 `D:\c\Users\...` 路径，两条路不一样）。③ 分批模式原本阶段 FAIL 也返 rc=0——脚本化消费会误判，补上退出码传导。④ `find_helm` 的相对路径候选按 `REPO/../../` 算成 `D:\Documents\_iip_setup`，实际在 `D:\_iip_setup`——加绝对路径候选。⑤ 子检查原本只在失败时留输出，快 PASS 无法复核——改为成功也落盘。
  - **接线**：`scripts/tasks.ps1` +`verify-e2e` 任务与 `$TaskArgs` 透传口（list 22 项）；`.gitignore` +`scripts/_e2e/`；README 验证链路节加第六行（编排器）+ 任务表补 3 行并修「五服务」→「七服务」；CHANGELOG 加 `[Unreleased]` 节（含 2 条 Known limitations）。
  - **诚实声明**：① S7 容器链路 BLOCKED——本机 docker daemon 仍不在跑（Docker Desktop 未启动），compose up 计时与镜像全链路未实证；② 汇总的判据计数取自各阶段自己的报告文件，阶段超时/崩溃时可能是上一轮旧值（以阶段状态为准）；③ E2E 有意不含 golangci-lint/vet（属 CI 三色范畴，而 CI 因 remote 空从未跑过）与人工 UX 计时；④ 未打新 tag——T7.7 不涉及版本变更，`v0.1.0` 仍是当前发布版。

**验收标准（= MVP 总验收）**：

- 干净机器 `git clone + docker compose up` ≤ 15 分钟完成部署；
- Replay 数据集 + 故障演练全部通过；
- 发布 v0.1.0。

---

## 12. 关键风险与应对

| # | 风险 | 触发信号 | 应对 |
|---|------|----------|------|
| R1 | 关联规则漏并/误并不达预期 | M2 Replay 指标差且调参无效 | 启用 §6 降级预案切三维模型；上线后人工确认队列收集反馈，V1.1 做有监督学习（预留 feedback 表从 M3 开始就写入） |
| R2 | 单人开发 UI 拖期（M5 最易超支） | T5.3 超 4 天 | 砍 T5.6/T5.7；再降级：列表+详情只读 + 处置操作走 API/curl 先跑通 |
| R3 | 外部联调环境不可用 | compose 内 prometheus/loki 起不来 | 全链路均有容器内本地栈兜底，不依赖用户真实生产数据 |
| R4 | webhook 高峰丢告警 | 压测出现 5xx | M7 T7.3 处理：ingest 先落 raw 表再异步处理的两段式改造预案。**2026-10-12 实测未触发**（1 万条零丢失、rejected=0）；但暴露出同源不同形态的性能问题，见 R7 |
| R5 | V1.0 文档语义漏洞 | 开发中发现（如 RESOLVED 后告警重 firing 是否重开 incident） | **决策默认值先记 CHANGELOG-decisions.md 再实现**，不回头改设计文档 |
| R6 | 工期系统性超支 | 任一里程碑超期 >2 周 | 优先级重排：M1+M3 是根，永不降级；M4 砍到只做 Prometheus；M6 通知可推 V1.0.1；发布节点顺延但不砍验收标准 |
| R7 | 关联算法 O(N²)，告警风暴下 ingest 被拖垮 | T7.3 压测：开关联吞吐 ~6 条/s（关关联 1785 条/s） | **已收口（2026-10-12 实施，2026-10-14 种子锚定补强）**：候选限额（每 incident 最近 20 条 + 种子锚定）+ 跳无用查询；方案②「批量预取」已实证否决并回退。压测 6/6 PASS（346 条/s、折算单条 p95=45.6ms、零丢失）；M2 Replay 指标无退化（1.0000/1.0000）；实时路径 A/B + 异质探针三跑证明限额不改标准场景分组、种子锚定消除「与事件同根因的新告警被漏合并」。**残留边界**：最佳匹配为非种子老成员时仍可能漏合并（窄，待 V1.1 反馈数据复查）。③ 异步化为更高吞吐的兜底 |

---

## 13. 进度跟踪机制

1. **每日**：勾选任务清单 + 在任务下记一行状态（阻塞/部分完成）。
2. **每周五（里程碑周）**：跑「里程碑回归包」——已完成里程碑的验收脚本一键执行，防新代码破坏旧功能；输出周报（完成/延期/阻塞）。
3. **每里程碑末**：打 tag、写该里程碑小结（含实际 vs 计划偏差），偏差 >30% 触发 §12 R6 重排。
4. **决策日志**：所有对 V1.0 设计的偏离/补全记入 `docs/decisions.md`，保证一年后仍能追溯"当初为什么这样做"。

## 14. MVP 之后（一句话衔接 V2.0）

V0.1.0 发布后，按 V2.0 提案的优先级顺序演进：**Evidence Graph → On-Call/SLA → Adaptive Correlation（用 MVP 收集的真实反馈数据训练）→ Explainable RCA → 多数据源 Connector**。MVP 期间刻意保留的接口（Collector 注册表、notification adapter、feedback 表、connector 配置结构）就是为这条演进线预埋的接缝。
