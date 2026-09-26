#!/usr/bin/env bash
# IIP 本地 pre-commit 检查（bash：macOS / Linux / git-bash）
# 规格依据：docs/IIP-M0详细开发计划.md §T0.7 —— "gofumpt -l 无输出才放行"
#
# 门禁：
#   1. gofumpt -l 必须无输出（有输出 = 存在未格式化文件 → 拦截）
#   2. golangci-lint run 必须通过
# 安装为 git hook：
#   ln -sf ../../scripts/pre-commit.sh .git/hooks/pre-commit
#   chmod +x scripts/pre-commit.sh
#
# 依赖：gofumpt（go install mvdan.cc/gofumpt@latest）、golangci-lint

set -euo pipefail

RED=$'\033[31m'; GREEN=$'\033[32m'; YEL=$'\033[33m'; NC=$'\033[0m'
fail() { printf '%s[pre-commit] %s%s\n' "$RED" "$1" "$NC"; exit 1; }
info() { printf '%s[pre-commit] %s%s\n' "$YEL" "$1" "$NC"; }
ok()   { printf '%s[pre-commit] %s%s\n' "$GREEN" "$1" "$NC"; }

command -v gofumpt >/dev/null 2>&1 || fail "未找到 gofumpt，请先安装：go install mvdan.cc/gofumpt@latest"
command -v golangci-lint >/dev/null 2>&1 || fail "未找到 golangci-lint，请先安装（>=1.60）"

# 1) 格式检查：gofumpt -l 列出需要格式化的文件
info "检查 Go 格式（gofumpt -l）..."
UNFORMATTED="$(gofumpt -l . || true)"
if [ -n "$UNFORMATTED" ]; then
  printf '%s以下文件未通过 gofumpt 格式化：%s\n%s\n' "$RED" "$NC" "$UNFORMATTED"
  fail "请运行 gofumpt -w . 后重新提交"
fi
ok "格式检查通过"

# 2) lint
info "运行 golangci-lint ..."
golangci-lint run ./... --config .golangci.yml --timeout 5m || fail "golangci-lint 未通过，请修复后重新提交"
ok "lint 通过"

ok "pre-commit 全部检查通过"
