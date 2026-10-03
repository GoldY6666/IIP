# IIP M4 详细开发手册（Evidence 自动收集）

> **状态：已由禹确认生效（2026-10-03）。** 本手册即M4 的唯一开发依据；
> 自确认起实现与手册不符即为缺陷，任何偏离须**先改手册再改代码**（沿用 M3 的纪律）。
>
> 体例对齐 `docs/IIP-M3详细开发手册.md`（11 章）。开发计划见 `docs/IIP开发计划.md` §8。

## 〇、M4 完成定义（DoD）

M4 完成的定义 = **同时满足以下六条**：

1. **任务队列可用**：`tasks` 表 + `FOR UPDATE SKIP LOCKED` 领取 + 指数退避重试（最大 3 次）+ 死信记录；多 worker 并行领取**同一批任务不得重复处理**（真库并发用例证明）。
2. **Collector 注册表可用**：按 `source` 可插拔，新增采集器不改既有代码（`Collect(incident, window) []Evidence` 接口）。
3. **三类采集器均有 mock server 单测**：Prometheus（RED 指标窗口对比）、Loki（LogQL 错误日志 Top10 + 突增）、K8s（Warning 事件 + rollout 记录）。
4. **证据自动落库**：Incident 创建/合并后 **2 分钟内**自动出现 ≥5 条证据，`query` 字段可复制复现。
5. **外部数据源宕机时告警主链路零阻塞**：webhook 延迟无变化、worker 只记 error 日志、恢复后自动补采（真库 + 真二进制实测）。
6. **质量达标**：`go build/vet/test` + `gofmt` 全绿；`golangci-lint` rc=0（逐项修复，不加排除规则、不用 `nolint`）；真库门禁 SKIP=0；容器链路断言 0 失败。

---

## 一、M3 交接物核对（开工前必读）

| 交接物 | 位置 | M4 的用途 |
|--------|------|-----------|
| `evidence` 表 | `migrations/0001_init.up.sql` | **已建好、零写入**。M4 第一次写它。约束见 §二 的 E1 |
| worker 骨架 | `cmd/iip-worker/main.go` | polling 循环 / 心跳 / drain / panic 恢复齐备，`runOnce` 里留了 M4 注释钩子 |
| `evidence_found` 事件类型 | `0004` 的 `chk_timeline_event_type` | 已含此值，M3 未写；M4 写证据时**顺带**写 timeline |
| 配置项 `WorkerPoll` / `DBPollOnly` | `internal/config/config.go` | M4 在此加队列与采集器的开关（§4.6） |
| 仓储接口模式 | 接口定义在**消费方**包（`correlation.IncidentStore`） | M4 沿用：`task.Store` 定在 `internal/task`，实现放 `internal/repository` |
| sqlc 查询 | `internal/db/queries/*.sql` | 新增查询一律「先写 .sql → `sqlc generate`」，生成物入库 |
| 前端证据 Tab | `web/src/pages/IncidentDetail.tsx` | **S3 已就位**，空态文案写明「M4 尚未接入」；M4 换成真实内容 |
| 端到端链路 | `scripts/verify-func.py` | M4 的业务闭环在其后追加（§九） |

---

## 二、与当前仓库实测状态的对齐表（2026-10-03 实测）

| # | 计划假设 | 实测 | 处置 |
|---|---------|------|------|
| **E1** | `evidence` 表需M4 新建 | **0001 已建好**，含 `chk_evidence_type`（6 值）、`chk_evidence_confidence`（0–1）、3 个索引 | **不改表结构**。只加索引（见 §五） |
| **E2** | K8s 可采集 Warning 事件 | **仓库零 K8s 依赖**：compose 无 k8s 服务、无 kubeconfig、无 RBAC 清单 | **偏差 B1** → 见 §三 |
| **E3** | 队列用 Redis 或 DB polling 二选一 | `DBPollOnly=true` 已实现（V1.0 §2.2 允许 DB polling 替代 Redis） | **沿用 DB polling**，不引 Redis 依赖 |
| **E4** | `tasks` 表需新建 | **不存在** | 新建迁移 `0005`（不改 0001–0004，AGENTS.md 约束 10 冻结） |
| **E5** | `evidence.timestamp` 语义 | 注释明写「证据时间来自采集源，**不用 NOW() 兜底**」（ADR-003） | 采集器**必须给真实时间**；给不出就**跳过该条证据**，不写 NOW() |
| **E6** | 证据会被前端展示 | S3 已交付证据 Tab，空态文案指向 M4 | M4 只需换数据源，**不改前端**（除空态文案） |

