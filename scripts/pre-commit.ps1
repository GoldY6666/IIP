# IIP 本地 pre-commit 检查（Windows / PowerShell）
# 规格依据：docs/IIP-M0详细开发计划.md §T0.7 —— "gofumpt -l 无输出才放行"
#
# 门禁：
#   1. gofumpt -l 必须无输出（有输出 = 存在未格式化文件 -> 拦截）
#   2. golangci-lint run 必须通过
#
# 用法（手动执行）：
#   pwsh -File scripts\pre-commit.ps1
# 或
#   powershell -ExecutionPolicy Bypass -File scripts\pre-commit.ps1
#
# 安装为 git hook（Windows 下 git 用 sh 执行 hook，故另见 scripts/pre-commit.sh）：
#   推荐：让 git 直接调用本脚本 ->
#     "D:\...\scripts\pre-commit.ps1" 的内容包一层在 .git/hooks/pre-commit 里
#     (内容示例：powershell -ExecutionPolicy Bypass -File "$(git rev-parse --show-toplevel)\scripts\pre-commit.ps1")
#
# 依赖：gofumpt（go install mvdan.cc/gofumpt@latest）、golangci-lint（>=1.60）

$ErrorActionPreference = 'Stop'
$repoRoot = (git rev-parse --show-toplevel)
Set-Location $repoRoot

function Fail($msg) { Write-Host "[pre-commit] $msg" -ForegroundColor Red; exit 1 }
function Info($msg) { Write-Host "[pre-commit] $msg" -ForegroundColor Yellow }
function Ok($msg)   { Write-Host "[pre-commit] $msg" -ForegroundColor Green }

if (-not (Get-Command gofumpt -ErrorAction SilentlyContinue)) {
    Fail "未找到 gofumpt，请先安装：go install mvdan.cc/gofumpt@latest"
}
if (-not (Get-Command golangci-lint -ErrorAction SilentlyContinue)) {
    Fail "未找到 golangci-lint，请先安装（>=1.60）"
}

# 1) 格式检查
Info "检查 Go 格式（gofumpt -l）..."
$unformatted = & gofumpt -l .
if ($LASTEXITCODE -ne 0) { Fail "gofumpt 执行失败（退出码 $LASTEXITCODE）" }
if ($unformatted) {
    Write-Host "以下文件未通过 gofumpt 格式化：" -ForegroundColor Red
    $unformatted | ForEach-Object { Write-Host "  $_" }
    Fail "请运行 gofumpt -w . 后重新提交"
}
Ok "格式检查通过"

# 2) lint
Info "运行 golangci-lint ..."
& golangci-lint run ./... --config .golangci.yml --timeout 5m
if ($LASTEXITCODE -ne 0) { Fail "golangci-lint 未通过，请修复后重新提交" }
Ok "lint 通过"

Ok "pre-commit 全部检查通过"
