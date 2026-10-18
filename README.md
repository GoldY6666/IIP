# IIP — Incident Intelligence Platform

IIP 是一个开源的事件智能平台，为**自托管可观测性栈**设计：把海量异构告警自动关联聚合为可操作的事件（Incident），并为每个事件自动收集可追溯的证据链，最终生成可解释的根因假设与修复建议。

> **当前阶段：v0.1.0 已发布（T7.1–T7.7 加固线全部收口）**。M0–M5 已收口（tag `v0.0-m0` ~ `v0.0-m5`）；手册 M6（证据深化与遗留缺口收口）S1–S5 已交付、DoD 3 于 2026-10-12 闭环（`v0.0-m6` tag 按报告决定未打，原因见「M6 验收状态」）；T7 硬化线七项全部收口：T7.1（alerts 按月分区 + retention）/ T7.2（索引调优）/ T7.3（压测）/ T7.4（故障演练）/ T7.5（compose 生产化 + Helm）/ T7.6（README + CHANGELOG + 发版）/ T7.7（E2E 一键回归）——其中 T7.5 与 T7.7 的**容器链路段**待 Docker daemon 恢复后补跑（BLOCKED，不判红）。**开发计划 §10 的 M6 六项已全部收口**：T6.1（Slack 通知）/ T6.2（去风暴 + 通用 webhook）/ T6.3（`/metrics`）/ T6.4（抓取配置 + 规则 + Grafana 面板）/ T6.5（[接入指南](docs/接入指南-Alertmanager.md) + 可执行验收），但**均未发版**，属 `CHANGELOG [Unreleased]`；**如实标注的未交付项**：K8s / trace 采集器未接真实后端（默认关闭）；CI 因仓库无 remote 从未在 GitHub 上跑过。**验证链路共五条**：`verify-func.py`（业务闭环）、`verify-images.py`（容器链路）、`verify-runtime.py`（宿主机全栈）、`check-binaries.py`（镜像内容物）、`fault_drill.py`（故障演练，T7.4 新增）；T7.7 起 `verify-e2e.py` 把各链路 + 真库测试 + UI + 压测编排成一键回归，按 M1–M6 验收项与 MVP 总验收出总报告（最近一轮 `--no-bench`：S0–S5 全 PASS、S6 压测跳过、S7 容器链路因 daemon 缺席 BLOCKED）。真库门禁最近一轮 **1088 PASS / 1 SKIP / 0 FAIL**（含子用例；SKIP 为 Windows symlink 权限，Linux 容器侧 SKIP=0，属可接受项）；`golangci-lint` v2 rc=0。完整变更见 [`CHANGELOG.md`](CHANGELOG.md)，已知限制见其「Known limitations」节。

规格权威源：**[`docs/IIP-M0详细开发计划.md`](docs/IIP-M0详细开发计划.md)**（M0 技术规格以它为准）。架构与数据模型见 [`docs/IIP-V1.0-技术设计文档.md`](docs/IIP-V1.0-技术设计文档.md)。M1 的开发依据见 **[`docs/IIP-M1详细开发手册.md`](docs/IIP-M1详细开发手册.md)**。项目协作约束见 [`AGENTS.md`](AGENTS.md)。

---

## Quickstart（三步跑起来）

面向新用户，从干净克隆到能在浏览器/终端看到服务响应。**假设本机已装齐下方"环境依赖"表里的工具。**

**1. 拉取依赖**

```bash
make deps
```

```powershell
# Windows 未装 make 时用等价脚本
pwsh -File scripts/tasks.ps1 deps
```

作用：`go mod download` 拉 Go 依赖 + 在 `web/` 执行 `pnpm install` 拉前端依赖。

**2. 起本地依赖服务（五服务）**

```bash
make compose-up
```

```powershell
pwsh -File scripts/tasks.ps1 compose-up
```

作用：`docker compose -f deploy/docker-compose.yml up -d`，拉起 postgres / redis / prometheus / alertmanager / loki 五个容器。等约 30 秒让 healthcheck 变绿，然后确认：

```bash
docker compose -f deploy/docker-compose.yml ps   # 五行 STATUS 应为 Up (healthy)
```

**3. 运行 iip-server 并验证健康检查**

先按需设置数据库连接串（`IIP_PG_URL` 为必填项，默认无值；下方与 compose 的 PG 一致）：

```bash
export IIP_PG_URL="postgres://postgres:dev@localhost:5432/iip?sslmode=disable"
make run-server
```

```powershell
$env:IIP_PG_URL = "postgres://postgres:dev@localhost:5432/iip?sslmode=disable"
pwsh -File scripts/tasks.ps1 run-server
```

新开一个终端验证：

```bash
curl http://localhost:8080/healthz     # 预期输出 ok
```

```powershell
curl.exe http://localhost:8080/healthz  # 预期输出 ok
```

`/healthz` 会真实查询数据库（`SELECT 1`）：PG 正常返回 `ok`（HTTP 200），PG 不可用返回 `unavailable`（HTTP 503）。

**可选：Slack 通知**

默认关闭。三个环境变量：

| 变量 | 默认 | 说明 |
|------|------|------|
| `IIP_SLACK_WEBHOOK_URL` | 空（关闭） | Slack Incoming Webhook 地址；**是凭据**，不会被写进日志 |
| `IIP_SLACK_TIMEOUT` | `5s` | 单条发送的 HTTP 超时 |
| `IIP_UI_BASE_URL` | `http://localhost:5173` | 事件深链前缀（`<base>/incidents/<id>`）；为空则消息不带链接 |

两类消息：新事件（标题/严重度/服务/关联告警数/证据数/开始时间）与状态变更
（`from → to` + 操作人）。通知走异步队列，**Slack 挂掉不会影响告警接入**；
计数查询失败时该条通知被放弃（不发送「证据 0 条」这类误导消息）。

**去风暴**：同一事件 10 分钟（滑动窗口）内最多 2 条**中间态**更新
（acknowledged / investigating / mitigated）；终态（resolved / ignored）与重开
**必发**。被抑制的条目不进队列，日志里有「状态变更通知被去风暴抑制」。

**通用出站 webhook**（给外部系统，与 Slack 独立开关）：

| 变量 | 默认 | 说明 |
|------|------|------|
| `IIP_NOTIFY_WEBHOOK_URL` | 空（关闭） | 接收端地址，POST JSON |
| `IIP_NOTIFY_WEBHOOK_TIMEOUT` | `5s` | 单条发送的 HTTP 超时 |
| `IIP_NOTIFY_WEBHOOK_TOKEN` | 空 | 可选，发送 `Authorization: Bearer <token>`；不会写进日志 |

payload：`{"kind","incident":{"id","title","severity","service","status","started_at"},
"from_status","to_status","actor","alert_count","evidence_count"}`
（`from_status`/`to_status`/`actor` 仅在状态变更时出现）。两个出口都**不跟随
重定向**，接收端返回 3xx 会记为发送失败。

**自监控指标 `/metrics`**

默认开启，Prometheus 文本格式（业务指标 + Go 运行时/进程指标）。

| 变量 | 默认 | 说明 |
|------|------|------|
| `IIP_METRICS_ENABLED` | `true` | server 是否暴露 `/metrics`；设 `false` 则该路径 **404**（是「不存在」而非「禁止」） |
| `IIP_WORKER_METRICS_ADDR` | `:9091` | worker 指标监听地址；**留空=关闭**；端口被占用只记 WARN，采集照常 |

两个进程**各暴露一部分**（定义集中在 `internal/metrics`）：

| 指标 | 类型 | 标签 | 进程 |
|------|------|------|------|
| `iip_alert_ingest_total` | counter | `result=stored\|duplicate\|invalid\|error` | server |
| `iip_alert_dedup_total` | counter | `result=hit\|miss\|race` | server |
| `iip_webhook_duration_seconds` | histogram | 无 | server |
| `iip_correlation_score` | histogram | `action=new\|review\|merge` | server |
| `iip_incident_created_total` | counter | 无 | server |
| `iip_evidence_collect_duration_seconds` | histogram | `source=prometheus\|loki\|k8s\|trace` | worker |
| `iip_task_retry_total` | counter | `reason=stale_reclaim\|attempt_failed` | worker |

不变量：`dedup{hit} + dedup{miss} == ingest{stored} + ingest{duplicate}`
（`race` 是 `miss` 的子集，表示「判 miss 之后插入时撞了唯一约束」，不参与等式）。
去重命中率**用 PromQL 算**，而不是进程内维护一个 Gauge：

```promql
# 权威定义在 deploy/prometheus/rules/iip-self.yml（记录规则 iip:dedup_hit_ratio:5m），
# 面板一律引用它，不要各自复制这段表达式：
sum(rate(iip_alert_dedup_total{result="hit"}[5m]))
  / clamp_min(sum(rate(iip_alert_dedup_total{result=~"hit|miss"}[5m])), 1e-9)
```

口径要点：

- **分母只有 `hit + miss`，分子只有 `hit`**。`race` 是 `miss` 的**子集**
  （判 miss 之后插入撞唯一约束时，miss 已经记过一次），因此它既不进分子也不进分母 ——
  写进分子等于把「并发兜底生效」当成了命中，写进分母则是同一条告警被数了两次。
- **标签纪律**：没有任何标签取事件/告警/服务维度（无界基数会拖垮 Prometheus）。
  `internal/metrics` 的 `TestLabelNamesAreBounded` 是这条的机器守卫 ——
  新增标签必须先登记白名单。
- `/metrics` **不受 `IIP_AUTH_TOKEN` 保护**（Prometheus 抓取不带业务 token）。
  暴露到公网时请在反向代理限制来源网段：进程数与事件速率本身也是侦察素材。
- 关闭端点不影响埋点计数（进程内照常累计），但进程重启会归零。
- worker 的采集耗时**包含失败的采集**：只记成功的会让「某个采集器一直超时」
  在图上彻底消失。失败次数看 `iip_task_retry_total{reason="attempt_failed"}`。
- **抓取配置与面板**：`deploy/prometheus/prometheus.yml`（两个 job，端口跟默认监听一致）、
  `deploy/prometheus/rules/iip-self.yml`（记录规则 + 2 条告警）、
  `deploy/grafana/iip-overview.json`（9 个面板，导入即用）。改完跑
  `python scripts/verify-observability.py` 核对名字/标签/端口一致性。
  该脚本**不核对 PromQL 语法与渲染**（需 promtool / Grafana），别把它当成「面板已验证可用」。



**卸载/清理**

```bash
make compose-down            # 停止并移除五服务，保留 named volume 数据
# 如需彻底清空数据：docker compose -f deploy/docker-compose.yml down -v
```

### 把应用也装进容器（可选）

上面的 Quickstart 让 iip-server 在本机裸跑、依赖服务在容器里。
如果要把 **iip-server / iip-worker 也容器化**，走 `app` profile：

