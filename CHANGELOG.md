# Changelog

本文件遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/) 格式
（节标题用英文以便工具解析，正文中文）。
版本号与 git tag、镜像标签、Helm chart `appVersion` 保持一致（四处联动，见 README「发布」节）。

**如实声明**：本仓库当前无 git remote，下列 tag 均为本地 tag；
GitHub Release 与 CI 执行待 remote 就绪后补（见 v0.1.0 的 Known limitations）。

## [Unreleased]

### Added

- **可观测性接入：抓取配置 + 记录/告警规则 + Grafana 面板（T6.4）**：把 M0 留在
  `prometheus.yml` 里的注释占位换成两个真实 job（`iip-server:8080` /
  `iip-worker:9091`），并补上操作者真正能用的三件东西。
  - `deploy/prometheus/rules/iip-self.yml`：4 条记录规则（其中
    `iip:dedup_hit_ratio:5m` 是去重命中率的**唯一权威定义**，分母只含 `hit+miss`，
    `race` 是 miss 的子集不进分母）+ 2 条告警（`IIPTargetDown`、`IIPAlertIngestStalled`）。
  - `deploy/grafana/iip-overview.json`：9 个面板（入库速率 / 去重命中率 / target 状态 /
    新建事件 / webhook 延迟 p50-p99 / 关联分数热图 / 采集耗时 p95 / 重试速率 / 抓取耗时），
    带数据源变量，导入即可用。**不新增 Grafana 容器**：会破坏「七服务」计数并牵动
    `verify-images`/`verify-e2e` 的断言，面板以可导入文件形式交付。
  - 静态判据 `scripts/verify-observability.py`（`tasks.ps1 verify-observability`）：
    核对指标名闭包、指标覆盖、标签名与取值、记录规则引用与 `rule_files` 通配、
    抓取端口漂移、面板结构六类。这些错法**全都不报错**，只表现为面板永久空图。
    变异 6/6 全红。**它不核对 PromQL 语法与渲染结果**（需 promtool / Grafana / Docker，
    本仓库三者皆无），故不声明「面板已验证可用」。
- **自监控 `/metrics`（T6.3 / ADR-026）**：7 个业务指标 + Go 运行时/进程指标，
  Prometheus 文本格式。`internal/metrics` 是叶子包（只依赖 `client_golang`），
  每个实例自带**独立注册表**（不用全局默认表：单测会因为重复注册 panic，
  且两个测试的数值互相污染）。
  - 接入侧（server）：`iip_alert_ingest_total{result}`、
    `iip_alert_dedup_total{result}`、`iip_webhook_duration_seconds`、
    `iip_correlation_score{action}`、`iip_incident_created_total`。
  - 采集侧（worker）：`iip_evidence_collect_duration_seconds{source}`、
    `iip_task_retry_total{reason}`。
  - 不变量 `dedup{hit}+dedup{miss} == ingest{stored}+ingest{duplicate}`；
    去重命中率用 PromQL 算而不是进程内 Gauge（Gauge 要额外 goroutine 定时重算，
    且比率的分母窗口与抓取周期一错就对不上）。
  - **标签纪律**：任何标签都不取事件/告警/服务维度（无界基数会拖垮 Prometheus），
    `TestLabelNamesAreBounded` 是机器守卫。
  - 新增直接依赖 `prometheus/client_golang v1.25.0`（间接 6 个模块）。
- **`/metrics` 端点与 worker 独立监听（T6.3）**：server 的 `/metrics`
  与 `/healthz` 同级、**不受 `IIP_AUTH_TOKEN` 保护**（Prometheus 抓取不带业务
  token），`IIP_METRICS_ENABLED=false` 时不注册路由（404，而不是 403）。
  worker 没有 HTTP 服务，故独立监听 `IIP_WORKER_METRICS_ADDR`（默认 `:9091`，
  留空关闭）；端口被占用只记 WARN 不影响采集 —— 让监控端口冲突把采集进程拖死
  是方向反了。compose 给 worker 加了 `expose: 9091`（不发布到宿主机）。
