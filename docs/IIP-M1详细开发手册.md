# IIP M1 详细开发手册：告警接入与持久化

> 版本：v1.0（对齐版）｜日期：2026-09-25｜状态：AI 起草，**待技术负责人确认**
> 本文性质：**M1 的唯一开发依据**。范围、契约、迁移与验收以本文为准。
>
> 上游依据（优先级从高到低）：
> 1. `docs/IIP-V1.0-技术设计文档.md` §4（Alert 接入与处理流程）
> 2. `docs/IIP-M0详细开发计划.md` §4（M0 → M1 交接物）
> 3. `AGENTS.md`（项目协作约束）
> 4. `docs/decisions.md`（ADR 与悬而未决项登记册）
>
> 冲突处理：与上游文档冲突时**先停下提问，不得自行扩范围**（AGENTS.md 约束 1）。

---

## 〇、M1 完成定义（DoD）

**一句话判据**：`Alertmanager` 真推一条告警 → 库里出现该告警行；再推一条重复的 → 表内**不增行**且 HTTP **200**；推一条 `resolved` → 原行状态变更、**不新增行**。

可验证清单见 §八。**未真实执行过的项一律标注「待环境就绪后执行」，不得写成「通过」**（AGENTS.md 约束 12）。

---

## 一、M0 交接物核对（开工前必读）

M0 已为 M1 铺好的接缝，以及 M1 需要补的动作：

| # | 交接物 | 实际位置 | 状态 | M1 要做什么 |
|---|--------|----------|------|-------------|
| 1 | chi 路由挂载点 | `cmd/iip-server/main.go:207-209`（三条注释） | 已留 | 把 handler 接上去；`newRouter` 保持零业务 |
| 2 | sqlc 配置 | `sqlc.yaml`（version `"2"`，out=`internal/db`，pgx/v5，jsonb→`json.RawMessage`） | 已就绪 | **先跑 `sqlc generate`**（M0 明确未生成任何 Go 产物） |
| 3 | sqlc 查询骨架 | `internal/db/queries/alerts.sql` | 已就绪（3 个查询） | 定稿参数 + 追加查询，**不改名** |
| 4 | Alertmanager 推送源 | `deploy/alertmanager/alertmanager.yml:43` → `host.docker.internal:8080/api/v1/alerts/webhook` | 已就绪（含 `send_resolved: true`） | M1 实现后自动由 404 变 200 |
| 5 | 连接池与迁移 | `internal/store/store.go`（`Connect` / `Migrate` / `HealthCheck`） | 已就绪 | **复用**，不另建池；新迁移会被自动应用 |
| 6 | handler 目录 | `internal/api/`（仅 `.gitkeep`） | 占位 | handler 落这一层 |
| 7 | 集成测试的 PG 套路 | `scripts/verify-runtime.py` + compose `iip-postgres` | 已就绪 | 复用同一套「随机建库 → 迁移 → 跑 → DROP」 |

---

## 二、与 M0 仓库实测状态的对齐表

> 本节是开工前必须逐条确认的 7 处接缝。**A/B/C 三项必修**。

| 编号 | 接缝 | 事实依据 | 处置 | 状态 |
|------|------|----------|------|------|
| **A** | `sqlc generate` 从未执行 | `sqlc.yaml:6` 明确「M0 只落配置与查询骨架，**不生成任何 Go 产物**」 | T1.2 第一步执行；并决定生成物是否入库 | 待裁定（OPEN-2） |
| **B** | 「DB 约束兜底去重」缺前提 | `migrations/0001_init.up.sql:37` 只有**非唯一**的 `idx_alerts_fingerprint`；表上**没有** fingerprint 唯一约束 | 新增迁移建**部分唯一索引**（见 §五） | 必修 |
| **C** | 迁移号有歧义 | `AGENTS.md` 约束 10 与 `docs/migration-notes.md:19` 写「只能新增 `0003_*.sql`」，但 **0002 从未存在**（`migration-notes.md:15-17` 说明 down 与 up 共用 0001） | 需技术负责人裁定用 `0002_` 还是沿用文档的 `0003_` | 待裁定（OPEN-1） |
| D | 查询名不得重命名 | 骨架里的真实名是 `GetAlertByFingerprint` / `InsertAlert` / `ListAlertsByIncident` | 只在 `alerts.sql` 追加与定稿，**不重命名 M0 交接物** | 已明确 |
| E | 分层落位 | `internal/store` 是「连接池 + 迁移」，**不是** repository；`internal/db` 是 sqlc 生成目标 | 仓储层**新建、与 `store` 并列**，复用其 `*pgxpool.Pool`，包装 sqlc 的 `internal/db`；**禁止第二个连接池** | 已明确 |
| F | `resolved` 生命周期未被 V1.0 覆盖 | `alertmanager.yml:44` 有 `send_resolved: true` → 同 fingerprint 会再推 `status=resolved` | 必须走**更新既有行**，不是插新行（见 §4.4） | 必修 |
| G | 字段级陷阱 6 条 | `endsAt` 哨兵 / `severity` 枚举收窄 / payload 自带 fingerprint / NOT NULL JSONB / `incident_id` 归属 / 幂等 | 逐条写死在 §4.2 与 §4.3 | 已明确 |

