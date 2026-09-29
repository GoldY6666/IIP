# IIP M2 详细开发手册：Correlation Engine（关联引擎）

**版本**：V1.0（草案，待确认）
**日期**：2026-09-29
**编制依据**：`docs/IIP开发计划.md` §6（M2 任务清单 T2.1–T2.9）+ `docs/IIP-V1.0-技术设计文档.md` §5（五维评分）
**前置**：M1 已完成并端到端验证（`README.md`「M1 验收状态」全 PASS）
**本文性质**：**M2 的唯一开发依据**。范围、公式、配置、迁移与验收以本文为准。经确认后状态改为「已确认」，此后任何偏离须走变更流程。

---

## 〇、M2 完成定义（DoD）

M2 完成的定义 = **同时满足以下四条**：

1. **引擎可用**：五维加权评分（时间/服务/标签/拓扑/历史）与三档决策（MERGE ≥0.80 / REVIEW ≥0.50 / NEW <0.50）实现，并接入实时链路（webhook 落库后同步关联）。
2. **配置可调**：权重（Wt/Ws/Wl/Wd/Wh）与阈值存于数据库，修改后**无需重启**即对下一条告警生效。
3. **效果达标**：Replay 三场景——① 精确率 100%（不错并）② 召回率 ≥90%（同根因不漏并）③ 杂告警组不误合并。
4. **质量达标**：`internal/correlation` 包测试覆盖率 ≥90%；1000 条告警回放 <60s；`go build/vet/test` 与 `gofmt` 全绿。

---

## 一、M1 交接物核对（开工前必读）

| 交接物 | 位置 | 用途 |
|--------|------|------|
| 告警模型与五阶段管线 | `internal/alert/{model,validate,normalize,fingerprint,service}.go` | 复用 `alert.StoredAlert` 作为评分输入；指纹已就绪 |
| 告警仓储 | `internal/repository/alerts.go` | 查询告警（列表/按指纹），关联时需按 alert_id 回写 `incident_id` |
| sqlc 生成物 | `internal/db/*.go` + `internal/db/queries/alerts.sql` | 新增查询一律「先写 .sql → `sqlc generate`」 |
| 路由装配 | `cmd/iip-server/main.go` `newRouter()` | 已预留 `r.Get("/api/v1/incidents", ...)` 占位；M2 在此注入 correlation service |
| handler 契约工具 | `internal/api/{alert_webhook,alert_list}.go` | 复用 `writeJSON`/`writeError`/`msgStorageUnavailable` 错误码风格 |
| 迁移 | `migrations/0001_init.*`、`0002_*` | **二者已冻结**（AGENTS.md 约束 10）；M2 只允许新增 `0003_*` |
| 跨包集成测试基建 | `internal/testsupport`（`NewDB`，`IIP_TEST_PG_URL` 未设则 `t.Skip`） | 关联的集成测试沿用此约定 |

> **告警时序前置**：M1 的 `resolved` 已能正确更新原行（ADR-007：`ORDER BY starts_at DESC, id DESC`）。M2 关联**只对 `status='firing'` 的新增告警触发**；`resolved` 不创建/合并 Incident（其生命周期归 M3 状态机）。

---

## 二、与当前仓库实测状态的对齐表

| 项 | 实测状态（2026-09-29） | M2 动作 |
|----|------------------------|---------|
| `incidents` / `incident_alerts` / `correlation_rules` / `correlation_feedback` 表 | 已在 `0001_init` 建好（含 `incident_alerts.correlation_score`） | 直接复用；0003 只做种子 + 一列 |
| `internal/correlation` 包 | **不存在** | 新建 |
| `internal/db/queries/` | 仅 `alerts.sql` | 新增 `incidents.sql` / `incident_alerts.sql` / `correlation_rules.sql` |
| `incidents`/`incident_alerts` 查询 | **不存在** | 新增并 sqlc 生成 |
| 配置入口 | 无权重/阈值配置 | 新增 `GET/PUT /api/v1/correlation/config` |
| 拓扑文件 | 无 | 新增 `config/topology.yaml` |
| `testdata/replay/` | 仅占位 README | 新增 3 个场景 JSON + Go 回放测试 |
| Go 工具链 | 本机 Go 1.23.8 已就位 | 质量门禁可本地实跑 |

---

## 三、范围

### 3.1 做什么（M2 锁定）

