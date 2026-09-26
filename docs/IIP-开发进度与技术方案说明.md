# IIP 开发进度与技术方案说明

> 生成日期：2026-09-25
> 适用范围：IIP（Incident Intelligence Platform）项目 **M0 阶段**已完成部分 + 后续规划
> 代码位置：`D:\Documents\qwen-agent\iip`
> 文档状态：AI 起草，**待技术负责人确认**
> 本文性质：**汇总性文档**——把分散在 11 份上游文档里的进度、方案、过程、证据收进一份，便于整体阅读与交接

---

## 〇、阅读指引：本文与其他文档的关系

本文是**唯一的汇总入口**，不替代任何原始文档。原始文档仍是权威源：

| 文档 | 定位 | 何时读它 |
|------|------|----------|
| `docs/IIP开发计划.md` | V1.0 → MVP 的**总路线图**（M0-M7 里程碑） | 要了解整体规划与风险 |
| `docs/IIP-V1.0-技术设计文档.md` | **原始技术设计**（架构、DDL、算法伪代码） | 要改 schema 或算法 |
| `docs/IIP-M0详细开发计划.md` | M0 的**唯一技术规格**（DoD、T0.1-T0.7、验收清单） | 有冲突时以它为准（AGENTS.md 约束 1） |
| `docs/IIP-M0-AI开发计划.md` | M0 的**执行方式**（P1-P6 任务包、review 点） | 要复现这种协作模式 |
| `docs/IIP-M1详细开发手册.md` | **M1 的唯一开发依据**（Webhook 契约 / 五阶段流水线 / 迁移变更 / 任务拆解 / 验收清单） | 要开始 M1 开发 |
| `docs/decisions.md` | **ADR 决策记录** + 悬而未决项登记册 | 要追溯「为什么这么定」 |
| `docs/M0-总验收报告.md` | M0 验收清单**逐条核对** | 要验收结论与证据 |
| `docs/v1-测试报告.md` | **三条验收链路**的完整实测记录（51 项断言） | 要复现验收或看细节 |
| `docs/M0-小结.md` | 耗时对照、偏差说明、过程问题 | 要复盘工期与偏差 |
| `docs/migration-notes.md` | 迁移文件的**决策与冻结约定** | 要新增迁移 |
| `docs/P5-CI破坏性测试方案.md` | 防「CI 假绿」的**破坏性测试**方案 | 打 tag 前 |

> 本文会把关键结论复述一遍，但**任何数字如需引用到对外材料，请回到原始文档核对**。

---

## 一、项目定位与目标

### 1.1 一句话定义

IIP 是一个**面向自托管可观测性栈的开源事件智能平台**：把海量异构告警自动收敛成可操作的事件（Incident），并为每个事件自动组织可追溯的证据链，最终给出可解释的根因假设。

### 1.2 核心能力链路

```
Alert  →  Incident  →  Evidence  →  RCA
告警      事件         证据          根因
```

| 环节 | 做什么 |
|------|--------|
| Alert → Incident | 异构告警归一化、去重、五维相似度关联聚合 |
| Incident → Evidence | 为事件自动采集、组织可追溯的证据（指标/日志/变更） |
| Evidence → RCA | 基于证据生成可解释的根因假设与修复建议 |

### 1.3 五条设计原则

| 原则 | 说明 |
|------|------|
| Modular Monolith | V1.0 不做微服务，单体 + Worker 模式 |
| Evidence-First | 每条结论必须可追溯到原始数据 |
| No AI Required | V1.0 核心价值不依赖 LLM，AI 是增强层而非底座 |
| Self-Hosted | Docker Compose 一键部署，K8s/Helm 可选 |
| Prometheus-Native | 原生集成 Prometheus / Alertmanager / Loki / OTel |

### 1.4 里程碑路线图与当前位置

| 里程碑 | 内容 | 计划工期 | 计划周次 | 状态 |
|--------|------|----------|----------|------|
| **M0** | **项目骨架与环境** | **5 天** | 第 1 周 | **已完成（待打 tag）** |
| M1 | 告警接入与持久化 | 8 天 | 第 3 周 | 未开始 |
| M2 | Correlation Engine（技术核心） | 10 天 | 第 5 周 | 未开始 |
| M3 | Incident 生命周期与 Timeline | 5 天 | 第 6 周 | 未开始 |
| M4 | Evidence 自动收集 | 8 天 | 第 8 周 | 未开始 |
| M5 | 最小 Web UI | 10 天 | 第 10 周 | 未开始 |
| M6 | 通知与基础指标 | 5 天 | 第 11 周 | 未开始 |
| M7 | 加固与发布（v0.1.0） | 8 天 | 第 13 周 | 未开始 |

**当前进度：M0 的「可静态验证部分」与「需真实运行的部分」均已实跑通过，共 51 项断言 PASS / 0 FAIL / 0 BLOCKED。**
尚缺两个发版动作（推远端看 CI、打 `v0.0-m0` tag），技术负责人已裁决本轮「先本地运行，跳过提交」。

### 1.5 M0 的完成定义（DoD）

原定 DoD 为：

> `docker compose up` 拉起完整依赖 → server/worker 空壳可运行 → `/healthz` 返回 200 → 迁移在干净 PG 上可重复执行 → CI 三色通过 → 打出 tag `v0.0-m0`

其中前四条已**真实验证通过**；「CI 三色」的本地等价命令全部通过，但**真实 Actions 结果尚未观察**（见第六章）。

---

## 二、技术方案

### 2.1 技术选型（开工前一次性定死）

| 层 | 选型 | 实际版本 | 选型理由 |
|----|------|----------|----------|
| 语言 | Go | 1.23（本机 1.23.8） | V1.0 伪代码即 Go；单体 + Worker 模式契合 |
| HTTP 框架 | chi | v5.1.0 | 轻量、标准库兼容、中间件生态够用 |
| 数据库访问 | pgx | v5.7.1 | 类型安全、性能好 |
| SQL 生成 | sqlc | 配置就绪（`sqlc.yaml` + `queries/alerts.sql` 骨架） | 编译期校验 SQL，减少低级错误 |
| 迁移 | golang-migrate | v4.18.1 + `lib/pq` v1.10.9 | 与 V1.0 的 DDL 直接对应，支持 embed |
| 配置 | koanf | v2.1.2 + `pflag` v1.0.5 | 适配 Docker Compose 的多来源覆盖 |
| 队列 | V1.0 用 **DB polling**（`SELECT ... FOR UPDATE SKIP LOCKED`），Redis 仅作接口预留 | — | 降低部署依赖 |
| 前端 | React + Vite + TypeScript + Ant Design | React 18.3.1 / Vite 5.4.10 / TS 5.6.3 / antd 5.21.6 | 表格/时间线组件现成，个人项目出活最快 |
| 前端数据层 | @tanstack/react-query | 5.59.16 | 服务端状态管理 |
| 前端路由 | react-router-dom | 6.27.0 | — |
| 包管理 | pnpm | 9.15.9 | 比 npm 快且 lockfile 干净 |
| 容器 | Docker Compose | Docker 29.8.0 / compose v5.5.1 | V1.0 设计原则之一 |
| CI | GitHub Actions | 两个并行 job | 每次 push 必跑 |