---

## 三、范围

### 做什么

| 任务 | 内容 | 计划工时 |
|------|------|---------|
| T4.1 | Worker 任务框架：`tasks` 表 + SKIP LOCKED 领取 + 指数退避（最大 3 次）+ 死信记录 | 1.5d |
| T4.2 | Collector 注册表接口：`Collect(incident, window) []Evidence`，按 `source` 可插拔 | 1d |
| T4.3 | PrometheusCollector：按 `service` label 查 RED 指标，窗口前后对比，存 query + value + 变化幅度；超时 5s、失败降级不阻塞 | 2d |
| T4.4 | LokiCollector：LogQL `label_match`，错误级别日志 Top 10 + 突增统计 | 1.5d |
| T4.5 | K8sCollector：见 §三的偏差处理 | 1.5d |
| T4.6 | Evidence 查询 API（按 incident 聚合 + 按类型过滤）+ 采集预算（单 incident 最多 50 条，超限截断） | 0.5d |

### 偏差 B1：K8sCollector 的处置（禹2026-10-03 裁决）

计划要求「list 相关 namespace 近 1h 的 Warning 事件 + Deployment rollout 记录」，
但**本仓库没有任何 K8s 依赖**（E2）。三个选项里禹选了「**只做客户端 + mock server 单测，
接入留配置开关且默认关闭**」：

- 实现完整的 K8s 客户端（`/api/v1/namespaces/{ns}/events` + `apis/apps/v1/deployments` rollout 读取）
  与解析逻辑，**mock server 单测全覆盖**；
- 接入由 `IIP_EVIDENCE_K8S_ENABLED` 控制，**默认 false**——默认部署下该采集器不注册，
  worker 日志会明确打出「k8s 采集器已禁用」；
- 因此**M4 的验收标准不包含「真实集群的 Warning 事件」**，只要求「mock server 下正确」。

理由：写一个接不上任何真实集群的采集器，比不写更危险——它看起来在工作，
但永远采不到东西。要接真实集群是**部署侧的事**（用户自带集群 + kubeconfig）。

### 不做什么

- **不引 Redis**：E3 已确认 DB polling 够用。
- **不改 `evidence` 表结构**：E1。
- **不做证据的 RCA 生成**：属 M5（`evidence → 根因假设` 是 V1.0 §1.1 的第三段链路）。
- **不做 trace 类型证据**：`chk_evidence_type` 有 `trace` 但本期无采集器，
  不用「先插一条占位 trace」来凑够 ≥5 条。

---

## 四、详细设计

### 4.1 `tasks` 表与队列语义（��4.1）

**表结构**（迁移 `0005`，见 §五）：

| 列 | 类型 | 说明 |
|----|------|------|
| `id` | BIGSERIAL PK | |
| `kind` | VARCHAR(32) NOT NULL | 任务类型，M4 只有 `collect_evidence` |
| `payload` | JSONB NOT NULL |任务参数（如 `{"incident_id": 12}`） |
| `status` | VARCHAR(16) NOT NULL | `pending` / `in_flight` / `done` / `dead` |
| `attempts` | INT NOT NULL DEFAULT 0 | 已尝试次数，**每次领取 +1** |
| `available_at` | TIMESTAMPTZ NOT NULL DEFAULT NOW() | 可被领取的最早时刻（退避靠它） |
| `claimed_at` | TIMESTAMPTZ NULL | 最近一次被领取的时刻（判超时用） |
| `claimed_by` | VARCHAR(64) NULL | 领取者标识（worker 实例） |
| `last_error` | TEXT NULL | 最近一次失败原因 |
| `created_at` / `updated_at` | TIMESTAMPTZ | |

**关键设计决策**（每条都有理由，不是默认值抄来的）：

1. **领取用 `FOR UPDATE SKIP LOCKED`**：多个 worker 同时跑时，各自取到**不重叠**的行，
   不互相阻塞。这是 T4.1 的核心断言，必须有并发真库用例证明。

2. **`available_at` 而不是 `sleep` 实现退避**：退避靠「这个时刻才可见」表达，
   worker 不需要为失败任务保持内存计时器。进程重启也不丢退避状态。

