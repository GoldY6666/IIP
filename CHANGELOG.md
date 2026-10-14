# Changelog

本文件遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/) 格式
（节标题用英文以便工具解析，正文中文）。
版本号与 git tag、镜像标签、Helm chart `appVersion` 保持一致（四处联动，见 README「发布」节）。

**如实声明**：本仓库当前无 git remote，下列 tag 均为本地 tag；
GitHub Release 与 CI 执行待 remote 就绪后补（见 v0.1.0 的 Known limitations）。

## [Unreleased]

（空）

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