**关于 `0002` vs `0003`**：`golang-migrate` 不要求版本号连续，两者都能跑；差别只在可读性——不连续的号会让人反问「0002 去哪了」。**建议用 `0002_`**，但这是项目约定，需技术负责人落槌。

---

## 三、范围

### 3.1 做什么（M1 锁定）

| 优先级 | 功能 | 验收标准摘要 |
|--------|------|-------------|
| P0 | `POST /api/v1/alerts/webhook` 接收 Alertmanager v4 告警 | 合法 payload → 200；告警落库 |
| P0 | 五阶段流水线：Validate → Normalize → Fingerprint → Deduplicate → Persist | 每阶段有独立单测 |
| P0 | 指纹算法（排除动态 label 的 SHA256） | 仅 `instance` 不同 → 同一指纹 |
| P0 | 去重（应用层判定 + DB 约束兜底） | 重复 firing 不增行 |
| P0 | `resolved` 生命周期 | 状态更新而非新增行 |
| P0 | 查询接口骨架就位（供 M3/M5 使用） | 编译通过、有集成测试 |
| P1 | `GET /api/v1/alerts/{id}` | 单条可查 |

### 3.2 明确不做（Out-of-Scope，锁定）

| 不做的功能 | 原因 | 何时考虑 |
|------------|------|----------|
| **Correlation**（合并到 Incident / 创建 Incident） | 属 M2，是技术核心，需要 replay 评估集 | M2 |
| Incident 查询接口的业务逻辑（`GET /api/v1/incidents`） | 属 M3（Incident 生命周期） | M3 |
| Evidence 采集 | 属 M4 | M4 |
| 前端页面 | 属 M5 | M5 |
| Redis 队列 | V1.0 明确 `IIP_REDIS_DISABLED` 默认 true，走 DB polling | M6 起评估 |
| 告警的**人工修正**（改 severity / 忽略） | 需 Incident 上下文与审计 | M3 |
| `fingerprint` 排除表做成可配置 | M1 按 V1.0 §4.3 写死四键即可 | 有真实需求时（OPEN-4） |

> 开发中若有人提出上表之外的功能，**直接拒绝**；确需新增则走变更流程。

### 3.3 AGENTS.md 约束在 M1 的映射

| 约束 | M0 原文 | M1 处置 |
|------|---------|---------|
| 2 | 「**本阶段（M0）**禁止编写任何业务逻辑」 | **M0 限定随 M1 启动失效**——M1 的全部工作就是写业务逻辑。需技术负责人改写为「M1 范围红线：不得越出 §3.2 Out-of-Scope」 |
| 4 | 「migrations/ 下任何 SQL 变更，必须单独列出表/列/索引/约束清单」 | M1 有 1 处新迁移 → 已在 §五 按此格式给出 |
| 5 / 12 | 验收命令必须真实执行过 | 全手册的验收项一律实跑后回填 |
| 6 | 不许跳过 CI 门禁 | 新代码同样受 `golangci-lint` / `gofmt` 门禁 |
| 7 | 依赖钉具体版本 | M1 **不引入任何新依赖**（见 §4.5），此约束无新增负担 |
| 9 | Windows 环境、LF 行尾、命令给 PowerShell 语法 | 本文命令同时给 bash / PowerShell |

> 团队级 P0 规则（禁 emoji 作图标、禁紫→粉渐变、禁 AI 模板味文案）：M1 为纯后端，**不触发**；自 M5（Web UI）起适用。

---

## 四、详细设计

### 4.1 Webhook 契约

**端点**：`POST /api/v1/alerts/webhook`
**Content-Type**：`application/json`

请求体为 Alertmanager v4 webhook payload（完整样例见 `docs/IIP-V1.0-技术设计文档.md` §4.1）。关键结构：

```json
{
  "version": "4",
  "groupKey": "namespace:production/alertname:HighLatency",
  "status": "firing",
  "receiver": "iip-webhook",
  "groupLabels":   { "alertname": "HighLatency", "namespace": "production" },
  "commonLabels":  { "service": "checkout", "team": "platform" },
  "commonAnnotations": { "summary": "..." },
  "alerts": [
    {
      "status": "firing",
      "labels": { "alertname": "HighLatency", "service": "checkout",
                  "instance": "checkout-abc123", "namespace": "production",
                  "severity": "critical" },
      "annotations": { "summary": "High latency on checkout-abc123", "value": "1250ms" },
      "startsAt": "2026-09-24T18:31:00Z",
      "endsAt":   "0001-01-01T00:00:00Z",
      "fingerprint": "abc123def456"
    }
  ]
}
```