> 所有依赖版本**钉具体版本号，禁止 latest**（AGENTS.md 约束 7）。

### 2.2 系统架构

**两个进程 + 五类依赖：**

| 组件 | 职责 |
|------|------|
| `iip-server` | HTTP API 进程。M0 仅 `/healthz`（真查库）；M1 起接入 Alert 接收与查询接口 |
| `iip-worker` | 后台 Worker。polling 循环 + 优雅退出；M4 起承担任务队列与证据采集 |
| PostgreSQL 16 | 主数据库，存 Alert / Incident / Evidence / Timeline |
| Redis 7 | 队列接口预留（**V1.0 代码不连**，用 DB polling 替代） |
| Prometheus | 指标源 + 告警规则（M1 造真告警用） |
| Alertmanager | 告警路由，receiver 指向 `host.docker.internal:8080/api/v1/alerts/webhook` |
| Loki | 日志存储（M4 证据采集用，M0 只保证健康） |

**进程模型要点：**

- `iip-server`：chi 路由 + `RequestID` / `RealIP` / `AccessLog` / `Recoverer` 中间件；`ReadHeaderTimeout` 5s；SIGTERM 后 10s 超时优雅关闭；启动时**自动执行迁移**（幂等）。
- `iip-worker`：`time.Ticker` polling；每轮任务在**独立 goroutine** 中执行并计入 in-flight；内置 **panic recovery**（单任务 panic 不带崩 worker）；60s 心跳日志；收到信号后 **drain**——停止领新任务、等 in-flight 完成、上限 15s。

**两处刻意的设计选择：**

1. **`/healthz` 必须真查库**（`SELECT 1`）。不查库的 healthz 是假健康——它在数据库已经挂掉时仍返回 200，等于把探针变成装饰品。
2. **worker 不配 healthcheck**。worker 没有监听端口，无法用端口/HTTP 探活；它的健康判据就是「进程还活着」，由 `restart: unless-stopped` 兜底。**显式不配，而不是配一个无意义的假探针。**

### 2.3 代码仓库结构（实际）

```
iip/
├── cmd/
│   ├── iip-server/         # HTTP API 入口 + --healthcheck 自检模式 + healthcheck_test.go
│   └── iip-worker/         # Worker 入口 + main_test.go
├── internal/
│   ├── api/                # M1 起 HTTP handler 分层（M0 占位 .gitkeep）
│   ├── config/             # koanf 配置加载 + 表驱动单测
│   ├── db/queries/         # sqlc 查询定义（alerts.sql 骨架）
│   ├── logging/            # log/slog JSON 日志装配
│   ├── middleware/         # chi 访问日志中间件
│   └── store/              # pgxpool 连接、迁移执行、健康检查
├── migrations/             # 0001_init.up.sql / .down.sql + embed.go
├── web/                    # Vite + React + TS + antd（M0 占位页）
├── deploy/
│   ├── docker-compose.yml  # 五依赖 + 两应用（profiles: [app]）
│   ├── prometheus/         # prometheus.yml + rules/highlatency.yml
│   ├── alertmanager/       # alertmanager.yml
│   └── loki/               # loki-config.yml
├── scripts/                # 三个验收脚本（Python）+ pre-commit + tasks.ps1
├── testdata/replay/        # M2 关联算法回放评估集（M0 仅 README）
├── docs/                   # 规格与决策文档（权威源）
├── Dockerfile              # 多阶段构建，--target server|worker
└── .github/workflows/ci.yml
```

**分层纪律（架构约定的边界，M0 已守住）：**

- `cmd/` 只做「装配」——读配置、连库、起循环、处理信号。**零业务逻辑**（AGENTS.md 约束 2）。
- 能力下沉到 `internal/` 各包：`store` 管连接与迁移，`config` 管配置，`logging` 管日志装配，`middleware` 管横切。
- M0 阶段 `internal/api/` 只有 `.gitkeep`——这是**刻意的空**，不是遗漏。M0 的价值在于地基每块砖都验收过。

### 2.4 数据模型

**8 张表**（7 张业务表 + `schema_migrations` 版本表）：

| 表 | 作用 | 关键点 |
|----|------|--------|
| `alerts` | 告警原始记录 | `fingerprint`（稳定 label 集 SHA256）、`labels`/`annotations`/`raw_payload` 三个 JSONB、`GIN(labels)` 索引；V1.0 **不分区**（ADR-002） |
| `incidents` | 事件主体 | `root_causes` / `recommendations` JSONB、`service`、`started_at`/`resolved_at` |
| `incident_alerts` | Alert ↔ Incident 多对多 | 复合主键 `(incident_id, alert_id)`、`correlation_score DECIMAL(5,4)` |
| `evidence` | 证据条目 | `type`（metric/log/trace/deployment/config_change/historical）、`query`、`value`、`confidence` |
| `timeline` | 事件时间线 | `event_type`（alert_fired/alert_resolved/status_change/evidence_found/user_comment）、`actor` |
| `correlation_rules` | 可配置关联规则 | `rule_config` JSONB、`weight`、`UNIQUE(name)` |
| `correlation_feedback` | **M2 反馈学习预留** | 建表成本 5 分钟，避免 V1.x 加表迁移（ADR-004） |
| `schema_migrations` | golang-migrate 版本表 | 由迁移工具自动维护，`version=1` 且 `dirty=false` |

**结构规模（实测）**：主键 7 个（7 张业务表各一）、外键 5 个、索引 27 个（含 `GIN(labels)` 与 `UNIQUE(name)`）。

**四条工程决策（已在 `docs/decisions.md` 记录为 ADR-001~004）：**