- **通知去风暴 + 通用出站 webhook（T6.2 / ADR-025）**：同一事件 10 分钟滑动窗口内
  最多 2 条**中间态**更新；终态（resolved/ignored）与重开必发且不消耗配额——
  静默丢掉「已解决」会让 on-call 以为故障还在。被抑制的条目不进队列，
  每抑制一条记一行 INFO（含窗口内已发条数）。
  新增 `GenericSink`：POST 机器可读 JSON（`kind` / `incident{...}` /
  `from_status` / `to_status` / `actor` / `alert_count` / `evidence_count`），
  可选 `Authorization: Bearer`，三键 `IIP_NOTIFY_WEBHOOK_URL` /
  `_TIMEOUT` / `_TOKEN`，默认关闭、与 Slack 独立开关。
  两个 sink 统一**拒绝重定向**（302 会把请求体与凭据转给第三方，实测过）。
- **Slack 通知（T6.1 / ADR-024）**：新事件与状态变更两类消息，含事件摘要、
  关联告警数/证据数、UI 深链。`internal/notify` 是叶子包（只依赖标准库）：
  异步派发（256 队列 + 单 worker，`Notify` 非阻塞），失败只记日志
  ——通知的可用性不参与告警接入的成败。默认关闭：未配
  `IIP_SLACK_WEBHOOK_URL` 时装配处不注入，无「误发」路径。
  计数查询失败**放弃该条通知**而不是发「证据 0 条」，避免 on-call 误判。
  关闭顺序 `srv.Shutdown → notifier.Close → pool.Close`（排空残留）。
- **`alerts` 按月 RANGE 分区（T7.1，迁移 0008 / ADR-022）**：分区键 `starts_at`，
  PK `(id, starts_at)`，月度边界按 UTC 计算——修掉了 `date AT TIME ZONE 'UTC'`
  在非 UTC 会话时区下把月末界算错（+08 下重叠 FAIL、负偏移下**静默分错月**）
  的真 bug，永久守卫 `TestPartitionBoundsAreUtcMonthRegardlessOfSessionTz`。
  保留期清理 `iip_drop_expired_alert_partitions(retention_days)` 走整表 DELETE
  + 行级 CASCADE（实测被 FK 引用的分区 DROP/DETACH/TRUNCATE 全部失败，
  对象级依赖与行数无关）；`incident_alerts` 增加 `alert_starts_at` 列、
  FK 改复合以放行清理。另有两个维护函数：插入路径补建分区（23514 重试）、
  重复 firing 曝光（见下 Known limitations）。
- **tasks 事件表达式索引（T7.2，迁移 0009 / ADR-023）**：
  `idx_tasks_payload_incident ON tasks((payload->>'incident_id'))
  WHERE kind='collect_evidence'`。LastCollectWindowEnd / EnqueueTaskIfAbsent
  两条消费查询原先全表扫，且 tasks 无保留期（done 行按设计永久保留供审计），
  成本随表量线性增长；索引把「按事件找任务」变成定长探测。
- **E2E 一键回归（T7.7，`scripts/verify-e2e.py`）**：把各验证链路 + 真库测试 +
  UI + 压测编排为 S0–S7 八个阶段，按开发计划的 M1–M6 验收项与 MVP 总验收逐项
  出状态。特性：阶段结果独立落 `scripts/_e2e/<id>.json`（支持分多批跑完再
  `--summary` 汇总）；端口开跑前探测（5434 被三条链复用，占位即可读 FAIL）；
  标签四联动守卫判据（Makefile / compose×3 / deploy README×2 / Chart×2 /
  values / verify-images / check-binaries / tasks.ps1 + git tag 对齐，治 T7.6
  发现的「标签停两代没人发现」病）；干净克隆构建检查（MVP 总验收「干净机器
  clone 后可构建」的直接实证，GOPROXY=off + 冷 GOCACHE）；BLOCKED 语义
  （daemon 缺席等环境问题不判红但大字警告，`--strict` 下判红，供 CI）；
  `--dry-run` / `--no-bench` / `--stages` 子集。
- `scripts/tasks.ps1` 新增 `verify-e2e` 任务（透传 `--stages`/`--summary`/
  `--strict`），参数块加 `$TaskArgs` 剩余参数口。
- README 任务表补 `verify-runtime`/`check-binaries`/`verify-e2e` 三行，
  修正 compose「五服务」→「七服务」陈旧描述。

### Changed