```bash
make app-up          # 构建两个镜像 + 起全部七个服务（五依赖 + 两应用）

# 或者一条命令跑完「构建 → 起服务 → 逐项断言 → 出报告 → 清理」：
make verify-images
```

> `make verify-images` 会自动完成端到端验证并产出 `_verify_images.txt` 报告，
> 断言包含 healthz 200 / 容器内自检 / 迁移版本 / 表数量 / worker 心跳；
> M2 起另含容器内的关联语义：种子权重逐位比对、配置 API 的 400/422 判别，
> 以及「改阈值 → 同批告警判 NEW 还是被并入同一个 Incident」的对照实验。
> 脚本**可重复执行**：每次运行注入独立标签，不会与上一轮留在卷里的告警互相去重。
> 加 `IIP_KEEP=1` 可保留容器便于人工排查。

等价的原始命令：

```bash
docker build --target server -t iip-server:0.1.0 .
docker build --target worker -t iip-worker:0.1.0 .
docker compose -f deploy/docker-compose.yml --profile app up -d
```

#### 中国大陆网络：先把基础镜像备到本地

Docker Hub 在国内常不通，且 `mirror.gcr.io` 这类源**只覆盖官方 `library/*`**，
`prom/*`、`grafana/*`、`gcr.io/distroless/*` 都拿不到。用脚本多源拉取并重打标准名：

```bash
python scripts/prepull-mirror.py           # 多源自动降级，按优先级依次尝试
python scripts/prepull-mirror.py --force   # 镜像源变更后强制重拉
python scripts/prepull-mirror.py --check   # 只做清单漂移检查，不联网
```

拉取完成后，`Dockerfile` 的 `FROM` 与 compose 的 `image:` 会**直接命中本地镜像**，
构建与起服务全程不需要外网（也**不需要改 `daemon.json`**）。

> `--check` 用来防「脚本清单与 compose / Dockerfile 版本号漂移」——
> 有人改了版本却忘了改脚本，拉取会静默失效。建议在提交前跑一次，它不联网、立即返回。
>
> 已知限制：Docker Desktop（WSL2 后端）以 GUI 里的配置为 `daemon.json` 的权威来源，
> **从外部手改该文件会被覆盖**。要加镜像源请在 GUI 的 Docker Engine 编辑器里改。

两个镜像由**同一个 `Dockerfile`** 多阶段构建产出，运行层是
`gcr.io/distroless/static-debian12:nonroot`（约 20 MB、非 root、无 shell）。

验证：

```bash
docker compose -f deploy/docker-compose.yml --profile app ps
# iip-server 应为 Up (healthy)
curl.exe http://localhost:8080/healthz     # 预期 ok
```

> **注意**：容器内的 `IIP_PG_URL` 指向 compose 服务名 `postgres:5432`，
> 与宿主机 `localhost:5433` 是**两个不同的库，数据不互通**。
> 细节与权衡见 [`deploy/README.md`](deploy/README.md)。

> **为什么应用组不在默认启动集里**：用 `profiles: ["app"]` 隔离，
> 保证 T0.6 原验收命令（`up -d` → 五服务全 healthy）语义完全不变。

### 备选：无 Docker 的本地运行（已实测跑通）

未安装 Docker 时，可用**绿色版 PostgreSQL** 替代 compose 起的五服务，跑起 server + 前端。
2026-09-25 已在本机实测通过，三条链路同时在线：

| 服务 | 地址 | 说明 |
|------|------|------|
| PostgreSQL | `127.0.0.1:5433` | 绿色版 16.4，数据目录 `D:\pgsql16\data` |
| iip-server | `127.0.0.1:8080` | 启动时自动执行迁移（内嵌 `migrations/`） |
| web (Vite) | `127.0.0.1:5173` | 开发服务器，`/api` 代理到 `:8080` |

一键启动：

```bash
python scripts/run-local-nodocker.py
```

脚本会依次拉起 PostgreSQL → iip-server → Vite，持续心跳保活，并把全过程写入
`<_run_state.txt 所在目录>/_run_state.txt`（默认 `%IIP_PG_HOME%\logs`），
**保持这个窗口不关，整个栈就会一直在线**。

全部参数走环境变量，均有默认值：`IIP_PROJ` / `IIP_PG_HOME` / `IIP_PG_PORT` /
`IIP_PG_USER` / `IIP_PG_PASSWORD` / `IIP_PG_DB` / `IIP_HTTP_PORT` / `IIP_VITE_PORT` /
`IIP_LOG_DIR` / `IIP_STRICT_PORT`。

验证：

```bash
curl.exe http://127.0.0.1:8080/healthz      # 预期 ok
start http://127.0.0.1:5173/                # 预期渲染「IIP 控制台 · M0 开发中」
```

**行为约定（避免误判）**：

- 端口**已被占用**时脚本直接拒绝启动并给出提示（`IIP_STRICT_PORT=1`，默认开），
  不会静默换端口。
- PostgreSQL **已在运行**时脚本会复用现有实例（汇总行标注 `(reused)`），
  并且 teardown 只回收自己启动的进程。
- `http://127.0.0.1:5173/healthz` 返回的是 SPA 首页 HTML 而非 `ok`——
  Vite 只代理 `/api`，`/healthz` 未配代理会落到 index.html。这不是故障。
- M2 起开放五个路由：`GET /healthz`、`POST /api/v1/alerts/webhook`、
  `GET /api/v1/alerts`（告警列表，返回最新 100 条、不做 status 过滤，支持 `?limit=&offset=`，
  参数缺省/越界自动夹紧）、`GET /api/v1/correlation/config`、
  `PUT /api/v1/correlation/config`（关联权重/阈值，支持部分更新，写入即生效免重启）。
  其余 `/api/v1/*`（**事件**列表 / 详情）属 **M3**，**当前访问返回 404 是预期行为**
  ——M2 手册 §3.2 已将「Incident 查询 API」明确列为 Out-of-Scope。
  webhook 的契约与状态码矩阵见 `docs/IIP-M1详细开发手册.md` §4.1；列表接口见 §3.1 P1 / §4.6；
  配置接口的状态码矩阵（400 / 413 / 422 / 503）见 `docs/IIP-M2详细开发手册.md` §4.4。

手动分步（各自独立终端）：

```powershell
# 1) 起库（若已跑着可跳过）
D:\pgsql16\bin\pg_ctl.exe -D D:\pgsql16\data -o "-p 5433" start

# 2) 起后端
$env:IIP_PG_URL = "postgres://postgres:dev@127.0.0.1:5433/iip?sslmode=disable"
.\bin\iip-server.exe --http-addr :8080        # 自动迁移后监听 8080

# 3) 起前端
cd web; pnpm dev --host 127.0.0.1 --port 5173 --strictPort
```

> 该路径仅覆盖 server 与前端所需的 PostgreSQL；
> redis 不参与 V1.0（`IIP_REDIS_DISABLED` 默认 `true`，走 DB polling），
> prometheus / alertmanager / loki 属 M1 告警接入后才需要的服务。

---

## 环境依赖

| 工具 | 版本要求 | 验证命令（PowerShell） | 说明 |
|------|----------|------------------------|------|
| Go | ≥ 1.23 | `go version` | 官网 MSI 安装，装完重开终端 |
| Git | ≥ 2.40 | `git --version` | 全局配 `git config --global core.autocrlf true` |
| Docker Desktop | 最新版 | `docker version` | 启用 WSL2 后端；`docker compose version` 可用 |
| Node.js | ≥ 20 LTS | `node -v` | 前端构建 |
| pnpm | ≥ 8（本仓库用 9） | `pnpm -v` | 无则 `npm i -g pnpm`；lockfile 为 v9.0 |
| golangci-lint | ≥ 1.60 | `golangci-lint --version` | `go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest` |
| sqlc | ≥ 1.27 | `sqlc version` | `go install github.com/sqlc-dev/sqlc/cmd/sqlc@latest` |
| golang-migrate CLI | ≥ 4.17 | `migrate -version` | 本地手动执行迁移调试用 |

> Windows 提示：`make` 可选。未安装时用 `scripts/tasks.ps1` 的同名任务；两条路径命令等价。

---

## 项目结构

```
iip/
├── cmd/
│   ├── iip-server/     # HTTP API 服务入口（M0 仅 /healthz）
│   └── iip-worker/     # 后台 worker 入口（M0 为空壳 polling 循环）
├── internal/
│   ├── api/            # M1 起 HTTP handler 分层（M0 占位，见 .gitkeep）
│   ├── config/         # 配置加载（koanf，flag>env>本地yaml）+ 单测
│   ├── db/queries/     # sqlc 查询定义（queries/alerts.sql）
│   ├── logging/        # log/slog JSON 日志装配
│   ├── middleware/     # chi 访问日志等中间件
│   └── store/          # pgxpool 连接、迁移执行、健康检查
├── Dockerfile          # iip-server / iip-worker 多阶段构建（--target 区分）
├── .dockerignore       # 构建上下文排除清单
├── migrations/         # golang-migrate 迁移（0001_init.up.sql / .down.sql + embed.go）
├── web/                # 前端（Vite + React + TS + antd）
├── deploy/             # docker-compose（五依赖 + 两应用 profile）+ 各服务配置
├── scripts/            # pre-commit、tasks.ps1、验收 SQL、容器链路验收（verify-images / prepull-mirror / lib_docker）
├── testdata/replay/    # 关联算法 replay 评估集（M2 用）
└── docs/               # 规格与决策文档（权威源在此）
```

---

## 系统架构

```mermaid
flowchart LR
    subgraph EXT["外部数据源（部署侧提供）"]
        AM["Alertmanager :9093"]
        PROM["Prometheus :9090"]
        LOKI["Loki :3100"]
        K8S["K8s API（可选·默认关）"]
        TRACE["链路后端（可选·默认关）"]
    end

    subgraph IIP["IIP（本仓库）"]
        WEB["Web UI<br/>Vite/React，server 静态托管"]
        SRV["iip-server :8080<br/>chi API + 五维关联引擎"]
        WRK["iip-worker<br/>轮询领任务 + 四类采集器"]
        PG[("PostgreSQL 16<br/>9 张表 + 0001-0007 迁移")]
    end

    BROWSER["浏览器"] --> WEB
    WEB -->|"同源 /api/v1"| SRV
    AM -->|"POST /api/v1/alerts/webhook"| SRV
    SRV -->|"落库 → 关联 → 入队（同事务外 best-effort）"| PG
    SRV -->|"启动时自动执行内嵌迁移"| PG
    WRK -->|"领取任务 / 回写游标 / 写证据"| PG
    WRK -->|"PromQL / LogQL / K8s API / Jaeger Query"| PROM
    WRK --> LOKI
    WRK --> K8S
    WRK --> TRACE
    BROWSER -.->|"UI 面板深链（浏览器侧直连）"| PROM
```