**响应契约（状态码矩阵，必须写死）**：

| 场景 | 状态码 | 响应体 | 说明 |
|------|--------|--------|------|
| 全部成功 | 200 | `{"accepted":1,"skipped":0,"updated":0,"rejected":0}` | 计数是**单条告警**维度，不是批次 |
| 全部重复（已存在 firing） | **200** | `{"accepted":0,"skipped":1,...}` | **重复不是错误**，不能返 409/500 |
| 部分单条非法 | **200** | `{...,"rejected":2,"errors":[...]}` | 合法条目照常落库，整批不失败 |
| 请求体非 JSON / JSON 畸形 | 400 | `{"error":"..."}` | |
| 请求体不是合法 UTF-8 | 400 | `{"error":"请求体不是合法的 UTF-8：…"}` | M6 T6.6 补：`encoding/json` 不校验字符串的 UTF-8 合法性，非法字节会一路到 PG 才以 22021 报错，届时已伪装成 503「存储不可用」。故在**解码之前**拦（ADR-027） |
| 请求体超过大小上限 | 413 | `{"error":"payload too large"}` | 上限 `IIP_WEBHOOK_MAX_BYTES`（默认 1048576） |
| `alerts` 为空数组 | 200 | `{"accepted":0,...}` | Alertmanager 的空 group 属正常，不作错误 |
| 数据库不可用 | **503** | `{"error":"storage unavailable"}` | 故意的：非 2xx 会触发 Alertmanager 重试 |
| 方法不对 | 405 | — | chi 默认 |

**幂等要求**：同一 payload 重复投递任意次，结果必须一致（表内行数与内容不变），且始终 200。**这是硬要求**——Alertmanager 在超时/非 2xx 时会重试。

**入口统一走 `readBody`（M6 T6.6，ADR-027）**：任何需要读 JSON 请求体的 handler
都必须调用 `internal/api/body.go` 的 `readBody(w, r, maxBytes, logger)`，不要自己写
`io.ReadAll` / `json.NewDecoder(r.Body)`。前者会漏掉 UTF-8 校验，后者会把
`MaxBytesReader` 的超限错误并进 `Decode` 的返回里（413 变 400）。**新入口除了接入
`readBody`，还要把用例加进 `body_test.go` 的 `jsonEntries()`** —— 那是矩阵判据唯一的
维护点，漏加就等于没被覆盖。

**超时**：handler 内 `context.WithTimeout(r.Context(), 5*time.Second)`。

### 4.2 处理流水线（五阶段）

```
POST body → Validate → Normalize → Fingerprint → Deduplicate → Persist → 200/计数
```

#### 阶段一：Validate（逐条）

| 规则 | 失败处置 |
|------|----------|
| body 能解析为 JSON（**不** `DisallowUnknownFields`，向前兼容上游新增字段） | 整批 400 |
| `version` 存在（Alertmanager v4 = `"4"`） | 整批 400 |
| `alerts` 为数组（可为空） | 整批 400 |
| 单条 `labels` 非空且含 `alertname` | 该条 rejected |
| 单条 `status` ∈ {`firing`, `resolved`} | 该条 rejected |
| 单条 `startsAt` 可解析（RFC3339） | 该条 rejected |

> 「整批 400」只用于**结构级**错误；**字段级**错误一律降级为该条 rejected，不牵连同批其他告警。

#### 阶段二：Normalize（逐条 → 领域对象）

| 字段 | 规则 | 陷阱 |
|------|------|------|
| `source` | 固定 `"alertmanager"` | — |
| `severity` | 映射表 + **未知值兜底**（见下） | CHECK 只允许三值，未知值直接抛错会让上游配置问题变成 500 |
| `status` | **以单条 `alerts[i].status` 为准**；顶层 `status` 仅用于一致性告警 | 两者理论上一致，不一致时打 WARN 并按单条处理 |
| `labels` | 原样；`nil` → `{}` | 列是 `NOT NULL DEFAULT '{}'` |
| `annotations` | 原样；`nil` → `{}` | 同上 |
| `starts_at` | RFC3339 → `TIMESTAMPTZ` | — |
| `ends_at` | **哨兵值 `0001-01-01T00:00:00Z` / 零值 / 缺失 → `NULL`** | **不处理**会写进一个公元 1 年的时间戳 |
| `raw_payload` | 存**该条 alert 的原始 JSON**（Evidence-First） | 不是整批 payload；是否另存整批上下文见 OPEN-7 |
| `incident_id` | **M1 恒为 `NULL`** | M2 才有关联，此处不得臆造 |

**severity 映射表（必须带兜底）**：

| 输入（大小写不敏感，取首值） | 输出 |
|------------------------------|------|
| `critical` / `page` / `emergency` / `fatal` / `error` | `critical` |
| `warning` / `warn` | `warning` |
| `info` / `notice` / `none` / `debug` | `info` |
| **缺失 / 空串 / 其他未知值** | `info` + 打 WARN 日志（含原值） |

