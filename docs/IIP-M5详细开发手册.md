# IIP M5 详细开发手册（Web UI 收口与部署形态）

> 状态：**已确认生效**（禹 2026-10-04 确认范围与 OPEN 项处置）。
> 本文档是 M5 的唯一开发依据；实现与本文不符即视为缺陷，
> 任何偏离须**先改本文再改代码**。
>
> 体例沿 M3/M4（十一章固定结构），对齐表数据全部为 2026-10-04 实测。
>
> **OPEN-M5-1 / 2 / 3 的处置**（禹确认「按手册暂定推进」）：
> ① S3 **不做鉴权**，`deploy/README.md` 须显著标注「V1.0 仅供内网 / 演示环境」；
> ② `IIP_UI_*` 默认值在 F3 用真 compose 验证点击可达；
> ③ S4 若延期则登记新 OPEN 项，**不得**把 §4.8 写成通过。

---

## 〇、M5 完成定义（DoD）

M5 完成的定义 = **同时满足以下六条**（与 M3/M4 的 DoD 体例一致）：

1. **配置页可用**：`/settings` 能读出并修改关联权重/阈值，保存后**无需重启即生效**，
   非法输入在**提交前**被拦下且错误信息指明是哪个字段。
2. **证据可跳原始面板**：每条 metric / log 类证据旁有「在 Grafana / Prometheus 打开」
   链接，URL 由证据自带的 `query` 与 `source` 拼出，点击可直接复现该次查询。
3. **UI 有部署形态**：`docker compose --profile app up -d` 之后，
   浏览器打开一个地址即可使用 UI（含 `/api` 反向代理），**不需要**手工跑 vite dev。
4. **前端有自动化测试**：组件级测试（vitest + Testing Library）覆盖
   证据卡、处置按钮、配置表单三条主路径；E2E（Playwright）跑通
   「列表 → 详情 → 处置」并在处置后**回读 timeline 确认落库**。
5. **不回归**：M5 引入的测试设施**不得**让既有四条验证链路任一条变红
   （`verify-func` / `verify-runtime` / `verify-images` / 静态门禁）。
6. **质量达标**：`go build/vet/test` + `gofmt` 全绿；`golangci-lint` rc=0
   （逐项修复，不加排除规则、不用 `nolint`）；真库门禁 SKIP=0；
   容器链路断言 0 失败；**前端 `tsc -b` 与 `vite build` rc=0**。

---

## 一、M4 交接物核对（开工前必读）

| 项 | 实测状态（2026-10-04） | M5 能否依赖 |
|---|---|---|
| M4 手册 §八 十条判据 | 全部有实测支撑（详见 `docs/M4-交付详细报告.md`） | 是 |
| 全量真库门禁 | 425 PASS / 0 FAIL / 0 SKIP（串行 22.5 分钟） | 是 |
| 四条验证链路 | 全绿；容器链路 59 条、宿主全栈 45 条、业务闭环 75 条 | 是 |
| 里程碑 tag | `v0.0-m3`（40ba6cc）、`v0.0-m4`（7b4daee）已补 | 是 |
| 工作区 | `git status` 干净，无 TODO/FIXME 遗留 | 是 |
| 证据 API | `GET /api/v1/incidents/{id}/evidence` 可用，`truncated`/`total` 自洽 | 是 |
| 关联配置 API | `GET/PUT /api/v1/correlation/config` 已存在（M2 建的） | 是 |
| 前端页面 | 事件列表 / 详情双 Tab / REVIEW 队列 / 告警列表 四页已在（M3 S3） | 是 |
| **前端部署形态** | **无** —— compose 无 web 服务，代码无静态托管 | **否，缺口 C1** |
| **前端测试设施** | **无** —— `package.json` 无 test 脚本、无 vitest/playwright | **否，缺口 C2** |

**结论**：M4 交付完整，M5 可以开工。但存在两个**此前从未被任何判据覆盖**的缺口，
必须在 M5 内解决（见 §三）。

---

## 二、与当前仓库实测状态的对齐表（2026-10-04 实测）

### 2.1 开发计划的 M5 任务 vs 实际现状

`docs/IIP开发计划.md` §9 定义 M5 = **最小 Web UI（10 天，8 个任务）**。
逐项核对仓库后，**其中 5 项已被 M3 的前端交付提前做完**：

