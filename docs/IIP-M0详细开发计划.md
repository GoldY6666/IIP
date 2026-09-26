# IIP M0 详细开发计划：项目骨架与环境

**周期**：5 个工作日（D1-D5，可整体顺延，不可压缩验收）
**前置依据**：《IIP 开发计划》§4
**完成定义（DoD）**：`docker compose up` 拉起完整依赖 → server/worker 空壳可运行 → `/healthz` 返回 200 → 迁移在干净 PG 上可重复执行 → CI 三色通过 → 打出 tag `v0.0-m0`

---

## 0. 开工前置：本机环境准备（0.5 天，建议提前或 D1 上午穿插）

你是 Windows 11 环境，先确认以下工具就位，**这一步最容易吞掉半天，不要省**：

| 工具 | 版本要求 | 验证命令（PowerShell） | 说明 |
|------|----------|------------------------|------|
| Go | ≥ 1.23 | `go version` | 官网 MSI 安装，装完重开终端确认 PATH |
| Git | ≥ 2.40 | `git --version` | 全局配 `git config --global core.autocrlf true`（Windows 必须，防 LF/CRLF 混乱） |
| Docker Desktop | 最新版 | `docker version` | 启用 WSL2 后端；确认能跑 `docker run --rm hello-world` |
| Node.js | ≥ 20 LTS | `node -v` | 前端脚手架用 |
| pnpm | ≥ 8 | `pnpm -v`（无则 `npm i -g pnpm`） | 比 npm 快且 lockfile 干净 |
| make（可选） | — | `make` | Windows 下可改用 `Taskfile` 或直接把 Makefile 命令写进 `scripts/*.ps1`，二选一，别纠结 |
| golangci-lint | ≥ 1.60 | `golangci-lint --version` | `go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest` |
| sqlc | ≥ 1.27 | `sqlc version` | `go install github.com/sqlc-dev/sqlc/cmd/sqlc@latest` |
| golang-migrate CLI | ≥ 4.17 | `migrate -version` | 本地手动执行迁移调试用 |

**环境验证通过标准**：上面每条命令都有正常输出，且 Docker Desktop 内 `docker compose version` 可用。

---

## 1. 逐日任务安排总览

| 日 | 任务 | 预算 | 产出物 |
|----|------|------|--------|
| D1 上午 | T0.1a 仓库初始化 + Go module + 目录骨架 | 1.5h | 可 commit 的空骨架 |
| D1 下午 | T0.1b web/ 前端脚手架 + T0.3a 配置文件结构 | 2.5h | 前端 dev server 可启动 |
| D2 全天 | T0.2 数据库迁移（6 张表全量 DDL） | 4h | 0001_init.sql + 本地验证记录 |
| D3 上午 | T0.3b config 加载包 + 启动时自动迁移 | 1.5h | internal/config |
| D3 下午 | T0.4 server 空壳（chi + healthz + 日志中间件） | 2.5h | iip-server 可运行 |
| D4 上午 | T0.5 worker 空壳（polling 循环 + 优雅退出） | 3h | iip-worker 可运行 |
| D4 下午 | T0.6a docker-compose 五服务编排 | 3h | compose 一键起环境 |
| D5 上午 | T0.6b compose 联调修复 + T0.7 CI 流水线 | 3h | Actions 绿 |
| D5 下午 | 整体验收清单逐条过 + 打 tag + M0 小结 | 2h | v0.0-m0 |

> 规则：单日任务超时 1.5 小时即记录阻塞点并跳过，当天先完成"可 commit 的最小状态"，遗留项挪到 D5 缓冲处理；D5 缓冲仍不够则触发周报中的延期声明，不通宵硬追。

---

## 2. 任务卡（逐任务的操作步骤与起步代码）

### T0.1 初始化 monorepo（D1）

**操作步骤**：