> 兜底取 `info` 而非报错，理由：上游 severity 命名不可控，报错会把「上游改了个标签名」升级成「告警全丢」。是否改为兜底 `warning` 见 OPEN-3。

#### 阶段三：Fingerprint

见 §4.3。

#### 阶段四：Deduplicate

见 §4.4。

#### 阶段五：Persist

- 新告警 → `INSERT`（`incident_id = NULL`）。
- `resolved` 命中既有行 → `UPDATE alerts SET status='resolved', ends_at=$2, updated_at=NOW() WHERE id=$1`。
- 注意：`updated_at` 在 0001 里**没有触发器**，必须显式写 `NOW()`，否则永远停在插入时刻。

### 4.3 Fingerprint 算法

**按 `docs/IIP-V1.0-技术设计文档.md` §4.3 原样落地**：

```go
// GenerateFingerprint 基于稳定 label 集合生成 SHA256 指纹。
// 排除动态 label —— 否则容器重建、Pod 漂移都会换指纹，同一故障无法去重。
func GenerateFingerprint(labels map[string]string) string {
    stable := make(map[string]string, len(labels))
    for k, v := range labels {
        if !excludeKeys[k] {
            stable[k] = v
        }
    }
    keys := make([]string, 0, len(stable))
    for k := range stable {
        keys = append(keys, k)
    }
    sort.Strings(keys)

    var buf bytes.Buffer
    for _, k := range keys {
        buf.WriteString(k)
        buf.WriteString("=")
        buf.WriteString(stable[k])
        buf.WriteString(",")
    }
    sum := sha256.Sum256(buf.Bytes())
    return hex.EncodeToString(sum[:])   // 64 字符，列宽 VARCHAR(128) 有余
}
```

**排除键（写死四键）**：`instance`、`pod`、`container`、`node`。

**两条硬规则**：

1. **必须忽略 payload 里的 `alerts[i].fingerprint`**。Alertmanager 自算的指纹**包含 `instance`**，机器/容器重建后必然改变；而 IIP 要做的是「同一故障跨实例重建仍能去重」。两者语义不同，以 IIP 自算为准。
2. **不得把 `annotations` 纳入指纹**——注解值（如 `value: 1250ms`）会随每次评估变化，纳入即等于永不重复。

### 4.4 去重与 resolved 生命周期

「库中最新一条」由既有查询 `GetAlertByFingerprint`（`ORDER BY starts_at DESC, id DESC LIMIT 1`）取得。

> **`id DESC` 不可省略**（M1 端到端实测发现并修复，见 `docs/decisions.md` ADR-007）。
> 本手册初版写作 `ORDER BY starts_at DESC LIMIT 1`，该写法有缺陷：
> Alertmanager 重发同一告警时 `startsAt` 与首次**完全相同**，
> 于是「已 resolved 的旧行」与「重新起火的 firing 新行」在 `starts_at` 上并列，
> 而并列时返回哪一行是**未定义**的。实测中同一轮运行的两次调用分别取到了不同行，
> 后果是 resolved 被误判为「重复 resolved」而跳过，告警**永远停在 firing 状态**。
> 以 `id DESC` 收尾：同一指纹内 `id` 单调递增，最大者即最近写入的那条。

| 入站 `status` | 库中最新一条 | 动作 | 计入 |
|---------------|--------------|------|------|
| `firing` | 无 | `INSERT` | `accepted` |
| `firing` | `firing` | **跳过**（按 V1.0：「相同 fingerprint + firing → 跳过」，不回写） | `skipped` |
| `firing` | `resolved` | `INSERT`（重新起火，**保留历史行**） | `accepted` |
| `resolved` | `firing` | `UPDATE` 该行：`status='resolved'`、`ends_at=payload.endsAt`（缺失则 `NOW()`） | `updated` |
| `resolved` | `resolved` | 跳过 | `skipped` |
| `resolved` | 无 | 跳过 + WARN（上线前已起火、M1 没见过的告警） | `skipped`（见 OPEN-5） |
| `suppressed` | — | Alertmanager 不会推；CHECK 允许该值，防御式映射为跳过 | `skipped` |

**并发兜底（必做）**：同一 fingerprint 的并发投递无法靠「先查后写」保证唯一。已在 §五 增加部分唯一索引，并把 `InsertAlert` 定稿为：

```sql
-- name: InsertAlert :one
INSERT INTO alerts (
    fingerprint, source, severity, status, labels, annotations,
    starts_at, ends_at, raw_payload, incident_id
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
ON CONFLICT (fingerprint) WHERE status = 'firing' DO NOTHING
RETURNING *;
```

仓储层把 `pgx.ErrNoRows` 翻译为 `(nil, inserted=false, nil)` → 计入 `skipped`。**这样即使应用层判定被并发绕过，数据库这一层也不会产生重复 firing 行。**

### 4.5 分层与包结构

