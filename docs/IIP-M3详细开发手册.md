# IIP M3 详细开发手册：Incident 生命周期与事件页面

**版本**：V1.0（草案，待确认）
**日期**：2026-10-01
**编制依据**：`docs/IIP开发计划.md` §7（M3 任务清单 T3.1–T3.4）+ `docs/IIP-V1.0-技术设计文档.md` §6（Incident 状态机）
**前置**：M2 已完成并端到端验证（`README.md`「M2 验收状态」全 PASS，容器链路 32 项断言 0 失败）
**本文性质**：**M3 的唯一开发依据**。范围、状态机、事件类型、API、迁移与验收以本文为准。经确认后状态改为「已确认」，此后任何偏离须走变更流程。

> **与仓库实测状态的四项偏差已在本手册内修正**（记为 A1–A4，见 §二）。这四项都源自「M3 计划文本与 0001/0003 实际 schema 不一致」，不是实现缺陷。

---

## 〇、M3 完成定义（DoD）

M3 完成的定义 = **同时满足以下六条**：

1. **状态机可用**：显式转换白名单实现（§4.1），非法转换返回 422；V1.0 §6.2 被截断的三条转换（RESOLVED→OPEN / MITIGATED→RESOLVED / INVESTIGATING→IGNORED）已补齐并逐一有测试。
2. **Timeline 自动留痕**：状态流转、告警合入、用户操作三类事件自动写入 `timeline` 表（这是 M2 遗留的空白——该表此前**零写入方**）。
3. **查询可用**：事件列表（status / severity / service / 时间范围过滤 + 分页）与详情（事件本体 + 关联告警 + timeline 聚合）均可用。
4. **处置闭环**：前端可从列表进入详情并完成「认领 / 开始调查 / 缓解 / 解决 / 忽略 / 重开」六种处置，带备注。
5. **迁移可回滚**：`0004` up→down→up 三连通过，且 down 的数据影响已如实列出（§五）。
6. **质量达标**：`go build/vet/test` + `gofmt` 全绿；`golangci-lint` rc=0（逐项修复，不加排除规则、不用 nolint）；真库门禁跳过用例数 = 0；`go test -race` 容器内等价复现通过。

---

## 一、M2 交接物核对（开工前必读）

| 交接物 | 位置 | 用途 |
|--------|------|------|
| 关联引擎 | `internal/correlation/`（8 实现 + 8 测试，823 / 1654 行） | M3 需要**改它的写入路径**以补写 timeline |
| 事件仓储 | `internal/repository/incidents.go` | 已实现 CreateIncident / ListOpenIncidentAlerts / LinkAlertToIncident / SetAlertIncident；M3 在其上扩展 |
| 仓储接口模式 | `correlation.IncidentStore`（接口定义在**消费方**包） | M3 的 `incident.Service` 沿用同一模式：接口在 `internal/incident`，实现放 `internal/repository` |
| sqlc 查询 | `internal/db/queries/{alerts,incidents,correlation_rules}.sql` | 新增查询一律「先写 .sql → `sqlc generate`」，生成物入库 |
| 路由装配 | `cmd/iip-server/main.go` `newRouter()` | 已留 M3 占位注释（`// M3: r.Get("/api/v1/incidents", ...)`） |
| 错误体 | `internal/api/alert_webhook.go:49` `errorBody{Error}` / `writeError` | M3 在此**扩展**（A4），既有调用点不改 |
| 列表分页约定 | `internal/api/alert_list.go`（default 100 / max 1000 / 脏参数退化为默认） | M3 列表接口与之保持同一手感 |
| 迁移 | `0001` / `0002` / `0003` | **全部冻结**（AGENTS.md 约束 10）；M3 只允许新增 `0004_*` |
| 跨包集成测试基建 | `internal/testsupport`（`NewDB`，`IIP_TEST_PG_URL` 未设则 `t.Skip`） | M3 集成测试沿用 |
| 前端 | `web/src/`（`App.tsx` 直接渲染 `pages/Alerts.tsx`） | **未接路由**；`react-router-dom@6.27.0` 与 `@tanstack/react-query@5.59.16` 均已安装，M3 直接启用 |

> **时序前置**：M2 只对 `status='firing'` 的新增告警触发关联。M3 不改变这一点；`resolved` 告警仍不创建/合并事件。

---

## 二、与当前仓库实测状态的对齐表（2026-10-01 实测）

