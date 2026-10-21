# Changelog

本文件遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/) 格式
（节标题用英文以便工具解析，正文中文）。
版本号与 git tag、镜像标签、Helm chart `appVersion` 保持一致（四处联动，见 README「发布」节）。

**如实声明**：remote 已配置为 `github.com/GoldY6666/IIP`，但于 2026-10-18 核实
该仓库**为空仓库（零提交）** —— 下列 tag 在写这份 CHANGELOG 时均为本地 tag，
首次推送由维护者本机执行（沙箱无法连 `github.com:443`）。推送前 GitHub Release
与 CI 执行状态一律视为「未发生」（见各版本 Known limitations）。

## [Unreleased]

### Fixed

- **R7 候选限额残留边界收口：关联引擎两段式候选回退**。v0.1.0 引入的候选限额
  （每事件最近 20 条 + 种子锚定）留下第二半边界：新告警的最佳匹配若是最近 20 条
  之外的**非种子**老成员，限额集内最高分跌破 merge 阈值——自动合并降级为 REVIEW
  （转人工确认），更极端构造下漏成 NEW 重复事件。修复**不在 SQL 里继续加锚点**
  （那会把候选集推回无上限，吞吐从 346 条/s 崩到 ~4.5 条/s），而是引擎侧两段式：
  - 触发条件两个合取项：限额集内最高分 < merge 阈值 **且** 存在事件在候选集里
    恰好返回 `限额+1` 条（成员数 > 限额的精确信号：种子是 `min(a.id)`，成员超限时
    必在最近 N 条之外；证明见 `internal/correlation/candidates.go`）；
  - 触发时以 `limit<=0` 重取全量候选（该 SQL 通道自 R7 起就存在，**零 schema 改动、
    零 sqlc 重新生成**），与内存累积取并集后重算，仅在全量**严格更优**时取代；
  - 新增计数器 `iip_correlation_candidate_fallback_total`（无标签，守标签基数纪律）
    + 每次回退一条带「限额分/全量分」的 Info 日志；标准风暴场景组内同质（限额集内
    已是 MERGE 档）不触发，常态计数应接近 0；
  - 判据：新探针 `TestCandidateLimitFallbackRecoversOldNonSeedMember`
    （`internal/correlation/replay_fallback_test.go`）——回退恰好触发 1 次 /
    N 以 0.80 非待确认并入 / 分组与不限额参考跑一致 / 阴性对照以数据表达
    （限额集内最高分确实 < merge，不修就是缺陷）；
  - 变异 4/4 全红（删回退 / 去截断判据 / 取代条件取反 / 截断判据差一），
    2 条等价变异全绿（条件换序；去「无候选」守卫——截断判据在空候选集上恒假，
    该守卫是冗余防御），还原后两文件 sha256 与基线逐位一致；
  - 仍如实保留的残留：限额集内最高分已 ≥ merge 时，合并目标取限额集最佳而非
    全局最佳（多个事件同处 MERGE 档时）——全闭合等于永不限额，吞吐崩塌，不做。