| 开发计划任务 | 实际现状 | 证据 |
|---|---|---|
| T5.1 前端骨架（路由/布局/API 客户端/token） | **已完成** | `react-router-dom` 已在依赖；`App.tsx` 四条路由 + `components/Layout.tsx` 94 行 |
| T5.2 Incident Console 列表 | **已完成** | `pages/Incidents.tsx` 248 行，含 severity/筛选/搜索 |
| T5.3 Detail 三 Tab | **已完成** | `pages/IncidentDetail.tsx` 410 行 + `components/EvidenceList.tsx` 314 行 |
| T5.4 处置操作（四态 + 备注） | **已完成** | `api/incidents.ts` 的 `transitionIncident` + `ladder`/`canTransition`；详情页四态按钮 + 备注输入 |
| T5.5 REVIEW 确认队列 | **已完成** | `pages/Reviews.tsx` 190 行 |
| T5.6 外部面板直跳链接 | **未做**（非「部分」） | `grafana` 仅出现在注释；代码里无任何 URL 拼装逻辑 |
| T5.7 配置页 | **未做** | 无 Config/Settings 页；`src/api/` 下 5 个客户端均无 `correlation/config` |
| T5.8 前端 E2E | **未做** | 无 playwright/vitest 依赖，无 test 脚本 |

> **T5.6 更正说明**：上一轮口头汇报曾把它记为「部分完成」，属误判 ——
> 判据写成「有没有 grafana 字样」而该字样只存在于注释。
> **判据写成字符串字面量匹配会给出假结论**，本手册一律以能力级判据描述。

### 2.2 两个此前无人负责的缺口（本次核对新发现）

**缺口 C1：前端没有部署形态**

- `deploy/docker-compose.yml` 只有 6 个服务，**无 nginx / web / frontend**。
- `internal/` 里**没有任何静态托管代码**（无 `FileServer` / `Static` / `index.html` 处理）。
- `web/dist` 产物存在（`index.html` 401 B + CSS 621 B + JS 1,088,776 B），
  但**不被任何服务消费**。
- ⇒ 开发计划的验收标准「新用户从 `docker compose up` 到在 UI 处置完一个 incident
  ≤ 30 分钟」**当前无法达成**。

**缺口 C2：前端零自动化测试**

- `web/package.json` 的 `scripts` 为 `dev` / `build` / `preview` / `tsc`，**无 `test`**。
- `devDependencies` 无 vitest / jest / playwright / @testing-library。
- ADR-016 已裁定「M3/M4 期间延后」，但当时的前提是「M5 一定会做」——
  若 M5 仍不做，M3/M4 手册 §4.8 类条款将**永久 NOT DONE**，V1.0 发布时挂着。

### 2.3 可直接复用的现成资产

| 资产 | 位置 | 用途 |
|---|---|---|
| 前端反代实现 | `D:\_iip_setup\webproxy\main.go`（纯标准库 `ReverseProxy`，`GOPROXY=off` 可构建） | C1 的原型，产品化后可作实现参考 |
| 前端截图验收 | `D:\_iip_setup\s3_frontend_check.py`（真 PG + 真后端 + 无头 Chrome） | E2E 前的手工冒烟 |
| 重建前端 | `D:\_iip_setup\build_web.py` | 构建用，不入库 |

> C1 的实现**不应**直接把 `webproxy` 搬进仓库 —— 那是脚手架（单文件、
> 无静态托管、无 SPA 回退、无缓存策略）。产品化方案见 §4.5。

---

## 三、范围

### 3.1 做什么（M5 锁定）

| 编号 | 任务 | 来源 |
|---|---|---|
| **S1** | 配置页：权重/阈值编辑 + 保存生效 + 前置校验 | T5.7 |
| **S2** | 证据「跳原始面板」链接（Prometheus / Grafana / Loki） | T5.6 |
| **S3** | 前端部署形态：静态托管 + `/api` 反代 + SPA 回退 | **缺口 C1** |
| **S4** | 前端自动化测试：vitest 组件测试 + Playwright E2E | **缺口 C2** / ADR-016 |

S1/S2 是开发计划原有任务；S3/S4 是本次核对新发现的缺口，
不加这两个则 DoD 第 3、4 条无法满足，且 M3/M4 的 §4.8 欠账永远还不上。

### 3.2 明确不做（Out-of-Scope，锁定）

| 不做 | 原因 |
|---|---|
| 登录 / 用户体系 / 权限 | 开发计划写明「V1 先单管理员 token，不做用户体系」。S3 若需 token，只做**单值静态 token**（环境变量注入），不做登录页与多用户 |
| 前端国际化（i18n） | V1.0 只做简体中文 |
| 暗色/亮色主题切换 | M3 已定深色控制台（ADR-016），本期不加切换 |
| 移动端适配 | 桌面控制台，运维场景为主 |
| 前端性能优化（代码分割 / 懒加载） | 当前单 chunk 1.09 MB，gzip 342 KB。开发计划 R2 已把「性能」列为非目标；S4 引入测试后若仍超预算，只记录不优化 |
| 后端接口新增 | S1 只消费**已存在**的 `GET/PUT /api/v1/correlation/config`，不新增任何端点 |
| K8s 采集器接真实集群 | M4 偏差 B1 延续，仍默认关闭 |