| 项 | 实测状态 | M3 动作 |
|----|----------|---------|
| **A1** `incidents.status` CHECK | `chk_incidents_status` 限定 **5 个值且全小写**：`open, investigating, mitigated, resolved, ignored`（`0001_init.up.sql:58`）。**没有** `acknowledged` | 保持小写口径，**新增 `acknowledged`** → 0004 重建 CHECK 为 6 值（§4.1） |
| **A2** `timeline.event_type` CHECK | `chk_timeline_event_type` 限定 5 值：`alert_fired, alert_resolved, status_change, evidence_found, user_comment`（`0001_init.up.sql:115`）。M3 计划的 7 个类型**一个都不在** | 新增 7 个专用值 → 0004 重建 CHECK 为 12 值（§4.2） |
| **A3** `needs_review` / `correlation_score` 的归属 | 二者都在 **`incident_alerts`**（关联边）上，**不在 `incidents`**。`correlation_score` 见 `0001:74`，`needs_review` 见 `0003` | 详情页按「关联边」维度读；**禁止**在 `incidents` 上找这两列（§4.4） |
| **A4** 错误响应体 | 实际形状是 `{"error":"<消息>"}`（`errorBody{Error string \`json:"error"\`}`），**没有** `code` / `message` 两字段 | **扩展**为 `{"error":..., "code":...}`，`code` 省略则序列化时不出现 → 向后兼容（§4.3） |
| `incidents` 查询 | `incidents.sql` 只有 3 条（CreateIncident / ListOpenIncidentsWithAlerts / LinkAlertToIncident / SetAlertIncident） | 新增列表、详情、状态更新、队列相关查询 |
| `timeline` 表读写 | **零读取、零写入**（M2 报告已如实标注） | M3 补齐写入与读取（本里程碑核心） |
| `evidence` 表 | 零读写（归 M4） | M3 **不碰** |
| 前端路由 | `App.tsx` 直接 `return <Alerts />`，`react-router-dom` **无任何 import** | M3 引入路由 + 布局（§4.8） |
| 前端图标库 | `@ant-design/icons` **未作为直接依赖**（pnpm 严格布局下不可直接 import） | 见 OPEN-M3-1；M3 页面只用到少量状态与操作图标 |
| 前端测试框架 | 无（`package.json` 无 vitest/jest） | 前端门禁 = `pnpm tsc` + `pnpm build` + 容器链路可达性断言（与 M1 前端同一口径） |
| Go 工具链 | 本机 Go 1.23.8；`golangci-lint v1.61.0` 本地已装且 rc=0 | 质量门禁可本地实跑 |
| 仓库远端 | **无 git remote** → CI 从未执行 | 所有门禁一律本地/容器内真实执行，**不得**以「交由 CI 覆盖」结账 |

---

## 三、范围

### 3.1 做什么（M3 锁定）

| 编号 | 任务 | 产出 | 来源 |
|------|------|------|------|
| T3.2 | 状态机服务 | 转换白名单纯函数 + `Service.Transition` + 非法转换 422 + 补全 V1.0 被截断的三条 | 开发计划 §7 |
| T3.3 | Timeline | 状态变更 / 告警合入 / 用户操作自动写入 `timeline`，并补齐 M2 写入路径 | 开发计划 §7 |
| T3.1 | 事件查询 API | `GET /api/v1/incidents`（过滤 + 分页）、`GET /api/v1/incidents/{id}`（含关联告警 + timeline） | 开发计划 §7 |
| T3.4 | REVIEW 确认队列 API | `GET /api/v1/reviews` + `POST /api/v1/reviews/{incidentID}/{alertID}`（confirm / reject）+ 抑制表 | 开发计划 §7 |
| **T3.5** | **前端事件页面**（本里程碑新增，原属 M5） | 事件列表页 + 事件详情页（Timeline / 关联告警 两 Tab）+ 处置操作 | 计划 §7 已列为 M3「Incident 可被人查询、认领、处置、关闭，全程留痕」，页面是这句话的落地 |

### 3.2 明确不做（Out-of-Scope，锁定）

| 不做项 | 原因 | 何时考虑 |
|--------|------|----------|
| Evidence 采集与证据 Tab | `evidence` 表零读写，属 M4；此时页面上无数据可展示 | M4 |
| REVIEW 确认队列**页面** | 本里程碑只做队列 **API**（T3.4）；队列页面与其余 UI 一并在 M5 做 | M5 |
| 事件合并 / 拆分的人工操作 | M2 未提供，且需重算关联；属 V1.1 反馈学习 | V1.1 |
| 登录与鉴权 | V1 内网部署，计划 §9 T5.1 才引入单管理员 token | M5 |
| 告警的人工修正（改 severity / 忽略） | 需事件上下文与审计，M1 手册已列为 M3+ 之后 | V1.1 |
| 通知（邮件 / IM） | 属 M6 | M6 |
| 有监督学习 / 权重自学习 | 计划 §1.3 明确 V1.x | V1.1 |
| 事件列表的精确 total 计数 | 用 `has_more` 探测替代，省一次全表 COUNT（§4.3） | 出现真实分页需求时 |

### 3.3 AGENTS.md 约束在 M3 的映射

| 约束 | M3 落地 |
|------|---------|
| 2 / 11（只做当前里程碑） | 事件页面属 M3（§3.1 T3.5）；证据 Tab / 队列页面 / 通知一律不做 |
| 4（迁移变更须出清单） | 本文 §五 给出 0004 完整「表/列/索引/约束」清单，含 down 的**数据影响** |
| 5 / 12（如实标注、不修饰为通过） | 迁移 down 会删数据、历史 timeline 不回填、前端无自动化测试——一律如实写明 |
| 10（迁移文件冻结） | 不改 `0001`/`0002`/`0003`，只新增 `0004` |
| 3（禁止擅自改测试断言） | M3 新增断言；M2 既有断言一条都不放宽 |

---

## 四、详细设计

### 4.1 状态模型与转换白名单（A1 + T3.2 补全）

**状态集合（6 个，全部小写，与 0001 既有口径一致）**：

| 状态 | 语义 | 终态 |
|------|------|------|
| `open` | 刚创建，无人认领（M2 NEW 档产出） | 否 |
| `acknowledged` | **M3 新增**：已确认收到，尚未开始调查 | 否 |
| `investigating` | 正在调查 | 否 |
| `mitigated` | 已缓解，根因未除 | 否 |
| `resolved` | 已解决 | **是** |
| `ignored` | 误报 / 无需处理 | **是** |

**转换白名单（规范表）**：