三条设计主线（对应代码里的硬约束）：

1. **采集不在告警接入路径上**。webhook 只做「落库 → 关联 → 入队」，
   证据采集由 worker 异步完成——数据源宕机时 webhook 延迟不变（实测 31ms → 61ms），
   Alertmanager 不会因 IIP 采不到证据而重投放大风暴。
2. **V1.0 无 Redis**。`IIP_REDIS_DISABLED` 默认 `true`，任务队列就是 PG 表
   （`FOR UPDATE SKIP LOCKED` 领取、`stale_after` 回收）。D3 场景用依赖闭包
   静态证明了 worker 零 Redis 代码路径（T7.4）。
3. **server 无状态、迁移幂等**。启动时自动跑内嵌迁移（加锁），
   多副本并发启动不会互相踩；关联配置（权重/阈值）存 `correlation_rules` 单行，
   改后免重启即对下一条告警生效。

---

## API 摘要

全部路由注册在 `cmd/iip-server/main.go`（单一装配点，无分散注册）。
**鉴权列**指 `IIP_AUTH_TOKEN` 启用时的行为：未启用时全部放行（内网默认形态）。

| 方法 | 路径 | 里程碑 | 说明 | 鉴权 |
|------|------|--------|------|------|
| GET | `/healthz` | M0 | 真查库（`SELECT 1`）：200 `ok` / 503 `unavailable` | 否 |
| GET | `/metrics` | M6 T6.3 | 自监控指标（Prometheus 文本格式）；`IIP_METRICS_ENABLED=false` 时不注册（404） | 否 |
| GET | `/api/v1/auth/session` | M6 S4 | 前端凭据来源；无凭据返回 200 + `auth_required=false` | 否 |
| POST | `/api/v1/alerts/webhook` | M1 | Alertmanager 接入；请求体上限 1 MiB（超限 413） | 是 |
| GET | `/api/v1/alerts` | M1 | 告警列表，默认最新 100 条，`?limit=&offset=` 自动夹紧 | 是 |
| GET | `/api/v1/correlation/config` | M2 | 读关联权重/阈值（单行配置） | 是 |
| PUT | `/api/v1/correlation/config` | M2 | 改配置，免重启生效；非法值 422 / 不成形 400 | 是 |
| GET | `/api/v1/incidents` | M3 | 事件列表，过滤/排序/分页/聚合计数 | 是 |
| GET | `/api/v1/incidents/{id}` | M3 | 事件详情（关联告警超 200 条时 `truncated=true`，计数走 `count(*)` 真值） | 是 |
| POST | `/api/v1/incidents/{id}/transition` | M3 | 状态流转；乐观并发冲突 409，留痕与状态同事务 | 是 |
| POST | `/api/v1/incidents/{id}/comments` | M3 | 追加评论（timeline） | 是 |
| GET | `/api/v1/reviews` | M3 | 待确认队列（`needs_review`） | 是 |
| POST | `/api/v1/reviews/{incidentID}/{alertID}` | M3 | 人工判定（确认并入 / 抑制）；抑制可幂等 | 是 |
| GET | `/api/v1/incidents/{id}/evidence` | M4 | 事件证据列表；超 50 条截断并给 `truncated`/`total` | 是 |

错误码可判别性约定（各 handler 一致）：请求不成形 → **400**；体量超限 → **413**；
语义非法 → **422**；乐观并发冲突 → **409**；存储不可用 → **503**。
契约测试见 `internal/api/*_test.go` 与 `scripts/verify-func.py`。

> 静态托管（M5 S3）由 `internal/webui` 注册在**路由表末尾**：非 `/api/*` 且
> 非 `/healthz` 的 GET 回落 `index.html`（SPA 深链接可刷新打开），
> `/api` 前缀永不吞。目录穿越一律 400。

---

## 常用任务

Makefile 与 `scripts/tasks.ps1` 提供同名任务（三方合约：README ↔ Makefile ↔ tasks.ps1 目标名一致）：

| 任务 | 作用 |
|------|------|
| `deps` | 拉取 Go modules + 前端 pnpm 依赖 |
| `compose-up` / `compose-down` | 起 / 停 `deploy/docker-compose.yml` 七服务 |
| `run-server` / `run-worker` | 运行 iip-server / iip-worker |
| `test` | `go test -race -coverprofile=coverage.out ./...` |
| `lint` | `golangci-lint run ./...` |
| `migrate-up` / `migrate-down` | 对本地 PG 执行 / 回滚迁移 |
| `web-dev` | 启动前端 dev server（5173） |
| `build` | 编译二进制到 `bin/` |
| `tidy` | 整理 `go.mod` / `go.sum` |
| `pg-up` / `pg-down` | 仅起 / 删单个本地 PG 容器（轻量调试） |
| `image-build` | 构建 `iip-server` / `iip-worker` 两个镜像 |
| `image-ls` | 列出本项目镜像 |
| `app-up` / `app-down` | 构建镜像并起 / 停全部七个服务（含应用组） |
| `verify-images` | 容器链路验证：构建 + 起服务 + 逐项断言 + 出报告 + 清理 |
| `verify-runtime` | 宿主机全栈验证：真实 PG 从零建库 → 迁移 → 探活 → 心跳 → 退出 |
| `check-binaries` | 交叉编译 linux/amd64 并校验真静态链接（distroless 前提） |
| `run-local` | 无 Docker 的本地全栈启动（绿色版 PG + server + Vite） |
| `verify-e2e` | **E2E 一键回归**（T7.7）：编排 S0–S7，按 M1–M6 验收项 + MVP 总验收出总报告；透传 `--stages s2,s3` / `--summary` / `--strict` |

查看全部任务：`make help` 或 `pwsh -File scripts/tasks.ps1 list`。

### 验证脚本：五条链路 + 一个一键编排，别混用

| 脚本 | 链路 | 前置 | 报告 |
|------|------|------|------|
| `scripts/verify-func.py` | **业务闭环**：M1 接入 → M2 关联 → M3 处置 → 抑制生效，核对数据后果 | 绿色版 PostgreSQL（`IIP_PG_HOME` 可覆盖） | `_verify_func.txt` |
| `scripts/verify-runtime.py` | 宿主机全栈（偏 M1/M2 单点校验） | 宿主 PostgreSQL | `_verify_runtime.txt` |
| `scripts/verify-images.py` | 容器链路（偏部署形态与镜像内容物） | Docker daemon | `_verify_images.txt` |
| `scripts/check-binaries.py` | 镜像内容物（交叉编译 + ELF 解析） | 无 | 控制台 |
| `scripts/fault_drill.py` | **故障演练**（T7.4）：真 kill worker / 真停 PG / Redis 缺席，验恢复行为 | Go 工具链 + 绿色版 PostgreSQL + 5436/8083 空闲 | `_fault_drill.txt` |
| `scripts/verify-e2e.py` | **一键回归**（T7.7）：把上面各链路 + 真库测试 + UI + 压测编排成 S0–S7，按 M1–M6 验收项与 MVP 总验收逐项出状态 | 见各阶段（S7 需 Docker daemon，缺席记 BLOCKED 不判红） | `_verify_e2e.txt` + `scripts/_e2e/*.json` |

`verify-func.py` 默认用 **5434 / 8082**，`fault_drill.py` 用 **5436 / 8083**（独立 PGDATA），
与 `verify-runtime.py` 错开，几条链路可并存而不互相抢占。

**E2E 一键回归**（T7.7）把回归变成一条命令，并按开发计划的验收项出总账：

```bash
python scripts/verify-e2e.py                 # 一把跑完 S0-S7 并汇总（完整环境）
python scripts/verify-e2e.py --stages s2,s3  # 只跑真库测试 + 业务闭环（分批）
python scripts/verify-e2e.py --summary       # 汇总已有阶段结果，不出总报告
python scripts/verify-e2e.py --dry-run       # 只看阶段计划与端口，不执行
```

八个阶段：S0 前置（工具链/端口/仓库状态）→ S1 静态（二进制 + 标签四联动 +
helm + compose config + 干净克隆构建）→ S2 真库全量测试（含 M2 Replay）→
S3 业务闭环 → S4 UI（vitest + playwright）→ S5 故障演练 → S6 压测 →
S7 容器链路。汇总报告逐项标注 **M1–M6 验收项**与 **MVP 总验收三项**的状态；
环境缺条件（如 Docker daemon 不在）的阶段记 BLOCKED 并给出补救命令，
`--strict` 下 BLOCKED 也判红（CI 用）。每阶段结果独立落
`scripts/_e2e/<id>.json`，支持分多次调用跑完再 `--summary`。

> **自己写端到端验证时注意三条**（2026-10-03 各踩一次，详见 M3 报告 §14.7）：
> 1. 告警的 `startsAt` / `endsAt` **必须固定** —— firing 的 `endsAt` 落库为 NULL，
>    评分时用「当前时刻」兜底，投递间隔一变分数就变，会导致关联结果不可复现。
> 2. 要造「同指纹的第二条告警」，只能用 `instance`（动态键）区分，
>    用 `round` 之类会改变指纹，测的就不是你想测的东西了。
> 3. 造第二条前必须先把前一条投成 `resolved` —— 同指纹的 firing 行已在库会被
>    去重 `skipped`，连库都不进，也不会触发关联。

---

## M0 验收状态（如实标注）

M0 的完成定义（DoD）见 `docs/IIP-M0详细开发计划.md` §3。当前状态（2026-09-25 实跑）：