| ADR | 决策 | 理由 |
|-----|------|------|
| ADR-001 | 状态/类型列用 **VARCHAR + 命名 CHECK 约束**，不用 PG ENUM | PG ENUM 加值需 `ALTER TYPE ... ADD VALUE`，事务内受限且不可回滚；CHECK 更灵活 |
| ADR-002 | `alerts` 表**暂不分区** | 分区表主键必须含分区键，会在此刻锁死 fingerprint 全局唯一策略；单表 + 索引足够撑到 M7 再评估 |
| ADR-003 | 时间列**一律 TIMESTAMPTZ**，每表均有 `created_at` | 禁止本地时区；`evidence.timestamp` 用采集源时间，不用 `NOW()` 兜底 |
| ADR-004 | 建 `correlation_feedback` 预留表 | 5 分钟成本，省一次 V1.x 加表迁移 |

**约束完整性机制**：6 项非法值（`alerts.status` / `alerts.severity` / `incidents.status` / `incidents.severity` / `evidence.type` / `timeline.event_type`）在插入时**逐条被 CHECK 拒绝**，错误信息含约束名——由 `scripts/verify_inserts.sql` 实跑验证，6/6 全部拒绝。

**迁移冻结约定（AGENTS.md 约束 10）**：`0001_init.up.sql` / `0001_init.down.sql` **一经合并即冻结**。后续 schema 改动只能新增 `0003_*.sql` / `0004_*.sql`。

> 命名说明：M0 计划原文写 `0001_init.sql` + `0002_*.down.sql`，但 golang-migrate 要求同版本 up/down **成对**才能执行 `down-all`。经技术负责人 2026-09-25 裁决采用成对命名。

### 2.5 配置方案

**来源优先级（高 → 低）**：命令行 flag > 环境变量 > `config.local.yaml`（可选，已进 `.gitignore`）> 内置默认值

| 字段 | 环境变量 | 默认值 | 说明 |
|------|----------|--------|------|
| `HTTPAddr` | `IIP_HTTP_ADDR` | `:8080` | HTTP 监听地址 |
| `PGURL` | `IIP_PG_URL` | **无默认，必填** | PostgreSQL 连接串 |
| `LogLevel` | `IIP_LOG_LEVEL` | `info` | debug/info/warn/error |
| `WorkerPoll` | `IIP_WORKER_POLL_INTERVAL` | `2s` | worker 轮询间隔 |
| `DBPollOnly` | `IIP_REDIS_DISABLED` | `true` | 禁用 Redis，仅用 DB polling |

**快速失败原则**：配置字段级校验（`Validate()`）不触达外部依赖；连通性校验（`pgxpool.Ping`）由调用方在拿到 URL 后立即执行，**失败即 fatal 退出，不带病启动**。

### 2.6 部署形态

**镜像构建**：单个 `Dockerfile` 多阶段出两个镜像（`--target server` / `--target worker`），builder 阶段共享 Go 模块缓存。

| 阶段 | 基础镜像 | 说明 |
|------|----------|------|
| builder | `golang:1.23-alpine` | `CGO_ENABLED=0` 静态编译，`-trimpath -ldflags="-s -w -X main.version=..."` |
| runtime | `gcr.io/distroless/static-debian12:nonroot` | 无 shell、无包管理器、非 root（UID 65532） |

**实测镜像体积**：`iip-server` 13.8 MB、`iip-worker` 11.3 MB。

**distroless 带来的连锁设计**：镜像内没有 `sh`/`curl`/`wget`，因此**无法用 HTTP 探针做 healthcheck**。解决方案是让二进制自身充当探针——`iip-server --healthcheck --http-addr=:8080` 请求本机 `/healthz` 后按结果退出 `0/1`，compose 的 `healthcheck.test` 直接调用该 flag。退出码即健康状态。

**compose 编排（7 服务）**：

| 组 | 服务 | 镜像（版本钉死） | 端口 |
|----|------|------------------|------|
| 依赖组 | postgres | `postgres:16-alpine` | 5432 |
| | redis | `redis:7-alpine` | 6379 |
| | prometheus | `prom/prometheus:v2.54.0` | 9090 |
| | alertmanager | `prom/alertmanager:v0.27.0` | 9093 |
| | loki | `grafana/loki:3.1.0` | 3100 |
| 应用组（`profiles: [app]`） | iip-server | `iip-server:0.0-m0` | 8080 |
| | iip-worker | `iip-worker:0.0-m0` | 无 |

**为什么应用组用 `profiles: [app]` 隔离**：保证 T0.6 原验收命令（`up -d` → 五服务全 healthy）语义**完全不变**。默认 `up -d` 只起依赖组，需要连应用一起起时用 `--profile app`。

**healthcheck 策略（每个服务不同，且都有理由）**：

| 服务 | 探针 | 为什么这样 |
|------|------|------------|
| postgres | `pg_isready -U postgres -d iip` | `-U`/`-d` 必须与上面环境变量一致，否则因默认库不存在而恒 unhealthy |
| redis | `redis-cli ping \| grep -q PONG` | 文本响应固定 |
| prometheus | `wget -q -O - .../healthy \| grep -q Healthy` | 响应体是 `Prometheus Server is Healthy.` |
| alertmanager | `wget -q -O /dev/null .../healthy` | **只依赖退出码**——它的响应体是 `OK`，与 prometheus 不同（这里踩过坑，见 4.3） |
| loki | `wget -q -O - .../ready \| grep -q ready` | 响应体固定 |
| iip-server | 二进制自身 `--healthcheck` | distroless 无 shell |
| iip-worker | **不配** | 无监听端口，假探针无意义 |

### 2.7 质量门禁

**CI（`.github/workflows/ci.yml`，push 到任意分支 + PR 到 main）两个并行 job：**

| job | 步骤 |
|-----|------|
| `backend (go)` | checkout → setup-go@v5（`go-version-file: go.mod`）→ **校验 `go.sum` 已提交**（缺失给明确指引并 fail）→ `go mod download` → golangci-lint-action@v6（v1.61.0，`.golangci.yml`）→ `go build ./...` → `go test -race -coverprofile` → 上传覆盖率 artifact |
| `frontend (web)` | checkout → **校验 `pnpm-lock.yaml` 已提交** → pnpm/action-setup@v4（pnpm 9）→ setup-node@v4（node 20 + pnpm cache）→ `pnpm install --frozen-lockfile` → `pnpm tsc --noEmit` → `pnpm build` |

**设计要点**：
- 同分支新推送**取消旧的进行中运行**（`concurrency` + `cancel-in-progress`），省 CI 额度。
- `permissions: contents: read` 最小权限。
- 两个「前置校验」步骤是**刻意的**：`go.sum` / `pnpm-lock.yaml` 缺失时报出的是「请在本机执行 X 后提交」，而不是让 build 阶段抛出难以理解的错误。

