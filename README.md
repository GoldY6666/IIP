# IIP — Incident Intelligence Platform

[![CI](https://github.com/GoldY6666/IIP/actions/workflows/ci.yml/badge.svg)](https://github.com/GoldY6666/IIP/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/GoldY6666/IIP)](https://github.com/GoldY6666/IIP/releases)
[![License: Apache-2.0](https://img.shields.io/github/license/GoldY6666/IIP)](LICENSE)
[![Go](https://img.shields.io/github/go-mod/go-version/GoldY6666/IIP)](go.mod)

IIP 是一个开源的事件智能平台，为**自托管可观测性栈**设计：把海量异构告警自动关联聚合为可操作的事件（Incident），为每个事件自动收集可追溯的证据链，最终生成可解释的根因假设与修复建议。

它回答的是可观测性栈里「告警之后、定位之前」的那段空白：Prometheus 负责发现，Alertmanager 负责告警，Grafana 负责看板——而「这一百条告警其实是三件事、每件事的证据是什么、先看哪个」没有人回答。

## 核心能力

| 能力 | 说明 |
|------|------|
| 告警接入与去重 | Alertmanager webhook 直接接入；指纹去重（firing 唯一约束），重复投递不会炸库 |
| 五维关联引擎 | 多条告警按权重/阈值自动聚合成事件；配置单行存储，改后免重启对下一条告警生效 |
| 事件处置 | 状态流转（乐观并发，冲突返回 409）、评论时间线、人工判定队列（确认并入 / 抑制） |
| 证据链 | worker 异步采集四类证据（PromQL / LogQL / K8s API / Jaeger Query），超量截断并给出真值计数 |
| 根因假设 | 基于证据覆盖度生成可解释的根因假设与修复建议 |
| 通知 | Slack + 通用 webhook 双出口；去风暴（10 分钟滑动窗口内中间态最多 2 条），终态必发 |
| 自监控 | `/metrics` 暴露业务指标（接入量 / 去重命中 / 关联分布 / 采集耗时 / 任务重试），附 Grafana 面板 |
| Web 控制台 | Vite + React + antd；SPA 深链可刷新，由 server 同源静态托管 |

## 系统架构

```mermaid
flowchart LR
    subgraph EXT["外部数据源（部署侧提供）"]
        AM["Alertmanager :9093"]
        PROM["Prometheus :9090"]
        LOKI["Loki :3100"]
        K8S["K8s API（可选·默认关）"]
        TRACE["链路后端（可选·默认关）"]
    end

    subgraph IIP["IIP（本仓库）"]
        WEB["Web UI<br/>Vite/React，server 静态托管"]
        SRV["iip-server :8080<br/>chi API + 五维关联引擎"]
        WRK["iip-worker<br/>轮询领任务 + 四类采集器"]
        PG[("PostgreSQL 16<br/>9 张表 + 0001-0010 迁移")]
    end

    BROWSER["浏览器"] --> WEB
    WEB -->|"同源 /api/v1"| SRV
    AM -->|"POST /api/v1/alerts/webhook"| SRV
    SRV -->|"落库 → 关联 → 入队（同事务外 best-effort）"| PG
    SRV -->|"启动时自动执行内嵌迁移"| PG
    WRK -->|"领取任务 / 回写游标 / 写证据"| PG
    WRK -->|"PromQL / LogQL / K8s API / Jaeger Query"| PROM
    WRK --> LOKI
    WRK --> K8S
    WRK --> TRACE
    BROWSER -.->|"UI 面板深链（浏览器侧直连）"| PROM
```

三条设计主线（对应代码里的硬约束）：

1. **采集不在告警接入路径上**。webhook 只做「落库 → 关联 → 入队」，证据采集由 worker 异步完成——数据源宕机时 webhook 延迟不变（实测 31ms → 61ms），Alertmanager 不会因 IIP 采不到证据而重投放大风暴。
2. **V1.0 无 Redis**。`IIP_REDIS_DISABLED` 默认 `true`，任务队列就是 PG 表（`FOR UPDATE SKIP LOCKED` 领取、`stale_after` 回收）；worker 侧零 Redis 代码路径（T7.4 用依赖闭包静态证明）。
3. **server 无状态、迁移幂等**。启动时自动跑内嵌迁移（加锁），多副本并发启动不会互相踩。

## 快速开始

前提：Go ≥ 1.26、Docker（含 compose v2）、Node.js ≥ 20、pnpm 9。完整环境依赖表见[工程详情](docs/README-工程详情.md#环境依赖)。

```bash
git clone https://github.com/GoldY6666/IIP.git
cd IIP

make deps                 # 拉 Go modules + 前端 pnpm 依赖
make compose-up           # 起五个依赖服务：postgres / redis / prometheus / alertmanager / loki

export IIP_PG_URL="postgres://postgres:dev@localhost:5432/iip?sslmode=disable"
make run-server           # 新开终端再跑 make run-worker
```

验证服务（另开终端）：

```bash
curl http://localhost:8080/healthz    # 预期输出 ok（真查库：PG 不可用返回 503 unavailable）
```

前端控制台：`make web-dev` 后打开 <http://localhost:5173>。
把 Alertmanager 接进来：见[接入指南](docs/接入指南-Alertmanager.md)。

> Windows 未安装 make 时，每条命令都有等价脚本：`powershell -File scripts/tasks.ps1 <任务名>`（如 `deps` / `compose-up` / `run-server`）。
> compose 拉起的 redis 容器 V1.0 并不使用——IIP 的任务队列就是 PG 表。

## 文档导航

| 文档 | 内容 |
|------|------|
| [docs/README-工程详情.md](docs/README-工程详情.md) | 工程详情：项目结构、环境依赖、API 摘要、常用任务、M0–M6 验收状态 |
| [docs/IIP-V1.0-技术设计文档.md](docs/IIP-V1.0-技术设计文档.md) | 架构与数据模型 |
| [docs/IIP-M1详细开发手册.md](docs/IIP-M1详细开发手册.md) | 开发手册（含协作约束） |
| [docs/接入指南-Alertmanager.md](docs/接入指南-Alertmanager.md) | Alertmanager 接入指南（代码块可原文实跑） |
| [deploy/README.md](deploy/README.md) | 部署：compose（含应用组）+ Helm |
| [CHANGELOG.md](CHANGELOG.md) | 变更记录与已知限制 |
| [AGENTS.md](AGENTS.md) | 协作约束 |

## 当前状态与边界（如实标注）

- 当前版本 **v0.2.0**（2026-10-19 冻结），许可证 [Apache-2.0](LICENSE)。
- 真库测试最近一轮：**1103 PASS / 1 SKIP / 0 FAIL**（SKIP 为 Windows 符号链接权限用例，Linux 下为 PASS）。
- E2E 一键回归（S0–S9）：S0–S6 / S8 / S9 **PASS**，S7 容器链路 **BLOCKED**（缺 Docker daemon，不判红）。
- 边界：K8s / trace 采集器未接真实后端（默认关闭）；CI 已配置于 `.github/workflows/ci.yml`，首次运行结果待观察。
- 更多已知限制见 [CHANGELOG.md](CHANGELOG.md) 的「Known limitations」。

## License

Apache License 2.0，见 [LICENSE](LICENSE)。