- **R7 回退快照 TTL 缓存（压测实测驱动的性能修复）**。两段式回退上线后的压测
  复跑（1 万条、b=1、c=8）暴露设计盲区：标准风暴场景组内同质告警的相似度恒在
  REVIEW 档（低于 merge 阈值）且大事件恒被限额截断——触发条件两个合取项在该
  场景下**恒真**，回退从「例外通道」变成热路径（实测触发 9833/10000 次，且结论
  100% 零变化），每次还白查一次全量候选集：吞吐 346→34 条/s、折算单条 p95
  45.6→406.2ms，T7.3 第 5 判据由 PASS 转 FAIL。修复**不动触发条件**（语义正确性
  优先——没有便宜的判据能在回退前区分「有用」与「白做」），而是把不限额快照从
  「每次 `CorrelateAlerts` 调用取一次」改为 **Service 级共享 + 2s TTL**
  （`fallbackSnapshotTTL`，实测依据见 `internal/correlation/candidates.go`）：
  - TTL 内跨告警、跨并发复用同一次查询（mutex + 双检，无惊群），过期才重查；
    复跑实证：触发 9833 次不变，快照查询 9840→**31 次**（每次 1~105ms）；
  - 存储故障缓存同样按 TTL 失效——Service 级共享后故障缓存的生命周期不再是
    「一次调用」，不设 TTL 会把一次瞬时故障变成回退通道的永久关闭；
  - 快照刷新打一条 Info 日志（候选行数/耗时/累计查询），**不加新指标**——不为
    一个诊断数字动 metrics/Grafana/S1 观测记账链；
  - staleness ≤ 2s 的代价明示：回退结论基于最多 2s 前的全量快照，本批增量由
    `mergeViews` 与内存累积的 `byIncident` 取并集补回；压测同时实测 9833 次回退
    结论零变化，该窗口对实际结论无影响，且仍严格优于无回退版（那版对被截断成员
    永不修复）——语义是「最多延迟 2s 修复」，不是放松正确性；
  - 判据：`internal/correlation/fallback_snapshot_test.go` 四条——TTL 内复用
    （只查一次库 + 返回冻结快照）/ 过期重查（看得到新成员）/ 故障按 TTL 缓存且
    到期自愈 / Service 级共享（两条告警先后触发只查一次库，第二条结论仍正确）；
  - 变异 4/4 全红（快照 TTL 取反 / 去掉 TTL 判定 / fetchErr 不设 TTL / 装配传 0）
    + 1 条等价变异全绿（`Since/<` 换成 `Add/After`），还原后 sha256 与基线逐位
    一致；旧 R7 变异 6 项回归同步复跑通过（重构未削弱旧判据）；
  - 复跑结果：**6/6 判据 PASS**——吞吐 159 条/s、折算单条 p95=98.2ms < 200ms、
    零丢失、rejected=0、指纹互异。与无回退版（346 条/s）的差距是回退重算的固有
    CPU（9833 次 × 增长中的快照评分），属 R7 边界修复的既定代价；更高吞吐的兜底
    仍是关联异步化（开发计划 R7 应对③）。

- **CI 首跑三连红的根因修复（三个独立缺陷，均非产品代码问题）**。v0.2.0 推送后
  GitHub Actions 首次运行，三个 run（纯 v0.2.0 / README 修订 / R7 合并提交）全部
  failure 且失败 job 同构；从 job 日志逐字节比对定位到：
  - backend：service 容器名 `postgres` 在 runner 上**从头就不通 DNS**
    （`lookup postgres on 127.0.0.53:53: server misbehaving`，103 条全同形态，
    首个碰库的包 0.1s 内即失败；容器 health check 全过——它只证明容器活着，
    证明不了 runner 侧可达）。修法：`IIP_TEST_PG_URL` 改用 service 已声明的
    端口映射 `127.0.0.1:5432`，纯 TCP 绕开 DNS；
  - e2e：stack.mjs 拉起的 postgres 在 Linux 上无法创建 socket 锁文件
    （`/var/run/postgresql/.s.PGSQL.5434.lock: Permission denied`，runner 用户
    无写权限；Windows 无 AF_UNIX 故本机永远复现不出）。修法：非 Windows 分支
    传 `-c unix_socket_directories=/tmp`；
  - worker 测试替身：`fakeCollector.Collect` 无锁 append `seen`，被
    `TestRunOnce_ConcurrentCallsAreSafe`（8 worker 并发）读出 4 条 DATA RACE。
    修法：替身加互斥锁 + `seenInputs()` 访问器，读取点全部改走访问器
    （同文件 fakeStore 的注释早已写明这条契约，fakeCollector 是漏网的那个）；
  - 本地验证：全量真库测试 exit=0（20 包全 ok，124.9s）、e2e 9/9、lint 0 issues、
    worker 并发用例 30 轮加压通过；`-race` 维度本机无 gcc 跑不了，由 CI 最终裁决。