### 3.3 AGENTS.md 约束在 M5 的映射

| 约束 | M5 的落点 |
|---|---|
| 未做项必须如实标注，不得写成通过 | §八 验收清单每条须有实测值；S4 若延期必须写 NOT DONE |
| 不新增 Go 依赖（`go.mod`） | S1–S3 不动 Go 代码的依赖；S3 若在 Go 内实现静态托管只用标准库 |
| 单文件 ≤ 300 行（项目代码） | 前端页面/组件同样遵守，`IncidentDetail.tsx` 已 410 行，**S1 新增要拆文件而非继续堆** |
| 不加 `nolint`、不加 lint 排除规则 | S4 若引入新 lint 工具，其默认规则集需显式配置并全绿 |

---

## 四、详细设计

### 4.1 S1 配置页（§4.1）

#### 4.1.1 对接契约（已存在，无需后端改动）

`internal/api/correlation_config.go` 已实现：

```
GET  /api/v1/correlation/config → 200 {weights:{time,service,label,dependency,historical},
                                   thresholds:{merge,review}, enabled:bool}
PUT  /api/v1/correlation/config → 200 同结构
                                   400 请求体不成形（非法 JSON / 未知字段）
                                   422 语义非法（权重和≠1 / merge<review）
```

校验规则（`internal/correlation/types.go`，前端表单必须镜像这套规则）：

| 规则 | 取值 | 前端行为 |
|---|---|---|
| 五个权重各 ∈ [0,1] | — | 滑块 + 数字输入，越界即拦 |
| 权重和 ≈ 1 | `weightSumEpsilon = 0.001` | **实时显示当前和**，超出容差时禁用保存并提示 |
| `merge ≥ review` | — | 交叉校验，不满足禁用保存 |
| `enabled` | bool | Switch 开关 |

> **权重和的容差必须在 UI 里如实显示**，不能让用户以为「拖到 1.000 就一定过」。
> 后端容差 0.001，UI 显示三位小数即可对齐。

#### 4.1.2 页面结构

```
/settings
├── 权重区：五个维度各一行（名称 + 说明 + 滑块 + 数字框）
│   维度语义直接取自 V1.0 §5：
│     time       时间邻近
│     service    服务相同
│     label      标签一致
│     dependency 拓扑依赖（来自 config/topology.yaml）
│     historical 历史共现
├── 汇总条：当前权重和（实时）+ 是否可保存
├── 阈值区：merge / review 两个滑块 + merge≥review 交叉提示
├── 总开关：enabled
└── 操作：保存 / 放弃改动
```

**保存语义**：PUT 成功即**无需重启生效**（M2 已实现热更新）。
保存后前端须重新 GET 一次并按返回值刷新界面 —— 不能假设 PUT 的返回就是最终态。

#### 4.1.3 前端新增文件

| 文件 | 职责 | 行数预估 |
|---|---|---|
| `src/api/correlationConfig.ts` | GET/PUT 客户端（含 422 的错误解析） | ≤ 120 |
| `src/pages/Settings.tsx` | 页面主体 | ≤ 280 |
| `src/components/WeightRow.tsx` | 单个维度的滑块+输入+说明 | ≤ 90 |

#### 4.1.4 必须处理的两处细节

1. **422 的错误信息要展示原文**。后端返回的是
   `{"error":"weights.time 必须在 [0,1] 区间，当前为 2"}` —— 这是**给用户看的**文案，
   前端不该自己重写一套提示。直接展示后端 `error` 字段。
2. **422 不是「表单校验失败」**。它在多数情况**不该被前端拦下**（如权重和 0.999
   在 UI 上四舍五入显示为 1.000 但实际和是 0.9994）。因此前端校验是**辅助**，
   后端 422 才是权威 —— 两者都要有，且后端的错误要显示出来。

### 4.2 S2 证据「跳原始面板」（§4.2）

#### 4.2.1 数据来源

`evidence` 行已带足拼 URL 的全部信息（实测契约键：
`collected_at` / `confidence` / `id` / `metadata` / `query` / `source` /
`timestamp` / `type` / `value`）：