| 编号 | 任务 | 产出 |
|------|------|------|
| T2.1 | 关联配置（权重 + 阈值）数据库化 + 配置 API | 单行配置 + `GET/PUT /api/v1/correlation/config` |
| T2.2 | TimeSimilarity | 纯函数 + 单测 |
| T2.3 | ServiceSimilarity | 纯函数 + 单测 |
| T2.4 | LabelSimilarity（Jaccard，`k=v` 对，排除动态 label） | 纯函数 + 单测 |
| T2.5 | DependencySimilarity（静态拓扑 YAML：HasEdge 0.9 / CommonNeighbors 0.5） | 拓扑加载 + 单测 |
| T2.6 | HistoricalSimilarity（`incident_alerts` 共现频率 SQL） | 查询 + 单测 |
| T2.7 | Correlate 主流程（取 open+investigating → 逐对评分 → 三档决策 → 事务写 `incidents`+`incident_alerts`） | service + 集成测试 |
| T2.8 | Replay 评估集（3 场景）+ 精确率/召回率脚本 | 数据集 + Go 回放测试 |
| T2.9 | 调权重并固化默认值 | 0003 种子默认值 = 调优结果 |

### 3.2 明确不做（Out-of-Scope，锁定）

| 不做项 | 原因 | 何时考虑 |
|--------|------|----------|
| Incident 状态机与 Timeline | 属 M3（§7） | M3 |
| 人工确认队列 UI 与「合并/拒绝」操作 | 属 M3 T3.4；M2 只**落库标记** REVIEW | M3 |
| K8s 服务拓扑自动发现 | 计划 §1.3 明确 V1.x | M2 验收评估（若 Replay 中依赖维全 0 再补） |
| 历史共现自学习/有监督学习 | 计划 §1.3 明确 V1.x | V1.1 |
| Incident 查询 API（列表/详情） | 属 M3 T3.1 | M3 |
| Evidence 采集 | 属 M4 | M4 |
| 关联结果的对外通知 | 属 M6 | M6 |

### 3.3 AGENTS.md 约束在 M2 的映射

| 约束 | M2 落地 |
|------|---------|
| 2（入口只装配、零业务） | `newRouter()` 只注入 service，评分逻辑一律在 `internal/correlation` |
| 4（迁移变更须出清单） | 本文 §五 给出 0003 完整清单 |
| 5 / 12（如实标注、不修饰为通过） | Replay 指标、覆盖率、耗时一律回填实测值 |
| 10（迁移文件冻结） | 不改 `0001`/`0002`，只新增 `0003` |
| 3（禁止擅自改测试断言） | 新增断言而非放宽既有断言 |

---

## 四、详细设计

### 4.1 评分模型（V1.0 §5.1 锁定）

```
CorrelationScore = Wt·TimeSimilarity  + Ws·ServiceSimilarity
                 + Wl·LabelSimilarity + Wd·DependencySimilarity
                 + Wh·HistoricalSimilarity
```

默认权重（V1.0 值，种子入库）：

| 维度 | 权重 | 说明 |
|------|------|------|
| TimeSimilarity | 0.25 | 时间窗口重叠度 |
| ServiceSimilarity | 0.25 | 服务/命名空间匹配度 |
| LabelSimilarity | 0.20 | 标签集合 Jaccard |
| DependencySimilarity | 0.20 | 拓扑依赖关联度 |
| HistoricalSimilarity | 0.10 | 历史共现频率 |

决策阈值为**二值语义、三档输出**：

```
score ≥ merge_threshold(0.80)        → MERGE   （合并到该 Incident）
score ≥ review_threshold(0.50)       → REVIEW  （关联到该 Incident，但标记待人工确认）
score <  review_threshold(0.50)      → NEW     （新建 Incident）
```

### 4.2 各维度算法（V1.0 §5.2 锁定）

**TimeSimilarity**
```
startMax = max(a.starts_at, b.starts_at)
endMin   = min(a.ends_at_or_now, b.ends_at_or_now)   // ends_at=NULL → 取当前时刻（仍 firing）
if endMin < startMax: return 0
overlap  = endMin - startMax
union    = max(a.duration, b.duration)               // duration = end - start
if union == 0: return 0
return overlap / union
```

**ServiceSimilarity**
```
if a.labels.service == b.labels.service 且非空:   return 1.0
if a.labels.namespace == b.labels.namespace 且非空: return 0.6
return 0.0
```