**本地门禁**：`scripts/pre-commit.sh` / `pre-commit.ps1`（`gofumpt -l` 无输出才放行）。**不许跳过**（AGENTS.md 约束 6：不加 `--no-verify`、不注释 lint 规则）。

**M0 实测质量门禁（全部 rc=0）**：

| 命令 | 结果 |
|------|------|
| `go build ./...` | ExitCode 0 |
| `go vet ./...` | ExitCode 0 |
| `go test ./...` | ExitCode 0（cmd/iip-server、cmd/iip-worker、internal/config 三包 ok） |
| `go mod verify` | ExitCode 0（all modules verified） |
| `gofmt -l .` | 零输出（无格式违规） |
| `golangci-lint run ./...` | ExitCode 0，**零告警**（62s） |
| `pnpm tsc --noEmit` | ExitCode 0 |
| `pnpm build` | ExitCode 0（3064 modules） |

---

## 三、开发过程（详细技术过程）

### 3.1 执行模式：AI 施工 + 人验收

M0 采用**压缩执行模式**：人工版 5 个工作日的计划，改由 AI 编码代理按 `P1-P6` 任务包施工，技术负责人只做四件事——**派任务包 → review diff → 跑验收命令 → 守门禁**。

| 任务包 | 对应 | 产出 |
|--------|------|------|
| P1 | T0.1 | 仓库骨架 + 前端脚手架 + `.gitignore`/`.editorconfig`/`.gitattributes` + Makefile |
| **P2** | T0.2 | `0001_init.*.sql` + INSERT 验证脚本。**唯一必须逐行精读的产出** |
| P3 | T0.3-T0.5 | config 包 + server 空壳 + worker 空壳 |
| P4 | T0.6 | compose 五服务 + prometheus/alertmanager/loki 配置 |
| P5 | T0.7 | CI 双 job + 本地 pre-commit + **破坏性测试** |
| P6 | D5 | 总验收 + decisions 整理 + M0 小结 + README |

**关键保险机制**：`AGENTS.md` 常驻 12 条约束，其中三条针对 AI 开发的三大事故源——**范围蔓延**（约束 2/11）、**改断言骗测试**（约束 3）、**幻觉式完成**（约束 5/12）。

### 3.2 阶段划分

整个 M0 实际走过四个阶段，而非计划里的「逐日串行」：

| 阶段 | 内容 | 特点 |
|------|------|------|
| 一 | 代码与骨架（T0.1-T0.7） | 相对顺利，AI 并行施工 |
| 二 | **环境攻坚** | **耗时最长，且完全不在原计划预算内** |
| 三 | 三条验收链路 | 链路 A/B 先行，链路 C 等环境就绪后补测 |
| 四 | 缺陷修复与复测 | 链路 C 补测暴露了 6 个此前不可能发现的缺陷 |

### 3.3 阶段二：环境攻坚（最耗时、也最值得记录的部分）

**这是 M0 最大的时间消耗，且全部不在 M0 计划预算内的活动。**

#### 3.3.1 工具链：GitHub 不可达导致安装受阻

Go 与 Git 的安装均受 GitHub 不可达影响，需绕镜像解决。最终环境台账：

| 组件 | 位置 | 版本 |
|------|------|------|
| Go | `C:\Program Files\Go\bin\go.exe` | 1.23.8 windows/amd64 |
| Git | `C:\Program Files\Git\cmd\git.exe` | 2.55.0.windows.3 |
| golangci-lint | `D:\_iip_setup\bin\golangci-lint.exe` | v1.61.0 |
| **PostgreSQL** | `D:\pgsql16`（绿色版） | 16.4，端口 **5433** |
| PG 数据目录 | `D:\pgsql16\data` | `initdb -E UTF8 --locale=C -A scram-sha-256` |
| Docker | `%LOCALAPPDATA%\Programs\DockerDesktop\resources\bin` | 29.8.0 + compose v5.5.1 + buildx v0.37.1 |
| WSL 运行时 | — | 2.7.14.0（`wsl.2.7.14.0.x64.msi`，SHA256 已校验） |

#### 3.3.2 PostgreSQL 绿色版：解压策略决定成败

全量解压 22649 个条目（`pgAdmin 4` 独占 18359 个）**实测 >11 分钟未完成**。

**改为只解 `bin` / `lib` / `share` 三个目录后，85 秒完成。** 这是一个可直接复用的经验——逆向工程一个绿色版数据库时，先问「我到底需要哪些目录」，而不是「解压全部再说」。

#### 3.3.3 WSL2 与 Docker Desktop：两层结构 + 装错包

**关键认知：WSL 是两层，不是一层。**

