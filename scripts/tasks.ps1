<#
.SYNOPSIS
    IIP M0 本地开发任务入口（PowerShell 等价物）。
.DESCRIPTION
    Makefile 的同名目标 PowerShell 版本，供 Windows 未安装 make 时使用。
    用法：
        powershell -File scripts/tasks.ps1 run-server
        powershell -File scripts/tasks.ps1 test
        powershell -File scripts/tasks.ps1 list
    依赖版本与命令依据 docs/IIP-M0详细开发计划.md §2 T0.1。
#>

[CmdletBinding()]
param(
    [Parameter(Position = 0)]
    [string]$Task = 'list',

    # 本地 PG 连接串，覆盖默认值即可连别的库
    [string]$PgUrl = 'postgres://postgres:dev@localhost:5432/iip?sslmode=disable',

    [string]$PgContainer = 'iip-pg',

    # 镜像标签：与 Makefile 的 IMAGE_TAG、deploy/docker-compose.yml、
    # scripts/verify-images.py 的 IIP_TAG、Helm chart 的 appVersion 四处联动。
    # 当前版本由 scripts/bump-version.py 统一维护（16 处，判据在 E2E S1）；
    # 别手改这里 —— 单改一处只会让 S1 红。
    [string]$ImageTag = '0.2.0',

    # 任务自有参数的透传口：verify-e2e 需要 --stages/--summary/--strict 等，
    # 而 $Task 是位置参数，额外开关走这里（powershell -File scripts/tasks.ps1 verify-e2e --stages s2,s3）。
    [Parameter(ValueFromRemainingArguments = $true)]
    [string[]]$TaskArgs = @()
)

$ErrorActionPreference = 'Stop'
$Root = Split-Path -Parent $PSScriptRoot
Set-Location $Root

$ComposeFile = 'deploy/docker-compose.yml'

function Invoke-Deps {
    Write-Host '[tasks] deps' -ForegroundColor Cyan
    go mod download
    Push-Location web
    try { pnpm install } finally { Pop-Location }
}

function Invoke-ComposeUp {
    Write-Host '[tasks] compose-up' -ForegroundColor Cyan
    docker compose -f $ComposeFile up -d
}

function Invoke-ComposeDown {
    Write-Host '[tasks] compose-down' -ForegroundColor Cyan
    docker compose -f $ComposeFile down
}

function Invoke-RunServer {
    Write-Host '[tasks] run-server' -ForegroundColor Cyan
    go run ./cmd/iip-server
}

function Invoke-RunWorker {
    Write-Host '[tasks] run-worker' -ForegroundColor Cyan
    go run ./cmd/iip-worker
}

function Invoke-Test {
    Write-Host '[tasks] test' -ForegroundColor Cyan
    go test -race -coverprofile=coverage.out ./...
}

function Invoke-Lint {
    Write-Host '[tasks] lint' -ForegroundColor Cyan
    golangci-lint run ./...
}

function Invoke-MigrateUp {
    Write-Host '[tasks] migrate-up' -ForegroundColor Cyan
    migrate -path migrations -database $PgUrl up
}

function Invoke-MigrateDown {
    Write-Host '[tasks] migrate-down' -ForegroundColor Cyan
    migrate -path migrations -database $PgUrl down-all
}

function Invoke-WebDev {
    Write-Host '[tasks] web-dev' -ForegroundColor Cyan
    Push-Location web
    try { pnpm dev } finally { Pop-Location }
}

function Invoke-Build {
    Write-Host '[tasks] build' -ForegroundColor Cyan
    New-Item -ItemType Directory -Force -Path bin | Out-Null
    go build -o bin/iip-server ./cmd/iip-server
    go build -o bin/iip-worker ./cmd/iip-worker
}

function Invoke-Tidy {
    Write-Host '[tasks] tidy' -ForegroundColor Cyan
    go mod tidy
}

function Invoke-PgUp {
    Write-Host '[tasks] pg-up' -ForegroundColor Cyan
    docker run --name $PgContainer -e POSTGRES_PASSWORD=dev -e POSTGRES_DB=iip -p 5432:5432 -d postgres:16-alpine
}

function Invoke-PgDown {
    Write-Host '[tasks] pg-down' -ForegroundColor Cyan
    docker rm -f $PgContainer
}

# ---- 以下 8 个于 2026-10-14 T7.6 补齐：Makefile 早有同名目标，
# ---- 本文件却缺失，导致 README「Makefile ↔ tasks.ps1 同名任务」的合约名不副实。

function Invoke-ImageBuild {
    Write-Host "[tasks] image-build (tag=$ImageTag)" -ForegroundColor Cyan
    docker build --target server -t "iip-server:$ImageTag" .
    docker build --target worker -t "iip-worker:$ImageTag" .
}