```powershell
mkdir D:\Documents\qwen-agent\iip ; cd D:\Documents\qwen-agent\iip
git init
go mod init github.com/<你的用户名>/iip     # module 名后续固定，别乱改
mkdir cmd\iip-server, cmd\iip-worker, internal, migrations, web, deploy, testdata\replay, scripts, docs
```

1. 写 `.gitignore`（必含：`bin/`, `*.exe`, `node_modules/`, `web/dist/`, `.env`, `coverage.out`, `__pycache__/`）
2. 写 `.editorconfig`：`[*] charset=utf-8 insert_final_newline=true indent_style=space indent_size=4`，`[*.{yml,yaml,json,sql}] indent_size=2`
3. 前端脚手架：`pnpm create vite web --template react-ts`，进 `web/` 装 `antd`、`@tanstack/react-query`、`react-router-dom`，`pnpm dev` 验证 5173 端口可访问后 Ctrl+C
4. 写 `Makefile`（或 scripts/ps1 等价物）目标：`run-server` / `run-worker` / `test` / `lint` / `migrate-up` / `web-dev`
5. 首次 commit：`chore: bootstrap monorepo skeleton`

**踩坑提示**：Go module 路径带大写会被校验拒绝，用户名全小写；Windows Defender 首次编译 Go 会卡 1-2 分钟，属正常，把项目目录加入排除列表可提速。

---

### T0.2 数据库迁移（D2）⭐ 地基中的地基

写 `migrations/0001_init.sql`，**严格按 V1.0 设计文档 §3.2 落地 6 张表**，外加 M0 阶段就需要的两处工程决策（先声明，避免 M7 返工改表）：

**表清单**：
1. `alerts` —— 含 fingerprint/source/severity/status/labels JSONB/annotations JSONB/starts_at/ends_at/raw_payload JSONB/incident_id + 4 个 B-tree 索引 + `GIN(labels)`
2. `incidents` —— 含 root_causes JSONB / recommendations JSONB + status/severity/service/started_at 索引
3. `incident_alerts` —— M:N，复合主键 (incident_id, alert_id)，correlation_score DECIMAL(5,4)
4. `evidence` —— type/source/query/value/timestamp/confidence/metadata JSONB
5. `timeline` —— event_type/description/actor/metadata JSONB
6. `correlation_rules` —— name/enabled/rule_config JSONB/weight

**M0 就做的 3 个决策（记入 docs/decisions.md）**：
- ① `alerts.status`、`incidents.status`、`evidence.type` 用 `VARCHAR + CHECK 约束` 而非 PG enum 类型——PG enum 后续加值需要 ALTER，CHECK 更灵活；
- ② `alerts` 表暂不做分区（分区表主键必须含分区键，会逼着你现在定 fingerprint 全局唯一策略），V1.0 单表 + 索引足够撑到 M7 再评估；
- ③ 所有表加 `created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()`，时间列一律 `TIMESTAMPTZ`，禁止本地时区。
- ④（前瞻）为 M2 反馈学习预留一张 `correlation_feedback` 表（incident_id, alert_id_pair, decision, actor, created_at）——建表成本 5 分钟，避免 V1.x 加表迁移。

**验证命令**：

```powershell
docker run --name iip-pg -e POSTGRES_PASSWORD=dev -e POSTGRES_DB=iip -p 5432:5432 -d postgres:16-alpine
migrate -path migrations -database "postgres://postgres:dev@localhost:5432/iip?sslmode=disable" up
migrate -path migrations -database "postgres://postgres:dev@localhost:5432/iip?sslmode=disable" down-all   # 验证 down 可回滚
migrate ... up   # 再跑一遍确认可重复执行
docker rm -f iip-pg
```

**验收标准**：up → down-all → up 全链路无报错；`\d alerts` 能看到 5 个索引；每条 CHECK 约束在插入非法值时被拒绝（手动 INSERT 试一次）。

---

### T0.3 配置加载（D3 上午）

`internal/config/config.go` 起步结构（koanf）：