| 当前状态 | 允许转换为 | 触发方式 |
|----------|-----------|---------|
| `open` | `acknowledged`, `investigating`, `mitigated`, `resolved`, `ignored` | 认领 / 自动分配 / 直接处置 |
| `acknowledged` | `investigating`, `mitigated`, `resolved`, `ignored` | 开始调查 / 直接处置 |
| `investigating` | `mitigated`, `resolved`, `ignored` | 缓解 / 解决 / 判为误报 |
| `mitigated` | `resolved`, `ignored` | 解决 / 判为误报 |
| `resolved` | `open` | **重开** |
| `ignored` | `open` | **重开** |

**实现口径 = 阶序 + 重开**（与上表逐格等价，作为唯一真源）：

```
ladder = [open, acknowledged, investigating, mitigated, resolved, ignored]   // 索引 0..5

CanTransition(from, to):
    if from == to:                     return false   // 同状态不算转换
    if from ∈ {resolved, ignored}:     return to == open    // 终态只能重开
    if to == open:                     return false   // 非终态不能回到 open
    return index(to) > index(from)                    // 只许前向
```

要点：
- **只能前向、不许回退**：`acknowledged → open`、`investigating → acknowledged` 均非法。
- **允许越级前向**：`open → mitigated`、`acknowledged → resolved` 合法（真实值班里「直接解决」很常见）。
- `ignored` 置于阶序末端，因此「任意非终态 → ignored」天然成立。
- 终态不可互转：`resolved → ignored`、`ignored → resolved` 非法（只能经 `open` 重开）。

**T3.2 要求补全的三条**，逐一落在白名单里：`MITIGATED→RESOLVED`（4>3，合法）、`INVESTIGATING→IGNORED`（5>2，合法）、`RESOLVED→OPEN`（重开规则，合法）。
V1.0 §6.2 已给出的三条（`OPEN→INVESTIGATING`、`OPEN→IGNORED`、`INVESTIGATING→MITIGATED`）全部保留。
> V1.0 §6.2 原表在 `| INVEST |` 处被截断且带有 `(AI生成)` 标记——本文即为其**权威补全**，此后以本文为准。

**终态副作用（决策 D1）**：
- 进入 `resolved` → `resolved_at = NOW()`；
- 从 `resolved` 重开 → `resolved_at = NULL`；
- 进入 `ignored` → **不写** `resolved_at`。理由：`ignored` 是误报关闭，写入会污染 MTTR 统计（把误报当成一次真实恢复）。代价是「忽略时刻」只能从 timeline 取。

**非法转换响应**：HTTP **422** + `{"error":"...","code":"invalid_transition"}`，`error` 文案须含 `from` 与 `to` 两个状态名，便于前端直接展示。

### 4.2 Timeline 事件类型（A2）

**最终 12 个取值** = 既有 5 个（保留不动）+ M3 新增 7 个：

| 新增 | 含义 | 写入时机 | actor | metadata |
|------|------|----------|-------|----------|
| `alert_linked` | 告警合入事件 | M2 关联决策为 MERGE 或 REVIEW 并成功建边时 | `system` | `{alert_id, score, needs_review}` |
| `acknowledged` | 认领 / 确认 | `→ acknowledged` | 操作人 | `{from, to}` |
| `investigation_started` | 开始调查 | `→ investigating` | 操作人 | `{from, to}` |
| `mitigated` | 标记缓解 | `→ mitigated` | 操作人 | `{from, to}` |
| `resolved` | 解决 | `→ resolved` | 操作人 | `{from, to}` |
| `ignored` | 判为误报 | `→ ignored` | 操作人 | `{from, to}` |
| `reopened` | 重开 | `resolved/ignored → open` | 操作人 | `{from, to, note?}` |

**保留但 M3 不再新写的 2 个**：
- `status_change`：通用状态变更。7 个专用类型已覆盖状态流转，M3 不再写它；该值**零存量行**，但为最小改动**保留**在 CHECK 里（是否在 0004 中删除见 OPEN-M3-3）。
- `alert_fired` / `alert_resolved` / `evidence_found` / `user_comment`：
  - `alert_fired`：**M3 起用于表达「事件创建」**（M2 NEW 档建事件时写一条），metadata `{alert_id, fingerprint}`；
  - `user_comment`：用户备注；**REVIEW 的确认/拒绝也复用此类型**（metadata `{action:"review_confirm"|"review_reject", alert_id}`），因为用户已锁定只新增 7 个专用值，不再为 REVIEW 单列类型；
  - `alert_resolved`：告警恢复，M3 暂不写（告警恢复不改变事件状态，归后续里程碑评估）；
  - `evidence_found`：M4 专用，M3 不写。

**前端映射（语义 → 视觉）**：页面对 12 个类型分组着色——生命周期起点（`alert_fired`/`alert_linked`）中性色、状态推进（`acknowledged`/`investigation_started`/`mitigated`）蓝色系、终结（`resolved`/`ignored`/`reopened`）分别用绿/灰/橙。**具体色值须走 Design Token，不得硬编码**；图标按 OPEN-M3-1 选定方案后锁定（禁止 emoji 作功能图标）。

### 4.3 API 契约

Base = `/api/v1`。**鉴权**：V1 内网部署，M3 全部端点**无认证**（登录属 M5 T5.1）。

