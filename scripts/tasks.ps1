<#
.SYNOPSIS
    IIP M0 本地开发任务入口（PowerShell 等价物）。
.DESCRIPTION
    Makefile 的同名目标 PowerShell 版本，供 Windows 未安装 make 时使用。
    用法：
        pwsh -File scripts/tasks.ps1 run-server
        pwsh -File scripts/tasks.ps1 test
        pwsh -File scripts/tasks.ps1 list
    依赖版本与命令依据 docs/IIP-M0详细开发计划.md §2 T0.1。
#>

[CmdletBinding()]
param(
    [Parameter(Position = 0)]
    [string]$Task = 'list',

    # 本地 PG 连接串，覆盖默认值即可连别的库
    [string]$PgUrl = 'postgres://postgres:dev@localhost:5432/iip?sslmode=disable',

    [string]$PgContainer = 'iip-pg'
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

function Invoke-List {
    Write-Host 'IIP 本地任务（PowerShell 等价物）：' -ForegroundColor Green
    @(
        '  deps          拉取全部依赖（Go modules + 前端 pnpm）',
        '  compose-up    起 deploy/docker-compose.yml 五服务',
        '  compose-down  停止并移除五服务（保留 named volume）',
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
        '  pg-down       删除本地 PG 容器'
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
    'list'         { Invoke-List }
    default {
        Write-Error "未知任务: $Task（可用: list/deps/compose-up/compose-down/run-server/run-worker/test/lint/migrate-up/migrate-down/web-dev/build/tidy/pg-up/pg-down）"
        exit 1
    }
}