3. **退避是指数的，但封顶**：`delay = base * 2^(attempts-1)`，`base=5s`，
   上限 **5 分钟**。封顶的理由：第 3 次失败后还要等 `5*2^2=20s`、`5*2^3=40s`…，
   不封顶会让第 10 次失败的任务排到几小时后才被看见，而那时人早忘了这回事。
   **最大 3 次**（计划规定）→ 3 次失败进 `dead`，**不删行**：删掉就看不出「曾经试过」。

4. **`in_flight` 超时要能回收**：worker 领到任务后崩溃，任务会永远停在 `in_flight`。
   因此 `claimed_at` 有回收规则：**超过 `stale_after`（默认 5 分钟）仍 `in_flight` 的行
   视为可再领**（回收时 `attempts` 照常+1，让它最终能进死信，不会无限重试）。

5. **`claimed_by` 用实例标识**（如 `hostname-pid`）：出问题时能从库里看出
   「这批任务被谁领走过」，比只有时间戳有用得多。

### 4.2 采集预算（T4.6 的「单 incident 最多 50 条」）

超限**截断并在 metadata 里标记**：

```json
{"truncated": true, "total_collected": 73, "kept": 50}
```

理由与 M3 的 `truncated` 一致：把「只保留了 50 条」说成「一共 50 条」，
用户会以为没有更多证据。**截断数必须能被API 读到**，否则这个上限等于悄悄丢数据。

**修订（2026-10-03，B7 实现时补明）**：本节原只规定了「超限截断并标记」，
但实现时发现有三件事未规定，而它们每一件都能让截断被**静默吞掉**
（页面显示正常、实际少数据）。现明确如下：

1. **排序依据是 `created_at DESC, id DESC`，不是 `timestamp DESC`。**
   证据列表的用途是「展示该事件最新的证据」，
   而 `created_at` 是「我们什么时候采到的」。
   用 `timestamp` 排的话，补采一条三小时前的日志会沉到列表底部，
   用户以为没采到。`id DESC` 收尾是为了同秒落库的证据有确定次序 ——
   没有它，顺序随执行计划变化，页面列表会在刷新间跳动。

2. **真计数来自 `count(*)`，不能用 `len(items)` 推算。**
   这是 M3 的同一个教训（详情页 `AlertCount` 取 `len(alerts)`，
   250 条显示成「200 条关联告警」）。
   本期实现上，`List` 与 `Count` 是两次查询：列表多取一条（`limit+1`）
   用来探测是否超限，计数单独查。
   少那一行探测的话，「正好等于上限」与「超了一点」返回的条数完全一样。

3. **截断判定必须按**过滤后**的集合算，不能用全量总数。**
   `?type=metric` 在该事件只有 40 条 metric、而全量 80 条时，
   页面必须显示「未截断」。若拿全量总数判，会在明明没截断时提示已截断。

配套迁移 `0006`（**纯索引，非破坏性**）：`idx_evidence_incident_created
(incident_id, created_at DESC, id DESC)`。0005 建的
`idx_evidence_incident_type(incident_id, type)` 服务的是「组合过滤」，
**不含排序列**，用不上它做排序 —— 证据表是 M4 里唯一随时间单调增长的表，
排序列缺失的代价随数据量线性上升。列顺序里 `id DESC` 不是冗余：
`created_at` 精度到微秒，同批采集仍可能撞在一起。

**修订（2026-10-03，B8 实现时补明）**：本节此前只规定了「一次采集窗口内保留 50 条」，
但**同一事件会被多次采集**（新告警合入触发新一轮，见 §4.7），
而「多次」必然带来重复与覆盖顺序两个问题。补明如下：

4. **重复采集靠窗口游标，不靠去重。** 同一批 RED 指标会以完全相同的
   `(source, query, timestamp)` 被采到两次。按内容去重有两个问题：
   一是 `evidence` 表**没有** `(incident_id, source, query, timestamp)` 唯一约束，
   去重要么加约束（改表结构，违反 E1）要么每次落库前查一次（写放大）；
   二是「去重键」本身是业务判断——两个 Prometheus 分片都返回了该样本时，
   去重会静默丢数据。**游标方案让第二轮压根不采已覆盖的区间**，从源头避免，
   而不是采完了再扔。