- **M0 烟测规则停用（T6.4）**：`deploy/prometheus/rules/highlatency.yml` 改名为
  `highlatency.yml.example`。里面两条规则是 M0 用来验证「Prometheus → Alertmanager →
  webhook」链路的**恒真假规则**；链路自 M1 起就是真的了，于是它们每 4 小时往
  `alerts` 表灌一条 checkout 的假告警（会被采集证据、可能建事件），并在 T6.4 的面板上
  表现为无法解释的周期尖峰。需要恢复联调时去掉 `.example` 后缀即可。
  同时把 `prometheus.yml` 的 `external_labels.env` 由 `m0` 改为 `local`（M0 已收口，
  继续叫 m0 会让「这批数据来自哪个阶段」变成谎言）。
- **删除 `idx_alerts_labels` GIN 索引（T7.2，迁移 0009 / ADR-023）**：零读者——
  44 条 sqlc 查询无 labels jsonb 谓词；600+ 次语句执行（覆盖全部触达 alerts
  的读路径）后，14 个分区副本 `idx_scan` 全为 0，同期其他索引有实际读数
  （证明统计口径有效而非「统计没开」）。GIN 是写入维护最贵的索引类型且分区后
  每分区一份副本，删除减少每条告警插入的维护开销与存储。工具脚本的
  `labels->>'x'` 过滤不构成读者（`->>` 等式本来就用不了 GIN）；若将来要做
  按 label 搜索，down 迁移已备好重建 DDL。
- **firing 唯一性语义随分区收敛（T7.1，同 0008）**：同指纹 firing 唯一性从
  「与 starts_at 无关」变为「同指纹 + 同 starts_at」（分区表唯一约束必须含
  分区键的硬性要求）。正常路径由应用层 LatestByFingerprint 先行去重，DB 层
  兜底并发投递（Alertmanager 保证重发不重置 startsAt）。
- S1 静态段新增判据：helm lint + 两组 template、`compose config --quiet`
  由 T7.5/T7.6 的临时命令固化为 E2E 阶段。

### Known limitations

- **T7.1 分区只增不减**：保留期清理删行不删分区（FK 对象级依赖使 DROP 分区
  不可行），空分区长期保留；磁盘回收依赖 VACUUM。空分区顺带为历史月
  回填/重放留位，是有意取舍。
- **T7.1 语义收敛的残余缺口**：同指纹、不同 starts_at 的两条 firing 可并存
  （「起火 → 未收到 resolved → 以新 startsAt 重火」），由
  `iip_scan_firing_duplicates()` 在维护 tick 定期曝光——是已知限制而非静默错误。
- **T7.2 删索引的恢复成本**：若将来要做「按 label 搜索告警」，需重新加回 GIN
  或改用表达式索引（键集合要先设计，脚本用到的键 run/alertname/instance 各不
  相同，直接建表达式索引是选择性灾难）。down 迁移已备好 GIN 的还原 DDL。
- E2E 的 S7 容器链路需要 Docker daemon：本机 daemon 未启动时记 BLOCKED，
  MVP 总验收第一项（干净机器 clone + compose up ≤15 分钟）只实证了
  clone + 构建部分，compose up 计时待 daemon 恢复后补跑
  （`python scripts/verify-e2e.py --stages s7`）。
- E2E 汇总的判据计数取自各阶段自己的报告文件，阶段超时/崩溃时计数可能
  是上一轮的旧值——以阶段状态（FAIL/BLOCKED）为准，计数仅作参考。

## [0.1.0] - 2026-10-14

首个公开发布版。聚合 M0–M6 全部功能交付与 T7.3–T7.5 硬化工作。
历史里程碑 tag 为 `v0.0-m0` ~ `v0.0-m5`（手册 M6 的 `v0.0-m6` 按报告决定未打，
其功能已包含在本版中，原因见 Known limitations）。

### Added

- **告警接入（M1）**：Alertmanager webhook 接入（请求体上限 1 MiB，超限 413）；
  指纹去重（同指纹 firing 唯一索引，重发/重复 resolved 不增行）；告警列表 API
  （分页参数自动夹紧）。
- **关联引擎（M2）**：五维加权评分（Time 0.25 / Service 0.25 / Label 0.20 /
  Dependency 0.20 / Historical 0.10）+ 三档决策（MERGE ≥0.80 / REVIEW ≥0.50 /
  NEW），同根因告警自动聚合为事件；权重/阈值配置 API，修改免重启生效；
  Replay 评估集与三场景夹具（精确率/召回率均 1.0000）。
- **事件生命周期（M3）**：6 态状态机（36 格转换矩阵逐格可判）、乐观并发控制
  （冲突 409）、状态流转与 timeline 同事务（双向验证）、待确认队列、人工判定
  与抑制（可幂等）、事件查询/流转/评论/复核共 6 个端点。