**LabelSimilarity**（Jaccard，集合元素是 `k=v` 对，**非**仅 key）
```
setA = { (k,v) | k∉excludeKeys }   // excludeKeys = instance, pod, container, node（与指纹排除键一致）
setB = 同
if |setA ∪ setB| == 0: return 0
return |setA ∩ setB| / |setA ∪ setB|
```

**DependencySimilarity**（静态拓扑）
```
svcA, svcB = labels.service；任一为空 → 0
if topo.HasEdge(svcA, svcB) 或 topo.HasEdge(svcB, svcA): return 0.9
if len(topo.CommonNeighbors(svcA, svcB)) > 0:            return 0.5
return 0.0
```
边在拓扑中**视为无向**（`HasEdge` 双向判定）。

**HistoricalSimilarity**
```
定义为「同一 Incident 内两个 fingerprint 的共现比例」：
  incidents(A)   = 含 fingerprint=A 的 Incident 集合
  incidents(B)   = 含 fingerprint=B 的 Incident 集合
  co  = |incidents(A) ∩ incidents(B)|
  total = |incidents(A) ∪ incidents(B)|
  if total == 0: return 0
  return co / total
```
单条 SQL 取回（见 §4.7）。**冷启动无历史时该维恒为 0**（Replay 场景默认即如此，如实标注）。

### 4.3 决策与写入（T2.7）

```
Correlate(ctx, newAlert):
  1. 若 newAlert.status != 'firing' → 跳过（返回 Noop）
  2. 载入候选：status ∈ (open, investigating) 的 Incident 及其关联告警（labels/starts_at/ends_at/fingerprint）
  3. 对每个候选 Incident 的每条关联告警计算 score，取该 Incident 的 max score
  4. 全局取 maxScore / bestIncident
  5. 决策：
     MERGE  → 事务：链接 alert→bestIncident（incident_alerts.correlation_score=maxScore, needs_review=false）
                     并回写 alerts.incident_id
     REVIEW → 同上链接，但 needs_review=true
     NEW    → 事务：INSERT incidents（title/severity/service/started_at 由 newAlert 派生）
                     + 链接该 alert（correlation_score=NULL 或 1.0 视实现，见下）
  6. 返回 CorrelationResult{Action, IncidentID, Score}
```

- **NEW 的 score**：新建 Incident 无「关联对象」，`correlation_score` 记为 `NULL`（语义为「无关联」）。
- **幂等**：同一 `(incident_id, alert_id)` 重复链接走 `ON CONFLICT DO NOTHING`（主键即这一对）。
- **可靠性铁律**：关联失败**不得**让 webhook 失败——`Correlate` 出错只记 error 日志，HTTP 仍返回 200（与 M4「告警主链路零阻塞」原则一致）。

### 4.4 配置模型（T2.1，决策 D1：复用 `correlation_rules` 单行）

在 `correlation_rules` 中固定一行 `name='default'`：

```jsonc
// correlation_rules.rule_config
{
  "weights":    { "time": 0.25, "service": 0.25, "label": 0.20, "dependency": 0.20, "historical": 0.10 },
  "thresholds": { "merge": 0.80, "review": 0.50 }
}
```

- 该行的 `weight` 列不使用（占位 `1.0`）；`enabled` 控制关联总开关。
- **读取**：每次 Correlate 前读一次（或 60s TTL 缓存），保证「改后免重启生效」。
- **写入校验**（`PUT`）：
  - 五个权重 ∈ [0,1]，且 `|Σweights − 1.0| ≤ 0.001`，否则 **422**；
  - `merge`、`review` ∈ [0,1] 且 `merge ≥ review`，否则 **422**；
  - 通过后 `UPDATE ... SET rule_config=$1, updated_at=NOW()`。
- **API 契约**：

| Method | Path | 功能 | 请求体 | 响应体 |
|--------|------|------|--------|--------|
| GET | `/api/v1/correlation/config` | 读当前权重/阈值 | — | `{weights,thresholds,enabled}` |
| PUT | `/api/v1/correlation/config` | 改权重/阈值 | 同上 | 200 + 生效后的配置；非法 → 422 |

### 4.5 拓扑文件（T2.5，决策 D4：`config/topology.yaml`）