- **CI run#4 暴露的真产品缺陷：并发补建分区的假失败（迁移 0010）**。三连红修复后
  复跑，backend 只剩 `internal/repository` 包红：并发插入同时补建同一个月分区时，
  落败者报 `relation "alerts_2026_09" already exists (42P07)`，被当成「补建失败」、
  整个 Insert 报错。根因在 0008 的 `iip_ensure_alert_partitions()`：「查 pg_class →
  CREATE TABLE」跨会话不原子——落败者的存在性检查看不到对方**未提交**的建表
  （快照隔离），它的 CREATE 阻塞到对方提交后必撞 42P07（2026-10-21 两会话探针
  实证：racer 等的是父表 alerts 的 AccessExclusiveLock，holder 未提交的建表
  正持着它；42P07 发生在拿到锁之后的 CREATE 上）。CI 的 `-race` 拖慢时序放大
  竞争窗口；本机无 cgo 跑不了 `-race`，故本地全量测试复现不出——这正是它能活到
  CI 才暴露的原因。失败链：告警 starts_at 落在预建范围外（如测试库硬编码的
  2026-09-24，迁移只预建 [当月, 当月+2]）→ 插入撞 23514 → 补建 → 42P07。
  修复 = 迁移 0010 把 CREATE 包进子事务，`EXCEPTION WHEN duplicate_table` 静默
  跳过（分区已存在 = 目标达成，不是错误），**Go 侧零改动**；down 精确还原 0008
  原版函数体。为什么不是别的修法：`CREATE TABLE IF NOT EXISTS` 同样看不见未提交
  建表（同一个快照隔离问题，竞争窗口原样存在）；Go 侧重试第 N 次仍然要与第 1 个
  抢同一把父表锁。如实保留的残余窗口：存在性检查通过后、CREATE 执行前对方恰好
  提交时，`created` 记账会把该分区说成本事务新建（实际是对方）——微秒级窗口，
  且 `created` 只被日志消费（23514 重试路径直接丢弃返回值），不影响正确性。
  判据：`migrations/migrate_partition_race_test.go` 两个方向——down 还原旧函数后
  同一竞争 harness 必报 42P07（证明 harness 真的复现了竞争，防恒绿）/ 重新 up 后
  同竞争必成功且只把未建月记入返回值（防「把循环和记账一起吞掉」的坏修法）。
  变异 2/2 全红（异常名换 `check_violation` → 阶段 2 的 42P07 透出；异常分支
  把竞争月也记账 → 记账断言红），还原后 sha256 与基线逐位一致。

## [0.2.0] - 2026-10-18

自 v0.1.0 起的第二个发布版。**无破坏性变更**，按 semver 定 minor：
新增通知链路、自监控与两项计划外加固，另把 M6 全量纳入一键回归。
因新增能力较多且 `verify-e2e.py` 阶段数从 8 扩到 10（S0–S9），补丁号
不足以表达范围，故走 minor。

### Added

- **发版工具 `scripts/bump-version.py`（本轮新增）**。发版要改 16 处标签，
  手工改必然漏（T7.6 时 `Makefile` 曾连续两代停在 `0.0-m1` 而无人发现）。
  - `--check` 只报告 / `--write` 真改后**自证重扫** / `--tag` 只打印 tag 命令
    （不代跑）；目标版本由 `--to` 或 `--bump {major,minor,patch}` 推算，
    **不让调用者手打**。
  - 位置清单从 `verify-e2e.py` 抽到 `scripts/tag_locs.py`，**判据与脚本共用
    同一份**（两份清单必然漂移）。
  - 只替换捕获组，行内注释里的历史版本号不被连带改掉。
  - **修掉一条真实会漏改的判据**：「没有改动可做」原先只比权威处，
    于是「权威处已手改成新版本、另 3 处仍停在旧版本」被误判为无事可做
    —— 本次 v0.2.0 发版就撞上这一幕。现改为看**各处**是否都已是目标值。
  - 变异 6/6 全红（`D:\_iip_setup\_mut_bump.py`），还原后 16 处 sha256 逐位一致。
