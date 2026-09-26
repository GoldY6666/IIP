# IIP M0 AI 开发计划（AI 施工 · 人验收版）

**周期**：1.5-2 个有效工作日（人工版 5 天的压缩目标）
**配套文件**：《IIP-M0详细开发计划.md》（技术规格以它为准，本文件只改执行方式）
**执行模型**：你是技术负责人，AI 编码代理（Claude Code / Codex / Cursor Agent 等）是开发工程师。你只做四件事：**派任务包 → review diff → 跑验收命令 → 守门禁**。

---

## 1. 开工前人工准备（约 1 小时，AI 无法替你做的部分）

| 事项 | 说明 |
|------|------|
| 本机环境 | 按 M0 详细计划 §0 装齐 Go 1.23 / Git / Docker Desktop / Node 20 / pnpm / golangci-lint / sqlc / migrate CLI，逐项验证通过 |
| 空仓库 | `git init` + 建 GitHub 私有仓 + 配好 Actions（AI 可代办，但账号授权只能你手动） |
| 上下文文件 | 把这三份放进仓库 `docs/` 目录：①V1.0 技术设计文档（docx 转 md）②IIP开发计划.md ③IIP-M0详细开发计划.md。**这是 AI 的规格来源，每个任务提示词都会引用它们** |
| git 身份 | `git config` 配好 user/邮箱、`core.autocrlf true` |

**约定一个固定工作目录**：`D:\Documents\qwen-agent\iip`（下文所有提示词中的路径按此替换）。

---

## 2. 全局门禁规则（贴进仓库 `AGENTS.md`，让 AI 每次会话自动遵守）

把下面这段原文放进项目根目录 `AGENTS.md`，所有任务共享这些约束：

```markdown
# 项目协作约束（AI 必须遵守）
1. 技术规格以 docs/IIP-M0详细开发计划.md 为准；有冲突先停下提问，不得自行扩范围。
2. 本阶段（M0）禁止编写任何业务逻辑：不写 webhook handler、不写关联算法、不写前端页面。
3. 禁止为了让测试通过而修改测试断言；测试失败只能修实现或报告原因。
4. migrations/ 下任何 SQL 的变更，必须在回复中单独列出"表/列/索引/约束"清单供人逐行审查。
5. 每个任务完成的标准 = 验收命令真实执行通过，不是"代码看起来对了"。
6. 不许跳过 CI 门禁（不加 --no-verify，不注释 lint 规则）。
7. 依赖版本必须钉具体版本号，禁止 latest。
8. 设计偏离决定记入 docs/decisions.md，格式：背景/选项/决定/理由，AI 只起草，标注"待人工确认"。
9. Windows 环境：所有文件写入使用 LF 行尾由 .gitattributes 管理；命令示例给 PowerShell 语法。
```

> 第 2、3、5 条是 AI 开发三大事故源（范围蔓延、改断言骗测试、幻觉式完成），必须常驻上下文。

---

## 3. 任务包（按序执行，标注可并行项）

每个任务包三段式：**提示词**（整段粘贴给 AI）→ **你 review 什么**（人工把关点）→ **验收命令**（跑过才算完）。

---

### P1 · 仓库骨架 + 前端脚手架（对应 T0.1，AI 约 20 分钟）

**提示词**：

> 阅读 docs/IIP-M0详细开发计划.md 的 T0.1 节，在仓库根目录完成：①go mod init（module 名用我指定的 github.com/<user>/iip，先问我用户名）；②创建 cmd/iip-server、cmd/iip-worker、internal、migrations、web、deploy、testdata/replay、scripts、docs 目录及占位 main.go（能编译、暂时只打印启动日志）；③在 web/ 用 Vite react-ts 模板初始化前端并安装 antd、@tanstack/react-query、react-router-dom，确认 pnpm build 通过；④.gitignore、.editorconfig、.gitattributes（强制文本文件 LF）；⑤Makefile 含 run-server/run-worker/test/lint/migrate-up/web-dev 六个目标（Windows 下若 make 不可用，改为 scripts/tasks.ps1 提供同名函数，两个都给我）。完成后列出全部新建文件清单。