```yaml
# 服务依赖边；a: [b] 表示 a 依赖 b（无向判定，a↔b 均算 HasEdge）
services:
  checkout: [payment]
  payment:  [mysql]
  frontend: [checkout, payment]
```

- 加载：启动时读 `IIP_TOPOLOGY_FILE`（默认 `config/topology.yaml`）。
- 文件缺失/为空：**不报错**，依赖维全 0，并记一条 WARN（保证无拓扑也能跑）。
- 校验：出现自环或重复边仅告警，不 fatal。

### 4.6 分层与包结构（新增/改动）

```
internal/
├── correlation/                 # 新增：M2 全部业务逻辑
│   ├── types.go                 # Weights / Thresholds / Config / CorrelationResult / Action
│   ├── similarity.go            # 五个纯函数（Time/Service/Label/Dependency/Historical 的可注入部分）
│   ├── topology.go              # TopologyGraph：HasEdge / CommonNeighbors / LoadYAML
│   ├── score.go                 # CalculateScore(weights) 
│   ├── decide.go                # Decide(score, thresholds)
│   ├── service.go               # Service.Correlate 编排 + 事务
│   ├── repository.go            # 仓储接口（便于单测注入假实现）
│   └── *_test.go                # 单测 + replay_test.go
├── repository/
│   ├── incidents.go             # 新增
│   └── correlation_rules.go     # 新增（读写单行配置）
├── api/
│   └── correlation_config.go    # 新增：GET/PUT 配置 handler（+ 契约校验）
└── db/queries/
    ├── incidents.sql            # 新增
    ├── incident_alerts.sql      # 新增
    └── correlation_rules.sql    # 新增（含共现频率查询）
config/topology.yaml             # 新增
migrations/0003_*.sql            # 新增
testdata/replay/*.json           # 新增
cmd/iip-server/main.go           # 装配 correlation service + 配置路由 + 落库后关联调用
```

### 4.7 sqlc 查询清单（T2.1/T2.6/T2.7）

| 文件 | 查询名 | 用途 |
|------|--------|------|
| `incidents.sql` | `CreateIncident :one` | NEW 档建事件 |
| `incidents.sql` | `ListOpenIncidentsWithAlerts :many` | 取 open+investigating 事件及其关联告警（供逐对评分） |
| `incident_alerts.sql` | `LinkAlertToIncident :exec` | `INSERT ... ON CONFLICT DO NOTHING`，含 score / needs_review |
| `incidents.sql` | `SetAlertIncident :exec` | 回写 `alerts.incident_id` |
| `correlation_rules.sql` | `GetCorrelationConfig :one` | 读单行配置 |
| `correlation_rules.sql` | `UpsertCorrelationConfig :exec` | 写单行配置 |
| `correlation_rules.sql` | `FingerprintCoOccurrence :one` | 历史共现比例（§4.2 定义） |

> 生成方式：编辑 `internal/db/queries/*.sql` → `sqlc generate` → 提交生成物（OPEN-2 已裁定生成物入库）。

### 4.8 迁移变更（0003）

```sql
-- 0003_correlation_m2.up.sql
-- 1) 种子默认关联配置（单行，name='default'）
INSERT INTO correlation_rules (name, description, enabled, rule_config, weight)
VALUES (
  'default',
  'M2 关联引擎默认权重与阈值（V1.0 §5）',
  TRUE,
  '{"weights":{"time":0.25,"service":0.25,"label":0.20,"dependency":0.20,"historical":0.10},
    "thresholds":{"merge":0.80,"review":0.50}}'::jsonb,
  1.0
)
ON CONFLICT (name) DO NOTHING;

-- 2) REVIEW 档落库所需：给关联边加「待人工确认」标记
ALTER TABLE incident_alerts
  ADD COLUMN IF NOT EXISTS needs_review BOOLEAN NOT NULL DEFAULT FALSE;

CREATE INDEX IF NOT EXISTS idx_incident_alerts_needs_review
  ON incident_alerts(needs_review) WHERE needs_review;
```

`0003_correlation_m2.down.sql`：`DROP INDEX` + `ALTER TABLE ... DROP COLUMN needs_review` + `DELETE FROM correlation_rules WHERE name='default'`。
（**三连 up→down→up 必须可重复**，纳入验收。）

### 4.9 配置新增

| 环境变量 | 默认 | 说明 |
|----------|------|------|
| `IIP_TOPOLOGY_FILE` | `config/topology.yaml` | 依赖拓扑文件路径 |
| `IIP_CORRELATION_ENABLED` | `true` | 关联总开关；`false` 时 webhook 落库后不关联 |

