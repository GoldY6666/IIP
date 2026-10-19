# IIP v0.2.0 Release Notes（GitHub Release 用稿）

> 用途：推送到 GitHub 后，在 `Releases → Draft a new release` 里
> tag 选 `v0.2.0`、标题填 `IIP v0.2.0`、正文粘贴下方分隔线之间的内容。
> 正文里**不含**任何未兑现的说法：CI 未跑、容器链路未实证，均如实写在末节。

---

## IIP v0.2.0 — 通知链路、自监控与全量回归

v0.1.0 之后的第二个公开发布版。**无破坏性变更**，但有**一处对外可见的行为变更**
（见下方「行为变更」），升级前请读。

### 本版新增

**通知链路（T6.1 / T6.2，ADR-024 / 025）**
事件状态变化现在可以推送到外部系统，默认**关闭**。

- Slack incoming webhook + 通用 HTTP webhook（JSON + `Bearer`）两个出口
- **异步、best-effort**：投递失败只记日志，绝不阻塞告警接入路径
- **去风暴**：中间态（acknowledged / investigating / mitigated）滑动窗口内最多 2 条/10 分钟；
  **终态（resolved / ignored）与重开（open）必发且不消耗配额**，创建新事件的通知永远发
- 两个出口都**拒绝跟随重定向**：否则请求体与 `Authorization` 会被转给第三方

**自监控 `/metrics`（T6.3 / T6.4，ADR-026）**

- 7 个业务指标（入库 / 去重 / webhook 耗时 / 关联分 / 事件新建 / 证据采集耗时 / 任务重试）
  + Go 运行时指标，Prometheus 文本格式
- server 与 worker **各自独立监听**（worker 独立 `:9091`），因此抓取配置里是**两个 job**
  —— 合并成一个只会让面板一半是空图
- 附 `deploy/prometheus/rules/iip-self.yml`（4 条记录规则 + 2 条告警）与
  `deploy/grafana/iip-overview.json`（9 个面板，导入即可用）
- 任何标签都不取事件/告警/服务维度（无界基数会拖垮 Prometheus），有单测守着
- **不变量**：`dedup{hit}+dedup{miss} == ingest{stored}+ingest{duplicate}`

**接入指南（T6.5）**
`docs/接入指南-Alertmanager.md` 是操作侧文档：receiver 配置 → 手发验证 →
三处确认 → payload 契约 → 排错表 → 通用 HTTP 源 → checklist。
其中的代码块由 `scripts/verify-onboarding.py` **原文抽出、在真栈上执行**，文档写错就红。

### 行为变更（升级前必读）

`POST /api/v1/incidents/{id}/comments` 与非 UTF-8 请求体的状态码变了（T6.6 / ADR-027）：

| 输入 | 旧 | 新 |
|---|---|---|
| 请求体超限（任意 JSON 入口） | comments 入口报 **400**「不是合法 JSON」 | 统一 **413** |
| 非 UTF-8 请求体（如 GBK） | **503**（存储不可用，实际是 PG `SQLSTATE 22021`） | **400** + 编码提示 |

第二条尤其重要：**原先把客户端编码错误报成了「IIP 存储坏了」**，运维会去查 PG。
这不是回归，是错误分类修对了。修法仍然是让调用方发 UTF-8。

### 加固

- **告警表按月分区 + 保留期**（T7.1 / ADR-022，迁移 0008）：分区键 `starts_at`，
  修掉非 UTC 会话时区下的**静默错月**（`+08` 重叠、负偏移错一个月）。
  已知限制：分区只增不减，磁盘回收依赖 VACUUM。
- **索引调优**（T7.2 / ADR-023，迁移 0009）：删掉**零读者**的 labels GIN（14 个分区副本
  `idx_scan` 全 0）；新增 `tasks` 证据任务的事件表达式索引（该路径原先永远全表扫）。
- **单文件 ≤ 300 行门禁**（T7.8 / ADR-028）：`scripts/check-file-size.py` + 存量基线
  **只减不增**。偿清 main.go（485→264）、config.go（616→212）、processor.go（483→253），
  纯同包移动、零行为变化。**存量仍有 44 个文件超限**，基线只保证「不会更糟」。
- **HTTP 入口统一 `readBody`**（T6.6 / ADR-027）：三个 JSON 入口共用一条校验路径。
  **同批裁定不做限流、不做 CORS**，理由见 CHANGELOG「Known limitations」——
  限流会在风暴时把最新告警排到退避队列后面，而风暴中最贵的恰恰是最新告警的时延。