| Method | Path | 功能 | 请求 | 响应 |
|--------|------|------|------|------|
| GET | `/api/v1/incidents` | 事件列表 | query：`status`（可重复/逗号分隔）、`severity`、`service`、`from`、`to`（RFC3339）、`limit`、`offset` | `{incidents:[Summary], count, has_more}` |
| GET | `/api/v1/incidents/{id}` | 事件详情 | — | `{incident:Detail, alerts:[AlertLink], timeline:[Entry], truncated:{alerts,timeline}}` |
| POST | `/api/v1/incidents/{id}/transition` | 状态流转 | `{to, actor, note?}` | 200 `{incident:Summary}`；非法转换 422 `invalid_transition` |
| POST | `/api/v1/incidents/{id}/comments` | 加备注 | `{actor, note}` | 201 `{entry:Entry}` |
| GET | `/api/v1/reviews` | REVIEW 队列 | query：`limit`、`offset` | `{reviews:[ReviewEdge], count, has_more}` |
| POST | `/api/v1/reviews/{incidentID}/{alertID}` | 确认 / 拒绝一条待确认关联边 | `{decision:"confirm"\|"reject", actor, reason?}` | 200；边不存在 404 `review_edge_not_found` |

**分页与过滤口径**（沿用 `alert_list.go` 的手感）：
- `limit` 默认 100、夹紧到 `[1, 1000]`；`offset` 默认 0、负数夹紧到 0；**脏参数退化为默认值而非 400**（列表是只读查询，不该因参数脏挡掉页面加载）。
- `has_more` 用 **`limit+1` 探测**：服务层按 `limit+1` 查询，命中则 `has_more=true` 并在返回前截断为 `limit`。**不做全表 COUNT**，也不返回可能失真的 `total`。
- 详情内联的 `alerts` 与 `timeline` **各最多 200 条**；被截断时 `truncated` 对应字段为 `true`（前端须显式提示「仅显示最近 200 条」，不得静默截断）。不做独立分页端点（见 OPEN-M3-5）。

**错误体（A4）**：

```go
// internal/api/errors.go（新增文件；errorBody 从 alert_webhook.go 迁入并扩展）
type errorBody struct {
    Error string `json:"error"`
    Code  string `json:"code,omitempty"`   // 省略 → 序列化时不出现
}
```

- **向后兼容**：既有 `writeError(w, logger, status, msg)` 与所有既有调用点**一行不改**，序列化结果仍是 `{"error":"..."}`（`code` 为空则省略）。
- 新增 `writeErrorCode(w, logger, status, code, msg)` 供 M3 新端点使用。
- M3 错误码表：

| code | HTTP | 场景 |
|------|------|------|
| `invalid_transition` | 422 | 转换不在白名单内 |
| `invalid_body` | 400 | 缺字段 / 未知字段（沿用 ADR-010 的 `DisallowUnknownFields`） |
| `incident_not_found` | 404 | `{id}` 不存在 |
| `review_edge_not_found` | 404 | 待确认边不存在或已处理 |
| `storage_unavailable` | 503 | 存储故障（与既有 webhook / 告警列表一致，让调用方可重试） |

> 既有端点**不追加** `code`，避免无谓扩大回归面（决策 D2）。

### 4.4 数据读取口径（A3）

**事实**：`needs_review` 与 `correlation_score` 都在 `incident_alerts`（关联边）上。`incidents` 表**没有**这两列，也没有「待确认计数」。

因此：
- 详情页的「关联告警」列表按 `incident_alerts` 逐行读，每项形如
  `AlertLink{alert_id, fingerprint, severity, labels, starts_at, ends_at, correlation_score, needs_review, correlated_at}`。
- 列表页若要展示「待确认数」，须用 `LEFT JOIN incident_alerts ... WHERE needs_review` 聚合，**不得**假设 `incidents` 上有该列。
- `correlation_score` 为 `NULL` 表示该告警是 NEW 档（无关联对象），前端显示「—」而非 `0`。

### 4.5 REVIEW 队列与抑制（T3.4）

- **队列来源**：`incident_alerts.needs_review = TRUE`（0003 已建部分索引 `idx_incident_alerts_needs_review`，直接可用）。
- **confirm（确认合并）**：`needs_review = FALSE`，保留关联边；写 timeline `user_comment`（`action=review_confirm`）。
- **reject（拒绝合并）**：删除该关联边；若该告警已无其他关联边，则把 `alerts.incident_id` 回写为 `NULL`；写 **抑制行** + timeline `user_comment`（`action=review_reject`）。
- **抑制表 `review_suppressions`（0004 新增，轻量）**：

| 列 | 类型 | 说明 |
|----|------|------|
| `id` | BIGSERIAL PK | |
| `fingerprint` | VARCHAR(128) NOT NULL | 抑制键：与告警指纹同源（动态 label 已排除，跨实例复用） |
| `incident_id` | BIGINT NOT NULL REFERENCES incidents(id) ON DELETE CASCADE | 被拒绝合入的事件 |
| `actor` | VARCHAR(128) | 做出该判定的人 |
| `reason` | TEXT | 拒绝理由（可选） |
| `created_at` | TIMESTAMPTZ NOT NULL DEFAULT NOW() | |

  唯一约束 `uq_review_suppressions (fingerprint, incident_id)`；索引 `idx_review_suppressions_fingerprint`。

- **抑制生效点**：M2 关联流程在判定为 REVIEW 档时，先查该 `fingerprint` 是否已被该候选事件抑制；命中则**不建边**，按 NEW 档处理并记一条 INFO 日志。
- > **为什么不用既有 `correlation_feedback` 表**：该表的 `alert_id_pair` 是**具体告警 ID 对**，新告警到来时 ID 必然不同，无法复用；抑制需要「按 fingerprint 跨次复用」的键。故按 T3.4 原文新增轻量表。
- 抑制的**有效期与匹配键**见 OPEN-M3-2（暂定永久 + 按 fingerprint）。