5. **游标存在 `tasks.window_end`（迁移 0007），不在 payload 里。**
   payload 是**入队输入**，游标是**执行结果** —— worker 改写调用方写的输入不洁；
   且 `EnqueueTaskIfAbsent` 的判重条件会连带受影响。
   也不能从 `evidence` 表取 `max(timestamp)`：那是「证据自身时刻」，
   与「窗口终点」不是一回事，且**采到 0 条证据时游标不推进**，
   同一个空窗口会被下一轮重采一遍直到进死信。
   `LastCollectWindowEnd` **只认 `status='done'`**：pending/in_flight 还没采完，
   dead 是采失败了 —— 拿它们的游标会让下一轮跳过还没采过的区间。

6. **即使采到 0 条证据也要推进游标。** 「这段时间没有异常」是完全正常的结论，
   若因此不推进游标，下一轮会从同一起点重采，每轮白白消耗一次 attempts。
   配套地，`Complete` 传零值时**不写**游标（写成 0001-01-01 会让下一轮从 1970 年开始）。

### 4.2b 采集窗口的推导（2026-10-03 补明）

本节与 §4.2 的「窗口」是两件事：§4.2 说**一次采多少条**，
本节说**这段时间采哪一段**。B8 实现时发现三条规则都未规定，
且它们互相耦合（改一条会连带影响另两条），现明确如下。
实现见 `internal/evidence/window.go` 的 `ResolveWindow`（纯函数，可完整单测）。

| 情形 | 窗口 | 理由 |
|------|------|------|
| 已有游标 | `[游标, now]` | 只采新增区间，天然不重复 |
| 首次采集 | `[事故起点 − 1h, now]` | 见下 |
| 拿不到事故起点 | `[now − 1h, now]` | 退化但不报错 |
| 回溯超过 24h | 夹到 `now − 24h` | 护栏，见下 |

**首轮为什么允许超过 1h 宽度，而不受 `IIP_EVIDENCE_WINDOW` 约束。**
本手册原以为首轮窗口是 `[max(事故起点−1h, now−1h), now]`——**这个 max 在数学上
永远取不到「事故起点−1h」**：它胜出的充要条件是事故开始时间晚于 `now`，
即事故还没开始。后果是「3 小时前才发现的事故」的首轮采集窗口是 `[now−1h, now]`，
**完全采不到事故时刻**；而游标只往前走、不会回头补，那段数据**永久丢失**。
用户看到的是「指标一直很平稳」，与事实相反。

`IIP_EVIDENCE_WINDOW` 封顶的目的是防「反复采集时查询越拖越长」，
而首轮是一次性回补。**「首轮多花一次长查询」与「永久看不到事故何时开始劣化」
相比，前者是可控的成本，后者是产品缺陷。** 故首轮不封顶，
只在 `incidents.started_at` 不可信（回溯超过 24h）时用 `maxLookback` 夹住 ——
该列来自告警源提供的 `startsAt`，是外部输入，
一个填成 1970 年的值会让首次采集拉出 56 年的区间。

**事故起点前提前 1h 而不是对齐。** 根因往往早于第一条告警：
连接池 10:00 开始变慢，10:05 才触发「响应超时」告警。
只从 10:05 开始采，就看不到指标开始劣化的那段爬升过程，
而「什么时候开始变的」正是判断根因时最先要回答的问题。

### 4.2c 入队时机与幂等（2026-10-03 补明）

| 时机 | 入队 | 理由 |
|------|------|------|
| 事件创建（NEW 档） | 是 | DoD 第 4 条「2 分钟内≥5 条证据」的起点 |
| 告警合入（MERGE / REVIEW 档） | 是 | 合入往往正是「范围扩大、该重新看指标」的时刻 |
| resolved 告警 | 否 | 其生命周期归状态机，不参与关联 |
| 关联引擎关闭 | 否 | 行为退回 M1，无事件产生 |

**入队挂 `EnqueueIfAbsent` 幂等**：同一事件已有 pending/in_flight 任务时不重复插。
因此关联路径可以对每次合入连续调用，队列不会膨胀。

**入队失败绝不能让关联失败。** 关联发生在 webhook 请求内，
失败会让 handler 返回错误 → Alertmanager 判定投递失败 → **重投整批告警** → 放大成风暴。
入队失败的代价只是「这次合入暂时没有证据」，而证据会在下一轮由
超时回收 + 重试补上。实现与 M3 的 timeline / suppressions 完全同构
（`internal/correlation/evidence.go`，可选钩子 + 失败只记日志）。

### 4.3 Collector 接口（T4.2）