### 验证

一键回归 `scripts/verify-e2e.py` 现在编排 **S0–S9** 十个阶段，按 M1–M6 验收项与
MVP 总验收出总报告。本版新增两段：

- **S8 通知链路**：真 PG + 真 server + 假出口，9 条判据（两出口送达 / 去风暴 /
  终态必发 / token 不外泄 / 优雅退出）
- **S9 接入指南可执行**：文档代码块原文在真栈执行；本机缺 bash/curl 时记 **BLOCKED**，
  不冒充通过

**本轮结果**（2026-10-18）：S0–S6 / S8 / S9 **PASS**，S7 容器链路因 Docker daemon
不在本机而 **BLOCKED**。汇总结论 `PASS-WITH-BLOCKED`，无红。
真库门禁 **1103 PASS / 1 SKIP / 0 FAIL**（SKIP 为 Windows 无 symlink 权限，
Linux 容器侧 SKIP=0）。

### 已知限制（如实）

- **CI 从未在 GitHub 上执行过**：`.github/workflows/ci.yml` 的三 job 至今没有一次真实
  运行记录；本版全部证据来自本机沙箱内的等价复现。首次推送后的 CI 结果仍是未知数。
- **容器链路未实证**：镜像标签 `0.2.0` 只经过交叉编译 + ELF 静态解析，
  未经容器实跑（本机 daemon 不可用）。
- **44 个文件仍超 300 行**，只是不再增长（基线在 `scripts/file-size-baseline.txt`）。
- **K8s / trace 采集器未接真实后端**，默认关闭。
- **关联候选限额的残留边界**：新告警的最佳匹配若是**非种子**老成员，仍可能漏合并。
- **不做限流、不做 CORS**：见上方说明，是有意取舍不是遗漏。

### 升级

无破坏性变更，无新增必填配置。若在用 `/metrics`，注意 server 与 worker 要**分别抓取**
（`:8080` 与 `:9091`，见 `deploy/prometheus/prometheus.yml`）。
通知功能默认关闭，需要时设 `IIP_NOTIFY_ENABLED=true` 并配置出口。

发版形态：git tag `v0.2.0`、镜像 `iip-server:0.2.0` / `iip-worker:0.2.0`、
Helm chart `appVersion: 0.2.0`。版本标签由 `scripts/bump-version.py` 统一维护
（16 处，判据在 E2E S1）。

---

## 附：正文所用的核对数据（不粘贴进 Release）

| 项 | 值 | 复核命令 |
|---|---|---|
| 版本 | 0.2.0（16 处一致） | `python scripts/bump-version.py --check` |
| 真库用例 | 1103 PASS / 1 SKIP / 0 FAIL（20 包，exit=0） | `python scripts/run_pgtests.py -v` |
| E2E | S0–S6/S8/S9 PASS，S7 BLOCKED | `python scripts/verify-e2e.py` |
| 发版前版本 | 0.1.0（tag 指向 `a6c184a`） | `git log --oneline v0.1.0 -1` |

**四处必须说清的边界**（免得把「跑过」说成「在 tag 那个提交上跑过」）：

1. **E2E 全量一轮跑在 `8374be2`**。此后只改了：`CHANGELOG.md`（补三条 Fixed）、
   `docs/release-v0.2.0.md`（本附录）、`.github/workflows/ci.yml`（e2e job 的 Go
   工具链改为从 `setup-go` 取）、`README.md`（门户化：根 README 重写为面向访客的
   门户版，原工程详情移至 `docs/README-工程详情.md` 并修正过期表述）。
   **四者都不在 E2E 判据范围内**；发布提交上复跑了
   **S0 + S1**（含干净克隆 + 全量 build）仍 PASS、rc=0。没有为了「让报告覆盖到
   最后一版」再跑一次全量 —— 那 10 分钟里没跑的东西就是没跑过，不能算进去。
2. **真库那 1103 是用 `-v` 跑出来的**（不带 `-v` 只能看到包级 `ok`，看不到子用例
   与 SKIP）。唯一 SKIP 是 `TestHandlerAllowsSymlinkedRoot`：Windows 非管理员
   无法建符号链接，Linux 容器侧该用例为 PASS。
3. **`golangci-lint` 是用 CI 钉的同一版本 v2.14.0 在发布提交上本地实跑的**：
   `run` **0 issues**、`fmt --diff` **rc=0 无 diff**。这是推送前的静态审计，
   不是 CI 真跑 —— `ci.yml` 至今没在 GitHub 上执行过一次。