```go
type Config struct {
    HTTPAddr   string        // IIP_HTTP_ADDR, 默认 :8080
    PGURL      string        // IIP_PG_URL, 必填
    LogLevel   string        // IIP_LOG_LEVEL, 默认 info
    WorkerPoll time.Duration // IIP_WORKER_POLL_INTERVAL, 默认 2s
    DBPollOnly bool          // IIP_REDIS_DISABLED, V1.0 默认 true（DB polling 模式）
}
```

- 来源优先级：flag > 环境变量 > `config.local.yaml`（可选，进 .gitignore）
- 启动时校验：PGURL 非空且 `pgxpool.Ping` 成功，否则 fatal 退出（**快速失败，不带病启动**）
- 自动迁移：server 启动时用 `golang-migrate/embed` 跑 migrations/（幂等），worker 不做迁移只连库
- 写第一个真正的单测：`config_test.go` 表驱动验证默认值与必填校验

---

### T0.4 iip-server 空壳（D3 下午）

`cmd/iip-server/main.go` 骨架：

```go
func main() {
    cfg := config.Load()
    ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
    defer stop()
    pool := store.Connect(ctx, cfg.PGURL)      // pgxpool
    defer pool.Close()
    if err := store.Migrate(ctx, cfg.PGURL); err != nil { slog.Error(...); os.Exit(1) }

    r := chi.NewRouter()
    r.Use(middleware.RequestID, middleware.RealIP, logrMiddleware, middleware.Recoverer)
    r.Get("/healthz", handleHealthz(pool))     // 查库 ping，ok→200 "ok"，失败→503

    srv := &http.Server{Addr: cfg.HTTPAddr, Handler: r, ReadHeaderTimeout: 5*time.Second}
    go func() { srv.ListenAndServe() }()
    <-ctx.Done(); // 10s 超时优雅关闭 srv.Shutdown(...)
}
```

要求：
- 日志一律 `log/slog` JSON 模式（生产可解析，本地配 `slog.LevelVar` 可调级）
- `/healthz` 必须真查库（`pool.Query(ctx, "SELECT 1")`），不查库的 healthz 是假健康
- 预留路由挂载点注释：`// M1: r.Post("/api/v1/alerts/webhook", ...)`

**验收**：`curl.exe http://localhost:8080/healthz` 返回 `ok`；拔掉 PG 容器后返回 503；Ctrl+C / `Stop-Process` 触发 SIGTERM 语义日志后进程干净退出。

---

### T0.5 iip-worker 空壳（D4 上午）

`cmd/iip-worker/main.go` 骨架（polling 框架就是 M4 任务队列的地基，按最终形态写）：

```go
func main() {
    ctx := signal.NotifyContext(...)
    pool := store.Connect(ctx, cfg.PGURL)
    ticks := time.NewTicker(cfg.WorkerPoll)
    for {
        select {
        case <-ctx.Done():  drain(); return      // 停止接新任务，等 in-flight 完成，上限 15s
        case <-ticks.C:     runOnce(ctx, pool)   // M0: SELECT 1 占位；M4: claim tasks (FOR UPDATE SKIP LOCKED)
        }
    }
}
```

要求：
- `runOnce` 内置 panic recovery（一个任务炸了不能带崩整个 worker）
- 心跳日志每 60s 一条 `worker alive, poll_count=N`
- 单测：模拟 ctx cancel，断言 drain 后循环确实退出

**验收**：server 与 worker 同时运行 10 分钟无报错无内存增长（`go tool pprof` 不用开，任务管理器观察即可）。

---

### T0.6 docker-compose 联调环境（D4 下午 + D5 上午）

`deploy/docker-compose.yml` 五服务，全部钉**具体版本号，禁用 latest**：