### 4.6 分层与包结构（新增/改动）

```
internal/
├── incident/                       # 新增：M3 状态机与时间线（纯领域逻辑，零 SQL）
│   ├── status.go                   # Status 常量 + ladder + CanTransition + 可读错误
│   ├── event.go                    # 7 个 EventType 常量 + 构造器（统一 actor/metadata 形状）
│   ├── models.go                   # Snapshot / Summary / Detail / AlertLink / Entry
│   ├── service.go                  # Transition / Comment / List / Detail / Review
│   ├── repository.go               # 仓储接口（定义在消费方，实现放 repository）
│   └── *_test.go
├── repository/
│   ├── incidents.go                # 扩展：列表 / 详情 / 状态更新
│   ├── timeline.go                 # 新增：写入与按事件查询
│   └── reviews.go                  # 新增：队列查询 / confirm / reject / 抑制命中
├── api/
│   ├── errors.go                   # 新增：errorBody 迁入并扩展 + writeErrorCode
│   ├── incident_list.go            # 新增
│   ├── incident_detail.go          # 新增
│   ├── incident_transition.go      # 新增（含 transition / comments）
│   └── review_queue.go             # 新增
└── db/queries/
    ├── incidents.sql               # 扩展
    ├── timeline.sql                # 新增
    └── incident_alerts.sql         # 新增（详情关联告警 + 队列）
web/src/
├── main.tsx                        # 包 BrowserRouter（改）
├── App.tsx                         # 改为「布局 + 路由出口」（改）
├── layouts/AppLayout.tsx           # 新增：侧边导航（告警 / 事件）
├── router.tsx                      # 新增：路由表
├── api/incidents.ts                # 新增：类型 + fetch 封装（沿用 api/alerts.ts 风格）
├── pages/IncidentList.tsx          # 新增
├── pages/IncidentDetail.tsx        # 新增（Timeline / 关联告警 两 Tab）
└── components/{Timeline,StatusTag,TransitionActions}.tsx   # 新增
```

**依赖方向**（不变式）：`api → incident → (接口)`，`repository → 实现 incident 接口`。
`internal/incident` **不得** import `internal/api` 或 `internal/repository`。
`incident` 包**不新增**对 `correlation` 包的依赖（避免 M2 回归面扩大）——M3 需要读事件状态时用自己的 `Snapshot`，`correlation.Incident`（写侧构造）保持不动。

### 4.7 sqlc 查询清单

| 文件 | 查询名 | 类型 | 用途 |
|------|--------|------|------|
| `incidents.sql` | `ListIncidents` | `:many` | 列表（过滤 + `limit+1` 探测） |
| `incidents.sql` | `GetIncidentByID` | `:one` | 详情本体 |
| `incidents.sql` | `UpdateIncidentStatus` | `:exec` | 状态 + `resolved_at` 同步更新 |
| `incident_alerts.sql` | `ListIncidentAlertsByIncident` | `:many` | 详情关联告警（含 score / needs_review / correlated_at） |
| `incident_alerts.sql` | `ListReviewEdges` | `:many` | 待确认队列 |
| `incident_alerts.sql` | `GetReviewEdge` | `:one` | 单边查询（判定前存在性校验） |
| `incident_alerts.sql` | `ConfirmReviewEdge` | `:exec` | `needs_review = FALSE` |
| `incident_alerts.sql` | `DeleteIncidentAlert` | `:exec` | reject 时删除边 |
| `incident_alerts.sql` | `CountIncidentAlertsByAlert` | `:one` | 判断是否需把 `alerts.incident_id` 置 NULL |
| `timeline.sql` | `InsertTimelineEntry` | `:one` | 写入并返回整行 |
| `timeline.sql` | `ListTimelineByIncident` | `:many` | 详情时间线（`timestamp DESC, id DESC`，上限 200） |
| `reviews.sql` | `InsertReviewSuppression` | `:exec` | 写抑制行（`ON CONFLICT DO NOTHING`） |
| `reviews.sql` | `IsSuppressed` | `:one` | 关联时判抑制 |

> 生成方式：编辑 `internal/db/queries/*.sql` → `sqlc generate` → 提交生成物。

### 4.8 前端路由与页面

| 页面 | 路由 | 核心内容 | 对应 API |
|------|------|----------|----------|
| 告警列表（既有） | `/alerts` | 保持现状 | `GET /api/v1/alerts` |
| 事件列表 | `/incidents` | 表格：标题 / 严重度 / 状态 / 服务 / 发起时间 / 持续时长 / 关联告警数；筛选项 = status / severity / service / 时间范围；分页凭 `has_more` | `GET /api/v1/incidents` |
| 事件详情 | `/incidents/:id` | 头部：标题 / 状态 / 严重度 / 服务 / 起止时间；Tab① Timeline（垂直时间线，按类型着色 + actor + 相对时间）；Tab② 关联告警（含 `correlation_score` 与「待确认」标记） | `GET /api/v1/incidents/{id}` |
| 处置操作 | 详情页头部按钮区 | 按白名单**只渲染当前合法的目标状态按钮**（前端用同一份转换规则做禁用态），必填备注可留空 | `POST /api/v1/incidents/{id}/transition` |