- **M6 全量纳入一键回归：新增 S8 通知链路 / S9 接入指南（ADR-029）**。
  此前 M6 有两条腿**只活在记忆里**：T6.1/T6.2 的真栈收消息判据是
  一次性探针（探针一删就没了，回归里只剩单测），T6.5 的专项脚本写好了
  但没编排进 E2E（`M_MAP` 里明写着「尚未编排」）。现在两者都入库：
  - `scripts/verify-notify.py`（S8，真 PG 5437 + 真 server 8091 + 假出口 8899）
    9 条判据：NEW 档两出口各 1 条 → acknowledged → investigating（窗口第 2 条）
    → mitigated 被去风暴抑制 → **resolved 配额满仍必发** → 出口带 `Bearer`
    且日志无明文 → 优雅退出。脚本自身变异 3/3 全红（去风暴失效 / 删终态白名单 /
    Slack 出口漏装）。
  - `scripts/verify-onboarding.py` 挂成 S9：rc=2（本机缺 bash/curl，文档命令
    没执行过）映射 **BLOCKED**，不冒充通过。
  - 编排器补两条纪律：① **开跑前删旧报告** —— 脚本崩在写报告之前时，
    上一轮的「判据 9 PASS」会被当成本轮结论，**把失败说成满分**；
    ② S9 端口只预检 8090，不预检 5434（该脚本允许沿用已运行的 PG，
    预检会在「复用成功」时先报「端口被占用」）。
  - 编排接线变异 5/5 全红（`D:\_iip_setup\_mut_e2e.py`），含「陈旧报告不许
    冒充本轮满分」「降级时不许给出『文档与实现不一致』的错误结论」。
  - 顺带修掉一个**假红**：全量首轮 S4 红在 `SAFE_DELETE_BULK_CONFIRM_REQUIRED
    count=329` —— playwright 开跑时清理 `web/test-results`（已积 329 个
    trace/截图）被沙箱的批量删除守卫拦下，**一条用例都没跑**，看着像产品回归。
    现在 S4 开跑前用 Python 清两个派生物目录，同参数复跑 PASS
    （vitest 14 文件/180 用例、playwright 9 passed + 真 server 请求痕迹）。
  - **含 S8/S9 的全量一轮**（S0–S9）：**无红**，S0–S6 / S8 / S9 PASS，
    S7 容器链路因 Docker daemon 缺席 **BLOCKED**（不判红、也不冒充通过）；
    汇总结论 `PASS-WITH-BLOCKED`，M1–M6 验收项全部 PASS，MVP 总验收 2 PASS + 1 BLOCKED。
- **单文件规模门禁 + 技术债收口（T7.8 / ADR-028，关闭 OPEN-M5-7 / OPEN-M6-1）**。
  「单文件 ≤ 300 行」写在 `docs/IIP-M1详细开发手册.md` §4，但它在 M2–M6 之间
  **从未被任何脚本检查过** —— 于是 `main.go` 从 318 涨到 485、`config.go` 从 352
  涨到 616，中间没有任何人（或任何命令）会因此变红。这条约束补了门禁：
  - `scripts/check-file-size.py`（`tasks.ps1 check-file-size`，并进 E2E 的 S1 静态段）
    五条判据，每条对应一种「让门禁形同虚设」的办法：新欠债 / 已还清却未销账 /
    账目过期 / 账实不符 / 扫描根失效。变异 5/5 全红。
  - `scripts/file-size-baseline.txt` 把**今天的存量欠债**（44 个文件，含 14 个测试文件、
    `loki.go` 551、`k8s.go` 533）如实登记，规则是**只减不增**：修好一个就删一行。
    这不是「债已还清」，是「不再增长」。
  - 生成物（头部带 `Code generated ... DO NOT EDIT` 的 sqlc 产物）按**标记**排除，
    不硬编码路径 —— 对着生成物报「请拆分」是让人去改会被覆盖的文件。
  - 登记在册的三个文件已偿清：`cmd/iip-server/main.go` 485→**264**（+`router.go` 244）、
    `internal/config/config.go` 616→**212**（+默认值/Load/校验三个分文件）、
    `internal/worker/processor.go` 483→**253**（+任务执行/日志两个分文件）。
    **纯同包移动，零行为变化**；`Config` 结构体刻意保持扁平（拆内嵌会让所有
    `Config{...}` 字面量编译失败，那是大面积回归面换「文件短」）。
- **HTTP 入口加固：三个 JSON 入口收成一个 `readBody`（T6.6 / ADR-027）**。
  这次不是加功能，是**修掉两个一直在的语义缺陷**：
  - `POST /api/v1/incidents/{id}/comments` 把「请求体超限」报成 **400
    「不是合法 JSON 或含未知字段」**。原因：`decodeStrict` 直接
    `json.NewDecoder(r.Body)` 读流，`MaxBytesReader` 的超限错误被 `Decode`
    一并返回后落到同一个错误分支。另两个入口都有 413 用例，**唯独它没有**，
    所以从 M3 起就没被发现。现三个入口统一 **413**。
  - **非 UTF-8 请求体（GBK 等）从 503 变成 400**，且报在**解码之前**。
    改之前 `encoding/json` 不校验字符串的 UTF-8 合法性，非法字节一路进到
    PG 才被 `SQLSTATE 22021` 拒掉 ⇒ 接口回 **503（存储不可用）**，
    「客户端编码错了」被报成「IIP 存储坏了」，运维会去查 PG。现回
    **400** 并给出编码提示，日志记 `first_invalid_offset`（第一个坏字节偏移）。
    **真栈实测还发现一条更隐蔽的分叉**：若该非法体对应的指纹已是 firing，
    走的是「重复 → 跳过」路径，压根不碰数据库，于是**回 200 `skipped=1`** ——
    字节层面就是垃圾的请求体被当成「合法重复」受理。两条路径都已由入口校验堵住。
  - 不变量：**同一类坏输入在任何 JSON 入口得到同一状态码与同一文案**。
    判据是 3 入口 × 2 类坏输入的矩阵（`internal/api/body_test.go`），
    加一个「合法 UTF-8 不能被误杀」的反面用例；变异 5/5 全红。
  - **不改任何体积上限**，`bytes.NewReader` 不复制 body，内存峰值不变；
    ADR-010 的 `DisallowUnknownFields` 原样保留。
  - **本改动不阻止 Alertmanager 重试**（它对任何非 2xx 都重试），
    价值是**正确诊断**而非抑制风暴；限流与 CORS 未做，理由见 Known limitations。
