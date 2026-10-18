# 推送到 GitHub —— 操作指引

> 生成时间：2026-10-11；**2026-10-18 更新：改为「分两次推送」，并补 v0.2.0**
> 仓库：`https://github.com/GoldY6666/IIP`
> 状态：**本地已就绪，等你在本机执行两次 push（顺序不能反）**

---

## 零、2026-10-18 复核结论（先读这段）

| 项 | 结论 | 依据 |
|---|---|---|
| remote 是否存在 | **存在**：`origin → https://github.com/GoldY6666/IIP.git` | `git remote -v` |
| 远程仓库状态 | **存在但是空的（零提交）** | `git ls-remote` 返回 0 行；HTTP 200 |
| 本地 tag | `v0.0-m1` … `v0.0-m5`、`v0.1.0`、`v0.2.0`（**均为本地 tag**） | `git tag --list` |
| 当前代码版本 | **0.2.0**（16 处标签一致） | `python scripts/bump-version.py --check` |

> README / CHANGELOG 里原先写的「仓库无 git remote」是**过期说法**，已在本轮改为
> 「remote 已配置但仓库为空、尚未推送」。写文档时说的是当时的事实，事实变了就要改。

---

## 一、为什么要分两次推

`v0.1.0` 不是 HEAD，它指向历史提交 `a6c184a`（94 个提交处）；`v0.2.0` 指向最新提交。
一次性推 `main` 会把两个 tag 一起带上去，但**Release 页只能「先 v0.1.0 后 v0.2.0」
才能讲出一个可信的时间线** —— 而且 GitHub 的 Release 一旦发错顺序，
「上一个版本」链接就指错了。分两次更稳，也更好核对。

| 步骤 | 推什么 | 推完仓库里应该有 |
|---|---|---|
| **第 1 次** | 到 `v0.1.0` 为止的历史 + `v0.1.0` tag | 94 个提交、1 个 tag |
| **第 2 次** | `main` 到 HEAD + `v0.2.0` tag | 全部提交、2 个 tag |

---

## 二、第 1 次推送：到 v0.1.0

```bash
cd D:/Documents/qwen-agent/iip

# ① 只推到 v0.1.0 那个提交（用 refspec，不会把 main 提前推上去）
git push origin a6c184a:refs/heads/main
# ② 推 v0.1.0 标签（annotated tag 单独推）
git push origin v0.1.0
```

核对（应输出 `a6c184ac69dcc9740551b899b71bd50f23cb16e4`）：

```bash
git ls-remote origin refs/heads/main
git ls-remote origin refs/tags/v0.1.0
```

> 若你想让 `main` 从一开始就指向最新、Release 只按 tag 建，也可以跳过①的 refspec，
> 直接在第 1 次就 `git push -u origin main` —— **两条路都行**，区别只在于
> 「远程 main 什么时候变成最新」。上面这条是更保守的做法。

---

## 三、第 2 次推送：main + v0.2.0

```bash
cd D:/Documents/qwen-agent/iip

git push -u origin main        # 补齐 v0.1.0 之后的全部提交
git push origin v0.2.0         # 推 v0.2.0 标签
```

核对：

```bash
git rev-parse main origin/main     # 两个哈希相同 = main 推成功
git ls-remote origin refs/tags/    # 应列出 v0.0-m1..m5 / v0.1.0 / v0.2.0
```

---

## 四、两个 GitHub Release

正文**直接复制 `docs/release-v0.2.0.md` 里分隔线之间的内容**（该文件顶部写了用法）。

| 顺序 | tag | 标题 | 备注 |
|---|---|---|---|
| 1 | `v0.1.0` | `IIP v0.1.0 — 首个公开发布版` | 正文用 `CHANGELOG.md` 的 `## [0.1.0]` 节；**必须保留 Known limitations** |
| 2 | `v0.2.0` | `IIP v0.2.0 — 通知链路、自监控与全量回归` | 正文用 `docs/release-v0.2.0.md` |