function Invoke-ImageLs {
    Write-Host '[tasks] image-ls' -ForegroundColor Cyan
    docker images iip-server
    docker images iip-worker
}

function Invoke-AppUp {
    Write-Host '[tasks] app-up' -ForegroundColor Cyan
    Invoke-ImageBuild
    docker compose -f $ComposeFile --profile app up -d
}

function Invoke-AppDown {
    Write-Host '[tasks] app-down' -ForegroundColor Cyan
    docker compose -f $ComposeFile --profile app down
}

function Invoke-VerifyImages {
    Write-Host '[tasks] verify-images' -ForegroundColor Cyan
    python scripts/verify-images.py
}

function Invoke-VerifyRuntime {
    Write-Host '[tasks] verify-runtime' -ForegroundColor Cyan
    python scripts/verify-runtime.py
}

function Invoke-CheckBinaries {
    Write-Host '[tasks] check-binaries' -ForegroundColor Cyan
    python scripts/check-binaries.py
}

function Invoke-RunLocal {
    Write-Host '[tasks] run-local' -ForegroundColor Cyan
    python scripts/run-local-nodocker.py
}

function Invoke-VerifyE2E {
    <#
    T7.7 E2E 一键回归：编排 S0-S7 八个阶段，按 M1-M6 验收项与 MVP
    总验收逐项出状态。$TaskArgs 透传（--stages s2,s3 / --summary /
    --no-bench / --strict / --dry-run）。
    分批跑的由来回：阶段 JSON 落 scripts/_e2e/，最后 --summary 汇总。
    #>
    Write-Host '[tasks] verify-e2e' -ForegroundColor Cyan
    python scripts/verify-e2e.py @TaskArgs
}

function Invoke-CheckFileSize {
    <#
    OPEN-M5-7 / OPEN-M6-1 的守卫：单文件 <= 300 行（docs/IIP-M1详细开发手册.md §4）。
    存量欠债记在 scripts/file-size-baseline.txt，规则是「只减不增」：
    新文件超限必红；已还清却未销账也红；账目行数过期也红。
    纯静态、秒级，可进 CI。
    #>
    Write-Host '[tasks] check-file-size' -ForegroundColor Cyan
    python scripts/check-file-size.py @TaskArgs
}

function Invoke-VerifyObservability {
    <#
    T6.4 可观测性配置的静态核对：面板/规则里引用的指标名、标签值、记录规则、
    抓取端口是否与代码里的定义一致。这些错法全都不报错，只表现为面板空图。
    不需要 Docker / Prometheus / Grafana，纯静态，可进 CI。
    #>
    Write-Host '[tasks] verify-observability' -ForegroundColor Cyan
    python scripts/verify-observability.py @TaskArgs
}

function Invoke-VerifyOnboarding {
    <#
    T6.5 接入指南的可执行验收：从 docs/接入指南-Alertmanager.md 里抽出代码块原文
    （payload / curl / resolve / 通用源）在真 PG + 真 server 上跑一遍，
    断言文档声称的 accepted/skipped/updated/rejected 与状态码。
    文档与实现一旦不一致，这里就红 —— 免得由操作者去踩。
    需要绿色版 PG（D:\pgsql16）与 Go 工具链。
    #>
    Write-Host '[tasks] verify-onboarding' -ForegroundColor Cyan
    python scripts/verify-onboarding.py @TaskArgs
}

function Invoke-VerifyNotify {
    <#
    T6.1/T6.2 通知链路的真栈验收（E2E 的 S8）：真 PG + 真 server + 假出口，
    断言两个 sink 都送达、状态变更去风暴（窗口内 2 条）、终态 resolved 必发、
    出口带 Bearer 而日志不留明文。单测只能证明 HTTP 层语义，收消息这一半
    必须在真栈上看见。
    端口 5437 / 8091 / 8899，需与其它链路错开。
    #>
    Write-Host '[tasks] verify-notify' -ForegroundColor Cyan
    python scripts/verify-notify.py @TaskArgs
}

function Invoke-BumpVersion {
    <#
    发版工具：统一改 16 处版本标签（权威值 = Makefile 的 IMAGE_TAG）。
    别手打版本号逐个改 —— 手工改必然漏，而漏掉的那处只会让 S1 判据红，
    它不会告诉你该改哪儿。

      powershell -File scripts/tasks.ps1 bump-version                    # 只检查
      powershell -File scripts/tasks.ps1 bump-version --write --bump minor
      powershell -File scripts/tasks.ps1 bump-version --write --to 0.3.0
      powershell -File scripts/tasks.ps1 bump-version --tag              # 只打印 tag 命令

    --write 改完会自己重扫自证；某处匹配 0 次或多次都会当场报错。
    #>
    Write-Host '[tasks] bump-version' -ForegroundColor Cyan
    python scripts/bump-version.py @TaskArgs
}