| 层 | 路径 | 职责 | 约束 |
|----|------|------|------|
| 装配 | `cmd/iip-server/main.go` 的 `newRouter` | 只做 wiring | 零业务（AGENTS 约束 2 的 M1 版） |
| Handler | `internal/api/alert_webhook.go` | HTTP 解析、限大小、状态码、计数汇总 | 单文件 ≤ 300 行 |
| 纯函数 | `internal/alert/{validate,normalize,fingerprint}.go` | 无 I/O、可表驱动单测 | 依赖只向下 |
| 领域模型 | `internal/alert/model.go` | `Alert` 领域对象 + 派生常量 | 不依赖 `internal/db` |
| 服务编排 | `internal/alert/service.go` | 串五阶段、编排事务外流程 | 通过接口依赖仓储 |
| 仓储接口 | `internal/alert/repository.go` | `interface`，供 handler 单测注入假实现 | 定义在消费方 |
| 仓储实现 | `internal/repository/alerts.go` | 包装 sqlc 生成的 `internal/db` | 持有 `*pgxpool.Pool` |
| sqlc 产物 | `internal/db/`（`db.go` / `models.go` / `alerts.sql.go`） | 生成物，不手改 | 由 `sqlc generate` 产生 |

**依赖方向**：`api → alert → repository → db / store`，**只向下，不向上**。

**M1 不引入任何新依赖**（`go.mod` 无变化）：SHA256/hmac 用标准库，HTTP 用 chi，DB 用 pgx 与 sqlc 产物。

### 4.6 sqlc 查询清单

**保留（M0 交接物，只定稿不改名）**：

| 查询 | 类型 | M1 定稿说明 |
|------|------|-------------|
| `GetAlertByFingerprint` | `:one` | 语义就是「该指纹的最新一条」，供去重判定。**不加 status 过滤**——需要知道最新一条是 firing 还是 resolved |
| `InsertAlert` | `:one` | 加 `ON CONFLICT (fingerprint) WHERE status='firing' DO NOTHING`（见 §4.4） |
| `ListAlertsByIncident` | `:many` | M1 不调用（M3/M5 用），保留不动 |

**追加**：

```sql
-- name: ResolveAlert :exec
-- M1 用于 resolved 生命周期：更新既有行而非插新行（见手册 §4.4）。
-- 注意：updated_at 在 0001 中无触发器，必须显式 NOW()。
UPDATE alerts
SET status = 'resolved', ends_at = $2, updated_at = NOW()
WHERE id = $1;

-- name: GetAlert :one
-- M1 可选：支撑 GET /api/v1/alerts/{id}（P1）。
SELECT * FROM alerts WHERE id = $1;
```

> `alerts.sql` 里现有两处 `TODO(M1)` 就此定稿：`GetAlertByFingerprint` 的 status 过滤问题 → **不过滤**；`InsertAlert` 的 `incident_id` → **M1 恒传 NULL**。注释随定稿更新。

### 4.7 配置新增

| 键 | 环境变量 | 默认 | 说明 |
|----|----------|------|------|
| `webhook-max-bytes` | `IIP_WEBHOOK_MAX_BYTES` | `1048576`（1 MiB） | 请求体上限，超限 413 |

按 `internal/config/config.go` 现有模式加：`fs.Int64(...)` + `k.Int64(...)` + `Validate()` 中校验为正数，并在 `config_test.go` 补表驱动用例。

---

## 五、迁移变更（AGENTS.md 约束 4 要求的清单）

**变更清单**（供逐行审查）：

| 类别 | 内容 | 数量 |
|------|------|------|
| 表 | 无新增 | 0 |
| 列 | 无新增、无修改 | 0 |
| **索引** | **新增部分唯一索引 `uq_alerts_fingerprint_firing ON alerts(fingerprint) WHERE status = 'firing'`** | **1** |
| 约束 | 无新增 CHECK（复用 0001 的 `chk_alerts_status` / `chk_alerts_severity`） | 0 |
| 数据 | 无数据迁移 | — |

**依据**：M1 的去重需要数据库层兜底（§4.4），而 `0001_init.up.sql` 已冻结（AGENTS.md 约束 10），只能新增迁移文件。

**up**（文件名待 OPEN-1 裁定，暂记 `0002_alerts_fingerprint_firing_uq.up.sql`）：

```sql
-- M1 去重兜底：同一 fingerprint 最多只能有一条处于 firing 的行。
-- 与 0001 的 idx_alerts_fingerprint（非唯一）并存：后者服务普通查询，本索引服务唯一性。
-- 设计要点：条件只覆盖 status='firing' —— resolved 行可以有多条历史，
--          否则「起火 → 恢复 → 再起火」的历史会被覆盖掉。
CREATE UNIQUE INDEX uq_alerts_fingerprint_firing
    ON alerts (fingerprint)
    WHERE status = 'firing';
```

**down**：

```sql
DROP INDEX IF EXISTS uq_alerts_fingerprint_firing;
```