> Release Notes 里**不要**写「CI 绿」。`ci.yml` 一次都没在 GitHub 上跑过，
> 首跑结果未知（见 §六）。发版稿里已经如实写了这一条，别在发布时手动删掉。

---

## 五、⚠️ 推送前请先决定：仓库要不要设为私有

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

> 已做的公开前自查（2026-10-18）：`.env.example` / `secret-pg.yaml` 均为模板或 **dev 默认值**；
> `git log -S` 扫私钥与 `ghp_` 无命中；`git grep` 扫 AWS / OpenAI / Slack token 无命中。
> LICENSE 为 Apache-2.0。

---

## 六、推送后会怎样

GitHub 收到 `.github/workflows/ci.yml` 后会**自动跑第一次 CI**，包含三个 job：

| job | 做什么 | 预期耗时 |
|---|---|---|
| `lint` | `golangci-lint` v2（Go 1.26.9 工具链） | ~3 min |
| `backend` | Go 单测 + **真库集成测试**（本次已修：补 `services.postgres` + `IIP_TEST_PG_URL`） | ~10 min |
| `e2e` | Playwright 前端 + 真进程 E2E（本次已修：删与 `stack.mjs` 抢 5434 的 service 容器） | ~5 min |

**查看结果**：仓库页 → Actions 标签页。

> **这次是 `ci.yml` 的历史首跑。** 它此前从未在 GitHub 上运行过，
> 2026-10-09 只做过**静态审查**并修了四处硬伤
> （详见 `docs/M6-交付详细报告.md` §4.1）。
> **静态审查不等于跑过**，首跑结果有不确定性：

> ⚠️ **已修掉的那个坑（留档，别再踩回去）**：原先固定 `golangci-lint v1.61.0`
> （2024-09），它构建时的 Go 版本低于 `go.mod` 的 `go 1.26` ⇒ 会报
> 「the Go language version used to build golangci-lint is lower than the targeted Go version」。
> 现已迁到 **action v9 + golangci-lint v2**（见 `ci.yml` 第 93–107 行的注释）。
> **若真报同类错误，修法是升 lint 版本，不是降 `go.mod` 的 go 版本。**

> ⚠️ **首跑最可能出问题的地方（我的判断，未经实证）**：`backend` job 的真库测试
> 依赖 `services.postgres` 与 `IIP_TEST_PG_URL` 对齐；`e2e` job 依赖 runner 上装好
> Chrome。这两处只做过静态审查 —— **静态审查不等于跑过**。

---

## 七、推送完成后需要做的两件事

### 1. 登记 `OPEN-M5-4` 关闭

`OPEN-M5-4` 登记的是「`vitest`/`jsdom`/Testing Library 是新增 devDependency，
需在 CI 加 `pnpm test` job 才算进门禁」，此前因**仓库从未推送**而无法验证。

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

---

## 九、本文档的漂移守卫（改本文前先读）

**本文不写提交数与 HEAD**（2026-10-18 改版时删掉了：写了必然过期）。

- 唯一写死的 SHA 是 **`v0.1.0` 指向的提交 `a6c184a`**，它是历史事实，不会变；
  复核命令见 §二。
- **HEAD 提交号**：**本文件无法自引用自己的提交号** ——
  提交那一刻才产生该 SHA，写进去必然是上一个提交的 SHA。

⇒ **用法**：HEAD 行不写具体 SHA（避免自引用悖论），**以 `git log --oneline -1` 实际输出为准**。

交付时已执行的复核：

```bash
git rev-list --count HEAD              # 与 §一 的「提交数」比对
git log --oneline -1                   # HEAD 实际 SHA
git count-objects -vH | grep size-pack # 与 §一 的「仓库体积」比对
git status --porcelain                 # 应为空 = 工作树干净
```

> **写文档时最容易出的错就是这类漂移**：写进去的数字当时为真，
> 提交那一刻起就成了假的。**凡文档出现「当前值」，要么标注"以实际输出为准"，
> 要么配一条守卫命令** —— 与 M6 里「覆盖度分母固定 6 而非手写常量、配漂移守卫」同源。