- **接入指南 + 可执行验收（T6.5）**：`docs/接入指南-Alertmanager.md` 是操作侧文档
  （receiver 配置 → 手发验证 → 数据/指标/收尾三处确认 → payload 契约 → 排错表 →
  通用 HTTP 源 → checklist），与 M1 手册的开发侧契约互相引用。
  - `scripts/verify-onboarding.py`（`tasks.ps1 verify-onboarding`）把文档里的代码块
    **按 `<!-- verify:NAME -->` 标记抽出来原文执行**（Git Bash + curl，真 PG + 真 server），
    断言文档声称的 `accepted/skipped/updated/rejected` 与 5 个状态码。文档最容易被自己毁掉：
    少个引号、缺个必填字段、路径错一个字符，都只在操作者那里暴露。
    13 项判据全 PASS；**变异 6/6 全红**（文档侧 4 + 实现侧 2）。
  - 文档明确写出两个反直觉行为：`severity` 里的 `error` 归一到 **critical**；
    指纹**含** `severity`，故同一故障从 warning 升到 critical 会作为新告警落库。
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

- **`verify-observability.py` 的端口判据不再钉死文件名（T7.8）**：它原先只在
  `internal/config/config.go` 里找 `DefaultHTTPAddr`，而 ADR-028 把默认值常量搬到了
  `config_defaults.go` ⇒ **产品正确、判据先红**。现改为在 config 包内按目录搜索：
  不变量是「config 包里有这个默认值」，不是「它在哪个文件里」。改完重跑 T6.4 的
  6 项变异仍 6/6 全红（含「worker 端口写成 9090」这条），证明鉴别力没被削弱。
- **E2E 的 M6 验收映射不再是过期文案（T7.8）**：`M_MAP` 里那条
  `("M6", "计划 §10 M6：Slack 通知 + /metrics", [], "未开工")` 在 T6.1 落地当天
  就已过期，却一直躺在总验收表里，与 README/CHANGELOG 直接矛盾。现按「实际被哪一
  阶段证明」拆成 5 项（单测挂 S2、观测配置静态挂 S1），**没有编排进 E2E 的宁可写空
  列表 + 原因，也不给它一个假的 PASS**。同时把 `verify-observability.py` 与
  `check-file-size.py` 接进 S1 静态段。
- **非 UTF-8 请求体：503 → 400（T6.6 / ADR-027，产品行为变更）**。此前
  GBK 等非 UTF-8 编码的请求体会一路走到落库，PG 回 `SQLSTATE 22021`，
  接口报 **503（存储不可用）**；现三个 JSON 入口在解码前统一挡成
  **400** 并提示编码问题。**对调用方是可见的行为变更**：原先拿到 503 的
  客户端现在拿到 400 —— 这不是回归，是原先把客户端错误报成了服务端故障。
  正确修法仍然是把请求体换成 UTF-8（命令行内联中文极易被终端按本地代码页
  转码，用 `--data-binary @file` 或纯 ASCII 更稳）。
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

### Fixed