**你 review**：module 路径拼写、.gitattributes 是否存在（防 CRLF 污染 CI）、有没有偷偷加业务代码。
**验收**：`go build ./...` 通过；`cd web; pnpm build` 通过；`git status` 干净（全部已 commit，message 用 chore:）。

---

### P2 · 数据库迁移（对应 T0.2，AI 约 30 分钟 + 你 20 分钟审查）⭐ 最重要的人工审查点

**提示词**：

> 阅读 docs/IIP-M0详细开发计划.md 的 T0.2 节（含 4 条工程决策）和 docs/ 中 V1.0 技术设计文档 §3.2 的原始 DDL，编写 migrations/0001_init.sql 与对应 0002 down 迁移。要求：6 张核心表 + correlation_feedback 预留表全部落地；逐条执行 T0.2 列出的 4 个决策（VARCHAR+CHECK、不分区的列注释说明、TIMESTAMPTZ、feedback 表结构）；V1.0 文档中已有的全部索引（含 GIN(labels)）一个不少；每条 CHECK 约束写独立命名（chk_alerts_status 式）。完成后输出"表/列/索引/约束"清单和一份 6 项手工 INSERT 验证脚本（合法+非法值混合）。

**你 review**（逐行看 SQL，这是唯一必须全文精读的产出）：①对照 V1.0 文档原文数表数索引；②DECIMAL(5,4) 精度；③ON DELETE CASCADE 只在从属表上；④feedback 表字段够不够 M2 用（alert 对、decision、actor）。
**验收**：跑 M0 计划 T0.2 的 up → down-all → up 三连命令；执行 AI 的 INSERT 验证脚本，非法值全部被 CHECK 拒绝。

---

### P3 · config + server 空壳 + worker 空壳（对应 T0.3-T0.5，AI 约 40 分钟，可让 AI 一次做完）

**提示词**：

> 阅读 docs/IIP-M0详细开发计划.md 的 T0.3、T0.4、T0.5 三节（含给出的 Go 代码骨架），完成三个包：①internal/config（koanf，flag>env>本地yaml 优先级，启动校验 PG 可连通否则 fatal），附表驱动单测；②cmd/iip-server：chi 路由 + RequestID/RealIP/slog日志/Recoverer 中间件 + /healthz 真查库（SELECT 1，失败503）+ SIGTERM 优雅关闭（10s 超时），预留 M1 路由挂载点注释；③cmd/iip-worker：ticker polling 循环 + panic recovery + 60s 心跳日志 + drain 语义（停止领新任务、in-flight 上限 15s）+ ctx cancel 单测。日志统一 log/slog JSON。给 Makefile 补充 docker 本地 PG 起停目标。写完后本地启动三件套自测并报告结果。

**你 review**：healthz 是否真查库；worker 的 drain 逻辑是占位假实现还是真做了 in-flight 计数；main.go 里没塞业务。
**验收**：`go test -race ./...` 绿；起 compose 的 PG 后 server/worker 并行跑 10 分钟；`curl.exe localhost:8080/healthz` = ok，停掉 PG 容器 = 503；kill 进程日志出现优雅退出。

---

### P4 · docker-compose 五服务（对应 T0.6，AI 约 30 分钟）

**提示词**：

> 阅读 docs/IIP-M0详细开发计划.md 的 T0.6 节（五服务版本表），编写 deploy/docker-compose.yml 及 prometheus/alertmanager/loki 的配置文件。要求：版本严格按表格钉死；五服务全配 healthcheck 与 restart: unless-stopped；alertmanager receiver 指向 host.docker.internal:8080/api/v1/alerts/webhook；prometheus 配一条必然触发的示例规则（evaluation_interval 15s）；postgres/loki 用 named volume；写一个 deploy/README.md 说明起停和常见故障。完成后实际执行 docker compose up -d 并报告五服务的 healthcheck 状态。

**你 review**：版本号有没有被写成 latest；alertmanager.yml 的 webhook url 路径拼写。
**验收**：`docker compose ps` 五服务 healthy；9093 页面能看到 firing 告警（receiver 404 属预期——这恰好证明告警真的在向后端推送）。