```go
// Collector 采集一类证据。实现必须**无状态且并发安全**：
// 同一实例会被多个 worker goroutine 同时调用。
type Collector interface {
    // Source 返回证据来源标识，落库到 evidence.source。
    Source() string
    // Collect 采集 incident 在 window 窗口内的证据。
    // 返回空切片表示「采到了但无内容」；返回错误表示「采集本身失败」。
    Collect(ctx context.Context, in Input) ([]Evidence, error)
}
```

**注册表**按 `Source()` 建索引，新增采集器只改注册表一处。
**关键约束：`Collect` 失败不得让整批任务失败**——三类数据源任一挂掉，
其余两类仍要落库。因此执行层是「逐采集器独立调用，错误收集起来，
最后若有全失败才算任务失败」。

### 4.4 采集器的时间语义（E5 的强制）

`evidence.timestamp` 是**证据自身的时间**（如 Prometheus 样本的时间戳、
K8s 事件的发生时刻），**不是采集时刻**。理由：Evidence-First 原则要求每条结论
可追溯到原始数据，用 `NOW()` 填满会让所有证据看起来「同时发生」，
而事件真相往往横跨一段时间。

**采不到真实时间的证据一律不写**，不得用 `NOW()` 兜底。

### 4.5 各采集器的降级行为

| 采集器 | 超时 | 失败降级 | 降级后 |
|--------|------|----------|--------|
| Prometheus | 5s | 记 error 日志 + 任务重试 | 其余采集器照常落库 |
| Loki | 5s | 同上 | 同上 |
| K8s | 5s | 同上 | 同上（默认已禁用） |

**告警主链路零阻塞**的验证方式：外部数据源宕机时，
webhook 的 P99 延迟与数据源正常时**无统计差异**（真库 + 真二进制实测，
不是靠读代码断言）。

### 4.6 新增配置项

| 变量 | 默认 | 作用 |
|------|------|------|
| `IIP_WORKER_TASK_BATCH` | 10 | 单次轮询最多领取几条 |
| `IIP_WORKER_TASK_STALE_AFTER` | 5m | `in_flight` 多久算超时可回收 |
| `IIP_WORKER_MAX_ATTEMPTS` | 3 | 超过即进死信 |
| `IIP_EVIDENCE_PROM_ENABLED` | true | Prometheus 采集器 |
| `IIP_EVIDENCE_LOKI_ENABLED` | true | Loki 采集器 |
| `IIP_EVIDENCE_K8S_ENABLED` | **false** | K8s 采集器（B1 的默认关闭） |
| `IIP_EVIDENCE_MAX_PER_INCIDENT` | 50 | 单事件证据上限（§4.2） |
| `IIP_EVIDENCE_WINDOW` | 1h | 采集窗口 |

配套的连接地址（`IIP_PROM_URL` / `IIP_LOKI_URL` / `IIP_K8S_URL`）无默认值。

**修订（2026-10-03，B4 实现时发现）**：本节原定「启用即校验地址」，
但与上表「Prometheus/Loki 默认 true」**相斥**——默认部署根本没配这两个地址，
按原设计会在启动即校验失败。取舍如下：

- **采集器开关默认仍为 true**（不改上表），但**地址校验改为「仅在显式配置了
  开关变量时才要求地址」**：命令行flag 或环境变量里出现了
  `evidence-prom-enabled` / `evidence-loki-enabled`（不论真假），才要求对应地址。
- 未配置开关变量时走默认「启用」，而因未配地址，**装配阶段不注册该采集器**，
  worker 日志明确打出「prometheus 采集器未配置地址，已跳过注册」。

理由：M4 的验收标准是「事件创建后 2 分钟内≥5 条证据」，
若为了凑默认值而让**默认部署直接起不来**，那是拿可用性换形式。
「说不清就不说」优先于「默认值好看」——采集器缺席时日志说清楚，
而不是让整个 worker 拒绝启动。

---

## 五、迁移变更清单（AGENTS.md 约束 4）