**执行注意**：

1. 迁移经 `migrations/embed.go` 嵌入二进制，`iip-server` 启动时自动应用（`store.Migrate`），**无需手工操作**。
2. 注意：若目标库中已存在同一 fingerprint 的多条 firing 行，建索引会失败。M1 是新库，风险低；若复用旧库，需先清理脏数据。清理语句**不在迁移里**（迁移必须可在干净库上无条件成功）。
3. 验收：`up → down → up` 三连通过，且第二条同指纹 firing 的 INSERT 被拒（`ON CONFLICT DO NOTHING` 使其静默跳过）。

---

## 六、任务拆解（8 天，对齐总计划工期）

| 日 | 任务 | 预算 | 产出物 |
|----|------|------|--------|
| D1 上午 | T1.1 新增迁移 + 本地三连验证 | 2h | 新迁移文件 + 验证记录 |
| D1 下午 | T1.2 `sqlc generate` + 查询定稿 | 3h | `internal/db/` 生成物 + `alerts.sql` 定稿 |
| D2-D3 | T1.3 领域模型 + 三个纯函数 + 单测 | 2d | `internal/alert/{model,validate,normalize,fingerprint}.go` + `_test.go` |
| D3-D4 | T1.4 仓储接口 + 实现 + 集成测试 | 1.5d | `internal/alert/repository.go`、`internal/repository/alerts.go` |
| D4-D5 | T1.5 handler + 路由装配 + handler 测试 | 1.5d | `internal/api/alert_webhook.go` + `main.go` 挂载 |
| D6 | T1.6 端到端联调（Alertmanager 真推） | 1d | 联调记录 + 修正 |
| D7 上午 | T1.7 质量门禁 + CI | 0.5d | 全绿证据 |
| D7 下午 | T1.8 文档回填 + 验收 | 0.5d | README / decisions / 本手册变更记录 |

> 规则：单任务超时 1.5h 即记录阻塞点并跳过，当天先完成「可 commit 的最小状态」。

### 各任务验收命令

**T1.1**
```powershell
# 起库（若已跑着可跳过）
docker run --name iip-pg -e POSTGRES_PASSWORD=dev -e POSTGRES_DB=iip -p 5432:5432 -d postgres:16-alpine
migrate -path migrations -database "postgres://postgres:dev@localhost:5432/iip?sslmode=disable" up
migrate -path migrations -database "postgres://postgres:dev@localhost:5432/iip?sslmode=disable" down-all
migrate -path migrations -database "postgres://postgres:dev@localhost:5432/iip?sslmode=disable" up   # 再跑一遍
```
断言：三连无报错；`\d alerts` 能看到新索引；第二条同指纹 firing 被静默跳过。

**T1.2**
```bash
sqlc generate          # 需 sqlc CLI v1.27.0
go build ./...         # 生成物能编译
```

**T1.3 - T1.5**
```bash
go test ./internal/... -count=1
golangci-lint run ./...
gofmt -l .             # 必须零输出
```

**T1.6（端到端真推）**
```bash
# Alertmanager 的 receiver 已在 compose 里配好 send_resolved: true
docker compose -f deploy/docker-compose.yml --profile app up -d
# 触发 Prometheus 里的示例规则（T0.6 已备），观察 IIP 侧
curl.exe -s http://localhost:8080/healthz
```
断言：库里出现 `source='alertmanager'` 的行；重复推送不增行；`resolved` 使原行 `status` 变更。

---

## 七、测试策略

### 7.1 分层

| 层 | 对象 | 手段 | 目标 |
|----|------|------|------|
| 单元 | `validate` / `normalize` / `fingerprint` | 表驱动，无外部依赖 | 行覆盖率 ≥ 90% |
| 集成 | `internal/repository` | 真 PG（随机建库） | 覆盖 §4.4 全部 7 种去重分支 |
| 接口 | handler | `httptest` + 假仓储（单元）/ 真仓储（集成） | 覆盖 §4.1 状态码矩阵 |
| 回归 | M0 既有断言 | 重跑 `scripts/verify-runtime.py` | 51 项不得回退 |

集成测试需真实 PG，通过环境变量指定；未设置时 `t.Skip`（保持 `go test ./...` 在无库环境可跑通）：

```go
// IIP_TEST_PG_URL=postgres://postgres:dev@127.0.0.1:5433/iip?sslmode=disable
```

建库/清理套路复用 `scripts/verify-runtime.py`：每次随机库名 `iip_m1_<8hex>` → 迁移 → 跑 → `DROP DATABASE`。

### 7.2 用例清单（最低要求）