| 验收项 | 状态 | 说明 |
|--------|------|------|
| 仓库骨架 + 前端脚手架可构建 | **PASS** | `go build ./...` / `go vet ./...` / `golangci-lint run` / `pnpm tsc --noEmit` / `pnpm build` 均 ExitCode 0 |
| 数据库迁移 up → down → up 可重复 | **PASS** | down 后 7 张业务表全清，再 up 后逐表名与基线一致；CHECK/索引/外键数量完全复原 |
| 6 项非法值被 CHECK 约束拒绝 | **PASS** | `scripts/verify_inserts.sql` 实跑：6/6 全部拒绝，逐条附 constraint 名 |
| `/healthz` 返回 ok；stop PG 返回 503 | **PASS** | 三态闭环：200 `ok` → 503 `unavailable` → 200 `ok` |
| worker 心跳 + 优雅退出 | **PASS** | 60s 心跳（`poll_count=59`）→ `drain 完成，worker 已干净退出` |
| **v1 端到端运行测试（全 51 项）** | **PASS** | 三条链路：镜像内容物（cross-compile + ELF 静态链接）→ 应用端到端（真实 PG 16.4 从零建库 → 迁移 → 探活 → worker 心跳 → 优雅退出）→ 容器运行（`docker build` + `compose --profile app up` + 容器内断言）。见 `docs/v1-测试报告.md` |
| **镜像内容物静态链接校验** | **PASS** | 按 Dockerfile 逐参数 cross-compile；ELF `PT_INTERP` 缺失 + 无 libc 引用 = 真静态；版本串注入生效；server 迁移 SQL 完整嵌入 |
| `go test -race ./...` | **PASS**（2026-10-01 等价复现） | 本机仍跑不了（Windows 下 `-race` 需 CGO，`CGO_ENABLED=0`、无 gcc，已裁决不装）。改为在 `golang:1.23-alpine` 容器内以 `CGO_ENABLED=1` 实跑：8 包全 ok、rc=0。证据 `D:\_iip_setup\_ci_equiv_report.txt` |
| 应用镜像可构建（`docker build`） | **PASS** | `iip-server` 13.8MB / `iip-worker` 11.3MB，linux/amd64。WSL2 装好后实测通过 |
| iip-server / iip-worker 容器可运行 | **PASS** | distroless 运行层，容器内 `--healthcheck` 退出码 0；worker 心跳 `poll_count=29` |
| compose 依赖服务全 healthy | **PASS** | postgres / redis / prometheus / alertmanager / loki 全 healthy；7 服务全部收敛到终态，无 unhealthy、无停留 starting |
| Alertmanager UI 可见 firing 告警 | **未验证** | 容器健康与 9093 可达已验证；但未构造真实告警场景，UI 上的 firing 呈现仍属 M4 范围 |
| CI backend + frontend 双 job 绿 | **未验证（无法验证）** | 本仓库**没有 git remote**，`.github/workflows/ci.yml` **从未在 GitHub 上执行过**，「由 CI 覆盖」这一说法无从兑现。两个 job 的命令改为**本地等价复现**：`-race` 容器内 PASS；`golangci-lint` 复现时曾为 FAIL（17 项），已于 2026-10-01 逐项修复至 **rc=0**（见下文「M1 验收状态」表的 lint 行与 `docs/decisions.md` ADR-012） |
| `docs/decisions.md` 记录 T0.2 四条决策 | 已就绪 | 状态「待人工确认」，见 `docs/decisions.md` |
| `git tag v0.0-m0` | 待执行 | 全部验收通过后打 tag |

> 本表由运维在环境就绪后逐条执行并更新；**任何一项不通过如实标注，不修饰为通过**（AGENTS.md 约束 5）。
> 实测原始输出留存于 `D:\_iip_setup\_rt_all.txt` / `_rt_downup.txt` / `_srv*.log` / `_wrk.log` / `pg.log`。
> v1 端到端证据留存于 `D:\_iip_setup\_v1_e2e_report.txt` / `_v1_binary_check.txt` / `_v1_quality_gate.txt`。
> 容器链路证据留存于 `D:\_iip_setup\_verify_images.txt`（2026-10-01 M2 补验：清空卷后的全绿报告）/ `_prepull.txt`；M1 期同名证据已归档为 `_verify_images_m1_20260925.txt`，未被覆盖。M3 前置清理（L1 lint / L2 容器拓扑）的**六道门禁汇总**见 `D:\_iip_setup\_l1l2_regression_report.txt`，补验后的容器链路报告见 `_verify_images_l2.txt`（32 项）。2026-10-02 再跑为 **34 项**（新增 4.5.0「干净起点」2 条），且是在**未清空 named volume** 的脏数据环境下全绿，属修复后的复现证据。
> 详细证据见 `docs/M0-总验收报告.md` §二 与 `docs/v1-测试报告.md`。

---

## M1 验收状态（如实标注）

M1 的完成定义见 `docs/IIP-M1详细开发手册.md` §八。当前状态（2026-09-25 实跑）：

| 验收项 | 状态 | 说明 |
|--------|------|------|
| 迁移 up → down → up 三连通过；新索引就位 | **PASS** | `migrations/migrate_test.go` 真库实跑 up→down-all→up，每步均断言版本号与 `uq_alerts_fingerprint_firing` 存在性（不只看命令返回码） |
| 第二条同指纹 firing 的 INSERT 被 DB 拒绝（或静默跳过） | **PASS** | 裸 INSERT 实得 SQLSTATE 23505；生产语句 `ON CONFLICT … DO NOTHING` 影响 0 行；resolved 历史行不受该索引约束 |
| `go build` / `go vet` / `go test ./...` / `gofmt -l` 全 rc=0 | **PASS**（四项） | build / vet / test / gofmt 均 rc=0 |
| `golangci-lint run` | **PASS**（2026-10-01 修复至 rc=0；原为 FAIL） | 2026-10-01 在**本表对应的 M1 终点提交 `a0ca8a5`** 上用同版本 v1.61.0 实跑得 **rc=1、1 项**（`internal/api/alert_list.go:77` gocritic ifElseChain）。即 M1 当时「lint 全绿、7 项已修」的说法**不成立**——该列表切片引入此项后 lint 再未执行过（本机 CLI 未装），「由 CI 覆盖」亦从未兑现（仓库无 remote）；到 M2 终点累计 **17 项**。**已于 2026-10-01 逐项修复至 rc=0**（M3 前置 L1）：四类 17 项全部按类修掉，唯一配置改动是 gocritic `hugeParam` 阈值 80 → 128 B（**阈值收敛，未加排除规则、未用 nolint**）。方案见 `docs/decisions.md` ADR-012；证据 `D:\_iip_setup\_lint_m3_final.txt`（`LINT_RC=0`，v1.61.0） |
| 纯函数包行覆盖率 ≥ 90% | **PASS** | `internal/alert` 91.2%。初测仅 51%——Service 分支只有跨包集成用例触达，而 Go 的 `-cover` 不计入，故按手册 §4.5 补 `service_test.go`（假仓储）覆盖 7 个去重分支 |
| 重复 payload 投递 → 表内不增行，HTTP 200 | **PASS** | 端到端实跑 6 步完整生命周期，HTTP 计数与库内行数逐一核对 |
| resolved 投递 → 原行 `status='resolved'` 且 `ends_at` 非空，不新增行 | **PASS** | 同上，含「重新起火后再次 resolved」场景 |
| `Alertmanager` **真推**告警 → 库里出现 `source='alertmanager'` 的行 | **PASS** | 向 `iip-alertmanager:9093` 的 `/api/v2/alerts` 推一条真实告警，由其 group 聚合后自动推给 webhook，IIP 落库 `source='alertmanager' severity=critical status=firing`。同时验证了 `deploy/alertmanager/alertmanager.yml` 的 `host.docker.internal:8080` 路由配置正确——这是只有真推才能暴露的配置风险点。**副作用（已处理）**：该验证把 `iip` 库迁移到版本 2，导致仅内嵌 0001 的 M0 镜像启动失败（`no migration found for version 2`）并陷入重启循环；已构建 `iip-server:0.0-m1` / `iip-worker:0.0-m1` 并把 `deploy/docker-compose.yml` 与 `Makefile` 的 `IMAGE_TAG` 同步推进到 `0.0-m1`，容器栈现为 `Up (healthy)` |
| `/healthz` 仍 200；停库后 503 | **PASS** | 端到端启动后 `/healthz` 200；停库 503 由 `TestWebhookH04_ClosedPoolIs503`（真库关闭连接池）与 `TestWebhookH04_StorageUnavailableIs503`（注入故障）双路覆盖 |
| M0 回归 `scripts/verify-runtime.py` | **PASS（38/38，OPEN-8 已修正）** | 原唯一失败项为 `verify-runtime.py` 第 292 行**硬编码**的 M0 时代版本断言 `out == "1\|f"`（彼时只有 0001 一个迁移）；M1 新增 0002 后版本应为 2 且 `dirty=false`（迁移干净完成），属**断言过期而非实现回归**。已按建议改为**动态校验**「版本 = `migrations/` 下最大迁移版本号」（OPEN-8 关闭）。注：本机无 5433 本地 PG、无法重跑该脚本，逻辑已静态核对，待有 PG 时复验。 |
| `docs/decisions.md` 的 OPEN-1 / OPEN-2 已裁定并关闭 | **PASS** | 均已置 RESOLVED 并补齐 Resolution |
| README 新增「M1 验收状态」表 | **PASS** | 本表 |
| `GET /api/v1/alerts` 告警列表接口 + 前端列表页 | **PASS** | 后端新增 `ListAlerts`（sqlc 生成）/ `Repository.List` / Service 转发 / handler `AlertList`（默认 100 条、`?limit=&offset=` 参数夹紧）/ 路由；补 `Alert/StoredAlert` 的 json tag（原缺 tag 致 PascalCase、与前端契约不符，已修）；前端 `web/src/api/alerts.ts` + `pages/Alerts.tsx`（antd Table + 「发送示例告警」按钮）。端到端实跑：healthz 200 → 列表返回既有告警 → POST 示例告警 200 accepted=1 → 列表 count +1、firing 行 `ends_at=null` 渲染「持续中」、`starts_at DESC` 排序正确、字段扁平化正确 |
| `go.mod` 无新增依赖 | **PASS** | require 仍为 chi / golang-migrate / pgx / koanf / pflag；M1 只用标准库（SHA256 指纹、`http.MaxBytesReader` 限流） |

**本轮实测发现并修复的缺陷**（详见 `docs/decisions.md` ADR-007）：

`GetAlertByFingerprint` 原按 `ORDER BY starts_at DESC LIMIT 1` 取「最新一条」。但 Alertmanager
重发同一告警时 `startsAt` 与首次完全相同，导致历史行在 `starts_at` 上并列，而并列时返回哪一行
是**未定义**的——实测同一轮运行的两次调用分别取到不同行，后果是 resolved 被误判为「重复 resolved」
而跳过，**告警永远停在 firing**。已改为 `ORDER BY starts_at DESC, id DESC`，并新增回归用例
`TestRefireThenResolveUsesLatestRow`；反向验证确认去掉 `id DESC` 后该用例立即转红。

> 本表同样遵循「任何一项不通过如实标注，不修饰为通过」（AGENTS.md 约束 5 / 12）。
> 集成测试依赖环境变量 `IIP_TEST_PG_URL`（未设置时相关用例 `t.Skip`，**不会**把「没库」伪装成通过）。

---

## M2 验收状态（如实标注）

M2 的完成定义见 `docs/IIP-M2详细开发手册.md` §八。当前状态（2026-10-01 实跑；容器链路于同日补验，此前如实标为 BLOCKED）：