function Invoke-List {
    Write-Host 'IIP 本地任务（PowerShell 等价物）：' -ForegroundColor Green
    @(
        '  deps          拉取全部依赖（Go modules + 前端 pnpm）',
        '  compose-up    起 deploy/docker-compose.yml 七服务',
        '  compose-down  停止并移除七服务（保留 named volume）',
        '  run-server    运行 iip-server',
        '  run-worker    运行 iip-worker',
        '  test          运行全部单测（-race）',
        '  lint          运行 golangci-lint',
        '  migrate-up    对本地 PG 执行迁移 up',
        '  migrate-down  回滚全部迁移 down-all',
        '  web-dev       启动前端 dev server',
        '  build         编译二进制到 bin/',
        '  tidy          整理 go.mod / go.sum',
        '  pg-up         起本地 PG 容器',
        '  pg-down       删除本地 PG 容器',
        '  image-build   构建 iip-server / iip-worker 镜像（多阶段，需 Docker daemon）',
        '  image-ls      列出本项目镜像',
        '  app-up        构建镜像并起全部七个服务（含应用组）',
        '  app-down      停止并移除全部服务（保留 named volume）',
        '  verify-images 容器链路验证：构建 + 起服务 + 逐项断言 + 出报告 + 清理',
        '  verify-runtime 应用链路验证：真实 PG 从零建库 -> 迁移 -> 探活 -> 心跳 -> 退出',
        '  check-binaries 交叉编译 linux/amd64 并校验真静态链接（distroless 前提）',
        '  run-local     无 Docker 的本地全栈启动（绿色版 PG + server + Vite）',
        '  verify-e2e    E2E 一键回归（M1-M6 验收 + MVP 总验收；可透传 --stages s2,s3 / --summary / --strict）',
        '  verify-observability 核对 Grafana 面板 / Prometheus 规则与代码里的指标定义是否一致（静态，不需 Docker）',
        '  verify-onboarding 从接入指南抽代码块原文，在真栈上跑一遍（真 PG + 真 server，需启动数据库）',
        '  verify-notify 通知链路真栈验收：两出口送达 / 去风暴 / 终态必发 / token 不外泄（5437/8091/8899）',
        '  check-file-size 单文件 <= 300 行门禁：存量记基线，只减不增（静态，秒级）',
        '  bump-version  发版工具：统一改 16 处版本标签（--check / --write --bump minor / --tag）'
    ) | ForEach-Object { Write-Host $_ }
}

switch ($Task.ToLowerInvariant()) {
    'deps'         { Invoke-Deps }
    'compose-up'   { Invoke-ComposeUp }
    'compose-down' { Invoke-ComposeDown }
    'run-server'   { Invoke-RunServer }
    'run-worker'   { Invoke-RunWorker }
    'test'         { Invoke-Test }
    'lint'         { Invoke-Lint }
    'migrate-up'   { Invoke-MigrateUp }
    'migrate-down' { Invoke-MigrateDown }
    'web-dev'      { Invoke-WebDev }
    'build'        { Invoke-Build }
    'tidy'         { Invoke-Tidy }
    'pg-up'        { Invoke-PgUp }
    'pg-down'      { Invoke-PgDown }
    'image-build'  { Invoke-ImageBuild }
    'image-ls'     { Invoke-ImageLs }
    'app-up'       { Invoke-AppUp }
    'app-down'     { Invoke-AppDown }
    'verify-images'  { Invoke-VerifyImages }
    'verify-runtime' { Invoke-VerifyRuntime }
    'check-binaries' { Invoke-CheckBinaries }
    'run-local'    { Invoke-RunLocal }
    'verify-e2e'   { Invoke-VerifyE2E }
    'verify-observability' { Invoke-VerifyObservability }
    'verify-onboarding' { Invoke-VerifyOnboarding }
    'verify-notify'  { Invoke-VerifyNotify }
    'bump-version'   { Invoke-BumpVersion }
    'check-file-size' { Invoke-CheckFileSize }
    'list'         { Invoke-List }
    default {
        Write-Error "未知任务: $Task（可用: list/deps/compose-up/compose-down/run-server/run-worker/test/lint/migrate-up/migrate-down/web-dev/build/tidy/pg-up/pg-down/image-build/image-ls/app-up/app-down/verify-images/verify-runtime/check-binaries/run-local/verify-e2e/verify-observability/verify-onboarding/verify-notify/check-file-size/bump-version）"
        exit 1
    }
}