| ID | 层 | 输入 | 期望 |
|----|----|------|------|
| U-01 | fingerprint | labels 键顺序不同 | 同一指纹 |
| U-02 | fingerprint | 仅 `instance` 不同 | 同一指纹 |
| U-03 | fingerprint | `pod`/`container`/`node` 变化 | 同一指纹 |
| U-04 | fingerprint | `alertname` 不同 | 不同指纹 |
| U-05 | fingerprint | 空 label 集 | 不 panic，返回确定性值 |
| U-06 | normalize | `endsAt = 0001-01-01T00:00:00Z` | `ends_at = NULL` |
| U-07 | normalize | `severity = page` | `critical` |
| U-08 | normalize | `severity` 缺失 | `info` + WARN |
| U-09 | normalize | `labels`/`annotations` 为 `null` | 落库为 `{}` |
| U-10 | validate | `labels` 无 `alertname` | rejected，不 panic |
| U-11 | validate | `startsAt` 非法格式 | rejected |
| I-01 | repo | 同指纹连插两次 firing | 第二次 `inserted=false`，表内 1 行 |
| I-02 | repo | 插 firing 再 resolved | 该行 `status=resolved`，`ends_at` 非空，表内仍 1 行 |
| I-03 | repo | resolved 后重新 firing | 表内 2 行（历史保留） |
| I-04 | repo | 并发 10 goroutine 插同指纹 firing | 表内恰好 1 行（DB 兜底生效） |
| I-05 | repo | 未知指纹的 resolved | 不新增行，计入 skipped |
| H-01 | handler | 合法 payload | 200，`accepted=1` |
| H-02 | handler | 非 JSON | 400 |
| H-03 | handler | 同一 payload 连发两次 | 两次均 200，第二次 `skipped=1` |
| H-04 | handler | 关库后投递 | 503（且日志含根因） |
| H-05 | handler | 超过 1 MiB | 413 |

### 7.3 反向验证（防「假绿」）

新写的断言必须**先造一个必定失败的输入**验证它真的会红，否则它可能永远是绿的。至少覆盖：I-01（临时去掉唯一索引应失败）、H-04（临时把 503 改成 200 应失败）。

---

## 八、验收清单（全过才算 M1 完成）

```text
[ ] 迁移 up → down → up 三连通过；新索引就位
[ ] 第二条同指纹 firing 的 INSERT 被 DB 拒绝（或静默跳过）
[ ] go build ./... / go vet ./... / go test ./... / golangci-lint run / gofmt -l 全 rc=0
[ ] 纯函数包行覆盖率 ≥ 90%
[ ] Alertmanager 真推告警 → 库里出现 source='alertmanager' 的行
[ ] 重复 payload 投递 → 表内不增行，HTTP 200
[ ] resolved 投递 → 原行 status='resolved' 且 ends_at 非空，不新增行
[ ] /healthz 仍 200；停库后 503
[ ] M0 的 51 项断言回归全绿（scripts/verify-runtime.py + verify-images.py）
[ ] docs/decisions.md 的 OPEN-1 / OPEN-2 已裁定并关闭
[ ] README「M0 验收状态」表旁新增「M1 验收状态」表，逐条如实标注
[ ] go.mod 无新增依赖
```

> 任何一项未通过 → **如实标注，不修饰为通过**（AGENTS.md 约束 5 / 12）。

---

## 九、端到端验证脚本（可直接粘贴）

```powershell
# 前置：数据库可用；IIP_PG_URL 指向它
$env:IIP_PG_URL = "postgres://postgres:dev@127.0.0.1:5433/iip?sslmode=disable"

# 1) 构建与质量门禁
go build ./...; if ($?) { go vet ./... }
go test ./... -count=1
golangci-lint run ./...

# 2) 起服务（自动跑迁移）
Start-Process -FilePath "go" -ArgumentList "run","./cmd/iip-server" -PassThru

# 3) 正常投递
curl.exe -s -X POST http://localhost:8080/api/v1/alerts/webhook `
  -H "Content-Type: application/json" `
  --data-binary "@testdata/alertmanager-firing.json"
# 断言：200 + accepted >= 1

# 4) 重复投递（幂等）
curl.exe -s -X POST http://localhost:8080/api/v1/alerts/webhook `
  -H "Content-Type: application/json" `
  --data-binary "@testdata/alertmanager-firing.json"
# 断言：200 + skipped >= 1；且库内行数不变

# 5) resolved 投递
curl.exe -s -X POST http://localhost:8080/api/v1/alerts/webhook `
  -H "Content-Type: application/json" `
  --data-binary "@testdata/alertmanager-resolved.json"
# 断言：200 + updated >= 1；库内该行 status='resolved'

# 6) 非法 payload
curl.exe -s -o NUL -w "%{http_code}`n" -X POST http://localhost:8080/api/v1/alerts/webhook `
  -H "Content-Type: application/json" --data-binary "not-json"
# 断言：400