| 验收项 | 状态 | 说明 |
|--------|------|------|
| 五维评分 + 三档决策实现，`correlation` 包单测全绿 | **PASS** | `internal/correlation` 新增 **8 个实现文件（823 行）+ 8 个测试文件（1654 行）**，共 16 个文件（行数按字节级统计 `\n`，等价 `wc -l`）。权重 Time 0.25 / Service 0.25 / Label 0.20 / Dependency 0.20 / Historical 0.10；阈值 MERGE ≥0.80、REVIEW ≥0.50、否则 NEW，边界一律取「大于等于」 |
| 权重/阈值存于 `correlation_rules` 单行，GET/PUT 配置 API 可用，非法配置 422 | **PASS** | 迁移 0003 种子 `name='default'` 单行；`internal/api/correlation_config_test.go` 12 项 HTTP 契约用例，逐项锁定 400 / 413 / 422 / 503 四种错误码的**可判别性**（请求不成形 → 400；语义非法 → 422；存储故障 → 503） |
| 改配置后无需重启即对下一条告警生效 | **PASS** | `TestCorrelateAlertsEndToEnd` 真库实跑：`PutConfig(enabled=false)` 后下一条同服务告警不再建事件、不回写 `incident_id`，事件数保持不变 |
| Replay 三场景：精确率 100% / 召回率 ≥90% / 杂告警不误合并 | **PASS** | scenario1 单服务风暴 202 条（200 风暴 + 2 跨域噪声）→ 3 组；scenario2 跨服务级联 4 → 2 组；scenario3 互不相关 5 → 5 组。**三场景精确率与召回率均为 1.0000**（19900 / 3 / 0 对同组对逐一比对，非仅看组数）；另断言「最大组恰含 200 条」以防「全并成一组」式的假达标 |
| `correlation` 包测试覆盖率 ≥90% | **PASS** | **96.4%**（`go test -cover`）。`internal/repository` 在真库下 82.5%，`internal/alert` 91.1% |
| 1000 条告警回放耗时 <60s | **PASS** | 实测 **466 ms**（预算 60s，留 128× 余量）；同规模另有 `BenchmarkClusterInMemory1000` 便于后续调参对比 |
| 迁移 0003 up→down→up 三连通过；`needs_review` 列与 default 配置行就位 | **PASS** | 真库实跑（`IIP_TEST_PG_URL`），**跳过用例数 = 0**。`TestMigrateUpDownUp` 走全量 up→down-all→up 并逐步断言版本号与**两个**索引；`TestCorrelationM2Migration` 用 `Steps(-1)` **只回滚 0003**，从而真正验证 0003 的 down 删掉了 `needs_review` 列与种子行（全量回滚会把 0001 的表一起删掉，那样「行不存在」是必然的，测不出漏删） |
| webhook 落库后同步关联生效；关联失败不影响 HTTP 200 | **PASS** | `internal/alert` 挂载 `Correlator`（接口定义在消费方，依赖方向仍 `alert ← correlation`）；`TestIngestSwallowsCorrelatorError` 钉死「关联报错只记日志，不冒泡」——否则 Alertmanager 会重投并放大风暴 |
| `go build ./...` / `go vet ./...` / `go test ./...` / `gofmt` 全绿 | **PASS** | 四项 rc=0；另在**真库模式**下重跑 `go test ./...` 全绿（8 个包，0 跳过） |
| `go.mod` 无新增依赖 | **PASS** | require 仍为 chi / golang-migrate / pgx / koanf / pflag + `gopkg.in/yaml.v3`。YAML 库**并非新增**：它本就是 koanf 的间接依赖且已在 `go.mod` 中，M2 用到拓扑解析后升级为**直接**依赖，`go.sum` 未新增任何模块（OPEN-M2-1 由此关闭） |
| 镜像内内容物（交叉编译 + ELF 静态链接 + 0003 已嵌入） | **PASS** | `scripts/check-binaries.py` 全项通过：iip-server 11.33 MB / iip-worker 8.77 MB，均 `PT_INTERP` 缺失 + 无 libc/libpthread/libdl 引用 = 真静态；版本串 `0.0-m2` 注入生效；server 含 0003 迁移与 `idx_incident_alerts_needs_review`，worker 按预期被死代码消除剥离 |
| 容器链路实测（`docker build` → `compose up` → 健康收敛） | **PASS** | 2026-10-01 补验（Docker 29.8.0 / linux/overlay2）：**清空 named volume** 后全绿，**27 项断言 0 失败**。覆盖迁移到版本 3 且非 dirty、8 张表、`needs_review` 列与部分索引、`correlation_rules` 种子权重逐位等于 V1.0 §5、GET 配置与种子一致、PUT 非法权重 422 / 未知字段 400，以及**容器内**的关键行为：阈值抬到 0.99 → 本轮 2 条告警均判 NEW（关联边 2 条、带分数 0 条、各自建事件）；阈值还原 → 本轮 2 条告警均被关联进**同一个** Incident（关联边 2 条、带分数 2 条、涉及事件仅 1 个）。全程**未重启任何容器**，即「配置热生效」的容器级证据。**补 L2 后重跑为 32 项断言 0 失败**（27 基线 + 5 项拓扑/依赖维新增，见上表「容器内 `config/topology.yaml` 与依赖维」行）；**2026-10-02 补 4.5.0「干净起点」后为 34 项 0 失败**（再加 2 项 TRUNCATE 校验），且该次是在**未清空 named volume** 的脏数据环境下跑的 |
| 验收脚本可重复执行（named volume 保留数据后重跑） | **PASS**（2026-10-02 修正：原结论记为「成立」，实为**当时未暴露**） | L2 追加依赖维断言后这条**不再成立**：2026-10-02 在**未清空** named volume 的情况下重跑，4.5.10 由 0.7133 抬到 **0.7667 → 假 FAIL**（容器里 `alert_id` 从 13 起、`incidents` 总数 6，是上轮残留）。根因为历史告警喂高了 historical 维：差额 `0.7667 − 0.7133 = 0.0534 = 0.534 × 0.10`，而该维权重恰为 0.10，钩稽得上。**未放宽断言区间**，改为在脚本中补 **4.5.0「干净起点」**：迁移后、投递前 `TRUNCATE incident_alerts, alerts, incidents, correlation_feedback CASCADE`（保留 `schema_migrations` 与 `correlation_rules` 的 default 种子行；`CASCADE` 不可省，因 `incident_alerts` 对 `alerts`/`incidents` 有外键）。修复后在**同一份脏数据**上复跑：truncate 生效、三表计数归零、得分精确回到 **0.7133**、**34/34** 全绿。另：去重按**指纹**判定且**无时间窗**（V1.0 §4.2），若不做跨**进程调用**的唯一化，第二轮会被上一轮遗留的 firing 行整批吃掉（实测 `skipped:2`、落库 0 行），故注入 `RUN_TOKEN` |
| `golangci-lint run` | **PASS**（2026-10-01 修复至 rc=0；原为 FAIL） | M2 终点累计 **17 项**（gocritic hugeParam 13 / rangeValCopy 1 / ifElseChain 1 + govet shadow 2），用与 CI 同版本 v1.61.0 + 同配置复现。**已于 2026-10-01 逐项修复至 rc=0**：无排除规则新增、无 `nolint`，唯一配置改动是 gocritic `hugeParam` 阈值 80 → 128 B。方案见 ADR-012；证据 `D:\_iip_setup\_lint_m3_final.txt`（`LINT_RC=0`）、`_l1_gate.txt` |
| 容器内 `go test -race ./...` | **PASS** | 本机 Windows 无 gcc 跑不了；在 `golang:1.23-alpine` 容器内 `apk add gcc musl-dev` → `CGO_ENABLED=1`，8 个有测试的包全 ok、`RACE_RC=0`。装模块走「挂载宿主 modcache + `GOPROXY=off`」，全程零网络。2026-10-01 的 L1/L2 改动后重跑仍 PASS |
| 容器内 `config/topology.yaml` 与依赖维 | **PASS**（2026-10-01 修复；原为「容器内缺失、依赖维恒 0」） | 原 `Dockerfile` 只 `COPY cmd/ internal/ migrations/`，`config/` **从未进过镜像**，而缺文件时 `LoadTopology` **不报错** → 容器内依赖维恒 0 却断言仍全绿（**静默降级**）。现改为 compose 挂载 `../config:/config:ro` + `IIP_TOPOLOGY_FILE`（ADR-013），并新增**可判定**正向断言：日志「服务拓扑已加载」且 `services` ≥4、跨服务直连边分数落在 [0.70, 0.73)（无拓扑时上限仅 0.5333）。容器链路 **34 项断言 0 失败**（27 基线 + 5 项拓扑/依赖维 + 2 项干净起点）。**注意**：依赖维这条断言依赖「无历史共现」的前提，跑第二次前必须走 4.5.0 干净起点，否则 historical 维会被 named volume 里的上轮数据喂高（2026-10-02 实测 0.7133 → 0.7667） |
| README 新增「M2 验收状态」表；`docs/decisions.md` 记录 M2 决策 | **PASS** | 本表 + ADR-008 / ADR-009 / ADR-010（另 M3 前置清理记 ADR-012 / ADR-013） |

**本轮实测发现并修复的缺陷**（详见 `docs/decisions.md` ADR-008）：

`CalculateScore` 直接把五维加权和作为判定依据。但默认 `merge` 阈值 0.80 与
「时间 1 + 服务 1 + 标签 1 + 历史 1」的**数学结果恰好相等**，而 IEEE 754 下
`0.25 + 0.25 + 0.20 + 0.10` 算出来是 `0.7999999999999999`（比 0.80 少 1 ulp），
于是数学上恰好达阈、按 V1.0 §5.3 的 `>=` 语义应判 MERGE 的组合**被误判为 REVIEW**。
已改为在 `CalculateScore` 出口统一量化到 6 位小数，并新增
`TestCalculateScoreThresholdBoundary` 钉死该边界（该用例同时断言未量化的原始浮点值确实低于阈值，
防止将来有人删掉量化后又以为测试多余）。

> 另外修正两处自身的测试缺陷，一并记录以免重蹈：
> ① 迁移用例里用 **map 遍历**累加权重和——Go 的 map 迭代顺序随机而浮点求和对顺序敏感，
> 导致该断言时红时绿（曾出现「同一份代码单包跑 FAIL、全量跑 PASS」）；已改为切片固定顺序 + 容差。
> ② `check-binaries.py` 的迁移标记只含 0001/0002，新增的 0003 漏在校验之外；
> 已补 `0003_correlation_m2.up.sql` 与 `idx_incident_alerts_needs_review` 两条标记。