### 4.10 触发时机（T2.7，决策 D3）

在 `POST /api/v1/alerts/webhook` 中，**五阶段流水线 persist 成功之后**，对本次**新增的 firing 告警**逐条调用 `correlationSvc.Correlate`：

- 同步调用（贴合 V1.0 §4.2 流水线 `Persist → Correlate`）；
- `IIP_CORRELATION_ENABLED=false` 时跳过；
- 关联异常只记日志，**不改 HTTP 结果**。

---

## 五、迁移变更清单（AGENTS.md 约束 4）

| 项 | 内容 |
|----|------|
| 新增文件 | `migrations/0003_correlation_m2.up.sql` / `.down.sql` |
| 表结构变更 | `incident_alerts` 增列 `needs_review BOOLEAN NOT NULL DEFAULT FALSE` + 部分索引 |
| 数据种子 | `correlation_rules` 增一行 `name='default'`（权重+阈值） |
| 不动 | `0001_*`、`0002_*`（冻结） |
| 回滚 | 见 §4.8 down 脚本 |
| 验证 | `migrations/migrate_test.go` 扩展：up→down-all→up 三连，断言版本号、`needs_review` 列存在、default 行存在 |

---

## 六、任务拆解（10 天，对齐总计划工期）

| 日 | 任务 | 产出 |
|----|------|------|
| D1 | T2.1 配置模型 + 0003 迁移 + 配置 API | 单行配置可读可改、校验生效 |
| D2 | T2.2/T2.3 时间 + 服务相似度 | 两个纯函数 + 单测 |
| D3 | T2.4 标签相似度 | Jaccard + 单测 |
| D4 | T2.5 拓扑加载 + 依赖相似度 | YAML + 图算法 + 单测 |
| D5 | T2.6 历史共现查询 | SQL + 单测 |
| D6–D7 | T2.7 Correlate 主流程 + 事务 + 接入 webhook | service + 集成测试 |
| D8 | T2.8 Replay 数据集（3 场景）+ 评分脚本 | 数据集 + 回放测试 |
| D9 | T2.9 调权重、固化默认值 | 0003 种子 = 调优值 |
| D10 | 文档回填（README/decisions/本手册）+ 验收 | 验收报告 |

### 降级预案（超期 2 周时启用，计划 §6）

砍 T2.5/T2.6（拓扑与历史维），退化三维模型（时间 0.35 / 服务 0.35 / 标签 0.30），拓扑与历史共现移至 V1.1。

---

## 七、测试策略

### 7.1 分层

| 层 | 范围 | 手段 |
|----|------|------|
| 纯函数单测 | 五个相似度、`CalculateScore`、`Decide`、拓扑图 | 表驱动，无需 DB |
| 编排单测 | `Service.Correlate` 的三档分支 | 假仓储注入（对齐 M1 的 `fakeRepo` 风格） |
| 集成测试 | 关联写入 `incidents`/`incident_alerts`、共现 SQL | 真 PG（`testsupport.NewDB`，`IIP_TEST_PG_URL`） |
| 回放评估 | 三场景精确率/召回率 | `replay_test.go` 读 `testdata/replay/` |
| 性能 | 1000 条告警耗时 | `go test -bench` 或计时测试 |

### 7.2 用例清单（最低要求）

- **T**：窗口不重叠→0；完全包含→1；`ends_at=NULL` 按 now；`duration=0` 双方→0。
- **S**：同 service→1.0；仅同 namespace→0.6；都不同→0。
- **L**：完全相同→1；无交集→0；仅 key 相同但 value 不同→不计入交集。
- **D**：直接边→0.9；共同邻居→0.5；无关→0；service 缺失→0。
- **H**：无历史→0；共现 1/2→0.5（构造库数据）。
- **决策**：0.80/0.50 边界（含恰好等于）；`merge<review` 配置被 422 拒绝；权重和≠1 被 422 拒绝。
- **Correlate**：MERGE 链接既有事件并写 score；REVIEW 链接且 `needs_review=true`；NEW 建事件且 score=NULL；`resolved` 告警跳过；仓储报错时 webhook 仍 200。
- **配置热生效**：改配置后，下一条告警按新配置计算（集成测试演示）。