**硬性要求**：
- 状态 → 视觉映射由 `StatusTag` 单一组件承担，**禁止在各页面散落色值**；色值走 Design Token。
- 处置按钮的合法性判断**必须**复用同一份转换规则（前端本地实现同构版本 + 单测覆盖边界），不得靠 if 判断拼凑，否则会与后端 422 语义漂移。
- 所有请求走 `@tanstack/react-query`（已装），成功后 `invalidateQueries` 刷新详情与列表。
- `vite` 已配置的 `/api` 代理沿用，**不新增**跨域配置。

### 4.9 配置新增

M3 **不新增**环境变量。列表默认页大小（100）与详情内联上限（200）以常量落在 `internal/api`，与 `alert_list.go` 的 `defaultListLimit` / `maxListLimit` 同处。

### 4.10 并发、幂等与失败语义

- **状态流转的并发**：`UpdateIncidentStatus` 的 `WHERE` 子句**必须**带 `AND status = <期望的 from>`，用受影响行数判定是否真的转换成功；0 行 → 说明被并发改了 → 回 **409**（`code:"stale_transition"`）并附当前状态。**不允许**先读后写而不加条件（会丢更新）。
- **事件 + timeline 必须同事务**：状态更新与 timeline 写入在一个事务里，避免「状态变了但没有留痕」。
- **关联失败不得阻塞 webhook**（M2 铁律，M3 继承）：M3 在 M2 关联路径上补写 timeline 时，**写入失败只记 error 日志**，不得让 HTTP 200 变成 500。
- **幂等**：`POST /transition` 到当前状态 → 422（同状态不算转换），不产生 timeline 行。
- **timeline 写入失败 vs 状态更新成功**：同事务，故二者要么都成要么都不成（与上一条的「webhook 主链路」不是同一处，勿混淆）。

---

## 五、迁移变更清单（AGENTS.md 约束 4）

| 项 | 内容 |
|----|------|
| 新增文件 | `migrations/0004_incident_lifecycle_m3.up.sql` / `.down.sql` |
| **约束变更 1** | `incidents`：`DROP CONSTRAINT IF EXISTS chk_incidents_status` → 重建为 `CHECK (status IN ('open','acknowledged','investigating','mitigated','resolved','ignored'))` |
| **约束变更 2** | `timeline`：`DROP CONSTRAINT IF EXISTS chk_timeline_event_type` → 重建为 12 值（既有 5 + `alert_linked`,`acknowledged`,`investigation_started`,`mitigated`,`resolved`,`ignored`,`reopened`） |
| **新增表** | `review_suppressions`（id / fingerprint / incident_id / actor / reason / created_at），唯一约束 `(fingerprint, incident_id)` |
| **新增索引 1** | `idx_timeline_incident_ts ON timeline(incident_id, timestamp DESC)`（详情时间线读取） |
| **新增索引 2** | `idx_review_suppressions_fingerprint ON review_suppressions(fingerprint)` |
| **不改列** | 不新增/删除任何列；`resolved_at` 复用既有列 |
| **不动** | `0001` / `0002` / `0003`（冻结） |
| **down 的破坏性（必须明示）** | ① `DELETE FROM timeline WHERE event_type IN (<7 个新值>)`；② `UPDATE incidents SET status='open' WHERE status='acknowledged'`；③ `DROP TABLE review_suppressions`。**这三步会真实丢数据**——down 只为「三连可重复」与本地回滚服务，生产回滚前须先备份。 |
| **验证** | `migrations/migrate_test.go` 扩展：up→down-all→up 三连；断言 ①`status='acknowledged'` 可插入 ②7 个新 `event_type` 可插入 ③旧值 `foo` 仍被拒 ④`review_suppressions` 存在且唯一约束生效 ⑤新索引存在 |

---

## 六、任务拆解（10 天）

| 日 | 任务 | 产出 |
|----|------|------|
| D1 | T3.2-a：`status.go` 转换白名单 + 全矩阵单测；0004 up/down + 三连 | 纯函数 + 迁移可回滚 |
| D2 | T3.2-b：`Service.Transition` + 仓储（含 `status = from` 乐观条件、`resolved_at` 副作用）+ 事务 | 状态机服务 + 集成测试 |
| D3 | T3.3-a：timeline 写入基建（`timeline.sql` + repository + `event.go` 构造器） | 留痕基建 |
| D4 | T3.3-b：接入 M2 关联路径（`alert_fired` / `alert_linked`），失败只记日志 | M2 路径补写完成 |
| D5 | T3.1-a：列表 API（过滤 + 分页 + `limit+1` 探测）| 列表可查 |
| D6 | T3.1-b：详情 API（本体 + 关联告警 + timeline 聚合 + `truncated`）| 详情可查 |
| D7 | T3.4：REVIEW 队列 API + 抑制表 + 抑制命中点 | 队列可用 |
| D8 | T3.5-a：前端路由 + 布局 + 事件列表页 | 列表页可用 |
| D9 | T3.5-b：详情页两 Tab + 处置操作 | 处置闭环 |
| D10 | 文档回填（README 验收表 / decisions / 本手册）+ 全量回归验收 | 验收报告 |

### 降级预案（超期 1 周时启用）

砍 T3.4 的**抑制**部分（只保留 confirm / reject 的边处理，不做 `review_suppressions` 与命中逻辑）→ 省约 1 天；`review_suppressions` 表与迁移一并延后。**不降级**：状态机、timeline、列表/详情 API、处置页面（这四项是 M3 的根）。

---

## 七、测试策略

### 7.1 分层