# 7) 数据落库核对
psql "$env:IIP_PG_URL" -c "SELECT id, fingerprint, severity, status, starts_at, ends_at FROM alerts ORDER BY id;"
```

> 需新增测试夹具：`testdata/alertmanager-firing.json`、`testdata/alertmanager-resolved.json`（从 V1.0 §4.1 样例派生，`instance` 刻意改一处以验证指纹排除生效）。

---

## 十、风险与开放项

| 编号 | 开放项 | 当前倾向 | 阻塞于 | 状态 |
|------|--------|----------|--------|------|
| OPEN-1 | 新迁移号用 `0002_` 还是文档写的 `0003_` | 倾向 `0002_`（保持连续） | — | **RESOLVED**（2026-09-25 裁定 `0002_`；三连验证已通过） |
| OPEN-2 | `sqlc generate` 生成物是否入库 | 倾向**入库**（CI 与镜像构建不必装 sqlc CLI） | — | **RESOLVED**（2026-09-25 裁定入库） |
| OPEN-3 | `severity` 未知值兜底 `info` 还是 `warning` | 维持 `info` + WARN —— 可发现性由 `recognized=false` + WARN 日志承担，不靠把兜底值升级来实现 | — | **RESOLVED**（2026-10-01，见 ADR-011；重估条件改为**可判定**形式：出现一例「未命中别名表、但语义确为严重」的真实告警） |
| OPEN-4 | fingerprint 排除表是否需可配置 | 维持**写死四键**（V1.0 §4.3）—— 可配置会让同一告警跨环境算出不同指纹，破坏去重 / 关联 / 回放的可复现性 | — | **RESOLVED**（2026-10-01，见 ADR-011；架构级决策） |
| OPEN-5 | 未知指纹的孤立 `resolved` 如何处置 | 维持 skip + WARN（不插孤儿行）—— 插孤儿行会让 M2 把它当新告警参与评分，可能在无 firing 起点的情况下凭空建 Incident | — | **RESOLVED**（2026-10-01，见 ADR-011；M2 上线后已在**真库**复核：14 项断言全过，孤立 resolved → `skipped=1` 且 alerts / incident_alerts / incidents 三项计数均不变） |
| OPEN-6 | 重复 firing 命中时是否回写 `annotations`/`raw_payload` | 维持**不回写** —— Evidence-First 保留首次现场；同指纹蕴含非动态标签相同，回写只能覆盖注解与原始载荷；Alertmanager 每周期重推 → 回写等于写放大 | — | **RESOLVED**（2026-10-01，见 ADR-011；真库实测：改注解重推 → `skipped=1`、仍 1 行、首次值未被覆盖；`resolved` → `updated=1`，再 firing → `accepted=1` 插新行） |
| OPEN-7 | `raw_payload` 是否需另存整批上下文（groupKey/receiver） | 维持只存单条 alert —— 批次上下文对同一批每行都相同，复制进 N 行是 N 倍写放大 | — | **RESOLVED**（2026-10-01，见 ADR-011；将来若确需，加标量列 `group_key`/`receiver` 或独立表，**不要**把整批 JSON 塞进每行） |
| OPEN-8 | M0 验收脚本 `scripts/verify-runtime.py:292` 硬编码「迁移版本=1」，M1 新增 0002 后必然失败 | 改为动态校验：版本 = `migrations/` 下最大迁移版本号，且 `dirty=false` | — | **RESOLVED**（2026-09-25 已修正为动态校验；本行原先多出一个单元格导致表格错位，2026-10-01 一并规范） |
| RISK-1 | `AGENTS.md` 约束 2/11 的「M0」限定需随 M1 启动改写 | 已在 §3.3 给出映射，待确认后落笔 | 技术负责人 | **已处置**（2026-09-25：约束 1/2/10/11 已按里程碑改为通用表述，保留改写说明） |
| RISK-2 | 复用旧库时，脏数据会让新唯一索引创建失败 | 迁移须能在干净库无条件成功，脏数据清理不入迁移 | — | 已缓解（§五 执行注意 2） |

---

## 十一、变更记录

| 日期 | 变更内容 | 原因 | 影响范围 |
|------|----------|------|----------|
| 2026-09-25 | 首版（对齐版）：基于 V1.0 §4 + M0 交接物 + 仓库实测状态编写；内嵌 §二「7 条接缝对齐表」 | M1 启动前对齐规格与既有代码 | 全篇 |
| 2026-09-25 | §4.4：`GetAlertByFingerprint` 的排序由 `ORDER BY starts_at DESC` 改为 `ORDER BY starts_at DESC, id DESC` | 端到端实测暴露缺陷：`startsAt` 并列时返回哪一行未定义，导致 resolved 被误判为「重复 resolved」而跳过，告警永远停在 firing | §4.4、§4.6；`internal/db/queries/alerts.sql`；新增回归用例 I-06；见 ADR-007 |
| 2026-09-25 | §4.7 配置键 `IIP_WEBHOOK_MAX_BYTES`（默认 1048576）已落地；§十 OPEN-1 / OPEN-2 已裁定关闭 | M1 实施完成，回填规格与实现的一致性 | §4.7、§十、`internal/config/config.go`、`docs/decisions.md` |