- **证据采集（M4）**：Prometheus / Loki / K8s / trace 四类采集器，窗口前后对比
  （RED 指标、错误模式 Top N、错误量突增）；PG 表任务队列（`FOR UPDATE SKIP LOCKED`
  领取、`stale_after` 回收、退避重试、死信）；证据 API（超 50 条截断并给
  `truncated`/`total` 真值）。**采集不在告警接入路径上**：数据源宕机时 webhook
  延迟不变（实测 31ms → 61ms）。
- **Web UI（M5）**：事件列表/详情双 Tab/处置交互/证据面板，与 API 同源同端口
  静态托管（SPA 深链接可刷新、`/api` 不被吞、目录穿越一律 400）；UI 侧面板深链
  （Prometheus/Grafana/Loki）；前端组件测试 172 条、E2E 9 条。
- **根因与置信度（M6 S1）**：`internal/rca` 三态置信度判据
  （Unassessable / Weak / Medium / Strong）+ 覆盖度评估 + 档位合成；
  **给不出依据的证据一律留空**，不填 1.0。
- **链路采集器（M6 S2）**：Jaeger 兼容 Query API 的 trace 采集器，默认关闭
  （接入是部署侧的事，不为「看起来在工作」而默认开）。
- **UI 鉴权（M6 S4）**：`IIP_AUTH_TOKEN` 静态 token 保护 `/api/v1/*`；
  `/healthz` 与 `/api/v1/auth/session` 放行；token 不回显日志。默认关闭
  （内网部署零改动可用），暴露到非本机网络时建议开启。
- **压测工具（T7.3）**：`scripts/ingest_bench.py`，1 万条 6/6 判据 PASS
  （吞吐 346 条/s、折算单条 p95=45.6ms < 200ms、零丢失、rejected=0）。
- **故障演练（T7.4）**：`scripts/fault_drill.py` 三场景真实进程级注入
  （真 kill worker / 真停 PG / Redis 缺席），33/33 判据 PASS。
- **部署生产化（T7.5）**：compose 七服务资源上限 + json-file 日志轮转；
  采集目标 TLS（系统根证书 / 自定义 CA / insecure 过渡三态，CA 读不到或非 PEM
  **启动即失败**，不静默降级）；`deploy/helm/iip` Helm 简化版 chart
  （内置单实例 PG 或外置双轨）；`.env.example`。

### Changed

- **关联候选限额（R7）**：每 incident 最近 20 条候选 + 种子成员锚定，
  开关联吞吐 4.5 → 346 条/s（约 40 倍提升），M2 Replay 指标无退化
  （三场景精确率/召回率逐位一致）。
- **版本形态**：镜像/compose 标签由 `0.0-m<n>` 切换为 semver `0.1.0`
  （compose / Makefile `IMAGE_TAG` / `verify-images.py` `IIP_TAG` /
  chart `appVersion` 四处联动；本次同时修复 Makefile 自 M2 起停滞在
  `0.0-m1` 的漂移）。
- **构建工具链**：Dockerfile Go 1.23 → 1.26（修复潜伏两个里程碑的版本漂移，
  `GOTOOLCHAIN=auto` 自举并校验一致性）；golangci-lint v1 → v2，
  `.golangci.yml` 随迁移（lint 0 issues）。

### Fixed

- **告警 resolved 后永远停在 firing**（ADR-007）：`GetAlertByFingerprint`
  按 `starts_at DESC` 取行，重发告警 `startsAt` 相同导致并列时行选择未定义
  （实测同轮两次调用取到不同行）→ 改为 `starts_at DESC, id DESC`。
- **评分边界 1 ulp 误判**（ADR-008）：`0.25+0.25+0.20+0.10` 浮点和为
  0.7999999999999999，数学上恰达阈值的组合被误判 REVIEW → 出口统一量化
  6 位小数，边界用例钉死。
- **事件详情「显示不全说成全部」**：`AlertCount` 用截断后的切片长度
  （永远 ≤200），与列表端 `count(*)` 真值自相矛盾 → 未截断时复用切片、
  截断时改走真计数 SQL。
- **K8s 事件挂错服务**：字符串前缀匹配把 `checkout-staging-1` 算作
  `checkout` 的事件 → 先全命名空间定唯一主人（最长匹配）再判归属。