> **容器链路补验中修掉的脚本缺陷**（均为验收脚本自身问题，**产品代码零改动**）：
> ① 断言里写成了 `score` 列名，而 `incident_alerts` 的该列在 0001 就定名为 `correlation_score`
> ——SQL 直接报错、辅助函数返回空串，看上去像产品 FAIL，实为脚本自己写错列名；
> ② JSONB 中的 numeric 输出为最简形式（种子写 `0.20`，取出得 `0.2`），断言按字面比对必然失败；
> 已改用 `to_char(..., 'FM0.00')` 归一（`FM` 不可省：不带它 PostgreSQL 会为负号预留一格，输出带前导空格的 ` 0.20`）；
> ③ 告警标签的 `run` 值原先只做到「跨**轮次**唯一」，未做到「跨**进程调用**唯一」。
> 去重按指纹且**无时间窗**，而 compose 用 named volume 保留数据，于是脚本第二次执行时
> 两轮告警被上一轮遗留的 firing 行整批吃掉（实测 `skipped:2`、落库 0 行）。
> 已改为注入 `RUN_TOKEN`（本次运行的时间戳），并**连跑两次**作为回归证据。
> ④ **缺「干净起点」，第二次执行必假 FAIL**（2026-10-02 实测）：`compose down` 不加 `-v`，
> named volume 里留着上轮的 alerts/incidents → 历史共现喂高 historical 维 →
> 4.5.10 依赖维得分由 0.7133 抬到 0.7667 而越出 [0.70, 0.73)。
> 识别方法：看容器里 `alert_id` 是否从 1 起。**修法是加 TRUNCATE，不是放宽区间**——
> 已补 4.5.0 干净起点（详见上表「验收脚本可重复执行」行）。
> 这条之所以在 M2 期没暴露：当时该断言还不存在，是补 L2 时才引入的。

> 复现方式：真库验收用 `python D:\_iip_setup\m2_pg_gate.py`（单次调用内起库 → 跑真库用例 → 回收，
> 报告落在 `D:\_iip_setup\_m2_pg_gate_report.txt`）。

---

## M3 验收状态（如实标注）

M3 的完成定义见 `docs/IIP-M3详细开发手册.md` §八，交付明细见 `docs/M3-交付详细报告.md`。
**当前状态：S1 + S2 已交付，S3（D8–D10 前端）未开工。** 下表按「已实测」与「未开工」分开标注，
未开工项一律不写成通过（AGENTS.md 约束 5 / 12）。

### 已完成并实测（S1 + S2）

| 验收项 | 状态 | 说明 |
|--------|------|------|
| `0004` 迁移 up/down/up 三连；`review_suppressions` 与约束放宽就位 | **PASS** | `migrations/0004_incident_lifecycle_m3.{up,down}.sql`；真库 4 用例全 PASS（`_m3_s1_mig.txt`），down 为**严格逆序**且三步破坏性写在文件头 |
| 6×6 状态转换矩阵逐格可判 | **PASS** | `internal/incident/status.go`（零 SQL）+ `status_test.go` **36 格逐格断言**，并与手册规范表交叉复核；未知状态双向不放行 |
| 乐观并发控制：条件更新 + 冲突回 409 | **PASS** | SQL `WHERE status = from_status` 用 `:execrows`，0 行即 `*ConflictError` → 409；真库 `TestLifecycleUpdateStatusOptimistic` |
| `resolved_at` 三态副作用 | **PASS** | 由 `ResolveAtEffect` 计算后再执行，真库覆盖「写 / 清 / ignored 不动」三种；`ignored` 不写是刻意设计——误报不该污染 MTTR |
| 状态转换与 Timeline **同事务**（两个方向都验） | **PASS** | 正向：冲突时 `return` 在 `AppendTimelineEntry` 之前 → 真库断言 **timeline 表不多一行**（2026-10-03 补，此前只在 handler 层用替身验过状态码）。反向：用 trigger 制造确定性写入失败，证明留痕失败时状态一起回滚 |
| 冲突不得污染 `resolved_at` | **PASS** | 2026-10-03 补：冲突时若误写 `resolved_at`，一次失败的流转就会给事件盖上「已解决」时间戳 |
| 6 个新端点（事件列表/详情/流转/待确认队列/判定/抑制）HTTP 契约 | **PASS** | `internal/api/incidents.go` + `reviews.go`；契约用例锁定 200/201/400/404/409/422/503 的**可判别性**，含两个路径参数都要校验 |
| 列表过滤/排序/聚合计数 | **PASS** | 真库 `internal/repository/lifecycle_query_test.go`；分页夹紧、空过滤展开、时间窗下推逐项断言 |
| 抑制（reject）生效且可幂等 | **PASS** | 真库 `internal/repository/reviews_test.go` 覆盖命中/幂等/级联/列值；`internal/repository/reviews.go` |
| **业务闭环端到端**（M1 接入 → M2 关联 → M3 处置 → 抑制生效） | **PASS** | 新增 `scripts/verify-func.py`（真实 PG + 真实二进制 + 真实 HTTP），**48 项断言全 PASS，连跑 4 次一致**。首轮即抓到 250 条真库用例都没抓到的缺陷（JSON tag 全漏，真实字节流是 `{"ID":2}` 而非 `{"id":2}`）→ 已补 `internal/api/response_contract_test.go` 键名契约测试 |
| 抑制后同指纹新告警降级为 NEW 并清空 `incident_id` | **PASS** | verify-func F1 / F2：抑制行落库、边被挡住、指针为 NULL |
| healthz 三态闭环 | **PASS** | verify-func F7：200 `ok` → 停库 503 `unavailable` → 恢复 200（真查库，非假健康）；特意排在最后 |
| `go build` / `go vet` / `gofmt` / `golangci-lint v1.61.0` | **PASS** | 四项 rc=0。S1 修 3 项 `sloppyReassign`、S2 修 2 项 `govet shadow`（改独立变量名，**未加排除规则**），查漏补缺轮 rc=0 |
| 真库用例全绿 | **PASS** | S2：**PASS=250 FAIL=0 SKIP=0**（809.9s）；无库对照组 SKIP=49，证明用例确实依赖真库。查漏补缺轮新增 7 条真库用例全 PASS（`_gap_check.txt`） |
| 容器内 `go test -race ./...` | **PASS** | S1 后 rc=0、8 包 ok（18m59s，`_race_s1.txt`），含新增的 `internal/incident` 与 `internal/repository` |
| 容器链路 `verify-images.py` | **PASS**（2026-10-03 修复后重跑） | **41 项断言 0 失败**。此前从 M3 起一直红着：`0004` 新增 `review_suppressions` 使表数 8→9，而断言还停在 M2 的数字。**未改成硬编码 9**，改为断言 8 张关键表各自存在（加迁移不会再失效）。另修掉端口探测把「连接超时」误判为「已占用」的缺陷（Windows 防火墙静默丢包 → `TimeoutError` 也属 `OSError`） |
| 验收脚本可重复执行 | **PASS** | `verify-func.py` 连跑 4 次一致；`verify-images.py` 在**脏数据环境**（named volume 未清）下同样 41/41，靠脚本内 4.5.0「干净起点」TRUNCATE 保证 |

### 未开工（如实标注，不得记为通过）

| 验收项 | 状态 | 说明 |
|--------|------|------|
| D8–D10 前端：路由与布局壳、事件列表、详情双 Tab、处置交互 | **NOT DONE** | 按 ADR-016 **S3 一次性交付**，不拆视觉壳。前端仍极薄：`web/src` 仅 5 个源文件 235 行，`App.tsx` 全文 7 行（直接 `return <Alerts />`），没有路由/布局壳/导航/主题定制——准确说法是「UI 还没搭」，不是「UI 要翻新」 |
| 前端自动化测试 | **NOT DONE** | 禹于 2026-10-02 裁定**延到 M5**（ADR-016）。手册 §4.8 的前端单测在 M3 为 NOT DONE，**不得写成通过** |
| `timeline` / `evidence` 两张表有写入方 | **NOT DONE** | 两表 0001 就建好，但 S1 的 `InsertTimelineEntry` **只经 `Transactor` 在流转路径写入**；`evidence` 至今**无任何代码写入**（`internal/db/queries/` 只有 alerts / correlation_rules / incidents / incident_alerts / reviews / timeline）。凡「信息丢弃由 timeline 承接」这类说法**目前不成立** |
| CI 在 GitHub 上执行 | **无法验证** | 本仓库**没有 git remote**，`.github/workflows/ci.yml` 从未在 GitHub 上跑过。两个 job 已做**本地等价复现**（`-race` 容器内 PASS、`golangci-lint` v1.61.0 rc=0），但这不等于 CI 绿 |

### 本轮（2026-10-03 查漏补缺）发现并修复的缺陷

**产品缺陷 1 —— 详情页把「只显示前 200 条」说成「一共 200 条」。**
M3 手册验收清单第 7 条「详情 API：超 200 条时 `truncated=true`」此前 **0 覆盖**——
要触发它得在真库里造 200+ 条关联边，写用例时最容易退化成「造 5 条、断言不截断」这种
没有信息量的检查。结果是**唯一会把「显示不全」说成「全部」的分支从未被真库跑过**，而它确实有缺陷：

```go
alerts, _ := s.tx.Alerts(ctx, id, MaxIncidentAlerts+1)   // 最多 201
if truncatedAlerts { alerts = alerts[:MaxIncidentAlerts] }  // 截到 200
AlertCount: len(alerts),   // ← 截断后的数，永远 ≤200
```

实际 250 条关联的事件，详情页显示「200 条关联告警」；而列表端走 SQL `count(*)` 是真值
——**同一个字段、两个端点自相矛盾**。已新增两条纯 SELECT（`CountIncidentAlertsByIncident` /
`CountNeedsReviewAlertsByIncident`）取真值，**迁移文件一行未改**。未截断时复用切片统计不多查库
（详情页是值守时最常打开的接口），这一条是 `TestDetailQueriesCountsOnlyWhenTruncated` 抓出来的
——第一版实现总在查，测试直接红了。

**脚本缺陷 2 —— 容器链路从 M3 起一直红着，且端口探测假阻塞。**
两处都是「测试自己坏了」而非产品坏，但同样让门禁失效：表数断言停留在 M2 的 8（`0004` 加了表）；
端口探测用 `connect()` 且只 `except OSError` 就判「占用」，而 Windows 防火墙对本机某些端口的
SYN **静默丢包**，`TimeoutError` 也属 `OSError`——「没人监听」被读成「被占用」，
脚本在**前置检查**就 BLOCKED。已改为：`ConnectionRefused` 与 `Timeout` 都判「未占用」，
其余 `OSError` 保守判占用（宁可误报阻塞，也不要判断错误时把容器拉起来撞端口）。
单点复现 4/4 与 `netstat` 一致。

> 复现方式：真库验收 `python D:\_iip_setup\m3_s2.py`（全量，约 30 分钟）；
> 业务闭环 `python scripts\verify-func.py`（约 1 分钟，48 项断言）；
> 容器链路 `python scripts\verify-images.py`（需 Docker daemon，约 4 分钟，41 项断言）。
> 本地库隔离粒度是 **schema 而非 database**——`testsupport.NewDB` 用「共享 session 库 + 每用例一个
> `iip_case_<hex>` schema」，因本机 `CREATE DATABASE` 恒定 35 秒且调 PG 参数无效（已实测证伪），
> 改后 61.2s/条 → 13.1s/条。