| 迁移 | 方向 | 内容 | 破坏性 |
|------|------|------|--------|
| `0005_evidence_tasks_m4` | up | 建 `tasks` 表 + `idx_tasks_claim` | 无 |
| `0005_evidence_tasks_m4` | up | `idx_evidence_incident_type (incident_id, type)` | 无（纯索引） |
| `0006_evidence_budget_idx` | up | `idx_evidence_incident_created (incident_id, created_at DESC, id DESC)` | 无（纯索引） |
| `0007_task_window_cursor` | up | `tasks.window_end`（**1 列**，采集窗口游标，见 §4.2 修订第 5 条） | 无（可空新列） |
| `0005_evidence_tasks_m4` | down | `DROP TABLE tasks` | **是**——全部任务记录（含死信）丢失 |
| `0006_evidence_budget_idx` | down | `DROP INDEX idx_evidence_incident_created` | **否**——纯索引回滚，不丢数据 |
| `0007_task_window_cursor` | down | `DROP COLUMN window_end` | **是（行为性）**——游标丢失后「只采增量」退回「从事故起点重采」，即本迁移要解决的问题 |

- **不改 `evidence` 表结构**（E1）。
- **不改 0001–0004**（约束 10 冻结）。
- down 的破坏性写在文件头（沿用 0004 的做法）。
- `0005` 只新增，**不修改任何既有 CHECK 约束**。

---

## 六、任务拆解（8 天）

| 批次 | 内容 | 依赖 |
|------|------|------|
| **B1** | `0005` 迁移 + `task` 包（`Store` 接口 + 退避计算纯函数 + 单测） | — |
| **B2** | 仓储实现（SKIP LOCKED 领取 / 成功 / 失败重试 / 死信 / 超时回收）+ **真库并发用例** | B1 |
| **B3** | `evidence` 包（`Input` / `Evidence` 结构体 + Collector 接口 + 注册表 + mock 单测） | B1 |
| **B4** | PrometheusCollector（RED 指标窗口对比 + mock server 单测） | B3 |
| **B5** | LokiCollector（LogQL + 突增统计 + mock server 单测） | B3 |
| **B6** | K8sCollector（B1：客户端 + mock server 单测，默认禁用） | B3 |
| **B7** | Evidence 仓储（落库 + 按事件聚合查询 + 50 条预算截断）+ 查询 API | B2 B4 |
| **B8** | worker 装配（`runOnce` 换成领取+ 执行）+ 门禁 + 端到端 + 报告 | 全部 |

---

## 七、测试策略

| 层 | 范围 | 手段 |
|----|------|------|
| 纯函数 | 退避计算、`dead` 判定、超时判定 | 表驱动，无需 DB |
| 仓储（真库） | SKIP LOCKED 不重复领取、指数退避可见性、死信、超时回收 | `testsupport.NewDB` |
| 编排单测 | 「三采集器中一个失败仍落库另两个」 | 假 Collector 注入 |
| 采集器单测 | 三类各自的解析与降级 | **`httptest` mock server**（计划明确要求） |
| 集成（真库） | 事件创建 → 任务入队 → worker 采到 → 证据落库 → API 读出 | 真 PG + 真二进制 |
| 容器链路 | 断言数从 41 起步（新增项见 §八） | `scripts/verify-images.py` |

**必测的边界**（每条都对应一个「不测就会出错」的场景）：

- 两个 worker 并发领取 10 条任务 → **每条恰好被领一次**（不多不少）。
- 第 1 次失败 → `available_at = now + 5s`；第 2 次 → `+10s`；第 3 次 → **进死信**。
- worker 领完崩溃（`claimed_at` 设为过去）→ **超时后能被别人再领**，且 `attempts` 继续累加。
- 退避封顶：即便把 `attempts` 人为设大，`available_at` 也不超过 `now + 5min`。
- 采集器返回空切片 → 任务**成功**（不是失败）：「采到了但没内容」是正常结果。
- 三个采集器**全部**失败 → 任务失败并重试（不是静默成功）。

---

## 八、验收清单（全过才算 M4 完成）

| # | 判据 | 手段 |
|---|------|------|
| 1 | 事件创建/合并后 2 分钟内自动出现 **≥5 条证据** | `verify-func` 追加断言 |
| 2 | 证据的 `query` 字段**可复制后复现**（人工验证 + 文档记录） | 人工 + `verify-func` 断言非空 |
| 3 | **webhook 延迟在数据源宕机时无变化**（零阻塞） | 真库实测前后 P99 对比 |
| 4 | worker 只记 error 日志、**恢复后自动补采** | 停/启 mock server 两轮 |
| 5 | 三类采集器**均有 mock server 单测** | `go test` 覆盖率 |
| 6 | 并发领取不重复 | 真库并发用例（§七） |
| 7 | 死信与超时回收 | 真库用例 |
| 8 | 证据超 50 条时截断**且 API 能读到截断标记** | 真库用例 |
| 9 | `0005` up→down→up 三连，破坏性已列 | 真库迁移用例 |
| 10 | 门禁：build/vet/test/gofmt/lint 全绿，真库 SKIP=0，容器链路 0 失败 | 全部 |