- **K8s 采集器开关是死配置**：开关打开且地址已配时只打日志、从不注册
  （「日志说已启用、证据恒 0 条且无报错」）→ 补装配层用例。
- **静态托管文件句柄泄漏**（M5）：`serveIndex` reader 从未关闭，
  测试 TempDir 清理失败 → 修复并留判据（TempDir cleanup 失败先查句柄）。
- **compose 弱默认密码**（#12）：PG 默认口令收敛为「仅本地开发」并集中到
  `.env` 覆盖点，附强制覆盖文档。

### Security

- 新增 UI 静态 token 鉴权（M6 S4，默认关闭；仅挡误暴露，不等于可公网部署——
  `deploy/README.md` 的内网限定标注保留）。
- 目录穿越防护（M5 S3）：12 组穿越载荷全部 400，未拿到文件内容。
- 采集链路 TLS 支持（T7.5）：默认系统根证书校验；支持自定义 CA；
  `insecure` 仅限内网过渡且启动日志打 WARN；CA 文件读不到/非 PEM 在启动期
  失败而非静默跳过校验。
- 依赖与工具链安全升级：pgx / chi / x/text / mapstructure 漏洞版本升级，
  移除 `lib/pq`；Go 1.23.8 → 1.26.9（清除 40+ 条标准库 CVE， lint 相应升 v2）。

### Known limitations

> 以下是 v0.1.0 **如实标注的未交付项与已知取舍**——不修饰为「已完成」。
> 每条的详细理由与证据见 `docs/decisions.md` 登记册和各阶段交付报告。

- **无通知能力**：开发计划 §10 的 M6（T6.1–T6.5：Slack adapter、通知去风暴、
  通用出站 webhook）**未开工**。v0.1.0 不会向任何外部系统推送事件通知。
- **无自监控指标**：`/metrics` 端点（T6.3）与 Prometheus 抓取示例（T6.4）未做，
  压测可观测性目前靠 `ingest_bench.py` 客户端侧统计。
- **K8s / trace 采集器未接真实后端**：默认关闭（偏差 B1）；mock server 单测 +
  变异测试覆盖，**没有任何端到端链路跑过真实集群/链路后端路径**（OPEN-M6-3）。
- **confidence 的已知边界**：给不出依据的证据留空（k8s rollout 侧与 trace
  恒为 Unassessable，OPEN-M6-2）；`Assign` 跨轮不重算（幂等是刻意取舍，
  OPEN-M6-4）——「同一事件里 metric 0.50、log 0.65」是设计行为不是算错。
- **关联候选限额的残留边界**（R7）：新告警的最佳匹配若是**非种子**老成员，
  仍可能漏合并（窗口更窄，待 V1.1 用真实反馈数据复查）。
- **告警表未分区、JSONB 索引未调优**（T7.1 / T7.2 未做）：数据量增长后的
  查询性能余量未验证。
- **CI 未在 GitHub 上执行过**：仓库无 git remote，`.github/workflows/ci.yml`
  （backend / frontend / e2e 三 job）从未真正跑过；现有证据为本地等价复现
  （`-race` 容器内 PASS、lint rc=0、`pnpm test:unit` 本机 PASS）。
- **真库测试宿主侧 1 条 SKIP**：`TestHandlerAllowsSymlinkedRoot` 在 Windows
  无管理员权限时 SKIP（`SeCreateSymbolicLink` 权限）；Linux 容器侧 SKIP=0。
  与真库无关，属平台权限可接受项。
- **容器链路真栈回归的后补状态**：T7.5 收口时本机 Docker daemon 不可用，
  compose 生产化与 TLS 只验到静态判据 + `compose config`；镜像构建与
  `verify-images.py` 全链路待 daemon 恢复后补跑（T7.5 报告已如实标注）。
- **`main.go` / `config.go` 超 300 行约束**（OPEN-M6-1，340 / 410 行）：
  自 M2 起存在，按既有策略只拆本里程碑新增部分，未做整体重构。
- **手册 M6 的 `v0.0-m6` tag 未打**：其交付报告明示「不宣布全部完成」
  （宿主 SKIP=1 + 上述 OPEN 项）；功能已随 v0.1.0 发布。

[Unreleased]: 本地 git 对比（无 remote，无可比对外链接）
[0.1.0]: 本地 tag `v0.1.0`（指向 T7.6 提交）
