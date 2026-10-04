# IIP — Incident Intelligence Platform

IIP 是一个开源的事件智能平台，为**自托管可观测性栈**设计：把海量异构告警自动关联聚合为可操作的事件（Incident），并为每个事件自动收集可追溯的证据链，最终生成可解释的根因假设与修复建议。

> **当前阶段：M3（事件生命周期）S1 + S2 已交付，S3 前端未开工**——M0 交付可运行的地基；M1 接入告警接收与落库（含 ADR-007 修复）；M2 实现五维加权评分 + 三档决策的关联引擎，把同根因告警自动聚合为 Incident；M3 在其上实现事件状态机（6 态 / 36 格转换矩阵）、乐观并发控制、状态与留痕同事务、待确认队列与人工判定、关联抑制，并新增 6 个 HTTP 端点（详见下文「M3 验收状态」）。**验证链路共四条**：`verify-runtime.py`（宿主机全栈）、`verify-images.py`（容器链路，**41 项断言 0 失败**）、`verify-func.py`（业务闭环 **48 项断言**）、`check-binaries.py`（镜像内容物）；真库门禁 **PASS=250 FAIL=0 SKIP=0**，`golangci-lint` v1.61.0 rc=0，容器内 `-race` rc=0。**尚未交付**：前端（路由/布局壳/事件列表/详情双 Tab/处置交互）按 ADR-016 留待 S3 一次性交付，前端自动化测试延至 M5——两项均如实标注 NOT DONE，未修饰为通过。

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
docker build --target server -t iip-server:0.0-m0 .
docker build --target worker -t iip-worker:0.0-m0 .
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

## 常用任务

Makefile 与 `scripts/tasks.ps1` 提供同名任务（三方合约：README ↔ Makefile ↔ tasks.ps1 目标名一致）：

| 任务 | 作用 |
|------|------|
| `deps` | 拉取 Go modules + 前端 pnpm 依赖 |
| `compose-up` / `compose-down` | 起 / 停 `deploy/docker-compose.yml` 五服务 |
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
| `verify-images` | 端到端验证：构建 + 起服务 + 逐项断言 + 出报告 + 清理 |
| `run-local` | 无 Docker 的本地全栈启动（绿色版 PG + server + Vite） |

查看全部任务：`make help` 或 `pwsh -File scripts/tasks.ps1 list`。

### 验证脚本：四条链路，别混用

| 脚本 | 链路 | 前置 | 报告 |
|------|------|------|------|
| `scripts/verify-func.py` | **业务闭环**：M1 接入 → M2 关联 → M3 处置 → 抑制生效，核对数据后果 | 绿色版 PostgreSQL（`IIP_PG_HOME` 可覆盖） | `_verify_func.txt` |
| `scripts/verify-runtime.py` | 宿主机全栈（偏 M1/M2 单点校验） | 宿主 PostgreSQL | `_verify_runtime.txt` |
| `scripts/verify-images.py` | 容器链路（偏部署形态与镜像内容物） | Docker daemon | `_verify_images.txt` |
| `scripts/check-binaries.py` | 镜像内容物（交叉编译 + ELF 解析） | 无 | 控制台 |

`verify-func.py` 默认用 **5434 / 8082**，与 `verify-runtime.py` 错开，
两条链路可并存而不互相抢占。

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
| 前端证据 Tab 接真实数据 | **达成** | `tsc -b` + `vite build` rc=0 |

### 未达成 / 降级（如实标注，不得记为通过）

| 项 | 状态 | 原因 |
|----|------|------|
| **容器链路断言 0 失败** | **达成** | 59 条断言全通过；M4 新增 18 条（表/索引/CHECK/采集器注册/evidence=7/6 类模式独立） |
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

> 复现方式：M4 真库门禁 `python D:\_iip_setup\m4_final_gate.py`（约 25 分钟，含全量 `./...`）；
> 业务闭环 `python scripts\verify-func.py`（约 3 分钟，M4 段含宕机与补采两场景）；
> 前端构建 `python D:\_iip_setup\build_web.py`（`tsc -b` + `vite build`）。