---

## M4 验收状态（如实标注）

**阶段**：M4（Evidence 自动收集）功能代码与验收脚本已全部落地，B1–B8 逐批提交。
详细报告见 `docs/M4-交付详细报告.md`；决策记录见 `docs/decisions.md`（ADR-017 / ADR-018）。

### 已完成并实测

| 判据（手册 §八） | 状态 | 实测 |
|------|------|------|
| 事件创建后 2 分钟内 ≥5 条证据 | **达成** | 3 个事件各 5 条，任务全 `done`，无死信 |
| `query` 字段可复制后复现 | **达成** | 断言非空 + 前端一键复制 |
| webhook 延迟在数据源宕机时无变化 | **达成** | 31ms → 61ms（预算 531ms）；采集不在接入路径上 |
| 恢复后自动补采 | **达成** | `attempts` 1→2→3 走完，该事件从 0 条变 5 条 |
| 三类采集器均有 mock server 单测 | **达成** | Prometheus / Loki / K8s（K8s 24 个用例） |
| 并发领取不重复 | **达成** | 真库并发用例，`FOR UPDATE SKIP LOCKED` |
| 死信与超时回收 | **达成** | 真库用例；退避 5s→10s 封顶 5min |
| 超 50 条时截断且 API 能读到标记 | **达成** | 响应含 `truncated`/`total`/`limit`，真计数来自 `count(*)` |
| 迁移 `0005` up→down→up 三连 | **达成** | `migrations` 包 4/4 全绿（164s） |
| build / vet / gofmt / lint | **达成** | 全部 rc=0 |
| 全量真库测试 | **达成** | **425 PASS / 0 FAIL / 0 SKIP**，串行 22.5 分钟 |
| **容器链路断言 0 失败** | **达成** | 59 条断言全通过（连跑 3 轮一致）；M4 新增 18 条：表/索引/CHECK 约束/采集器注册/`evidence=7`/6 类错误模式未聚合 |
| 宿主全栈冒烟链路 | **达成** | 45 PASS / 0 FAIL；从零建库 → 自动迁移 → 探活 → 优雅退出 → 清理全闭环 |
| 业务闭环链路 | **达成** | 75 PASS / 0 FAIL；含抑制闭环、跨事件指针、宕机零阻塞与恢复补采 |
| 前端证据 Tab 接真实数据 | **达成** | `tsc -b` + `vite build` rc=0 |

### 未达成 / 降级（如实标注，不得记为通过）

| 项 | 状态 | 原因 |
|----|------|------|
| K8s 采集器接真实集群 | **未验证** | 偏差 B1（ADR-017）：仓库零 K8s 依赖，只有 mock server 单测。**默认关闭** |
| `evidence.confidence` | **全部为空** | OPEN-M4-1：无采集器评估它。留空而非填 1.0 —— 填 1.0 等于宣称「确凿」而没有依据 |
| `trace` 类型证据 | **无采集器** | OPEN-M4-2 |
| 前端自动化测试 | **NOT DONE** | ADR-016：延到 M5。`web/package.json` 无 test 脚本，手册 §4.8 类条款在 M4 仍为 NOT DONE |

### 本阶段发现并修复的缺陷（四处，均由新写的测试抓出）

1. **K8s 事件被挂到错误的服务上**（ADR-017）。原用字符串前缀匹配判归属，
   `checkout-staging-1` 被算作 `checkout` 的事件 —— 等于把 A 服务的事故证据挂到 B 上，
   而页面上完全看不出来。改为「先在全命名空间定出唯一主人（最长匹配），再判归属」。
   中间踩过坑：只在「本服务的 Deployment 集合」内找最长匹配仍不够，
   因为 `checkout-staging` 不在集合里，于是被 `checkout` 认领。
2. **K8s 采集器开关是死配置**（ADR-017）。`EvidenceK8sEnabled=true` 且地址已配时，
   装配函数只打日志、**从不注册**。表现是「开关已开、地址已填、日志说已启用，
   但证据恒为 0 条且无任何报错」。之前没测出来是因为采集器的 24 个单测
   **全部用自己 new 的实例、绕过了 `buildRegistry`** —— 替身验不了装配链路。
   已补 `cmd/iip-worker/main_test.go` 的 3 个装配用例。
3. **迁移单步回滚用例的断言随迁移总数漂移**（ADR-018）。M4 加了 3 个迁移后，
   0003/0004 的单步回滚用例转红（断言 `latestVersion-2` = 2，实测 5）——
   **产品行为完全正确，红的是测试本身**，而且被测的 down 从头到尾没被测过。
   改用绝对版本 `Migrate(v)`，并把 `latestVersion` 改为从 embed 数出来。
4. **端到端判据 #3、#4 曾是假通过**。#4 的「证据 5→15 条」是因为新告警创建了新事件，
   而 `tasks` 3→3、`done=3` 第一轮就满足了 —— worker 从未重试过任何东西；
   #3 第二次投的告警与前面同指纹被去重 `skipped`。两处都改成真正能判别的断言。

> **复现方式**（四条链路互不重叠，缺一条就有一类缺陷漏网）：
>
> | 链路 | 命令 | 耗时 | 独占覆盖 |
> |------|------|------|----------|
> | 全量真库 | `python D:\_iip_setup\m4_final_gate.py` | 22.5 分钟 | 425 用例，**必须串行 `-p 1`** |
> | A 业务闭环 | `python scripts\verify-func.py` | 2 分 16 秒 | 抑制闭环 / 宕机零阻塞 / 恢复补采 |
> | B 宿主全栈 | `python D:\_iip_setup\rt_b.py` | 2 分 57 秒 | 从零建库 → 迁移 → 探活 → 优雅退出 |
> | C 容器链路 | `python scripts\verify-images.py` | 2 分钟 | **镜像内容物 / compose 注入 / 静默降级** |
> | D 静态门禁 | `go build/vet/gofmt` + `golangci-lint run` | 61 秒 | 规范类缺陷 |
> | E 前端构建 | `python D:\_iip_setup\build_web.py` | 12 秒 | `tsc -b` + `vite build` |
>
> 链路 B 依赖宿主 PG 在 5433，**必须走 wrapper**（脚本不起 PG，
> 不传 `IIP_PSQL` 会扑空并伪装成产品失败）。
> 链路 C 需 Docker daemon，compose down **不加 `-v`**（named volume 跨轮保留）。

---

## M5 验收状态（如实标注）

**阶段**：M5（Web UI 收口与部署形态）功能代码与验收脚本已全部落地，F1–F8 逐批提交。
详细报告见 `docs/M5-交付详细报告.md`；决策记录见 `docs/decisions.md`（ADR-021）。
里程碑 tag：**`v0.0-m5`**（指向 `88d363a`，2026-10-07 补打）。

> **本段补于 2026-10-07**。M5 阶段结束时漏了两项账：tag 未打、报告未写，
> 且 M5 的 10 条 OPEN 只有 1 条进了 `decisions.md` 登记册。均已补齐，
> 补账过程本身记在报告 §9。

### 已完成并实测

| 判据（手册 §八） | 状态 | 实测 |
|------|------|------|
| 配置页改权重/阈值后**免重启生效** | **达成** | **行为层**证明：关引擎 → 0 事件，开→ 并成 1 个，全程不重启 |
| 非法输入提交前被拦且指明字段 | **达成** | 双侧：前端拦截 + 后端 422 文案点名 |
| 证据可跳原始面板，query 正确编码 | **达成** | 真浏览器导航 3 条链接；`g0.expr` 反解逐字校验。⚠️ 见下方「未达成」第 1 条 |
| `compose --profile app up` 后打开一个地址即可用 UI | **达成** | 无头浏览器对 compose 映射的 8080 截图成功，**颜色种类 1198**（阈值 ≥4，纯白/纯黑失败页只有 1 种） |
| 静态托管**不吞 `/api`** | **达成** | 真进程 HTTP：`/api/v1/incidents` 200 + `application/json` + 体可 `json.loads` |
| 深链接 `/incidents/{id}` 刷新可打开 | **达成** | 5 条深链接 + 带 query 的全部 200 且是 index.html |
| 目录穿越被拒 | **达成** | **12 组穿越** + 真进程全部 400，未拿到文件内容 |
| 处置后**回读 timeline 确认落库** | **达成** | E2E-3，真进程 + API 回读 |
| 前端组件测试 ≥7 条 | **达成** | **159 用例 / 13 文件**全过（M5 收尾实测值，注见下） |
| E2E 5 条 | **达成** | F6 的 5 条 + F8 新增 4 条 = **9 条**，连跑 3 轮全绿 |
| 既有四条验证链路 0 失败 | **达成** | 容器链路**连跑 3 次**全绿（133s / 130s / 130s，每次 **64 PASS / 0 FAIL**） |
| `tsc -b` + `vite build` rc=0 | **达成** | `pnpm run build` rc=0 |
| Go 四项 rc=0、真库 SKIP=0、无 `nolint` | **达成** | 静态 5/5；真库宿主 **767 PASS / 0 FAIL**，SKIP 由容器内 Linux 实跑消除（见下） |

> **组件测试计数口径**：上面写的 **159 用例 / 13 文件**是 **M5 收尾时的实测值**。
> M6 S4 之后已变为 **172 用例 / 14 文件**（新增 `client.test.ts` 13 条）。
> 两个数都是当时真值，不统一改成当前值。

### 未达成 / 降级（如实标注，不得记为通过）

| 项 | 状态 | 原因 |
|----|------|------|
| 证据链接的「面板上真能看到结果」 | **未验证** | OPEN-M5-10：判据只到「URL 合法且能发起连接」。要验面板上真能看到这条查询，需真实 Prometheus/Grafana 实例 |
| UI 无鉴权 | **已由 M6 S4 关闭** | OPEN-M5-1。M5 阶段的部署形态无任何访问控制，当时靠 `deploy/README.md` 的「仅供内网」标注兜。**该标注在 S4 之后仍保留** —— 静态 token 只挡误连，不等于可公网暴露 |
| `evidence.confidence` | **全部为空** | OPEN-M5-9（OPEN-M4-1 延续）。需先有 RCA 才有能力评估，已排入 M6 的 S1 |
| 两个文件超 300 行 | **仍 OPEN，且已加剧** | OPEN-M5-7 / **OPEN-M6-1**。`main.go` 318→ **340**、`config.go` 352 → **410**（M6 S4 各加约 20 行）。超限自 M2 起存在，非 M5 造成 |
| CI 的 `pnpm test` job | **本地等价已复现，CI 侧未验证** | OPEN-M5-4。仓库**无 git remote** ⇒ `.github/workflows/ci.yml` 从未在 GitHub 上跑过 |
| 组件覆盖率 | **明确不追数字** | OPEN-M5-5。`EvidenceList.tsx` 44%、其余 5 个为 0。把覆盖率设成门禁阈值会诱导「为凑数字写测试」 |