| 层 | 范围 | 手段 |
|----|------|------|
| 纯函数单测 | `CanTransition` 全矩阵、`Status` 取值合法性、事件构造器 | 表驱动，无需 DB |
| 编排单测 | `Service.Transition` / `Comment` / `Review` 分支 | 假仓储注入（对齐 M2 的 `fakeRepo` 风格） |
| API 单测 | 4 个新 handler 的状态码与响应体（含 A4 的 `code`） | `httptest`，假 service |
| 集成测试 | 列表过滤/分页、详情聚合、流转落库 + timeline、队列 confirm/reject、抑制命中、乐观并发 | 真 PG（`testsupport.NewDB`，`IIP_TEST_PG_URL`） |
| 迁移测试 | 0004 up→down-all→up 三连 + 约束正反例 | 真 PG |
| 前端 | 类型检查与构建 | `pnpm tsc` + `pnpm build`（无前端测试框架，如实标注） |
| 容器链路 | 页面可达 + API 端到端 | 扩展 `scripts/verify-images.py` |

### 7.2 用例清单（最低要求）

- **状态机全矩阵**：6×6 = 36 个组合**逐一断言**（合法 14 个 → true；其余 22 个 → false），并断言错误文案同时包含 `from` 与 `to`。
- **T3.2 三条补全**：`MITIGATED→RESOLVED`、`INVESTIGATING→IGNORED`、`RESOLVED→OPEN` 三个用例**必须单独存在**（它们是 V1.0 文档缺失项，回归价值最高）。
- **`resolved_at` 副作用**：`→resolved` 写入；`resolved→open` 清空；`→ignored` 不写。
- **乐观并发**：两次并发流转同一事件，第二次必须 409 且**不产生** timeline 行。
- **timeline**：流转后按 `event_type` / `actor` / `metadata` 断言行内容；`alert_linked` 的 metadata 含 `score` 与 `needs_review`。
- **列表**：各过滤条件单独 + 组合；`limit=1000` 上限夹紧；`limit=abc` 退化为默认；`offset=-5` 夹紧为 0；**`has_more` 用「正好 limit 条」与「limit+1 条」两个边界夹具**验证。
- **详情**：`alerts` / `timeline` 超 200 条时 `truncated` 为 `true` 且长度恰为 200；`correlation_score = NULL`（NEW 档）序列化为 `null`；`{id}` 不存在 → 404。
- **A4 向后兼容**：既有 `GET /api/v1/alerts` 的 503 响应体**仍然是 `{"error":...}`**（不出现 `code` 键）——这条专门锁「扩展而非替换」。
- **A3 口径**：用例中显式断言 `needs_review` 从 `incident_alerts` 读出，而不存在 `incidents.needs_review` 的假设。
- **REVIEW**：confirm 后边仍在且 `needs_review=false`；reject 后边被删且 `alerts.incident_id` 置 NULL、抑制行写入；再次投递同 fingerprint 告警 → **不建 REVIEW 边**。

### 7.3 反向验证（防「假绿」）

- 把 `CanTransition` 的 `to == open → false` 判定去掉 → 「`investigating → open`」用例必须转红；
- 把终态重开规则去掉 → 「`resolved → open`」用例必须转红；
- 把 `UpdateIncidentStatus` 的 `AND status = from` 条件去掉 → 乐观并发用例必须转红；
- 把 timeline 写入从事务中移出/删掉 → 流转留痕用例必须转红；
- 0004 若漏掉 `incidents.status` 的 CHECK 放宽 → 「插入 `acknowledged`」用例必须被 PG 拒绝而转红；
- 把 `writeErrorCode` 改成给既有端点也带上 `code` → A4 向后兼容用例必须转红。

---

## 八、验收清单（全过才算 M3 完成）

```text
[ ] 转换白名单实现；6×6 全矩阵单测通过；非法转换 422 且文案含 from/to
[ ] V1.0 §6.2 被截断的三条转换（MITIGATED→RESOLVED / INVESTIGATING→IGNORED / RESOLVED→OPEN）各有独立用例并通过
[ ] resolved_at 三条副作用（写入 / 清空 / ignored 不写）均有断言
[ ] 状态流转与 timeline 写入同事务；并发冲突回 409 且不产生 timeline 行
[ ] Timeline 三写入点生效：事件创建(alert_fired) / 告警合入(alert_linked) / 用户操作
[ ] 列表 API：四种过滤 + 分页 + has_more 边界夹具通过
[ ] 详情 API：本体 + 关联告警 + timeline 聚合；超 200 条时 truncated=true
[ ] REVIEW 队列：confirm 保留边、reject 删边并回写 incident_id=NULL、抑制表写入且二次投递不再建边
[ ] 前端：/alerts 与 /incidents 路由可达；详情页两 Tab 渲染；处置按钮仅渲染合法目标状态
[ ] pnpm tsc 与 pnpm build 均 rc=0
[ ] 迁移 0004 up→down→up 三连通过；约束正反例均符合预期
[ ] go build ./... / go vet ./... / go test ./... / gofmt 全绿
[ ] golangci-lint rc=0（逐项修复，无 nolint、无排除规则新增）
[ ] 真库门禁跳过用例数 = 0；go test -race 容器内等价复现通过
[ ] 容器链路 verify-images.py 既有 32 项断言仍全绿 + M3 新增断言通过
[ ] README 新增「M3 验收状态」表；docs/decisions.md 记录 M3 决策；本手册变更记录已回填
```

> 任何一项未通过 → **如实标注，不修饰为通过**（AGENTS.md 约束 5 / 12）。

---

## 九、端到端验证脚本（可直接粘贴）

