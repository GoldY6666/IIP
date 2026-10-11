# 推送到 GitHub —— 操作指引

> 生成时间：2026-10-11
> 仓库：`https://github.com/GoldY6666/IIP`（当前为**空仓库**，从未推送过）
> 状态：**本地已 100% 就绪，等你在本机执行一次推送**

---

## 一、当前状态（推送前已核验）

| 项 | 值 |
|---|---|
| 本地仓库路径 | `D:\Documents\qwen-agent\iip` |
| 分支 | `main` |
| 提交数 | **80** |
| HEAD | `3bb325c chore: .gitignore 加规则拦 GitHub Desktop 的嵌套空克隆` |
| 工作树 | **完全干净**（无未提交改动） |
| 仓库体积 | **1.33 MiB**（已 `git gc` 压成单个 pack，1048 个对象） |
| remote | `https://github.com/GoldY6666/IIP.git`（**已确认无 token 残留**） |
| 远程分支 | `[]` —— 空仓库，确认从未推过 |

> **为什么强调体积**：`git gc` 把3.95 MiB 松散对象压成了 1.33 MiB 单个 pack。
> 这样推送只需传一个 pack 而不是1048 次请求，**在你本机网络上会快很多、也不易中断**。

---

## 二、⚠️ 推送前请先决定：仓库要不要设为私有

**当前 `GoldY6666/IIP` 是 `public`（公开）**，任何人都能看到。

推送上去的内容包含：

| 文件 | 内容 |
|---|---|
| `deploy/docker-compose.yml` | 完整部署形态（服务拓扑、端口映射、依赖顺序） |
| `config/topology.yaml` | 服务依赖关系定义（五维关联用） |
| `.github/workflows/ci.yml` | 完整 CI 流程与环境变量名 |
| `docs/` 下 20+ 份文档 | 架构设计、里程碑手册、**内部判据与账目记录** |

**建议：先改成 private 再推。**
路径：仓库页 → Settings → General → Danger Zone → Change visibility → Private

> ⚠️ **公开过的内容即使改回私有，也可能已被缓存或被爬虫抓走** —— 所以要在**推送之前**决定。

---

## 三、方式一：GitHub Desktop（推荐，最省事）

你的本机网络不受沙箱出口限制，这是最可靠的路径。

1. 打开 **GitHub Desktop**
2. 菜单 **File → Add Local Repository…**
3. 路径填：`D:\Documents\qwen-agent\iip`
   > ⚠️ **注意选父目录**，不要选 `D:\Documents\qwen-agent\iip\IIP`
   > （那个同名空目录已被 `.gitignore` 拦住，但**添加仓库时仍要选对**）
4. 点 **Add Repository**
5. 左栏应显示 **80 个 commit**，顶部有 **"Push origin"** 按钮
6. 点 **Push origin**
7. 首次会弹登录窗：用 `GoldY6666` + 你的 PAT
   > PAT 需含 **Contents: Read and write** + **Workflows: Read and write**
   > （`Workflows` 权限已加好；缺它 GitHub 会拒绝，因为仓库里有 `.github/workflows/ci.yml`）

> **如果 Desktop 仓库列表里还留着那个空的 `IIP` 仓库**，先在
> `Repository → Remove` 移除，避免推错地方。

---

## 四、方式二：命令行（本机 PowerShell 或 Git Bash）

```powershell
cd D:\Documents\qwen-agent\iip
git push -u origin main
```

弹出登录窗时填 `GoldY6666` + PAT。

**若不想每次输密码**，先设置凭据助手：

```bash
git config --global credential.helper manager
```

然后照常 `git push`，第一次弹窗填一次即可。

---

## 五、推完请验证（两条命令）

```bash
# ① 应输出 "main -> main" 或 "Everything up-to-date"（此时是真的了）
git push -u origin main

# ② 看本地记录的远程 HEAD 是否与本地一致
git rev-parse main origin/main
#    两个哈希相同 = 推送成功
```

---

## 六、推送成功后会发生什么

GitHub 收到 `.github/workflows/ci.yml` 后会**自动跑第一次 CI**，包含三个 job：

| job | 做什么 | 预期耗时 |
|---|---|---|
| `lint` | `golangci-lint v1.61.0` | ~2 min |
| `backend` | Go 单测 + **真库集成测试**（本次已修：补 `services.postgres` + `IIP_TEST_PG_URL`） | ~10 min |
| `e2e` | Playwright 前端 + 真进程 E2E（本次已修：删与 `stack.mjs` 抢 5434 的 service 容器） | ~5 min |

**查看结果**：仓库页 → Actions 标签页。

> **这次是 `ci.yml` 的历史首跑。** 它此前从未在 GitHub 上运行过，
> 2026-10-09 只做过**静态审查**并修了四处硬伤
> （详见 `docs/M6-交付详细报告.md` §4.1）。
> **静态审查不等于跑过**，首跑结果有不确定性：

> ⚠️ **最可能的一个坑**：`golangci-lint v1.61.0`（2024-09）构建时的 Go 版本
> 低于 `go.mod` 声明的 `go 1.23` ⇒ 报
> 「the Go language version used to build golangci-lint is lower than the targeted Go version」。
> **真出现时的修法是升 `golangci-lint` 到 v1.x 最新版（如 v1.64.x，配置格式不变），
> 不是降 `go.mod` 的 go 版本。**

---

## 七、推送完成后需要做的两件事

### 1. 登记 `OPEN-M5-4` 关闭

`OPEN-M5-4` 登记的是「`vitest`/`jsdom`/Testing Library 是新增 devDependency，
需在 CI 加 `pnpm test` job 才算进门禁」，此前因**仓库无 remote** 而无法验证。

推送 + CI 首跑（尤其是 `frontend` 的 `vitest` 真的在 runner 上跑起来）完成后，
这条才可以**就地关闭**。文件：`docs/decisions.md` 登记册。

### 2. 撤销这个 PAT + 改账号密码（安全）

对话里明文出现过你的 GitHub 账号密码，以及这个 PAT。
**推完请立刻处理**：

- **撤销 PAT**：Settings → Developer settings → Personal access tokens → 删除该 token
- **改 GitHub 密码**

---

## 八、如果推送失败

| 报错 | 原因 | 处置 |
|---|---|---|
| `refusing to allow a Personal Access Token to create or update workflow` | PAT 缺 `Workflows` 权限 | 加 `Workflows: Read and write` 后 Save |
| `Authentication failed` | PAT 失效或密码错 | 换用 PAT（密码认证 2021-08 起已被 GitHub 禁用） |
| `remote: Repository not found` | 仓库名大小写错 | 必须是 `GoldY6666/IIP`（**大写**） |
| `Updates were rejected` | 远程已有提交 | 先 `git fetch origin` 看远程状态 |
| `error: RPC failed; curl 56` | 网络中断 | 重试；对象已压成单 pack，正常不会发生 |