### 真库 SKIP=0 的消除方式（须如实说明）

M5 收尾时有一条 SKIP（`TestHandlerAllowsSymlinkedRoot`），根因是
**Windows 无管理员权限时 `os.Symlink` 返回 nil 却建不出有效链接**。

消除方式：在 `golang:1.23-alpine` 内跑 Linux 侧 → `internal/webui` **PASS 24 / SKIP 0**。
并**单跑一次独立复核**（`-run TestHandlerAllowsSymlinkedRoot` → `=== RUN` → `--- PASS`，9 秒）
—— 因为「零 SKIP」也可能是「压根没跑」。

### 本阶段发现并修复的缺陷

**产品缺陷一处**：`serveIndex` 的文件句柄从未关闭（原用 `ServeContent` + 自建 reader），
导致 `t.TempDir()` 在测试末尾删不掉目录。
> **判据：测试末尾出现 TempDir cleanup 失败，先查有没有未关闭的句柄，别急着 `t.Skip`。**

**三条测试写错而非产品缺陷**（写下来是因为它们看起来都像产品坏了）：

1. `os.Symlink` 在无管理员权限的 Windows 上返回 nil 却建不出有效链接 → 见上。
2. `httptest.NewRequest` 遇反斜杠直接 panic → 另写 `doRaw` 手工构造
   `&url.URL{Path: p}`。**恰恰是这种非法输入最需要测**，不能因为构造工具嫌它非法就跳过。
3. 断言 `/v1.0..next/index.html` 期望 200 实得 301，**301 是正确行为**
   （`http.ServeFile` 对以 `/index.html` 结尾的 URL 会规范化重定向）。
   **把标准库的既定行为记成产品 bug 是这类测试的典型错法。**

**M5 早期有两次容器链路因Docker daemon 半锁死未跑成**，
那两次**如实记为「未通过」，不作数**。上表的三次全绿是重启 Docker Desktop 之后的结果。

> **复现方式**（`verify-func.py` / `verify-runtime.py` / `verify-images.py` / 静态门禁
> 四条链路互不重叠，缺一条就有一类缺陷漏网；详见 `docs/M5-交付详细报告.md` §7）

---

## M6 验收状态（如实标注）

**阶段**：M6（证据深化与遗留缺口收口）——注意与开发计划 §10 的 M6（通知与基础指标）
**不是同一件事**：手册 M6 已交付（下表），计划 M6 **未开工**（见「T7 与后续」）。
详细报告见 `docs/M6-交付详细报告.md`；决策记录见 `docs/decisions.md`。

### 已完成并实测

| 项 | 状态 | 实测 |
|---|------|------|
| S1 confidence / RCA | **达成** | `internal/rca/` 三态判据（Unassessable / Weak / Medium / Strong）+ 覆盖度 `Assign` + 档位表 `Compose`；宿主链路 10 条断言全绿（`1.0 倍→0.50` / `10.0 倍→0.65`、覆盖度 `2/6`）；覆盖率 93.6%，变异测试 8/8 证明判据非恒真。**「给不出依据必须留空」约束仍在**：容器链路实测 `total=7 assigned=0 unassessable=7` 是判据起作用，不是缺陷 |
| S2 trace 采集器 | **达成** | `internal/collectors/trace.go` + worker 条件注册，零迁移（`trace` 早在 0001 的 CHECK 内）；容器链路 3 条断言（禁用 ×2 + 启用 ×1）全绿；变异测试（摘 `MustRegister` 后如期变红）证明判据有鉴别力 |
| S3 容器截图（DoD 3） | **已闭环**（2026-10-12） | `verify-images.py` 整体 **87 PASS / 0 FAIL**；「列表 → 详情 → 处置」10 PASS；三张截图非空白；处置真落库（`status=investigating`、`timeline 2→3`）。此前判据写完从未真跑（Docker 引擎不可用），已如实登记后补跑 |
| S4 UI 鉴权 | **达成** | `IIP_AUTH_TOKEN` 静态 token 护 `/api/v1/*`；`/healthz` 与 `/auth/session` 放行；token 不回显日志；容器链路 401/200 双断言 |
| S5 账目归零 | **达成** | M4/M5 期只写在报告正文未进登记册的 OPEN 已全部补登 `decisions.md`，并就地关闭已关闭项 |
| 真库门禁 | **达成（环境限定）** | 宿主 **PASS=912 / FAIL=0 / SKIP=1**；**Linux 容器侧 SKIP=0**（`TestHandlerAllowsSymlinkedRoot` 真 PASS）。宿主那条 SKIP 是 Windows 无 `SeCreateSymbolicLink` 权限，与真库无关，属可接受项——引用时**必须写清是哪个环境** |

### 未达成 / 取舍（如实标注，不得记为通过）

| 项 | 状态 | 原因 |
|----|------|------|
| `v0.0-m6` tag | **未打** | 报告明示「不宣布 M6 全部完成」：宿主侧 SKIP=1 + 4 条 OPEN。功能已随 v0.1.0 发布（见 CHANGELOG） |
| K8s / trace 证据恒「无法评估」 | **OPEN**（OPEN-M6-2） | 采集器写不出 `change_ratio`（K8s API 不保留历史副本数、trace 无比字段）。**保持现状**：不为凑覆盖率硬编强度值；k8s Warning 事件侧已补可信 `change_ratio`，但 rollout 侧仍给不出 |
| K8s 采集器默认不注册 | **OPEN**（OPEN-M6-3） | 偏差 B1：仓库零 K8s 依赖，无 kubeconfig / RBAC。接入是部署侧的事，**不为验收打开开关**；现有证据只有 mock 单测 + 变异测试 |
| `Assign` 跨轮不重算 | **OPEN**（OPEN-M6-4） | 幂等是故意的（避免后批证据悄悄改掉前批结论）。覆盖度偏低属已知取舍，非缺陷 |
| `main.go` / `config.go` 超 300 行 | **OPEN**（OPEN-M6-1） | 340 / 410 行，自 M2 起存在且随里程碑加剧。按既有做法只拆本里程碑新增部分 |
| CI 在 GitHub 上执行 | **无法验证** | 仓库无 git remote，`ci.yml` 从未在 GitHub 跑过（本地等价复现：`-race` 容器内 PASS、lint rc=0） |

---

## T7 与后续（加固线，如实标注）

| 项 | 状态 | 说明 |
|----|------|------|
| T7.1 告警按月分区 + retention | **已收口** | 迁移 0008（ADR-022，取代 ADR-002）：分区键 `starts_at`、PK `(id, starts_at)`、UTC 月界（修非 UTC 会话时区错月 bug + 永久守卫测试）；保留期走 `iip_drop_expired_alert_partitions()` 整表 DELETE + 行级 CASCADE（实测 FK 引用的分区 DROP/DETACH/TRUNCATE 全失败）；`incident_alerts` 复合 FK。已知限制：分区只增不减；同指纹跨 starts_at 的 firing 可并存（`iip_scan_firing_duplicates()` 曝光） |
| T7.2 JSONB 索引调优 | **已收口** | 迁移 0009（ADR-023）：先取证后裁定（5 万 alerts / 5 千 tasks 规模 EXPLAIN 采样）。**删** labels GIN——14 个分区副本 `idx_scan` 全 0 = 零读者；**建** `idx_tasks_payload_incident((payload->>'incident_id')) WHERE kind='collect_evidence'`——两条消费查询原先全表扫且 tasks 无保留期。Q3「join 补分区键」变体实测更慢/持平，记录否决 |
| T7.3 压测（1 万条） | **已收口** | **6/6 判据 PASS**：吞吐 346 条/s、折算单条 p95=45.6ms < 200ms、零丢失、rejected=0；M2 Replay 指标无退化（1.0000/1.0000）。残留边界：候选限额下「新告警最佳匹配为非种子老成员」仍可能漏合并（窄，待 V1.1 复查） |
| T7.4 故障演练 | **已收口** | `scripts/fault_drill.py` **33/33 PASS**：真 kill worker → ReclaimStale 回收（attempts=2）；真停 PG 25s → 不重启进程自动重连 + 队列不丢；Redis 缺席 = 默认形态（依赖闭包静态证明） |
| T7.5 compose 生产化 + Helm | **已收口** | 7 服务 resources + 日志轮转；采集器 TLS（CA / insecure 双模式，读不到 CA 启动即失败）；`deploy/helm/iip` 简化版 chart（内置/外置 PG 双轨）。真栈容器链路段待 Docker daemon 恢复后补跑（T7.5 收口报告已如实标注） |
| T7.6 README + CHANGELOG + v0.1.0 | **已收口** | 本文档重构 + `CHANGELOG.md` + 标签四联动；tag `v0.1.0` 本地已打（push / GitHub Release 待 remote） |
| T7.7 E2E 一键脚本 | **已收口** | `scripts/verify-e2e.py` 编排 S0–S7 按 M1–M6 验收项 + MVP 总验收出状态；最近一轮 7 阶段 PASS、S7 容器链路因 daemon 缺席 BLOCKED（`--stages s7` 可补跑） |
| 开发计划 §10 M6（通知 + 自监控） | **全部收口** | **T6.1 / T6.2 / T6.3 / T6.4 / T6.5 全部完成**（ADR-024 / 025 / 026）：Slack 通知、去风暴 + 通用出站 webhook、`/metrics` 七指标 + worker 独立监听、抓取配置 + 记录/告警规则 + Grafana 面板 + 静态核对 `verify-observability.py`、[接入指南](docs/接入指南-Alertmanager.md) + `verify-onboarding.py`（文档代码块原文在真栈跑）。五项均属 `[Unreleased]`，v0.1.0 不含 |

---

## 发布（v0.1.0）

版本形态：git tag `v0.1.0` + 镜像 `iip-server:0.1.0` / `iip-worker:0.1.0` +
Helm chart `appVersion: 0.1.0`。四处标签（compose / Makefile `IMAGE_TAG` /
`verify-images.py` 的 `IIP_TAG` / chart）**必须同步**——这是 M2 起就有的纪律，
本次发布已由脚本判据锁定（见「T7.6 收口记录」）。

```bash
git tag -a v0.1.0 -m "v0.1.0：首个公开发布版"   # 指向 T7.6 提交
git push origin v0.1.0                            # 需先有 remote（本仓库当前没有）
make image-build                                  # 本机构建 0.1.0 镜像
```

**如实声明**：本仓库当前**无 git remote**，`git push` 与 GitHub Release
尚未执行；CI（`.github/workflows/ci.yml`）因此从未在 GitHub 上跑过，
「CI 绿」目前只有本地等价复现证据。完整变更与已知限制见
[`CHANGELOG.md`](CHANGELOG.md)。