---

### P5 · CI 流水线（对应 T0.7，AI 约 20 分钟 + 一次破坏性测试）

**提示词**：

> 阅读 docs/IIP-M0详细开发计划.md 的 T0.7 节，编写 .github/workflows/ci.yml：backend job（setup-go@v5 读 go.mod、golangci-lint-action@v6 配 .golangci.yml 开启 errcheck/govet/staticcheck/gocritic、build、go test -race -coverprofile）与 frontend job（setup-node@v4 + pnpm cache、install --frozen-lockfile、tsc --noEmit、build）两个并行 job。另配本地 pre-commit 脚本（gofumpt -l 检查）。完成后 push 并报告 Actions 运行结果。

**你 review + 破坏性测试**：让 AI push 后，你手动提交一个故意未格式化的 Go 文件，确认 CI 变红，再让 AI 修好变绿。**这一步验证"门禁真的会拦人"，不可省略。**
**验收**：Actions 双 job 绿；红→修→绿的完整证据（截图或运行记录）存入 docs/。

---

### P6 · 总验收与发版动作（对应 D5 下午，AI 约 30 分钟，你 1 小时）

**提示词**：

> 按 docs/IIP-M0详细开发计划.md §3 的验收清单逐项执行，输出一张"清单项/结果/证据"三列表格；任何一项不通过如实标注 FAIL 和原因，禁止修饰。然后把 AGENTS.md 中记为"待人工确认"的 decisions 整理进 docs/decisions.md，写 M0 小结（实际耗时 vs 预算、各任务偏差）。最后更新 README.md 的 Quickstart（面向新用户的三步跑起流程）。

**你人工执行**：找一个"干净视角"——新开终端、删掉本地 bin 产物，严格按 README 三步走一遍。AI 自己验自己容易漏环境隐性依赖。全过 → 打 tag `v0.0-m0`。

---

## 4. 执行顺序与并行策略

```text
你: 环境准备(1h) ──→ P1 ──→ P2(重点精读) ──→ P3 ──→ P4 ──→ P5 ──→ P6(总验收)
                              │                    │
                              └─ P2/P3 可并行派给   └─ P4 与 P5 可并行
                                 两个 AI 会话*          （互不依赖）
```

*并行前提：两个 AI 会话改动的文件不重叠（P2 只碰 migrations/，P3 只碰 internal/、cmd/）。**并行时严禁两个会话同时改 go.mod / Makefile**——约定 P2 会话只读依赖文件，加依赖一律经 P3/P1 会话。

**节奏建议**：串行模式约 3-4 小时纯 AI 工时 + 你 2-3 小时 review/验收，一个工作日可完成 P1-P5；P6 放次日上午。

## 5. 本模式下的风险清单

| 风险 | 表现 | 对策 |
|------|------|------|
| 幻觉式完成 | AI 报告"验收通过"但没真跑命令 | 验收命令一律**你亲手在终端执行一遍**，P1-P6 表格里已分开"你 review"栏 |
| 上下文丢失 | 会话压缩后 AI 忘了 AGENTS.md 约束 | 每个任务提示词开头都带"阅读 docs/…为准"，规格落盘而非记在对话里 |
| 过度生成 | 一次派多个任务包，AI 顺手把 M1 写了 | 严格一会话一任务包；diff 超过当包范围直接打回 |
| SQL 静默改动 | 后续 AI 改迁移"顺手"删了个索引 | 约定：0001_init.sql 合并后冻结，改动只能新增 0003/0004 迁移 + 逐行列变更 |
| CI 假绿 | lint 被注释、job 被放宽 | P5 的破坏性测试是唯一防伪手段，每个里程碑末重做一次 |

## 6. 与人工版的差异一句话总结

人工版 M0 你写每一块砖；AI 版 M0 你**验每一块砖**——技术规格、决策、门禁标准完全不变（仍以《IIP-M0详细开发计划》为准），变的只是劳动分工。若 P6 总验收暴露两个以上 FAIL，退回对应任务包重做，不带病进入 M1。
