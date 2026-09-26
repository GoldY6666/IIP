# P5 / T0.7 破坏性测试证据 — CI 门禁有效性验证

**目的**：验证 CI 的 lint 门禁**真的会拦人**，而不是"配置看起来对"。这是 M0 唯一防伪手段（见 `docs/IIP-M0-AI开发计划.md` §5 风险表「CI 假绿」）。

**执行前置**：代码已推到 GitHub 私有仓库，`Actions` 已启用，`.github/workflows/ci.yml` 已就位。

**证据存放约定**：执行后把 Actions 运行记录截图/日志放入 `docs/evidence/`，文件命名 `P5-ci-red-<UTC日期>.png`、`P5-ci-green-<UTC日期>.png`。本文件记录**执行步骤与判定标准**，截图作为附件。

---

## 一、链路：故意弄红 → 确认红 → 修好 → 确认绿

### 步骤 1：制造一个"未格式化 + lint 必报"的 Go 文件

在 `internal/` 下临时新增 `internal/destructive_probe.go`（**破坏性测试专用，最终必须删除**）：

```go
// destructive probe: 故意不 gofmt + 忽略 error，用于验证 CI 门禁
package internal

import (
		"fmt"
	"os"
)

func DestructiveProbe() {
	// 故意的：糟糕缩进 + tab/空格混用（gofmt/gofumpt 会报）
	fmt.Println("probe")
	    os.Open("definitely-not-exist")   // errcheck 会报：未处理 error
}
```

> 说明：该文件同时命中两道门禁 —— `gofmt`（格式）与 `errcheck`（未处理 error）。任一命中即 CI 变红。

### 步骤 2：提交并推送，确认 CI 变红

**PowerShell**

```powershell
cd D:\Documents\qwen-agent\iip
git checkout -b ci-destructive-test
git add internal/destructive_probe.go
git commit -m "ci: destructive probe (temporary, will be reverted)"
git push -u origin ci-destructive-test
```

**bash**

```bash
cd /path/to/iip
git checkout -b ci-destructive-test
git add internal/destructive_probe.go
git commit -m "ci: destructive probe (temporary, will be reverted)"
git push -u origin ci-destructive-test
```

**确认红**（本地等价，可选；GitHub 侧看 Actions 页面）：

```powershell
gofumpt -l internal\destructive_probe.go   # 预期：输出该文件路径（非空 → 会被拦）
golangci-lint run ./internal/... --config .golangci.yml   # 预期：errcheck 报错，退出码非 0
```

**判定标准（红）**：
- GitHub Actions 页面 `CI / backend (go)` job 显示 **失败（红叉）**；
- 失败步骤应为 `golangci-lint`（格式/errcheck 命中）；
- `frontend (web)` job 若不受影响可为绿（两个 job 独立，符合并行设计）。

**证据**：截取 Actions 失败页面 → 存 `docs/evidence/P5-ci-red-<UTC日期>.png`。

### 步骤 3：修复，确认 CI 变绿

删除破坏性文件（最干净的"修复"）：

```powershell
git rm internal/destructive_probe.go
git commit -m "ci: revert destructive probe"
git push
```

或改成合规写法（保留文件但格式化 + 处理 error），二选一即可，推荐直接删除。

**判定标准（绿）**：
- `backend (go)` 与 `frontend (web)` 两个 job 均 **成功（绿勾）**；
- `golangci-lint`、`Build`、`Test (race + coverage)` 三步全过。

**证据**：截取 Actions 成功页面 → 存 `docs/evidence/P5-ci-green-<UTC日期>.png`。

### 步骤 4：合并入主干并清理临时分支

```powershell
git checkout main
git merge --no-ff ci-destructive-test -m "merge ci destructive test evidence"
git branch -d ci-destructive-test
git push origin --delete ci-destructive-test
```

> 若直接删除文件的分支合并后等价于无代码变更，仅保留 `docs/evidence/` 截图与本文档作为证据。

---

## 二、本地 pre-commit 门禁同样自测一次

在步骤 1 制造破坏文件后，**先不 commit**，直接跑：

```powershell
powershell -ExecutionPolicy Bypass -File scripts\pre-commit.ps1
```

**预期**：退出码非 0，输出"以下文件未通过 gofumpt 格式化：internal/destructive_probe.go"并 `Fail`，commit 被拦。
修好后重跑应输出"pre-commit 全部检查通过"。

```bash
scripts/pre-commit.sh
```

---

## 三、通过判据汇总（P6 总验收会引用）

| 项 | 判据 | 证据 |
|----|------|------|
| CI 会变红 | 推入未格式化 Go 文件后 backend job 失败 | `docs/evidence/P5-ci-red-*.png` |
| CI 会变绿 | 删除/修复后双 job 成功 | `docs/evidence/P5-ci-green-*.png` |
| lint 拦人 | 失败步骤为 golangci-lint，非被注释绕过 | 同上截图 |
| 本地门禁有效 | pre-commit 脚本退出码非 0 拦截提交 | 终端输出记录 |
| 规则未被削弱 | `.golangci.yml` 中 errcheck/govet/staticcheck/gocritic 仍启用 | `git diff` 无对该文件的放宽改动 |

**红线**：执行过程中**禁止**用 `--no-verify` 跳过 hook、禁止注释 lint 规则、禁止 `continue-on-error: true` 放水。若发现 CI 变红靠的是 job 被放宽而非真拦截，本项判为 FAIL。