| 服务 | 镜像:版本 | 端口 | 用途 |
|------|-----------|------|------|
| postgres | postgres:16-alpine | 5432 | 主库，healthcheck `pg_isready` |
| redis | redis:7-alpine | 6379 | 队列预留（V1.0 代码不连，compose 里先占位） |
| prometheus | prom/prometheus:v2.54.0 | 9090 | 挂载 `deploy/prometheus/prometheus.yml`（含一条示例规则 HighLatency，M1 造告警用） |
| alertmanager | prom/alertmanager:v0.27.0 | 9093 | 配置 receiver 指向 `host.docker.internal:8080/api/v1/alerts/webhook`（host 网络别名，Windows Docker Desktop 原生支持该域名） |
| loki | grafana/loki:3.1.0 | 3100 | M4 证据采集用，M0 只保证健康 |

要点：
- 每个服务配 `healthcheck` + `restart: unless-stopped`
- `deploy/alertmanager/alertmanager.yml` 写好 route → iip-webhook receiver 完整配置（M1 第一天就靠它发真告警，必须 D4 调通）
- Prometheus 配 `rule_files` + `evaluation_interval: 15s`，示例规则用 `up{job="fake"} == 1` 之类必然触发的假规则，保证随时能 fire
- 数据卷：postgres 和 loki 挂 named volume，`down -v` 前数据可保

**验收**：`docker compose up -d` → `docker compose ps` 五服务全 healthy → 浏览器开 9093 能看到 Alertmanager UI 且收到由 9090 规则触发的告警（当前 receiver 404 属预期，M1 修复）。

---

### T0.7 CI 流水线（D5 上午）

`.github/workflows/ci.yml`，push + PR 触发，两个并行 job：

**job: backend**（runs-on: ubuntu-latest）
1. `actions/setup-go@v5` (go-version-file: go.mod)
2. golangci-lint-action@v6（配置 `.golangci.yml`：errcheck、govet、staticcheck、gocritic 必开）
3. `go build ./...`
4. `go test -race -coverprofile=coverage.out ./...`（M0 只有 config 包的测试，先跑通管道）

**job: frontend**
1. setup-node@v4 + pnpm cache
2. `pnpm install --frozen-lockfile` → `pnpm tsc --noEmit` → `pnpm build`

本地 pre-commit（husky 或 gofumpt+lint 脚本均可，选最简单的）：`gofumpt -l` 无输出才放行。

**验收**：推到 GitHub 后 Actions 全绿；故意提交一个未格式化 Go 文件验证 CI 会红（然后修好）。

---

## 3. D5 下午：整体验收清单（全过才允许打 tag）

```text
[ ] 干净克隆仓库 → 按 README 三步（make deps / compose up / run-server）能在 15 分钟内跑起来
[ ] /healthz 返回 ok；kill PG 后返回 503；恢复后回 200
[ ] migrate up → down-all → up 可重复执行，6+1 张表、全部索引与 CHECK 就位
[ ] server+worker 并行 10 分钟：无 panic、日志含心跳、Ctrl+C 均优雅退出
[ ] Alertmanager UI 可见 firing 告警（receiver 404 属预期）
[ ] CI backend+frontend 双 job 绿；lint 能拦坏代码（做过一次红→修→绿）
[ ] docs/decisions.md 已记录 T0.2 的 4 条决策
[ ] git tag v0.0-m0 && git push --tags
[ ] 写 M0 小结：实际耗时 vs 预算（5d）、超时任务及原因 → 若偏差 >30%，按总计划 §12-R6 重排 M1
```

## 4. M0 → M1 交接物

M0 完成时，M1 第一天（写 webhook handler）应当零障碍的原因是这些接缝已就位：
- chi 路由挂载点已留（T0.4）
- alerts 表 + sqlc 查询层入口就绪（T0.2 后 D2 可顺手加 `sqlc.yaml` 和 queries/alerts.sql 的骨架）
- Alertmanager 真实推送源已在 compose 里活着（T0.6）
- 集成测试的 PG 容器化套路已在 CI 管道验证（T0.7）

**M0 唯一该"克制"的地方**：不要提前写任何业务逻辑。M0 的价值在于地基的每一块砖都验收过，而不是快。