---

## 九、端到端验证脚本

在 `scripts/verify-func.py` 后追加 M4 段（而非新建脚本 —— 同一份造数
已能产生事件，重复造数只会让断言互相干扰）：

1. 投递告警 → 等事件创建 → 断言 `tasks` 表出现 `collect_evidence` 行；
2. 等 worker 领取 → 断言 `evidence` 表 ≥5 行、`query` 非空；
3. 停 mock Prometheus → 投递新告警 → 断言 webhook 延迟无变化、任务重试计数递增；
4. 起 mock → 断言补采成功、`tasks` 无残留 `pending`；
5. 断言单事件证据 ≤50 条且 metadata 带截断标记。

---

## 十、风险与开放项

| 风险 | 缓解 |
|------|------|
| 数据源不可用导致证据长期缺失 | 前端空态说明原因；worker 日志汇总每小时缺失数 |
| 采集放大（一次事件几百条证据） | §4.2 的 50 条硬上限 + 截断标记 |
| `in_flight` 泄漏（worker 崩溃） | §4.1 决策 4 的超时回收 |
| K8s 采集器接不上真实集群 | B1 已把这点写进 DoD，**不假装它能工作**；默认禁用 |
| 退避把长尾任务推到很久以后 | 指数退避**封顶 5 分钟** |

**OPEN-M4-1**：`evidence` 的 `confidence`（0–1，对假设的支撑强度）本期**无采集器填写**。
留空而非填 1.0——填 1.0 等于宣称「这条证据确凿」，而没有依据。RCA（M5）才有能力评估它。

**OPEN-M4-2**：`trace` 类型证据无采集器（T3 类型里有，本期无实现）。

**OPEN-M4-3**：前端自动化测试仍延到 M5（ADR-016）。M4 只改证据 Tab 的数据源，
不改其结构，**不新增前端测试**。

---

## 十一、变更记录

| 日期 | 变更内容 | 原因 | 影响范围 |
|------|----------|------|---------|
| 2026-10-03 | 初稿（草案）。§二 记录 6 项与实测的偏差（E1–E6）；§三 记录偏差 **B1**（K8sCollector 只做客户端 + mock server 单测、默认关闭）与三项裁决 | M4 开工前对齐仓库现状；K8s 无依赖需如实降级 | 全手册 |
| 2026-10-03 | 禹确认生效，状态由「草案」改为「已确认」。B1–B3 实现中发现的三处SQL 类型推断缺陷已就地修正并加注（见 `internal/db/queries/tasks.sql` 对应注释），**不改变本手册的设计规定** | 手册既定设计（SKIP LOCKED、指数退避封顶、死信保留行、超时回收）在实现中原样落地；缺陷都在 SQL 表达层 | 状态行；§4.1 无实质变更 |
| 2026-10-03 | B4 实现时发现 §4.6「启用即校验地址」与同表「Prometheus/Loki 默认 true」**相斥**，已修订（见 §4.6 的「修订」段） | 默认部署根本没配这两个地址，按原设计启动即失败。而 M4 的验收标准是「2 分钟内≥5 条证据」，为了让默认值好看而让默认部署起不来是拿可用性换形式 | §4.6 |
| 2026-10-03 | B7 新增 `0006` 迁移（纯索引）并把 §4.2 的预算语义落成可断言的契约，见 §4.2 的「修订」段 | §4.2 原只说「超限截断并标记」，未规定**排序依据**、**真计数来源**与**过滤后的截断判定**三件事。三者都会让「截断」被静默吞掉 | §4.2；§五 |
| 2026-10-03 | B8 补明 §4.2 修订第 4–6 条（**窗口游标**机制）、新增 §4.2b（窗口推导三条规则）与 §4.2c（入队时机与幂等），并新增 `0007` 迁移 | ① 原设计未考虑「同一事件被多次采集」，重复证据会无声膨胀；② 实现时发现首轮窗口的 `max(事故起点−1h, now−1h)` **在数学上永远取不到前者**（见 §4.2b），会让长事故永远采不到事故起点且游标不会回头补 | §4.2；新增 §4.2b / §4.2c；§五 |