| 字段 | 用途 |
|---|---|
| `source` | 决定目标面板：`prometheus` → Prometheus/Grafana，`loki` → Grafana Explore |
| `query` | Prometheus 的 PromQL 或 Loki 的 LogQL（**已含 service 字面量**，采集器写入的） |
| `timestamp` | 决定时间范围 |
| `metadata.window` | 事故窗口，两端都有时间戳 |

#### 4.2.2 URL 拼装（新增配置项）

| 环境变量 | 默认 | 说明 |
|---|---|---|
| `IIP_UI_PROMETHEUS_URL` | `http://localhost:9090` | Prometheus 根地址 |
| `IIP_UI_GRAFANA_URL` | `http://localhost:3000` | Grafana 根地址 |
| `IIP_UI_LOKI_URL` | `http://localhost:3100` | Loki 根地址 |
| `IIP_UI_LOKI_TENANT_ID` | 空 | Loki 多租户时需要 |

> **为什么必须做成配置而不是硬编码**：容器内服务名是 `prometheus:9090`，
> 宿主机裸跑是 `localhost:9090`，生产又是另一套。硬编码任一个都会在其余两个环境里
> 给出点不开的链接 —— 这与 M4 的 compose 漏注入是同一类错误（ADR-019）。
> **默认值必须让「默认部署形态」开箱可用**，否则等于没做。
>
> 默认值取 `localhost` 而不是 compose 服务名，理由：UI 是给**人在浏览器里点**的，
> 浏览器解析的是**宿主机**地址，不是容器网络地址。容器内服务的宿主映射端口
> 已在 compose 里声明（9090/3000/3100），故 `localhost` 对默认部署成立。

#### 4.2.3 三种链接形态

| source | 目标 | URL |
|---|---|---|
| `prometheus` | Prometheus 表达式页 | `{PROM}/graph?g0.expr={urlencoded query}&g0.end={ts}` |
| `loki` | Grafana Explore | `{GRAFANA}/explore?left={"datasource":..,"queries":[{"expr":{query},"range":{"from":..,"to":..}}]}` |
| `k8s` / `git` / 其他 | **不渲染链接** | — |

**必须用 `encodeURIComponent` 整段编码 query**。PromQL 里含 `{`、`"`、`$`，
Loki 的 LogQL 含 `{}` 与 `|`；不编码必然拼出坏链接。
Loki 链接的 `left` 参数是 JSON，**必须整体 URL 编码**。

#### 4.2.4 边界

- `query` 为空 → **不渲染链接**（不可渲染一个打不开的按钮）。空 query 本身是
  契约违例（S3 测试要覆盖），但前端不能因此崩。
- Grafana 未部署 → 链接仍渲染，但 `title` 提示「需自行保证 Grafana 可达」，
  不做可达性探测（前端探测会引入新的失败模式）。

### 4.3 S3 前端部署形态（§4.3）

#### 4.3.1 方案选择

三个候选，逐一权衡：

| 方案 | 优点 | 缺点 | 结论 |
|---|---|---|---|
| A. iip-server 内置静态托管 | 单容器、单端口，UI 与 API 同源，无跨域 | 要改 Go 代码；`/api` 反代逻辑与已有 router 混在一起 | **采纳** |
| B. compose 加 nginx 服务 | 不动 Go 代码；职责清晰 | 多一个容器/镜像；`/api` 与静态两处配置，易漂移 | 备选 |
| C. 独立静态服务 + 前端直连绝对地址 | 前后端彻底解耦 | 要引入环境相关的 API 地址配置（`.env`），部署者必须填对 | 否 |

**选 A**，理由：`API_BASE = '/api/v1'` 是**相对路径**（实测 5 个客户端全部如此），
这意味着前端默认就假设「API 与自己同源」。方案 B/C 都要额外处理跨域或地址注入，
而 A 直接让假设成立。**一个部署形态一个进程，运维心智负担最小。**

> 记忆里的坑正是在这条上：「前端静态托管必须补 `/api` 反代，
> 换纯静态服务器后 proxy 消失 → 打 5173 回 404，**极易误判成后端坏了**」。
> 选 A 就是为了让这个坑从根上不存在。

#### 4.3.2 实现要点（`internal/api` 或新建 `internal/webui`）