```powershell
# 前置：数据库可用；IIP_PG_URL 指向它
$env:IIP_PG_URL = "postgres://postgres:dev@127.0.0.1:5433/iip?sslmode=disable"

# 1) 质量门禁
go build ./...; if ($?) { go vet ./... }
go test ./... -count=1
gofmt -l .                      # 断言：无输出
golangci-lint run --config=./.golangci.yml --timeout=20m --max-issues-per-linter=0 --max-same-issues=0 ./...

# 2) 状态机与迁移
go test ./internal/incident/... -run Transition -v     # 断言：36 组合全断言
go test ./migrations/... -run Migrate -v               # 断言：up→down-all→up 三连

# 3) 起服务（自动迁移到版本 4）
go run ./cmd/iip-server &
# 等 "服务拓扑已加载" 后：
curl.exe -s http://localhost:8080/api/v1/incidents
# 断言：200 + {incidents, count, has_more}

# 4) 造一条事件（复用 M2 路径：投两条同服务告警 → 合并）
curl.exe -s -X POST http://localhost:8080/api/v1/alerts/webhook -H "Content-Type: application/json" --data-binary "@testdata/replay/scenario2_cascade.json"
# 取事件 id
$iid = (curl.exe -s http://localhost:8080/api/v1/incidents | ConvertFrom-Json).incidents[0].id

# 5) 合法流转：open -> acknowledged -> investigating -> resolved
curl.exe -s -X POST "http://localhost:8080/api/v1/incidents/$iid/transition" -H "Content-Type: application/json" -d '{"to":"acknowledged","actor":"tester"}'
curl.exe -s -X POST "http://localhost:8080/api/v1/incidents/$iid/transition" -H "Content-Type: application/json" -d '{"to":"investigating","actor":"tester"}'
curl.exe -s -X POST "http://localhost:8080/api/v1/incidents/$iid/transition" -H "Content-Type: application/json" -d '{"to":"resolved","actor":"tester"}'
# 断言：详情 timeline 依序出现 acknowledged / investigation_started / resolved，且 resolved_at 非空

# 6) 非法流转：resolved -> investigating（终态不可回退）
curl.exe -s -o NUL -w "%{http_code}`n" -X POST "http://localhost:8080/api/v1/incidents/$iid/transition" -H "Content-Type: application/json" -d '{"to":"investigating","actor":"tester"}'
# 断言：422 + code=invalid_transition

# 7) 重开：resolved -> open
curl.exe -s -X POST "http://localhost:8080/api/v1/incidents/$iid/transition" -H "Content-Type: application/json" -d '{"to":"open","actor":"tester"}'
# 断言：200，且详情 resolved_at 为 null、timeline 末尾为 reopened

# 8) A4 向后兼容
curl.exe -s http://localhost:8080/api/v1/incidents/99999999
# 断言：404 + {error, code:"incident_not_found"}

# 9) 前端
cd web; pnpm tsc; if ($?) { pnpm build }
```

---

## 十、风险与开放项

| 编号 | 开放项 | 当前倾向 | 阻塞于 | 状态 |
|------|--------|----------|--------|------|
| OPEN-M3-1 | 前端图标库 | **手写内联 SVG 图标组件**（零新增依赖，符合「依赖最小化」；`@ant-design/icons` 当前不可直接 import）。备选：新增 `@ant-design/icons` 并钉版本 | 你对新增前端依赖的取舍 | **待确认** |
| OPEN-M3-2 | 抑制表的匹配键与有效期 | 键 = `fingerprint`（跨实例复用，需保证**不**含 `incident_id`）；有效期 = 永久 | 首次上线后观察是否需加时间窗 | 暂定 |
| OPEN-M3-3 | `status_change` 是否删除 | **保留**（零写入，删除会扩大 CHECK 变更面） | — | 暂定 |
| OPEN-M3-4 | 历史 timeline 是否回填 | **不回填**（M2 期产生的关联无留痕，如实标注为历史空白） | 你对历史可追溯性的要求 | 暂定 |
| OPEN-M3-5 | 详情内联 200 条上限 | 暂用「上限 + `truncated` 标记」，不做独立分页端点 | timeline 超 200 变成真实问题 | 暂定 |
| OPEN-M3-6 | `alert_resolved` 是否在 M3 写 | 暂不写（告警恢复不改变事件状态） | M5 页面需要「告警已恢复」提示时 | 暂定 |
| RISK-M3-1 | 改 M2 关联路径以补写 timeline，可能伤及 M2 已验收行为 | 同事务、失败只记日志；M2 既有 32 项容器断言与三场景回放必须全绿 | — | 用回归门禁锁死 |
| RISK-M3-2 | 引入路由影响既有 `/alerts` 页 | `/` 重定向到 `/alerts`，保持单页行为不变 | — | 前端回归断言 |
| RISK-M3-3 | 前端转换规则与后端漂移 | 两侧同构实现 + 边界用例；后端始终是唯一裁决方（422 为准） | — | 集成用例锁定 |
| RISK-M3-4 | 详情页在大事件上变慢（数百告警 × 数百时间线） | 200 条上限 + 复合索引 | 实测超 1s | 加分页端点 |

---

## 十一、变更记录

| 日期 | 变更内容 | 原因 | 影响范围 |
|------|----------|------|----------|
| 2026-10-01 | 初稿（草案，待确认）。含 A1–A4 四项与实测 schema 的偏差修正；锁定 6 状态 + 7 个 timeline 新类型；前端范围为「列表 + 详情两 Tab + 处置」 | M3 启动 | 全手册 |