### 7.3 反向验证（防「假绿」）

- 把 `Decide` 的 `merge ≥ review` 校验去掉 → 对应非法配置用例必须转红；
- 把 TimeSimilarity 的 `ends_at` 缺省处理改回「0 值」→ `ends_at=NULL` 用例必须转红；
- 把 `needs_review` 写入去掉 → REVIEW 用例必须转红。

---

## 八、验收清单（全过才算 M2 完成）

```text
[ ] 五维评分 + 三档决策实现，correlation 包单测全绿
[ ] 权重/阈值存于 correlation_rules 单行，GET/PUT 配置 API 可用，非法配置 422
[ ] 改配置后无需重启即对下一条告警生效（集成验证）
[ ] Replay 三场景：精确率 100% / 召回率 ≥90% / 杂告警不误合并
[ ] correlation 包测试覆盖率 ≥90%
[ ] 1000 条告警回放耗时 <60s
[ ] 迁移 0003 up→down→up 三连通过；needs_review 列与 default 配置行就位
[ ] webhook 落库后同步关联生效；关联失败不影响 HTTP 200
[ ] go build ./... / go vet ./... / go test ./... / gofmt 全绿
[ ] go.mod 无新增依赖（YAML 解析用 gopkg.in/yaml.v3 需评估，见 OPEN-M2-1）
[ ] README 新增「M2 验收状态」表；docs/decisions.md 记录 M2 决策
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
go test -cover ./internal/correlation/...      # 断言 ≥90%

# 2) 回放评估（三场景精确率/召回率 + 1000 条耗时）
go test ./internal/correlation/ -run Replay -v
go test ./internal/correlation/ -bench BenchmarkCorrelate1000 -benchtime 1x

# 3) 起服务；投递两条「同服务同期」告警
go run ./cmd/iip-server &   # 自动迁移到版本 3
curl.exe -s -X POST http://localhost:8080/api/v1/alerts/webhook -H "Content-Type: application/json" --data-binary "@testdata/replay/scenario1_service_storm.json"
# 断言：两条告警合并到同一 incident（GET 配置与库内 incident_alerts 核对）

# 4) 改配置热生效
curl.exe -s -X PUT http://localhost:8080/api/v1/correlation/config -H "Content-Type: application/json" -d '{...merge:0.99...}'
# 再投一条同服务告警 → 断言：不再合并（阈值提高即时生效）

# 5) 非法配置
curl.exe -s -o NUL -w "%{http_code}`n" -X PUT http://localhost:8080/api/v1/correlation/config -H "Content-Type: application/json" -d '{"weights":{"time":2,...}}'
# 断言：422
```

---

## 十、风险与开放项

| 编号 | 开放项 | 当前倾向 | 阻塞于 | 状态 |
|------|--------|----------|--------|------|
| OPEN-M2-1 | YAML 解析依赖 | 引入 `gopkg.in/yaml.v3`（唯一新增依赖，需你确认；或手写极简解析器零依赖） | 你对「go.mod 无新增依赖」的取舍 | **待确认** |
| OPEN-M2-2 | REVIEW 档落库方式 | 复用 `incident_alerts` 加 `needs_review` 列（不新增表） | 你确认手册时可否决，改为独立 `correlation_reviews` 表 | **待确认** |
| OPEN-M2-3 | NEW 档 `correlation_score` 取值 | `NULL`（语义=无关联） | — | 暂定 |
| OPEN-M2-4 | 历史共现的统计口径 | 并集为分母（§4.2） | 有真实数据后复核 | 暂定 |
| RISK-M2-1 | 关联漏并/误并不达预期 | — | Replay 指标差且调参无效 | 启用 §六 降级预案（三维） |
| RISK-M2-2 | 依赖维全 0（无拓扑） | — | Replay 中 DependencySimilarity 恒 0 | M2 验收时评估是否补简单拓扑 |
| RISK-M2-3 | Correlate 复杂度 O(事件数×告警数) | — | 1000 条回放超 60s | 优化候选集（仅取近窗口事件） |
| RISK-M2-4 | 配置缓存导致「改后不生效」 | 每轮读一次或 60s TTL | — | 用集成测试锁死热生效 |

---

## 十一、变更记录

| 日期 | 变更内容 | 原因 | 影响范围 |
|------|----------|------|----------|
| 2026-09-29 | 初稿（待确认） | M2 启动 | 全手册 |