| 要点 | 要求 | 理由 |
|---|---|---|
| 挂载开关 | `IIP_WEB_ROOT` 非空才启用静态托管 | 不设时**零行为变化**，裸跑测试与现有环境不受影响 |
| 路由优先级 | `/api/*` 与 `/healthz` **先于**静态兜底 | 否则 API 会被 SPA 回退吞掉，返回 `index.html` 而非 JSON —— 这种失败极难排查 |
| SPA 回退 | 未命中静态文件且非 `/api` → 返回 `index.html` | 直接刷新 `/incidents/12` 必须能打开 |
| 路径逃逸 | 拒绝 `..`，用 `filepath.Clean` 后校验前缀 | **安全红线**：静态托管最容易被打成目录穿越 |
| 缓存 | `/assets/*`（带内容 hash）→ `Cache-Control: public, max-age=31536000, immutable`；`index.html` → `no-cache` | 改了前端没生效是最常见的「假修复」 |
| 类型 | 按扩展名给 Content-Type | distroless 里没有 `mime` 包的标准保证，显式表更可控 |
| 嵌入方式 | **不 go:embed**，改为从 `IIP_WEB_ROOT` 读文件 | dist 有 1.09 MB，embed 进二进制会让 server 镜像无谓变大，且前端迭代不必重编 Go |

> **不 go:embed 是有意的**：M3 的决策是前端用 vite dev 独立迭代。
> 若 embed 进去，改一行 CSS 就要重编 Go 镜像，与该决策相悖。

#### 4.3.3 compose 改动

- `iip-server` 增加 volume：`- ../web/dist:/usr/share/iip/web:ro`
- 环境变量：`IIP_WEB_ROOT=/usr/share/iip/web`
- 端口不变（8080），UI 与 API 同源，无需 CORS
- `depends_on` 不变

#### 4.3.4 与 Dockerfile 的关系

**不改 Dockerfile**。`web/dist` 走**挂载**而非烧进镜像，理由：
① 镜像从 distroless 起，没有 node，构建前端会把镜像从 14 MB 推到几百 MB；
② 前端迭代与 Go 镜像解耦，可独立发版；
③ 与 `config/` 的供给方式一致（都走挂载）。

代价：**首次部署必须先跑一次 `pnpm install --frozen-lockfile && pnpm run build`**。
须写进 `deploy/README.md` 与 M5 交付说明 —— 否则用户 `compose up` 后看到 404，
会以为产品坏了。

### 4.4 S4 前端自动化测试（§4.4）

#### 4.4.1 选型

| 工具 | 用途 | 理由 |
|---|---|---|
| **vitest** + `@testing-library/react` + `jsdom` | 组件与逻辑单测 | 与 vite 同构，不引第二套构建；配置最少 |
| **@playwright/test** | E2E | 开发计划 T5.8 明确指定；能验「真实浏览器里点完处置后回读 timeline」 |

**不引** jest / cypress / enzyme —— 与 vite 重复或已过时。

#### 4.4.2 组件测试的最小集（每条对应一个真实风险）

| 用例 | 验什么 | 真实缺陷来源 |
|---|---|---|
| `EvidenceList` 渲染 0 条证据 | 显示空态而非空白页 | 「缺数据被序列化成零值」的 UI 版本 |
| `EvidenceList` 渲染 1 条 `query` 为空 | **不渲染链接**、页面不崩 | §4.2.4 的边界 |
| `EvidenceList` 渲染 loki 证据 | 链接里 query 被正确 URL 编码 | §4.2.3 的编码问题 |
| `Settings` 权重和为 0.8 | 保存按钮禁用 + 显示和 | §4.1.1 的规则 |
| `Settings` merge < review | 交叉校验生效 | 同上 |
| `IncidentDetail` 点「标记缓解」 | 调 PUT transition 且带正确参数 | 处置链路 |
| `IncidentDetail` 后端返 422 | 展示后端 `error` 原文而非自造文案 | §4.1.4 第 1 条 |

#### 4.4.3 E2E 的最小集

```
E2E-1  打开 /            → 重定向到 /incidents
E2E-2  列表有事件        → 点进去看到详情三要素（时间线/证据/关联告警）
E2E-3  点「认领」        → 状态变 investigating，时间线**新增一条**（回读验证）
E2E-4  /settings 改权重  → 保存成功；**通过 API 回读**确认真的落库
E2E-5  无 API 时的表现   → 列表页显示错误态而非白屏
```

**E2E-3 必须回读 timeline**，不能只断言「按钮变色了」。
**E2E-4 必须用 API 回读**，不能只断言「提示保存成功」——
这两条都是记忆里「同源自证」陷阱的变体：前端说成功，不等于数据落库。

#### 4.4.4 环境依赖

Playwright 需要浏览器二进制。CI/离线环境的处置：
- `npx playwright install chromium` 需联网。**本机无网络时用已有 Chrome**
  （记忆里 `s3_frontend_check.py` 用的是无头 Chrome，可复用其定位方式）。
- 故 Playwright 配置里 `channel: 'chrome'` 作为回退（用系统 Chrome，不下载）。

---

## 五、迁移变更清单（AGENTS.md 约束 4）

**无。** M5 不改数据库：