| 层 | 内容 | 判定方式 |
|----|------|----------|
| ① Windows 可选功能 | `Microsoft-Windows-Subsystem-Linux` + `VirtualMachinePlatform` | `dism` / CBS 查询；启用后**必须重启** |
| ② WSL 运行时本体 | `C:\Program Files\WSL\` + `WslService` | 需单独安装 |

**踩坑**：先装的是 `wsl_update_x64.msi`，报
`This update only applies to machines with the Windows Subsystem for Linux`。
根因——那是 **Win10 时代的旧内核更新包**，它检查的是已废弃的 `LxssManager` 服务，
在 Win11 24H2+ 上必然失败。

**正解**：装完整运行时包 `wsl.2.7.14.0.x64.msi`（从 GitHub releases 取，**SHA256 校验通过**）。

#### 3.3.4 沙箱约束：Docker Desktop 必须由人手动启动

**现象**：Docker Desktop 后端每次启动都崩在
`rename daemon.json.tmp -> daemon.json: Access is denied`。

**根因**：不是我改坏了文件，而是**沙箱的 `strip_write` 机制剥夺了写权限**——
凡是由我的 shell 派生的进程，对 `%USERPROFILE%\.docker` 都没有写权限。
而 Docker Desktop 启动时要原子重写 `daemon.json`（写临时文件 + rename）。

**关键验证**：`dangerouslyDisableSandbox` **不解决问题**——它不传递给子进程。

**结论**：`Docker Desktop` 必须由用户从开始菜单手动启动。这条已作为「红线」写进运维手册。

#### 3.3.5 镜像拉取：Docker Hub 不通 → 多源降级 + 预拉取

**背景**：Docker Hub 直连 502/超时；而 `mirror.gcr.io`、`public.ecr.aws` 这类源**只覆盖官方 `library/*`**，`prom/*`、`grafana/*` 拿不到。

**镜像源实测对比（2026-09，用 `hello-world` 测）**：

| 源 | 耗时 | 覆盖 | 备注 |
|----|------|------|------|
| `docker.m.daocloud.io` | **5.7s** | 全量 docker.io | 最快，且有 gcr/ghcr 旁路 |
| `dockerproxy.net` | 5.9s | 全量 | |
| `docker.1panel.live` | 7.0s | 全量 | |
| `mirror.gcr.io` | 3.8s | **仅官方 `library/*`** | 第三方命名空间拿不到 |
| `docker.xuanyuan.me` | 18.5s | 全量 | 免费版会限流 `toomanyrequests` |
| `hub.rat.dev` | 43.8s | 全量 | 太慢 |
| `docker.1ms.run` | 64.1s | 全量 | 太慢 |
| `docker.nju.edu.cn` | FAIL | — | pull access denied |
| `dockerpull.org` | FAIL | — | 超时 |
| `public.ecr.aws/docker` | FAIL | 仅官方 | toomanyrequests |

**关键判据**：判断镜像源能不能用，**必须用 `docker pull`**，不能用 `curl`/Python 探活代替——后者的 UA 与 TLS 指纹会被 Cloudflare 风控拦成 403，产生「脚本说不通、实际通」的假结论。

**非 Docker Hub 的 registry 走 DaoCloud 旁路**：`registry-mirrors` 只对 `docker.io` 生效，管不了 `gcr.io`。实测 `gcr.io` 直连超时，而 `gcr.m.daocloud.io/distroless/static-debian12:nonroot` 14s 拉成功。

**最终采用的稳方案：预拉取 + 重打标签**

```bash
docker pull <mirror>/library/golang:1.23-alpine
docker tag  <mirror>/library/golang:1.23-alpine  golang:1.23-alpine
```

这样 `Dockerfile` 的 `FROM` 与 compose 的 `image:` **直接命中本地镜像，无需修改项目任何文件**，构建与起服务全程零外网（实测构建 2.2s / 0.7s 全命中缓存）。

#### 3.3.6 容器内拉不到 Go 模块

Dockerfile 里 `go mod download` 走官方 `proxy.golang.org`，**容器内直连超时**（实测 connect refused），而 `goproxy.cn` 通。

**处置**：Dockerfile 加 `ARG GOPROXY`，**默认值保持官方**（生产/CI 行为不变），本地验收时通过 `--build-arg GOPROXY=https://goproxy.cn,direct` 覆盖。

> 注意：`ARG` 必须在 `go mod download` **之前**声明，否则那个 `RUN` 取不到值。

### 3.4 阶段三：三条验收链路

Docker daemon 一度不可用。与其「等环境好了再说」，不如**把任务拆成三条可独立验证的链路**：

| 链路 | 内容 | 为什么这样拆 |
|------|------|--------------|
| **A. 镜像内容物** | 按 Dockerfile builder 阶段**逐参数一致**地交叉编译，用自写 ELF 解析器校验产物是否满足 distroless/static 的硬前提 | 验证的**就是镜像里将要放的那个二进制**（同源码、同参数、同 GOOS/GOARCH/CGO）。把链路 C 的失败面压缩到「Docker 本身」 |
| **B. 应用端到端** | 用真实 PostgreSQL 16.4 从零建库，跑完整「迁移 → 探活 → worker 心跳 → 优雅退出」 | 不依赖容器，覆盖全部应用逻辑 |
| **C. 容器运行** | `docker build` + `compose --profile app up` + 容器内断言 | 只有真跑才能验编排层 |

#### 链路 A 的关键技术点：真静态链接

distroless/static 的硬前提是**真的静态链接**。验证方式是自己写 ELF 解析器读 program header（不依赖 `file`/`ldd`）：

| 检查项 | 结果 | 含义 |
|--------|------|------|
| ELF64 小端格式 | PASS | — |
| `e_machine = EM_X86_64` | PASS | — |
| **`PT_INTERP` 缺失** | PASS | **无动态加载器** |
| `PT_DYNAMIC` 缺失 | PASS | 无动态段 |
| 无 `libc.so`/`libpthread`/`libdl.so`/`ld-linux` 引用 | PASS | — |
| 版本串已注入（`-ldflags` 未静默失效） | PASS | — |

`PT_INTERP` 缺失 + 无 libc 引用 = **真静态链接**。若这里是动态链接，镜像会在启动瞬间报 `exec format error` 或 `no such file or directory`。

#### 链路 B 的一个反直觉结果：worker 里没有迁移 SQL，这是**正确的**

`store.Migrate` 只在 `cmd/iip-server/main.go` 被调用，worker 从不调用它。Go 链接器的**死代码消除**会把未被引用的 `store.Migrate` 及其 `iofs` 依赖整体剥离，因此 `//go:embed` 的迁移 SQL **不会进入 worker 二进制**。

这是正确且更优的结果——worker 本来就不该有建表能力，剥离后体积也更小（8.77 MB vs 11.19 MB）。

> 这个结果一度被误判为「worker 缺迁移是缺陷」，实际是**脚本把 server 的检查项错套到了 worker 上**（见 4.4）。

### 3.5 阶段四：链路 C 补测暴露的缺陷

WSL2 装好后链路 C 开跑。**关键结论：解除阻塞并不等于全绿**——它当场暴露了 6 个此前不可能发现的缺陷（见第四章 4.3 / 4.4）。修复后复测 14 项断言全 PASS。

**这印证了链路划分的价值**：链路 A 的交叉编译预验证只覆盖「代码与构建」这一层，容器编排层（healthcheck 文案、容器命名、依赖收敛）**必须真跑才能验**。

---

## 四、踩坑记录与根因（按类别）

> 本章是本文最有复用价值的部分。每条都写明「现象 → 根因 → 处置」。

### 4.1 工具链与网络

| 现象 | 根因 | 处置 |
|------|------|------|
| `This update only applies to machines with the Windows Subsystem for Linux` | 装的是旧内核更新包 `wsl_update_x64.msi`，它检查已废弃的 `LxssManager` | 改用完整运行时包 `wsl.<ver>.0.x64.msi` |
| `No suitable WSL release found on github` | `wsl --install` 要访问 GitHub API | 走代理，或改用离线 MSI |
| 功能启用后 WSL 仍不可用 | 没重启，功能还在 InstallPending | 重启并用 `LastBootUpTime` 确认 |
| 脚本探活镜像源返回 403，但 `docker pull` 成功 | curl/Python 的 UA、TLS 指纹被 Cloudflare 风控 | **以 `docker pull` 为准** |
| 镜像加速配好了但 `prom/*`、`grafana/*` 拉不到 | 用的是只覆盖 `library/*` 的源 | 换全量代理，或预拉取+重打标签 |
| **手改 `daemon.json` 加了多源，`docker info` 却只显示一个** | **Docker Desktop 用 GUI 里的配置回写了文件** | 让用户在 GUI「Docker Engine」里改；或干脆预拉取，不碰镜像源 |
| 容器内 `go mod download` connect refused | 官方 `proxy.golang.org` 国内不通 | Dockerfile 加 `ARG GOPROXY`，本地传 `goproxy.cn` |

### 4.2 数据库与迁移

| 现象 | 根因 | 处置 |
|------|------|------|
| 全量解压 PostgreSQL >11 分钟未完成 | `pgAdmin 4` 独占 18359/22649 个条目 | 只解 `bin`/`lib`/`share`，**85 秒**完成 |
| 迁移 `down-all` 无法配套执行 | 计划原文的命名（`0001_init.sql` + `0002_*.down.sql`）不满足 golang-migrate 的成对要求 | 裁决改用 `0001_init.up.sql` / `0001_init.down.sql` |
| 非法值 INSERT 未命中 CHECK | 测试 INSERT 缺 `source` 等 NOT NULL 列，被 not-null 先拦下 | 补全所有必填列，让错误**只能**来自 CHECK |
| `psql -c` 输出解析失败 | `-c` 会把命令标签 `INSERT 0 1` 一起输出 | 新增 `psql_val()` 取第一行第一列纯值 |

### 4.3 容器与编排

| 现象 | 根因 | 处置 |
|------|------|------|
| **`iip-alertmanager` 恒为 unhealthy**，但手工 `curl` 接口是好的 | healthcheck 从 prometheus 复制而来（`grep -q Healthy`）；但 alertmanager 的 `/-/healthy` 响应体是 **`OK`**，永远匹配不上 | 改为只依赖 `wget` 退出码：`wget -q -O /dev/null .../-/healthy` |
| 等健康通过了，过一会儿才暴露某个服务坏 | 只在 `iip-server=healthy` 就 break，此时 alertmanager 还在 `starting` | 改为「**无任何服务停留在 starting/created**」才算收敛，并新增「无 unhealthy」断言 |
| `docker exec postgres` 报 `No such container` | compose 里服务名是 `postgres`，但 `container_name` 是 `iip-postgres` | 动态取真实容器 ID（`compose ps -q <svc>`） |
| Docker Desktop 后端崩 `rename daemon.json.tmp: Access is denied` | 沙箱 `strip_write` 剥夺了派生进程对 `.docker` 的写权限 | **用户手动启动** Docker Desktop（红线） |

> **alertmanager 这条值得单独强调**：它**只有在容器真正跑起来之后才能被发现**。
> 手工访问接口是好的、容器却报 unhealthy——断言与现象不一致，根因是「探针检查了别的东西」。
> 这类缺陷在链路 A/B 阶段结构上不可能暴露。

### 4.4 测试脚本自身的缺陷（会造成「假绿」/「假红」）

> 这类缺陷**不报错，只是悄悄让断言空转或漏判**，是最危险的一类。修完必须重跑拿到全绿才算数。

| 现象 | 根因 | 处置 |
|------|------|------|
| 报出「通过」但产品其实是坏的（**假绿**） | `echo "rc=$?"` 取到的是上一条 `ls` 的状态，`go build` 的真实 rc 被掩盖 | 改用 Python `subprocess` 直接捕获真实 rc/stdout/stderr |
| `gofmt` 检查永远绿 | 调 `go fmt -l .`（`go fmt` 不接受 `-l`），rc=2 却判成 PASS | 改用 `gofmt.exe -l .`，并加 rc!=0 判定 |
| 断言「worker 缺迁移」误报（**假红**） | 把 server 的检查项套到 worker 上（详见 3.4） | 改为按角色区分检查项 |
| 两条 postgres 断言报 `No such container`（**假红**） | 硬编码服务名，实际容器名来自 `container_name` | 新增 `svc_container()` 取真实 ID |
| 优雅退出验不出来 | Windows 上 `Popen.terminate()` = `TerminateProcess`，Go 收不到 SIGTERM | 改用 `CTRL_BREAK_EVENT` 投递真信号，**成功验证 drain 逻辑** |
| 约束查询得 6 张表而误判 FAIL | 表名列表误写 `integration_configs` 且漏了 `correlation_rules` | 与迁移文件核对后修正 |
| 函数名说 `free`，语义是 `in_use` | 命名与实际相反，易误读 | 改名 `port_in_use()` |

### 4.5 沙箱环境约束（本机特有，需知）

| 约束 | 表现 | 应对 |
|------|------|------|
| **进程树被回收** | 我派生的后台进程活不过一条命令（`DETACHED_PROCESS` 也救不了） | 需要持续运行的东西**一律放进容器**（Docker 容器由 Docker Desktop 服务托管，不在我的进程树内） |
| 后台守护进程被强杀 | PostgreSQL 启动后下一秒就 `no response` | 改用**单次一体化脚本**（起库 → 验收 → teardown 在一次执行内完成） |
| coreutils 整批失效 | `ls`/`wc`/`tail`/`head`/`dirname`/`which` 全部 not found | 改用 Python 或专用工具，不反复试 shell |
| 沙箱管辖会话内不能启动 Docker Desktop | 见 3.3.4 | 用户手动启动 |

### 4.6 误判后纠正（诚实记录）

| 一度认为 | 实测结论 |
|----------|----------|
| `go build -o /out/xxx` 在 `/out` 不存在时会失败，是 Dockerfile 缺陷 | **误判**。实测 `go build -o` 会自动创建父目录，rc=0。后来加的 `mkdir -p /out` 仅为可读性 |
| 手改 `%USERPROFILE%\.docker\daemon.json` 加镜像源即可生效 | **在 Docker Desktop 下不生效**（它以 GUI 配置为准并回写文件）。结论：不要把验收建立在镜像源上 |
| worker 二进制里没有迁移 SQL 是缺陷 | **误判**。那是 Go 死代码消除的正确结果 |

---

## 五、验收结论与证据

### 5.1 三条链路结果

| 链路 | 内容 | 结果 |
|------|------|------|
| A. 镜像内容物 | 交叉编译 + ELF 静态链接校验 | **PASS** |
| B. 应用端到端 | 真实 PG 16.4 从零建库 → 迁移 → 探活 → worker 心跳 → 优雅退出 | **PASS**（37 项） |
| C. 容器运行 | `docker build` + `compose --profile app up` + 容器内断言 | **PASS**（14 项补测） |

**合计：51 项 PASS / 0 项 FAIL / 0 项 BLOCKED。**

### 5.2 链路 C 断言明细（14 项）

| 阶段 | 断言 |
|------|------|
| 前置 | Docker daemon 可用（29.8.0 / linux / overlay2）｜端口 8080 未被占用 |
| 构建 | `docker build --target server` → `iip-server:0.0-m0`｜`--target worker` → `iip-worker:0.0-m0` |
| 启动 | `compose --profile app up -d` 返回 0 |
| 健康 | `iip-server` healthy｜**无服务 unhealthy**｜**无服务停留在 starting/created** |
| 功能 | `GET /healthz` 200 `ok`｜容器内 `--healthcheck` 退出码 0｜`schema_migrations=1` 且非 dirty｜public schema 表数 = 8｜worker 心跳日志｜worker 已连 PostgreSQL |

**链路 C 关键数字**：

| 项 | 值 |
|----|-----|
| iip-server / iip-worker 镜像体积 | 13.8 MB / 11.3 MB |
| 构建耗时 | server 2.2s / worker 0.7s（全命中 BuildKit 缓存，**零网络**） |
| 7 服务健康态 | 6 healthy + `iip-worker` running（按设计不设假探针） |
| worker 心跳 | `worker alive, poll_count=29` |

### 5.3 M0 验收清单逐条核对（§3 共 9 条）

| # | 清单项 | 结果 |
|---|--------|------|
| 1 | 仓库可克隆 + README 三步启动指引 | **PASS** |
| 2 | `go build ./...` 通过 | **PASS** |
| 3 | `golangci-lint run ./...` 零告警 | **PASS** |
| 4 | 迁移 up→down→up 幂等三连 | **PASS** |
| 5 | 非法值被 CHECK 拒绝（6 项） | **PASS**（6/6） |
| 6 | compose 依赖服务 healthy | **PASS**（2026-09-25 补测） |
| 7 | `/healthz` 200/503/200 三态 | **PASS** |
| 8 | worker 优雅退出 + 心跳 | **PASS** |
| 9 | CI 双 job 绿 | **待观察**（本地等价命令全过，真实 Actions 未推远端） |

> **统计**：PASS 8 条 / 待观察 1 条 / FAIL 0 条。

### 5.4 证据文件索引

| 文件 | 内容 |
|------|------|
| `D:\_iip_setup\_v1_binary_check.txt` | 链路 A 产物校验完整输出 |
| `D:\_iip_setup\_v1_build_log.txt` | 交叉编译真实 rc / 输出 |
| `D:\_iip_setup\_v1_quality_gate.txt` | Go 全量质量门禁输出 |
| `D:\_iip_setup\_v1_e2e_report.txt` | 链路 B 端到端测试完整输出 |
| `D:\_iip_setup\_verify_images.txt` | **链路 C 容器运行完整验收报告（14 项断言）** |
| `D:\_iip_setup\_prepull.txt` | 多源镜像拉取 / 镜像清单漂移检查输出 |
| `D:\_iip_setup\_v1e2e\server.log` / `worker.log` | 原始日志 |

**仓库内可复现脚本（`scripts/`）**：

| 文件 | 作用 |
|------|------|
| `verify-images.py` | 链路 C 一键验收：构建 → 起服务 → 断言 → 出报告 → 清理 |
| `verify-runtime.py` | 链路 B：无 Docker 时的本地全栈端到端验证 |
| `check-binaries.py` | 链路 A：交叉编译 + ELF 静态链接校验 |
| `prepull-mirror.py` | 国内网络多源拉取镜像并重打标准标签；`--check` 做清单漂移检查（不联网） |
| `lib_docker.py` | docker 定位 + cli-plugins PATH 装配（上面两个脚本共用） |

---

## 六、当前进度与未验证项

### 6.1 已完成

- M0 全部代码：仓库骨架、7 张业务表迁移、config 包、server 空壳、worker 空壳、compose 编排、CI 流水线
- 三条验收链路全部跑通，51 项断言 PASS
- 全部质量门禁 rc=0
- `docs/` 下 12 份文档齐备（路线图 / 技术设计 / M0 规格 / M0 AI 执行计划 / M1 详细开发手册 / ADR + 待决登记 / 总验收报告 / v1 测试报告 / M0 小结 / 迁移说明 / P5 破坏性测试方案 / 本文）
- 本地环境可一键起停（见 6.4）

### 6.2 未验证项（不读作通过）

| 项 | 状态 | 原因 |
|----|------|------|
| CI 双 job 真实结果 | **待观察** | 用户裁决本轮不推远端；两个 job 的本地等价命令均已实跑通过 |
| Alertmanager UI 可见 firing 告警 | **未验证** | 容器健康与 9093 可达已验证；但未构造真实告警场景，UI 上的 firing 呈现属 M4 范围 |
| `go test -race ./...` | **UNVERIFIED** | 本机缺 gcc（Windows 下 `-race` 需 CGO）；由 CI（ubuntu-latest 自带 gcc）覆盖 |
| linux/arm64 镜像实机运行 | **未验证** | 仅验证 linux/amd64。Dockerfile 已支持 `TARGETARCH` 覆盖，但未在 arm64 实机跑过 |

### 6.3 待办动作

| # | 动作 | 说明 |
|---|------|------|
| 1 | `git init` + 首次 commit + push | 观察 Actions 双 job 真实结果 |
| 2 | **P5 破坏性测试（红→修→绿）**并留存证据 | 防「CI 假绿」的**唯一手段，不可省略**。方案见 `docs/P5-CI破坏性测试方案.md` |
| 3 | 新开终端、删本地 bin 产物，严格按 README 三步走一遍 | 需要「干净视角」——AI 自验容易漏环境隐性依赖 |
| 4 | `git tag v0.0-m0 && git push --tags` | 全部验收通过后 |
| 5 | 据实回填 `docs/M0-小结.md` 第一节真实工时 | — |
| 6 | 确认 `.editorconfig` 的 `[*.ps1] charset = utf-8-bom` 一行 | 该项在 M0 小结中仍标「待技术负责人确认」 |

### 6.4 本地环境起停（当前可用）

| 端点 | 地址 | 实测 |
|------|------|------|
| **前端控制台** | http://localhost:5173 | 200（容器 `iip-web-view` 服务构建产物） |
| 后端探活 | http://localhost:8080/healthz | 200 `ok` |
| Prometheus | http://localhost:9090 | 200 `Prometheus Server is Healthy.` |
| Alertmanager | http://localhost:9093 | 200 `OK` |
| Loki | http://localhost:3100/ready | 200 `ready` |
| Redis / PostgreSQL | 6379 / 5432 | 端口监听 |

**起停命令**：

```bash
# 起整个容器栈（7 服务）并等到收敛
python D:\_iip_setup\start_local.py

# 停
python D:\_iip_setup\start_local.py --down

# 前端 dev 模式（带热更新）——需在本人终端执行，
# AI 派生的 dev server 会被沙箱回收
make web-dev
```

> **前端当前是 M0 占位页**（`web/src/App.tsx` 只有标题 + 一句说明）。
> 按 `AGENTS.md` 约束 2/11，M0 阶段禁止写业务页面，仅验证脚手架可渲染。

---

## 七、下一步（M1）

### 7.1 M1 目标

**告警接入与持久化**（计划 8 天）：`webhook API + 处理流水线 1-5 步`。

处理流水线（V1.0 §4.2）：

```
Webhook Received → Validate → Normalize → Fingerprint → Deduplicate → Persist
```

### 7.2 M0 → M1 交接物就绪情况

| 交接物 | 状态 |
|--------|------|
| chi 路由挂载点已留 | **就绪**（`cmd/iip-server/main.go` 内有 M1 路由注释） |
| `alerts` 表 + sqlc 查询层入口 | **就绪**（表已建，`sqlc.yaml` + `queries/alerts.sql` 骨架已补） |
| Alertmanager 真实推送源在 compose 里活着 | **就绪**（receiver 指向 `host.docker.internal:8080/api/v1/alerts/webhook`，当前 404 属预期） |
| 集成测试的 PG 容器化套路 | **部分就绪**（本地验收脚本已验证，testcontainers 待 M1 引入） |

### 7.3 建议的开工顺序

1. **先补完 6.3 的待办动作**（尤其第 2 项破坏性测试）——M0 不带病进入 M1。
2. M1 第一件事写 webhook handler（Validate → Normalize → Fingerprint → Dedup → Persist），
   每步配表驱动单测。
3. **Fingerprint 算法**要特别注意：它需要排除动态 label（`instance`/`pod`/`container`/`node`），
   否则每次容器重启都会生成新指纹，去重直接失效。
4. 引入 testcontainers-go 做集成测试（M0 的 compose 链路已验证 PG 可容器化）。

---

## 附录 A：M0 阶段修复的完整缺陷清单

### A.1 产品代码（4 项）

| # | 缺陷 | 根因 | 层级 |
|---|------|------|------|
| 1 | `--healthcheck=false` 报 `unknown flag` | flagset 未注册该 flag | 代码 |
| 2 | `-ldflags "-X main.version=..."` **静默失效** | `main.version` 变量根本不存在，链接器不报错但版本号永远注不进去 | 代码 |
| 3 | `iip-alertmanager` 恒为 unhealthy | healthcheck 文案从 prometheus 复制，响应体不匹配 | 部署配置 |
| 4 | 国内网络下 `docker build` 必然失败 | `go mod download` 走官方代理，容器内不通 | 构建配置 |

### A.2 编排与工程配置（3 项）

| # | 问题 | 影响 |
|---|------|------|
| 5 | `.golangci.yml` 的 `errcheck.check-blank: true` | 4 处官方推荐写法被误报，`golangci-lint` 必红 |
| 6 | `web/tsconfig.node.json` 违反 TS project references（`TS6310`） | **CI frontend job 原本必红** |
| 7 | `web/package.json` 的 `packageManager` 三方不一致（9.12.3 / lockfile 9.15.9 / CI 9） | 统一为 `pnpm@9.15.9` |

### A.3 第二层独立验证（QA）补获（9 项）

| 级别 | 问题 |
|------|------|
| 阻断 | `README.md` **完全缺失**（验收清单第 1 条无对象） |
| 重要 | `main_test.go` import 顺序违反 gofmt（CI lint 必红） |
| 重要 | `internal/store/store.go` import 分组不规范 |
| 重要 | `docs/IIP-V1.0-技术设计文档.md` 为 CRLF（133 处） |
| 重要 | 调试产物 `_tmp/`(22 项) / `_probe.txt` / `_tree.txt` 未清理且未忽略 |
| 重要 | M0→M1 交接物 `sqlc.yaml` / `queries/alerts.sql` 缺失 |
| 次要 | `web/package.json` 的 `lint` 与 `tsc` 重复 |
| 次要 | 两个 `.ps1` 无 BOM 导致 Windows PowerShell 中文解析失败 |
| 次要 | `packageManager` 三方不一致（同上第 7 项） |

> **重要方法论结论**：项目总监自身的第一轮静态审计**遗漏了**「README 缺失」「CRLF」「调试产物」三项，
> 由 QA 的第二层独立验证补获。这印证了 M0 计划 §5 的判断——
> **AI 自验容易漏环境隐性依赖，必须有一方的干净视角。**

### A.4 测试脚本自身缺陷（10 项）

见 4.4 节完整表格。

---

## 附录 B：环境耗时实测（环境就绪后）

| 操作 | 实测耗时 |
|------|----------|
| `go build ./...` | 47s |
| `go vet ./...` | 32s |
| `go test ./...` | iip-worker 2.481s / config 2.178s |
| `golangci-lint run ./...` | 62s |
| `pnpm install` | 13m32s（140 包） |
| `pnpm tsc --noEmit` | 3s |
| `pnpm build` | 12s（3064 modules，dist 448,981 B） |
| 运行时一体化验收 | 3m28s（含 65s 心跳等待） |
| migrate down/up 往返验收 | 1m20s |
| 容器链路验收（verify-images.py） | 1m31s |
| 环境搭建（不含上述） | 约 35 分钟 |

> **口径声明**：本次执行**不适用「人工版 5 个工作日」的耗时基准**。
> M0 采用 AI 并行施工模式，与人工版「逐日串行」的时间结构不可直接对比；
> 且最大时间消耗在「本机环境从零搭建」（GitHub 不可达、PostgreSQL 绿色版、
> 沙箱对后台进程的强杀约束）——**这些均非 M0 计划预算内的活动**。
> 因此**不计入偏差率判断**，避免用不成立的基准推导出虚假结论。

---

## 附录 C：本文档的维护约定

1. **本文是汇总层，不是权威源**。任何规格冲突以 `docs/IIP-M0详细开发计划.md` 为准。
2. 里程碑推进时更新第一章 1.4 节的状态列与第六章。
3. 新增缺陷时同步更新附录 A。
4. **增量维护，禁止整篇重写**——每次只追加/修正与本次相关的具体条目。