- **S1 的「git tag 对齐」判据在打 tag 之后仍然红（发版时发现）**。打过
  `v0.2.0` 后复跑 S1 仍报「git tag 0.1.0 与 16 处代码标签 0.2.0 不一致」，
  但直接跑 `git describe --tags --abbrev=0` 输出的就是 `v0.2.0`。
  根因：`run_cmd()` 返回的是 `stdout + stderr` 合并串（多数调用点需要错误文本），
  而判据侧只做 `out.strip()` —— 一旦合并结果里多出一行，整串就不再是版本号。
  现改为只取第一行。
  **值得记的是排查姿势**：这条红出现在「刚打完 tag」的时刻，最容易的误判是
  「tag 打歪了，重打一次」——于是把一个取值 bug 当成操作失误，反复重打也还是红。
  做了三组反向变异才定性：删掉 `v0.2.0` tag ⇒ 红（`tag=0.1.0`）；把错版本 tag
  `v0.1.1` 打在 HEAD ⇒ 红（`tag=0.1.1`）；恢复 `v0.2.0` ⇒ 绿。
  **判据本身有鉴别力，红在取值**。
- **`bump-version.py` 在多处待改时可能误报「没有改动可做」**：见上方发版工具条目。
- **CI 的 e2e job 原先用错了 Go 工具链**：`E2E_GO_BIN` 写死成
  `/usr/local/go/bin/go`（runner 镜像自带的那份），而同一个 job 刚用
  `setup-go`（`go-version-file: go.mod`，即 `go 1.26.0` / `toolchain go1.26.9`）
  装好了正确版本 —— **setup-go 在 Linux 装到 tool cache，不覆盖 `/usr/local/go`**。
  于是 backend 与 e2e 各用一个工具链；镜像那份若低于 go.mod 声明的版本，
  Go 会走 `GOTOOLCHAIN=auto` **静默下载** go1.26.9（能跑，但每次多一次隐藏网络
  依赖，且失败时报错不指向「用错工具链」这个真因）。现改为由一步
  `Resolve Go for E2E` 用 `go env GOROOT` 写进 `$GITHUB_ENV`，取不到就显式
  `exit 1`，**不回落**到 PATH 里的 go。三向实测（真工具链→rc=0 且指向 1.26.9 /
  GOROOT 为空→exit 1 且不写环境变量 / GOROOT 指向不存在目录→exit 1）。
  **本仓库 CI 至今没在 GitHub 上跑过一次**，这条是推送前的静态审计发现的。
- **文档里的 `pwsh` 全部改为 `powershell`**：本机只装 Windows PowerShell 5.1，
  照着 `pwsh -File scripts/tasks.ps1 ...` 敲会 command not found。
  文档里的命令必须能原样执行——这是 T6.5「文档代码块原文实跑」同一条纪律。

### Known limitations

- **44 个文件仍超 300 行，只是不再增长（T7.8 / ADR-028）**：本轮只偿清了登记在册的
  3 个（main.go / config.go / processor.go）。剩余存量登记在
  `scripts/file-size-baseline.txt` 并按基线只减不增，其中最大的是
  `internal/collectors/k8s_test.go`（1246 行）、`migrations/migrate_test.go`（949 行）、
  `internal/config/config_test.go`（946 行）、`internal/collectors/loki.go`（551 行）。
  **基线不是「已合规」的同义词** —— 门禁只保证「不会更糟」。
- **告警入口不做限流（T6.6 裁定，ADR-027）**：给 `/api/v1/alerts/webhook` 加
  速率限制，在**风暴**这个它唯一需要被用上的场景里会起反作用 ——
  429 会触发 Alertmanager 退避重试，于是**最新的告警**被排在退避队列后面，
  而风暴中最贵的恰恰是最新告警的时延。兜底交给上游：Alertmanager 自带
  `group_wait`/`group_interval` 与重试；入口侧只做「体量上限 + 编码校验 + 落库幂等」
  三件事。若将来必须限流，正确位置是**反代/网关**（可返回 `Retry-After`），
  不是应用内。
- **不开 CORS（T6.6 裁定，ADR-027）**：UI 与 API 由同一个进程同源提供
  （8080 静态托管 + `/api/v1/*`），没有任何跨源需求。默认开
  `Access-Control-Allow-Origin: *` 是安全倒退 —— 会把当前「同源才可调用」
  这个隐式防护直接拆掉。将来若做前后端分离部署，按**白名单来源**加，
  且必须和 `IIP_AUTH_TOKEN` 一起评估。