- S1 只读写已存在的 `correlation_rules.rule_config`（单行 JSON，M2 建）。
- S2/S3/S4 不碰数据库。

**明确不新增迁移**，避免「有迁移必先改手册」的连锁。

---

## 六、任务拆解（5 天）

> 开发计划写的是 10 天，但其中 5 个任务已在 M3 完成（§二）。
> 剩下 4 项按实际工作量重估。**若耗时超 2 周，启用 §6 的降级预案。**

> **包管理器口径（2026-10-04 实测修正）**：本仓库前端用 **pnpm 9.15.9**
> （`web/pnpm-lock.yaml` 存在、`package.json` 的 `packageManager` 锁定、
> CI 用 `pnpm install --frozen-lockfile`）。本文初稿写的 `pnpm test` / `npm run`
> **是错的**，一律改为 `pnpm test` / `pnpm run`。用 npm 会在无
> `package-lock.json` 的情况下另建一棵依赖树，与 CI 装的不是同一套。

| 批次 | 内容 | 预估 | 依赖 | 状态 |
|---|---|---|---|---|
| **F1** | 前端测试设施底座：装 vitest + Testing Library + jsdom，配置 `pnpm test` | 0.5d | — | ✅ 完成（2026-10-04） |
| **F2** | 静态托管 + SPA 回退 + `/api` 优先级（Go）+ compose 挂载 + `deploy/README.md` 更新 | 1d | — | 待开工 |
| **F3** | S2 直跳链接：`src/api/externalLinks.ts` + `EvidenceList` 集成 + 配置项注入 | 0.5d | F2（需知道 UI 从哪访问） | 待开工 |
| **F4** | S1 配置页：`correlationConfig.ts` + `Settings.tsx` + `WeightRow.tsx` + 路由/导航挂入口 | 1d | F1 | 待开工 |
| **F5** | 组件测试 7 条（§4.4.2） | 0.5d | F1/F3/F4 | 待开工 |
| **F6** | Playwright E2E 5 条（§4.4.3）+ `pnpm run test:e2e` | 1d | F2 | 待开工 |
| **F7** | 四条既有验证链路回归 + 门禁 | 0.5d | 全部 | 待开工 |

### F1 实测记录（2026-10-04）

超出原「只跑通类型」的要求，先落了**一条有实证价值的组件测试**（`EvidenceList.test.tsx`，
11 条用例，全过，7.9 s）。理由：底座若只验证「能跑」，等于验证了 vitest 自己；
真正要验证的是**本仓库组件在 jsdom 下能测**，而这一点只有真写一条用例才知道。

| 项 | 实测值 |
|---|---|
| 新增依赖 | `vitest@2.1.9`、`jsdom@25.0.1`、`@testing-library/react@16.1.0`、`@testing-library/jest-dom@6.6.3`、`@testing-library/user-event@14.5.2`、`@vitest/coverage-v8@2.1.9` |
| scripts | `test` / `test:watch` / `test:coverage` |
| 配置文件 | `web/vitest.config.ts`（**独立于 `vite.config.ts`**，理由见文件头注释） |
| setup 桩 | `matchMedia`、`ResizeObserver`（jsdom 缺，antd 依赖）；**刻意不桩** `scrollWidth` |
| 门禁验证 | `pnpm test` rc=0（11/11）、`pnpm tsc` rc=0、`pnpm build` rc=0 且产物与 F1 前**逐字节一致**（3080 模块、同 hash） |
| 覆盖率 | 仅 `EvidenceList.tsx` 44.21%，其余 0 —— 符合 F1 定位（不追覆盖率数字） |

**F1 顺带修掉的两处仓库卫生问题**（不属原任务，但属 F1 引入后的直接后果）：

1. `web/tsconfig.node.tsbuildinfo` **曾被误入库**。`web/.gitignore` 里
   早就有 `*.tsbuildinfo` 规则（还带注释说明），但 **.gitignore 对已入库文件无效** ——
   这是「规则写了却没生效」的典型。已用 `git rm --cached` 移出（**保留本地文件**），
   关闭 M3 报告 §10.3 登记的 OPEN 项。
2. `test:coverage` 脚本**初版存在但跑不了**：vitest 2.x 的 `--coverage` 需要
   `@vitest/coverage-v8`，未装时报 `MISSING DEPENDENCY`。已装并实测 rc=0。
   → 判据：**新增 script 必须真跑一次**，不能只看 `package.json` 写对了。

**`.gitignore` 分层纪律**：`*.tsbuildinfo` 规则**只在 `web/.gitignore`**，
不往根 `.gitignore` 加 —— 根文件只放全仓通用规则（`node_modules/`、
`coverage.out`），前端特有产物（`coverage/`、`*.tsbuildinfo`）归前端文件。

### 降级预案（超期 2 周时启用）

按开发计划 R2 的既定预案，优先级顺序削减：

1. **先砍 F6 的 E2E-5**（错误态），保留 E2E-1~4。
2. **再砍 F5 到 3 条**（只留 `query` 编码、空 query 不崩、保存按钮禁用）。
3. **再砍 S2**（T5.6，0.5d，最不影响处置闭环）→ 降级为「证据卡展示 query 原文 +
   复制按钮」（M4 已有复制）。
4. **F1/F2/F4 不砍** —— 砍掉等于 M5 白做（DoD 第 1、3 条全废）。
5. **F7 永不砍** —— 砍掉会把「不回归」这条 DoD 也丢掉。

**F2 与 F4 绝不可砍**的硬理由：DoD 第 3 条（部署形态）与第 1 条（配置页）
是 M5 存在的两个主要理由，砍掉任一则 M5 无交付物。

---

## 七、测试策略

### 7.1 分层

| 层 | 工具 | 数量目标 | 跑法 |
|---|---|---|---|
| Go 单测与真库 | `go test` | 不新增（M5 不改 Go 逻辑） | `-p 1` 串行 |
| 前端组件 | vitest | **≥7 条**（§4.4.2 全覆盖） | `pnpm test` |
| 前端 E2E | Playwright | **5 条**（§4.4.3） | `pnpm run test:e2e` |
| 既有四链路 | 见 §0 | 不变 | 见 README |

### 7.2 反向验证（防「假绿」）

本手册最该防的失败模式，逐条给出反向手段：

| 风险 | 反向验证 |
|---|---|
| 前端测试全部 pass 但测的是**替身** | 每个组件用例必须 mock `fetch` 并**断言请求 URL 与参数**；只断言「渲染没崩」等于没测 |
| E2E 只断言「点击后 UI 变样」 | E2E-3/4 必须**用 API 回读**确认数据落库（§4.4.3） |
| 静态托管把 `/api` 吞掉 | 组件/E2E 各有一条用例专门打 `/api/v1/incidents` 并断言返回 **JSON 而非 HTML** |
| SPA 回退没做，深链接刷新 404 | E2E 直接访问 `/incidents/12`（不经前端路由跳转） |
| 「页面能打开」冒充「部署形态可用」 | compose 起真容器后用**无头浏览器**打 8080 端口截图，不看 `dist` 文件存在与否 |
| 权重和 0.999 被 UI 放过又被后端拒 | 组件测试必须有一条**故意用 0.9995** 的用例 |
| 改前端弄坏既有链路 | F7 强制跑全四条链路，任一红则 M5 不算完成 |

### 7.3 门禁

M5 的门禁 = 既有门禁 + 前端门禁：

```
go build ./... / go vet ./... / gofmt -s / golangci-lint run   （rc=0）
go test -p 1 ./...                                             （SKIP=0）
pnpm test                                                       （rc=0）
pnpm run tsc && pnpm run build                                    （rc=0）
pnpm run test:e2e                                                （rc=0）
python scripts/verify-func.py                                   （rc=0）
python scripts/verify-images.py                                 （0 失败）
```

---

## 八、验收清单（全过才算 M5 完成）

| # | 判据 | 手段 |
|---|------|------|
| 1 | 配置页能读出当前权重/阈值，改动保存后**免重启生效** | `Settings` 页面 + **API 回读确认** |
| 2 | 非法输入在提交前被拦，且错误信息指明字段 | 组件测试 7 条中的 3 条 + 手工 |
| 3 | 每条 metric/log 证据有可点击的原始面板链接，query 被正确编码 | 组件测试 + 手工点击一次真实链接 |
| 4 | `compose --profile app up -d` 后浏览器打开一个地址即可用 UI | 容器链路加断言 + 无头浏览器截图 |
| 5 | 静态托管**不吞 `/api`** | 组件 + E2E 各一条，断言返回 JSON |
| 6 | 深链接 `/incidents/{id}` 直接刷新可打开（SPA 回退） | E2E 直访 |
| 7 | 目录穿越（`../../etc/passwd`）被拒 | 单测（Go 侧） |
| 8 | 处置操作后**回读 timeline 确认落库** | E2E-3 |
| 9 | 前端组件测试 ≥7 条且 rc=0 | `pnpm test` |
| 10 | E2E 5 条且 rc=0 | `pnpm run test:e2e` |
| 11 | 既有四条验证链路 0 失败 | 全跑 |
| 12 | `tsc -b` + `vite build` rc=0 | `pnpm run build` |
| 13 | 门禁：Go 四项全 rc=0、真库 SKIP=0、无 `nolint` | 静态门禁 + 真库门禁 |