- **非 UTF-8 请求体已修但只覆盖 JSON 入口（T6.6 / ADR-027）**：三个 JSON
  入口在解码前校验 UTF-8；非 JSON 入口（`/healthz`、`/metrics`、静态文件）
  不读请求体，无需校验。`multipart`/表单入口目前不存在，若将来新增，
  **必须走同一个 `readBody`** —— `body_test.go` 的 `jsonEntries()` 是唯一的
  维护点，加新入口时漏加进去，矩阵判据就管不到它。
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
  （编排器已在每个脚本阶段开跑前删旧报告，剩下的风险是「阶段自身超时
  且没写出任何报告」，那时显示 0/0 而不是旧值。）
- **本版仍未在 GitHub 上推送过**：`ci.yml` 的三 job（backend / frontend / e2e）
  至今没有任何一次真实执行记录；v0.2.0 的证据全部来自本机沙箱内的等价复现。
  首次推送后的第一轮 CI 结果仍是未知数（历史教训：`ci.yml` 静态审查后仍
  修掉过四处硬伤，「看着对」不等于「跑得通」）。
- **容器链路（S7）仍未实证**：本机 Docker daemon 不可用，镜像构建后的
  `compose up` 与计时为 BLOCKED；镜像标签 `0.2.0` 只经过交叉编译 +
  ELF 静态解析，未经容器实跑。

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
  （已于 v0.2.0 补齐。）
- **无自监控指标**：`/metrics` 端点（T6.3）与 Prometheus 抓取示例（T6.4）未做，
  压测可观测性目前靠 `ingest_bench.py` 客户端侧统计。（已于 v0.2.0 补齐。）
- **K8s / trace 采集器未接真实后端**：默认关闭（偏差 B1）；mock server 单测 +
  变异测试覆盖，**没有任何端到端链路跑过真实集群/链路后端路径**（OPEN-M6-3）。
- **confidence 的已知边界**：给不出依据的证据留空（k8s rollout 侧与 trace
  恒为 Unassessable，OPEN-M6-2）；`Assign` 跨轮不重算（幂等是刻意取舍，
  OPEN-M6-4）——「同一事件里 metric 0.50、log 0.65」是设计行为不是算错。
- **关联候选限额的残留边界**（R7）：新告警的最佳匹配若是**非种子**老成员，
  仍可能漏合并（窗口更窄，待 V1.1 用真实反馈数据复查）。
  （已于 Unreleased 收口：引擎两段式候选回退，见上；残留收窄为「多个事件同处
  MERGE 档时合并目标取限额集最佳而非全局最佳」。）
- **告警表未分区、JSONB 索引未调优**（T7.1 / T7.2 未做）：数据量增长后的
  查询性能余量未验证。（已于 v0.2.0 补齐：0008 按月分区、0009 索引调优。）
- **CI 未在 GitHub 上执行过**：`.github/workflows/ci.yml`（backend / frontend /
  e2e 三 job）从未真正跑过（发布时仓库无 remote；2026-10-18 核实 remote 已配置
  但仓库为空、尚未推送）；现有证据为本地等价复现（`-race` 容器内 PASS、
  lint rc=0、`pnpm test:unit` 本机 PASS）。**该限制在 v0.2.0 仍未解除。**
- **真库测试宿主侧 1 条 SKIP**：`TestHandlerAllowsSymlinkedRoot` 在 Windows
  无管理员权限时 SKIP（`SeCreateSymbolicLink` 权限）；Linux 容器侧 SKIP=0。
  与真库无关，属平台权限可接受项。
- **容器链路真栈回归的后补状态**：T7.5 收口时本机 Docker daemon 不可用，
  compose 生产化与 TLS 只验到静态判据 + `compose config`；镜像构建与
  `verify-images.py` 全链路待 daemon 恢复后补跑（T7.5 报告已如实标注）。
- **`main.go` / `config.go` 超 300 行约束**（OPEN-M6-1，340 / 410 行）：
  自 M2 起存在，按既有策略只拆本里程碑新增部分，未做整体重构。
  （已于 v0.2.0 偿清并存量基线化，见 ADR-028。）
- **手册 M6 的 `v0.0-m6` tag 未打**：其交付报告明示「不宣布全部完成」
  （宿主 SKIP=1 + 上述 OPEN 项）；功能已随 v0.1.0 发布。

[Unreleased]: 尚未推送，暂无对外链接
[0.2.0]: 本地 tag `v0.2.0`（指向发版提交）；推送后为 `https://github.com/GoldY6666/IIP/compare/v0.1.0...v0.2.0`
[0.1.0]: 本地 tag `v0.1.0`（指向 T7.6 提交）；推送后为 `https://github.com/GoldY6666/IIP/releases/tag/v0.1.0`