**第 4 条是本里程碑新增的判据**（此前四个里程碑都没有「部署形态可用」这一项）。
它是开发计划验收标准「用户从 `docker compose up` 到在 UI 处置完一个 incident
≤ 30 分钟」的前半段；后半段是**人工计时**，须在交付时记录实测用时。

---

### F1 期间新增的 OPEN 项

| 编号 | 内容 | 处置 |
|---|---|---|
| OPEN-M5-4 | `vitest`/`jsdom`/Testing Library 为**新增 devDependency**，需在 CI 加 `pnpm test` job 才算进门禁；仓库无 remote，该 job 同样只能本地等价复现 | F7 处理。未加之前 §4.8 仍记 NOT DONE |
| OPEN-M5-5 | 组件覆盖率仅 `EvidenceList.tsx` 44%，其余 5 个页面/组件为 0 | **不追数字**。F5 补测试时自然上升；把覆盖率设成门禁阈值是错的（会诱导为凑数字写测试） |

---

## 九、端到端验证脚本

```bash
# 0) 前置：先构建前端产物（S3 是挂载不是 build，故必须手动构建一次）
cd web && pnpm install --frozen-lockfile && pnpm run build && cd ..

# 1) 静态与真库门禁（必须 -p 1，见 M4 教训）
python D:\_iip_setup\m4_final_gate.py

# 2) 容器链路（含新增的部署形态断言）
python scripts/verify-images.py

# 3) 前端门禁
cd web
pnpm test               # 组件测试，期望 rc=0 且 7 条全过
pnpm run tsc             # 类型检查
pnpm run build           # 产物构建
pnpm run test:e2e        # E2E，期望 5 条全过
cd ..

# 4) 手工体验（要记录用时，对应判据 4 的「≤30 分钟」）
docker compose -f deploy/docker-compose.yml --profile app up -d
# 浏览器打开 http://127.0.0.1:8080
# 计时：从打开页面到处置完一个 incident

# 5) 清理
docker compose -f deploy/docker-compose.yml --profile app down
```

---

## 十、风险与开放项

| 风险 | 缓解 |
|------|------|
| Playwright 需下载浏览器，离线环境装不上 | 配置 `channel: 'chrome'` 回退用系统 Chrome；E2E 可在有网环境跑一次并录屏存档 |
| 静态托管引入路径穿越漏洞 | Go 侧单测专测 `..`（判据 7）；实现必须 `filepath.Clean` + 前缀校验 |
| SPA 回退吞掉 `/api`，症状是「接口全 404」 | 路由优先级写死 `/api` 与 `/healthz` 在前；组件 + E2E 各一条断言 |
| `dist` 未构建导致 compose 起不来 UI | 写进 `deploy/README.md`；判据 4 用**无头浏览器打真端口**而非看文件存在 |
| 前端单 chunk 1.09 MB 未做分割 | 记录不优化（§3.2）；V1.0 上线后若首屏慢再开 V1.1 |
| 引入测试设施后 `tsc` 变严（测试文件也纳入类型检查） | F1 单独配置 `tsconfig`，测试类型与生产代码分开但都受检 |
| 单人开发前端拖期（开发计划 R2 已预警） | §六 降级预案，F2/F4 不可砍 |

**OPEN-M5-1**：单管理员 token 如何注入（§3.2 决定只做环境变量注入的静态值），
**是否需要在 M5 内实现，待禹确认**。若不做，S3 的部署形态即为**无鉴权**——
须在 `deploy/README.md` 显著位置写明「V1.0 仅供内网/演示环境」。

**OPEN-M5-2**：`IIP_UI_*` 四个新环境变量的**默认值是否适合默认部署形态**，
须在 F3 完成时用真实 compose 验证一次点击可达，不能只验 URL 拼装正确。

**OPEN-M5-3**：ADR-016 裁定「前端自动化测试延到 M5」——
若 S4 因故延期，M3/M4 的 §4.8 类条款将**永久 NOT DONE**。
届时须在 `docs/decisions.md` 登记新的 OPEN 项，并**不得**把 §4.8 写成通过。

---

## 十一、变更记录

| 日期 | 变更 | 说明 |
|------|------|------|
| 2026-10-04 | 初稿 | 基于 M4 交接实测 + 开发计划 §9 起草。范围裁剪为 S1–S4 四项（5 个开发计划任务已在 M3 完成，另发现 2 个此前无人负责的缺口 C1/C2）。待禹确认 |
