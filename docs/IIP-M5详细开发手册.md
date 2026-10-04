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
| **S5** | 前端明暗主题切换（暗色为默认，亮色为可选） | **禹于 2026-10-04 追加**，见 §4.5 与 ADR-021 |

S1/S2 是开发计划原有任务；S3/S4 是本次核对新发现的缺口，
不加这两个则 DoD 第 3、4 条无法满足，且 M3/M4 的 §4.8 欠账永远还不上。

S5 是**禹在 M5 期间追加的需求**，初稿 §3.2 曾把它列为 Out-of-Scope
（理由是「M3 已定深色控制台，本期不加切换」）。该条已从 §3.2 移入本表，
偏离理由与代价记在 **ADR-021**。

### 3.2 明确不做（Out-of-Scope，锁定）

| 不做 | 原因 |
|---|---|
| 登录 / 用户体系 / 权限 | 开发计划写明「V1 先单管理员 token，不做用户体系」。S3 若需 token，只做**单值静态 token**（环境变量注入），不做登录页与多用户 |
| 前端国际化（i18n） | V1.0 只做简体中文 |
| ~~暗色/亮色主题切换~~ | **已移出 Out-of-Scope**：禹于 2026-10-04 追加为 S5（§4.5 / ADR-021）。<br>原理由「M3 已定深色控制台」是**默认基调**的裁定，不是「只有深色」的禁令 —— 实测 ADR-016 原文为「深色专业控制台」（选型 a），并未排除亮色 |
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

``
GET  /api/v1/correlation/config → 200 {weights:{time,service,label,dependency,historical},
                                   thresholds:{merge,review}, enabled:bool}
PUT  /api/v1/correlation/config → 200 同结构
                                   400 请求体不成形（非法 JSON / 未知字段）
                                   422 语义非法（权重和≠1 / merge<review）
``

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

``
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
``

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

##### 4.2.2.1 这些变量怎么到前端（本轮实现时补，原初稿遗漏）

前端是**构建产物**，`IIP_UI_*` 是**后端运行时环境变量**，两者之间原本没有任何通道
（实测 `web/src` 下 `import.meta.env` / `VITE_*` / `window.__*` **零命中**）。
四个候选方案：

| 方案 | 优点 | 缺点 | 结论 |
|---|---|---|---|
| A. vite `VITE_*` 构建时注入 | 零后端改动 | **改地址必须重新构建前端**；且构建机上的值会被烤进产物，与「运行时配置」矛盾 | 否 |
| B. 新增 `GET /api/v1/ui-config` | 契约清晰 | **违反 §3.2「不新增后端端点」** | 否 |
| C. 后端启动时生成 `config.js`，前端动态加载 | 改配置无需重建前端 | 多一次异步加载；`config.js` 404 时要有兜底 | 备选 |
| **D. 后端在 `index.html` 里注入 `window.__IIP_UI__`** | 无新端点、无额外请求、**改配置无需重建前端** | 只在启用静态托管时生效 | **采纳** |

**采纳 D**，理由与实现约束：

- 注入点在 `internal/webui`，**只在 `IIP_WEB_ROOT` 非空时执行**；不启用静态托管时
  无任何行为变化（与 S3 的开关语义一致）。
- 注入内容是**四段地址**，不含任何密钥，可安全内联进 HTML。
  刻意**不放**采集器的 token / 租户密钥 —— 那属于采集器配置，不属于「给人点的链接」。
- 注入失败（无 `index.html`、占位符不存在）**不阻断请求**：降级为
  前端 `DEFAULT_UI_ENDPOINTS`，即 localhost 默认值。理由是「地址没配对」不该让
  **整个 UI 打不开** —— 那种故障的影响面远大于链接点不开。
- 前端读取封装在 `web/src/api/uiConfig.ts`，**必须容忍 `window.__IIP_UI__` 不存在**
  （dev 模式无后端注入、单元测试无 window 注入）。

> **这是「先改手册再改代码」的又一例**：初稿 §4.2.2 只列了环境变量，
> 没写它们怎么抵达前端。实现时才发现这条通道不存在。
> 若不加这一节，代码就会「凭空依赖一个不存在的机制」。

#### 4.2.3 链接形态（实现后为四种，初稿写三种）

`buildExternalLinks` 返回**数组**而非单个：日志类证据值得同时给 Grafana（看图、下钻）
与 Loki 原生（确认数据是否存在）—— 两个面板回答的是不同问题，只给一个的话
运维得自己猜另一个在哪。

| source | 目标 | URL |
|---|---|---|
| `prometheus` | Prometheus 表达式页 | `{PROM}/graph?g0.expr={query}&g0.range_input=1&g0.start_input={from}&g0.end_input={to}` |
| `loki` | Grafana Explore | `{GRAFANA}/explore?left={urlencoded JSON}` |
| `loki` | Loki 原生 query_range | `{LOKI}/loki/api/v1/query_range?query={query}&start={from}&end={to}&direction=backward` |
| `k8s` / 其他 | **不渲染链接** | — |

**编码一律走 `URLSearchParams`，不手工拼。** PromQL 含 `{`、`"`、`$`，
LogQL 含 `{}` 与 `|`；Loki 链接的 `left` 是 JSON，内层还含 `"`，必须整体编码。
用 `URLSearchParams.set()` 的额外好处是它自己转义空格为 `+`（Prometheus 接受），
而手工 `encodeURIComponent` 会留 `%20`（也接受，但两者混用容易写出双编码的 bug）。

**时间范围的两级回退**（先 window 再 timestamp，缺 window 时 Loki 走 ±1h 窗口）：

| 字段 | 何时用 | 回退到 |
|---|---|---|
| `metadata.window` | 有且**恰好两段**且两端都能解析 | `timestamp`（Loki 再扩 ±1h） |
| `timestamp` | 上述任一条件不满足 | — |

`metadata` 是 `Record<string,string>`，**不保证有 `window`**：实测三个采集器
（prometheus / loki / k8s）都写，格式统一为 `RFC3339/RFC3339`（`formatWindow`），
但 M4 之前的历史数据没有，将来新采集器也未必写。
→ **解析失败必须退回 `timestamp` 而不是拼坏链接。** `window` 出现三段（格式变了）
时按「不可用」处理，错解析比不解析更危险。

#### 4.2.4 边界

- `query` 为空 → **不渲染链接**（不可渲染一个打不开的按钮）。空 query 本身是
  契约违例（S3 测试要覆盖），但前端不能因此崩。
- `query` 前后有空白（`"  {...}  "`）→ **先 trim 再判空**。SQL/采集器侧拼接很容易
  带空白，而 `{ }` 这种空查询模板在 Prometheus 里是**语法错误**，
  会跳到一个报错的页面 —— 那比不渲染更让人困惑。
- Grafana 未部署 → 链接仍渲染，但 `title` 提示「需自行保证 Grafana 可达」，
  不做可达性探测（前端探测会引入新的失败模式）。
- **Grafana 数据源 UID**：不同安装的 Loki 数据源 uid 不一致（常见是 `loki`，
  也可能是别的）。传错时 Grafana **回退到默认数据源而不报错**，
  表现为「打开了但查的是别的源」。故默认用 `Loki`，并在 `title` 里
  显式提示「数据源 UID 与本环境可能不同」；多租户时用 `Loki-{tenantId}`。
- 根地址末尾的斜杠必须先 trim：`http://h:3000/` + `/explore` → `//explore`，
  Grafana 会 404（Prometheus 多数能容忍，故这类 bug 往往只在 Grafana 上暴露）。

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

``
E2E-1  打开 /            → 重定向到 /incidents
E2E-2  列表有事件        → 点进去看到详情三要素（时间线/证据/关联告警）
E2E-3  点「认领」        → 状态变 investigating，时间线**新增一条**（回读验证）
E2E-4  /settings 改权重  → 保存成功；**通过 API 回读**确认真的落库
E2E-5  无 API 时的表现   → 列表页显示错误态而非白屏
``

**E2E-3 必须回读 timeline**，不能只断言「按钮变色了」。
**E2E-4 必须用 API 回读**，不能只断言「提示保存成功」——
这两条都是记忆里「同源自证」陷阱的变体：前端说成功，不等于数据落库。

#### 4.4.4 环境依赖

Playwright 需要浏览器二进制。CI/离线环境的处置：
- `npx playwright install chromium` 需联网。**本机无网络时用已有 Chrome**
  （记忆里 `s3_frontend_check.py` 用的是无头 Chrome，可复用其定位方式）。
- 故 Playwright 配置里 `channel: 'chrome'` 作为回退（用系统 Chrome，不下载）。

---

### 4.5 S5 明暗主题切换（禹于 2026-10-04 追加，ADR-021）

四项裁定（禹定）：**GitHub Light 同源配色** / **默认暗色 + localStorage 记忆** /
**入口在 Header 右侧图标按钮** / **改手册并记 ADR-021**。

#### 4.5.1 核心约束：`palette` 17 处内联引用不会跟着 antd token 走

实测（2026-10-04）：`web/src` 下 `palette.*` 共 **17 处**引用，跨 **5 个文件**：

| 文件 | 处数 | 用到的键 |
|---|---|---|
| `components/Layout.tsx` | 6 | `surface.border` ×2、`semantic.info`、`semantic.warning`、`text.primary`、`text.tertiary` |
| `pages/Settings.tsx` | 6 | `text.primary`、`text.secondary`、`text.tertiary`、`semantic.critical` |
| `components/WeightRow.tsx` | 2 | `text.primary`、`text.tertiary` |
| `pages/Reviews.tsx` | 2 | `text.tertiary` |
| `pages/Incidents.tsx` | 1 | `text.tertiary` |

这些是**内联 style 的字面颜色**，`ConfigProvider` 换 `theme` **不会**让它们变。
若只改 `theme.ts`，切到亮色后会出现「卡片变白、但文字仍是暗色的 `#e6edf3`」
—— 即**亮色主题下白底白字**。

**有利的事实**：17 处引用的全是 `text.*` / `surface.*` / `semantic.*`
这类**语义键**，**零处硬编码十六进制值**（实测）。所以正确解法不是
「把 17 处改成 `theme.useToken()`」（那是 17 次改动、且每处都要包 hook），
而是**让 `palette` 变成按当前主题解析的动态对象**。

#### 4.5.2 方案对照

| 方案 | 做法 | 17 处调用点 | 缺点 |
|---|---|---|---|
| A. 逐处改 `useToken()` | 每处调 `theme.useToken()` 取色 | **要改 17 处** | 改动面大；且 `useToken` 只能在组件内调，模块级常量处无法使用 |
| **B. 动态 palette（采纳）** | 主题变更时更新一个**可变导出**，`palette.*` 读取时取当前值 | **零改动** | 「导出可变对象」是反模式；须用**受控的读取函数**而非裸对象 |
| C. CSS 变量 | 主题切换时改 `:root` 上的变量，内联 style 改用 `var(--x)` | 17 处 | 内联 style 里写 `var()` 可行，但同样要改 17 处；且 antd token 侧仍需单独配 |

**采纳 B**，但**不是**「可变的裸对象」——那样 React 不会重渲染，
颜色会停在旧值。实现要求见 §4.5.3。

#### 4.5.3 实现要点

1. **配色表按主题各存一份**（`themes.dark` / `themes.light`），
   `iipLightTheme` 与 `iipDarkTheme` 同为 `ThemeConfig`。
2. **亮色语义色按白底重新校准**，不能直接用暗底那一档 ——
   暗底上 `#f85149` 对比度约 4.0:1（对白底会**不足 3:1**）。
   亮色需用同色相的深一档。
3. **主题状态提升到 `main.tsx`**：`ConfigProvider` 已在顶层，
   在其外层加 `ThemeModeProvider`，避免在 `Layout` 里再开一个 `ConfigProvider`。
4. **`palette` 改为 `getPalette()` 或 `PaletteContext`** ——
   17 处调用点若改成调函数，就是 17 处改动，违背 §4.5.2 的采纳理由。
   **正确解法：保留 `palette` 导出名，但让它成为「随主题重建的对象」**，
   并让引用方通过 `usePalette()` 拿到**属于自己那次渲染**的那一份。
5. **首屏不闪白（FOUC）**：`localStorage` 在 `main.tsx` 顶层同步读取
   （`useState` 初始值），**不能放在 `useEffect` 里** ——
   那会让首帧先用暗色渲染再切亮色，白闪一下。
6. **`prefers-color-scheme` 不作为默认**（禹裁定「默认暗色」）：
   值守场景多数在夜间，跟随系统会让部分用户白天看到不适配的亮色。

#### 4.5.4 必须验的四条（否则等于没做）

| 判据 | 为什么必须 |
|---|---|
| 切到亮色后**内联文字色**真的变了 | 17 处不跟 token 走，这是本需求唯一的真风险 |
| 刷新后主题**保持** | 验 `localStorage` 真的写了 |
| 首次访问**默认暗色** | ADR-016 的默认基调未被推翻 |
| 亮色下 `critical/warning/success` **对比度 ≥ 4.5:1** | 直接抄暗底色值必然不达标 |

第 1 条与第 4 条**只能在真浏览器 / 截图里验**，jsdom 读不到计算后的颜色。
第 2、3 条单测可覆盖。

---

### 4.6 S1 补测：处置链路的失败语义（F5，2026-10-04 实测后补写）

§4.4.2 的 7 条里 4 条已被 F2/F4 的用例覆盖，F5 只补剩下 3 条。
补测过程中**抓到一处真实产品缺陷**，故本节规格为「先定规格、后改代码」。

#### 4.6.1 缺陷：流转失败后处置按钮被锁 3 秒

**现象**（vitest 定量测量，非目测）：

| 路径 | `ant-btn-loading` 解除耗时 |
|---|---|
| 流转成功 | **156 ms** |
| **流转失败（后端 422）** | **3141 ms** |

**机制**（已归因，非竞态猜测）：

1. `onError` 调 `message.error(e.message)`；
2. antd 的 message 走 `rc-notification`，用 `ReactDOM.render` 挂到**独立容器**，
   默认 `duration = 3s`；
3. 该容器挂载与卸载各触发一次跨树 re-render，把 `IncidentDetail` 子树重渲染一遍；
4. 这两次重渲染与 react-query v5 解除 `isPending` 的状态更新**同批提交**，
   `loading` 属性的解除被推迟到 message 卸载那一刻。

实测把两件事的时间戳对齐过：message 全部卸载耗时 **3160 ms**，
`ant-btn-loading` 恰在**同一刻**消失。→ 结论是
「**失败提示的展示时长直接成了按钮的解锁延迟**」，不是竞态 bug
（释放后功能完全正常，二次点击 POST 数 1→2）。

**为什么必须修**：`IncidentDetail` 是唯一「点一下就改变线上状态」的页面。
用户误点了目标状态（例如想「已缓解」却点了「已解决」）拿到 422 后，
**所有处置按钮灰掉 3 秒**。值班场景下会被读成「系统卡了」，进而重复点击或放弃处置。

**修法**：把「按钮 loading 态」与「message 提示时长」**解耦** ——
loading 不再读 mutation 的 `isPending`，改用组件内**只被 mutation 生命周期驱动**的本地状态。

**验收判据（缺这条就等于没修）**：
失败后 **1 秒内** `ant-btn-loading` 必须已解除，且此时点另一个目标能发出第二个 POST。
**只验「最终会恢复」会假通过** —— 实测它 3.1 秒也会恢复。

#### 4.6.2 三条补测各自的判据

| 用例 | 判据 | 真实缺陷来源 |
|---|---|---|
| loki query 编码 | **两个 loki 链接承载方式不同**：Grafana 是 `/explore?left=<JSON 串>`（query 藏 `queries[0].expr`），Loki 原生是 `?query=<LogQL>`。断言「解码后能 `JSON.parse` 且 `expr` 还原为原文」 | §4.2.3。既有 F3 用例只验「两个链接都在」，**没验编码**，是 §4.4.2 第 3 条的缺口 |
| 处置链路 | POST `/transition` 的 body 带正确 `to` 与**非空** `actor` | `actor` 漏传不报错，只让 timeline 查不到是谁处置的 —— 审计线索静默断掉 |
| 422 原文 | 界面上出现**后端那句原文**（含服务端具体状态名与建议路径），且**不出现**「已流转到…」 | §4.1.4 第 1 条。失败时谎报成功比报错文案难看严重得多 |

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
| **F2** | 静态托管 + SPA 回退 + `/api` 优先级（Go）+ compose 挂载 + `deploy/README.md` 更新 | 1d | — | ✅ 完成（2026-10-04） |
| **F3** | S2 直跳链接：`src/api/externalLinks.ts` + `EvidenceList` 集成 + 配置项注入 | 0.5d | F2（需知道 UI 从哪访问） | ✅ 完成（2026-10-04，提交 `2042aa1`） |
| **F4** | S1 配置页：`correlationConfig.ts` + `Settings.tsx` + `WeightRow.tsx` + 路由/导航挂入口 | 1d | F1 | ✅ 完成（2026-10-04，零后端改动） |
| **F5** | 组件测试补齐（§4.4.2 剩余 3 条）+ S5 明暗主题切换（ADR-021） | 1d | F1/F3/F4 | **已交付**（补测抓到 1 处产品缺陷并修复，见 §4.6） |
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

``
go build ./... / go vet ./... / gofmt -s / golangci-lint run   （rc=0）
go test -p 1 ./...                                             （SKIP=0）
pnpm test                                                       （rc=0）
pnpm run tsc && pnpm run build                                    （rc=0）
pnpm run test:e2e                                                （rc=0）
python scripts/verify-func.py                                   （rc=0）
python scripts/verify-images.py                                 （0 失败）
``

---

## 八、验收清单（全过才算 M5 完成）

| # | 判据 | 手段 | 状态 |
|---|------|------|------|
| 1 | 配置页能读出当前权重/阈值，改动保存后**免重启生效** | `Settings` 页面 + **API 回读确认** | F4 待验 |
| 2 | 非法输入在提交前被拦，且错误信息指明字段 | 组件测试 7 条中的 3 条 + 手工 | F4/F5 待验 |
| 3 | 每条 metric/log 证据有可点击的原始面板链接，query 被正确编码 | 组件测试 + 手工点击一次真实链接 | F3 待验 |
| 4 | `compose --profile app up -d` 后浏览器打开一个地址即可用 UI | 容器链路加断言 + 无头浏览器截图 | F2 部分（宿主全栈已验，容器链路待 F7） |
| 5 | 静态托管**不吞 `/api`** | 组件 + E2E 各一条，断言返回 JSON| ✅ F2 实测（真进程 HTTP） |
| 6 | 深链接 `/incidents/{id}` 直接刷新可打开（SPA 回退） | E2E 直访| ✅ F2 实测（真进程 HTTP） |
| 7 | 目录穿越（`../../etc/passwd`）被拒 | 单测（Go 侧）| ✅ F2 实测（12 组穿越 + 真进程） |
| 8 | 处置操作后**回读 timeline 确认落库** | E2E-3 | F6 待验 |
| 9 | 前端组件测试 ≥7 条且 rc=0 | `pnpm test` | 部分（F1 已 11 条，需 F5 补到 ≥7 条覆盖面） |
| 10 | E2E 5 条且 rc=0 | `pnpm run test:e2e` | F6 待验 |
| 11 | 既有四条验证链路 0 失败 | 全跑 | F7 待验 |
| 12 | `tsc -b` + `vite build` rc=0 | `pnpm run build` | ✅ F2 实测（pnpm run build rc=0） |
| 13 | 门禁：Go 四项全 rc=0、真库 SKIP=0、无 `nolint` | 静态门禁 + 真库门禁 | 部分（静态门禁 ✅，真库门禁待 F7） |

**「状态」列的读法**：只有 `✅` 才是**真进程实测通过**。
「部分」= 该判据已被本次交付顺带验证了一部分，剩下的部分要等后续批次 ——
**不算通过**。空白/「待验」= 尚未验证。
按 AGENTS.md 约束（未做项必须如实标注，不得写成通过），这里不做任何美化。

**第 4 条是本里程碑新增的判据**（此前四个里程碑都没有「部署形态可用」这一项）。
它是开发计划验收标准「用户从 `docker compose up` 到在 UI 处置完一个 incident
≤ 30 分钟」的前半段；后半段是**人工计时**，须在交付时记录实测用时。

---

### F2 实测记录（2026-10-04）

实现落在**新包 `internal/webui`**（4 个源文件 + 3 个测试文件），挂载点用
`r.NotFound(...)` 而非 `r.Handle("/*")`。理由：chi 的 `NotFound` 只在**所有
已注册路由都没命中**时触发，天然满足「`/api/*` 与 `/healthz` 先于静态兜底」，
且不与既有 `r.Get("/api/v1/incidents/{id}")` 之类参数路由抢匹配。

关键实现取舍（与 §4.3 的对应关系）：

| 要点 | 实现 |
|---|---|
| 开关 | `IIP_WEB_ROOT` 空 → `webui.Handler()` 返回 `nil` → **完全不注册**。不设时服务行为与 M4 逐字节一致 |
| 路由优先级 | `r.NotFound(h.ServeHTTP)`，注册在所有 API 之后 |
| SPA 回退 | 未命中且非 `/api/` → 返回 `index.html` |
| 路径逃逸 | `safeRel` 逐段判 `..`（**不**用整串 `strings.Contains`）+ 拒反斜杠 + `withinRoot` 用 `filepath.Rel` 二次校验 |
| 缓存 | `assets/` 前缀 → `public, max-age=31536000, immutable`；其余 `no-cache` |
| Content-Type | 显式扩展名表（**不用 `mime.TypeByExtension`**：它读宿主 `/etc/mime.types` 与注册表，distroless 里两者都没有 → 返回空串 → 浏览器拒绝执行脚本） |
| 不 go:embed | 确认。dist 走 compose 挂载 |

**实测证据（真 PG + 真进程 + 真实 HTTP，35 PASS / 0 FAIL）**：

| 判据 | 实测 |
|---|---|
| `GET /` | 200 + `text/html` + `Cache-Control: no-cache`，body 是真实 index.html |
| SPA 回退 | `/incidents`、`/incidents/12`、`/reviews`、`/alerts`、`/settings`、带 query 的 `/incidents/12?tab=evidence` 全部 200 且是 index.html |
| `/assets/index-*.js` | 200 + `immutable` + `text/javascript`，实测 1 088 776 字节（非空） |
| **`/api` 未被吞** | `/api/v1/incidents` 200 + `application/json` + 响应体可 `json.loads` |
| `/healthz` | 200，未被静态兜底吞掉 |
| 未注册 API | 404 + **JSON**（不是 index.html）——手册 §7.2 点名的反向验证 |
| 目录穿越 | `/../Dockerfile`、`/../../etc/passwd`、`/assets/../../go.mod` 全部 **400**，未拿到文件内容 |
| **不启用时行为不变** | 不带 `--web-root` 起：`/` 与 `/incidents/12` 均 404、`/api/v1/incidents` 仍 200 |
| Go 门禁 | `go build` / `go vet` / `golangci-lint` 全 rc=0；`gofmt -s` 对本包 0 差异 |
| 前端 | `pnpm run build` rc=0，产物 `assets/` + `index.html` |

**三条测试写错而非产品缺陷的记录**（写下来是因为它们都是「看起来是产品坏了」的样子）：

1. `os.Symlink` 在无管理员权限的 Windows 上**返回 nil 却建不出有效链接**
   （缺 `SeCreateSymbolicLink`），导致「root 是符号链接」这条路径长期 SKIP。
   已用 `os.Stat` 验链接有效性后 `t.Skip`，并**另写一条** `TestHandlerAcceptsNonCanonicalRoot`
   覆盖「root 路径带冗余段与 `..` 但经 Clean 后合法」这一可验证的部分。
   真实 symlink 形态**须在容器链路（Linux）补验** → 登记为 OPEN-M5-6。
2. `httptest.NewRequest` 遇反斜杠（`\..\SECRET.txt`）直接 panic
   （`invalid URI for request`）。已另写 `doRaw` 用 `&url.URL{Path: p}` 手工构造——
   **恰恰是这种非法输入最需要测**，不能因为构造工具嫌它非法就跳过。
3. 断言 `/v1.0..next/index.html` 期望 200，实得 301。**301 是正确行为**：
   `http.ServeFile` 对以 `/index.html` 结尾的 URL 会规范化重定向到 `./`。
   改成测 `/v1.0..next/data.json`。把标准库的既定行为记成产品 bug 是这类测试的典型错法。

**抓到的一处真缺陷**：`serveIndex` 原用 `ServeContent` + 自建 reader，
文件句柄从未关闭 → `t.TempDir()` 在测试末尾删不掉目录。
已改为 `os.Open` + `defer f.Close()`，并删掉不再需要的 `reader.go`。
**判据：测试末尾出现 TempDir cleanup 失败，先查有没有未关闭的句柄，别急着 Skip。**

**顺带修掉的既有环境陷阱**（本轮又踩了一次，记进脚本注释）：
`go build -o /d/_iip_setup/x.exe` 用 MSYS 路径 **go 不认**，产物根本不生成；
而 `| head -30` 又会吃掉退出码，让失败也报 `rc=0`。
→ 编译类命令**不接管道**，用 `; echo BUILD_RC=$?` 取真实退出码，`-o` 用 Windows 路径。

### F2 期间发现的既有超限（**不是本轮造成，但本轮已停止加剧**）

AGENTS.md 约束「单文件 ≤ 300 行」。交付自查时发现两个文件已超：

| 文件 | M4 收尾 | 加完 F2 未拆 | 拆完 F2（当前） |
|---|---|---|---|
| `cmd/iip-server/main.go` | 312 | 326 | **318** |
| `internal/config/config.go` | 341 | 363 | **352** |

**该超限自 M2 起就存在**（M4 收尾时已 312/341）。本轮的处理是**只拆自己新增的部分**，
不顺手重构既有装配代码 —— 那会是一次独立的回归面，与 F2 的目标（部署形态）
无关，硬塞进来会让「F2 改了什么」变得不可审。

已拆出：

- `internal/webui/register.go`（50 行）—— 静态托管的注册逻辑
- `internal/config/m5.go`（47 行）—— M5 的 flag 注册与字段读取

**为什么 `Register` 要收进 webui 包而不是留在 main.go**：把「静态托管必须最后注册」
这条知识放回调用方，唯一的约束就消失了。留在 main.go 的 `newRouter` 里，
将来任何人在后面加一行注册都不会察觉顺序风险；收进包内后，
调用点只剩一行且无法被无声改坏。`Router` 接口只约束 `NotFound(http.HandlerFunc)`
一个方法 —— **签名必须逐字匹配 chi**（chi 收 `HandlerFunc` 而非 `http.Handler`），
写错会报 `does not implement webui.Router`。

登记为 OPEN-M5-7：剩余的 318/352 行需在后续里程碑（非 M5 收尾时）拆到 300 以内。

### F3 实测记录（2026-10-04）

实现落在 4 处：`internal/webui/uiconfig.go`（注入）、`internal/webui/serve.go`
（挂进 index.html 响应）、`internal/config/m5.go`（4 个配置项 + flag 注册）、
`web/src/api/uiConfig.ts`（前端读取）。**`web/src/api/externalLinks.ts`** 为
§4.2.3 的 URL 拼装（原先不存在），`EvidenceList.tsx` 只加了按钮组与
`stopPropagation`。配置通道的方案推导见 §4.2.2.1（**初稿遗漏，实现时补的**）。

**抓到的一处真缺陷（单测验不出来的典型）**：

请求 `/` 时 `safeRel("/")` 返回 `("", true)`，`filepath.Join(root, "")` 就是 root 本身
→ 命中 handler 的**目录分支** → 走 `serveFile(index.html)` 而**不是** `serveIndex`，
于是**注入被整条跳过**。症状极具欺骗性：**没有任何 WARN、没有报错、页面正常打开**，
只是所有外链都指向默认值。已改为目录分支取 index.html 时统一走 `serveIndex`
（注入只该在 index.html 上做）。锁它的测试
`TestIndexAlwaysGetsUIConfigInjected` 刻意写成**反向**的——验「三条通往 index.html
的路都要注入」而不只是验 `serveIndex` 自己会注入。

**实测证据（真 PG + 真进程 + 真实 HTTP，49 PASS / 0 FAIL）**：

| 判据 | 实测 |
|---|---|
| 注入内容 | `window.__IIP_UI__={prometheus:"http://prom.test:19090",grafana:"http://grafana.test:13000",loki:"http://loki.test:13100",lokiTenantId:"team-x"}` |
| 响应字节 | 注入前 426 → 注入后 566（**先看字节数就知道机制通不通，再看内容对不对**） |
| 占位符 | `<!--iip-ui-config-->` 被替换，与注入不并存 |
| 重复请求 | 两次 `/` 响应**逐字节相同**（注入是每请求做的，挂载为 `:ro` 未改磁盘） |
| SPA 回退 | `/incidents/12` 同样含注入（深链接刷新的实际路径） |
| assets | 200 + 1 092 211 字节，**不含注入的配置值** |
| 不启用时 | 不带 `--web-root` 起：`/` 404、连 `__IIP_UI__` 都不存在、`/api` 仍 200 |
| 前端 | `pnpm tsc` rc=0；**50 用例全过**（`uiConfig` 8 + `externalLinks` 23 + `EvidenceList` 11 + `EvidenceLinks` 8） |
| Go 门禁 | `gofmt -s` / `go build` / `go vet` / `golangci-lint` 全 rc=0 |
| 真库全量 | `go test ./... -v -p 1`：**767 PASS / 0 FAIL / 1 SKIP**（`repository` 1349s） |

唯一 SKIP 是 `TestHandlerAllowsSymlinkedRoot` —— 即 F2 已登记的 **OPEN-M5-6**
（Windows 无 `SeCreateSymbolicLink` 权限），**已知且已记录，非本批次新增**。

**三条「测试写错而非产品缺陷」的记录**（与 F2 同一类，但判据错误更有代表性）：

1. 断言按 **JSON** 写 `"prometheus":"http://…"`，而注入的是**紧凑 JS 对象字面量**
   `prometheus:"http://…"`（key 无引号）→ 4 条全 FAIL。**注入 JS 字面量而非 JSON
   是正确形态**（省字节，且 `window.__IIP_UI__` 本就是 JS 全局）。
   → 断言必须**按实际输出形式写**，不能按「看起来更规范」的形式写。
2. 验「`assets/*.js` 不该被注入」时用 `b"__IIP_UI__" not in body` → **永远 FAIL**：
   **前端 bundle 里本来就有这个字面量**（`uiConfig.ts` 的 `const pY="__IIP_UI__"`）。
   → **判「X 没被处理」必须用只可能来自该处理的字符串**（此处用 `prom.test` / `team-x`），
   用 X 的名字当判据会让断言恒假或恒真。
3. 测试 fixture 的 index.html **不带占位符**，于是所有「注入是否发生」的断言恒为假。
   症状是测试日志**反复刷 WARN** 却没人当回事。
   → **fixture 必须与真实产物一致**；**测试里重复出现的 WARN 是「fixture 与产物不一致」的探针**，
   别忽略。补上占位符后 WARN 归零，断言才具备鉴别力。

**顺带确认的一件事**：`metadata.window` 在**三个采集器里都写了**（格式统一
`RFC3339/RFC3339`），所以 Loki 链接的时间窗有真实依据。核这一点的过程值得记：
第一次探查脚本只匹配 `Metadata: map[string]string{` 字面量，而 k8s 采集器走
`md` 变量构造 → **漏判成「只有部分采集器写了」**，差点照着错误的结论改代码。
→ **判据写窄了会得出方向相反的结论**，比不判更危险（见 §二纪律）。

### F4 实测记录（2026-10-04）

S1 配置页。**本批次零后端改动** —— `internal/api/correlation_config.go` 在 M2 已实现，
F4 只做前端。但「不需要后端改动」不等于「不需要验后端」：见下面的实测段。

| 文件 | 职责 | 行数 |
|---|---|---|
| `web/src/api/correlationConfig.ts` | GET/PUT 客户端 + 校验镜像 + `ConfigApiError` | 162 |
| `web/src/pages/Settings.tsx` | 页面主体 | 253 |
| `web/src/components/WeightRow.tsx` | 单维度的滑块 + 输入 + 说明 | 69 |
| `web/src/api/correlationConfig.test.ts` | 校验口径（22 例） | — |
| `web/src/pages/Settings.test.tsx` | 页面渲染与交互（11 例） | — |

`App.tsx` 加路由、`Layout.tsx` 加导航项并把 Header 的里程碑 Tag 从「M4 证据收集」
改成「M5 Web 控制台」。

**实测证据（真 PG + 真进程 + 真实 HTTP，27 PASS / 0 FAIL）**：

| 判据 | 实测 |
|---|---|
| GET 契约 | 200，含五维权重 + 两阈值 + `enabled:bool`；默认值与 `DefaultConfig` 一致（0.25/0.25/0.20/0.20/0.10、0.80/0.50） |
| **PUT 真落库** | 改权重后 PUT 200，**另 GET 一次**确认拿到新值（不看 PUT 响应 —— 那证明不了落库） |
| 422 原文 | 权重和 0.95 → 422，`error` 含「五维权重之和」且含**具体当前值 0.95**；`merge<review` → 422 含「不能小于」 |
| 400 未知字段 | `{"threshods":...}` → **400**（不是 200 静默忽略） |
| 边界 | `merge == review` → 200 |
| **部分更新** | 只提交 `thresholds` → 200，**权重保持上次值未被重置**（前端能整份提交的前提） |
| `/settings` 深链接 | 200 + `id="root"`（F2 静态托管回退的复用验证） |
| 前端 | `pnpm tsc` rc=0；**83 用例全过**（F3 的 50 + 本轮 33） |
| Go | `go build ./...` rc=0、`go vet ./...` rc=0（**本批次无 Go 改动**，故不重跑真库） |

**四条「测试写错而非产品缺陷」的记录**（本批次全部是这个类型，**产品零缺陷**）：

1. **`user.clear()` 在 antd InputNumber 上不是「清空」而是「置 0」** —— 实测
   clear 后 input 值是 `0.000`，`type('0.9')` 变 `0.0000.9`，被解析成 0：
   权重和算出 0.75 而非预期的 1.65。**而它是绿的**（看起来「确实改了值」），
   只有期望值对不上才暴露。→ 改用 `fireEvent.change` 一次给完整值。
2. **antd 给两个中文字的 Button 自动插空格** —— 可访问名是「保 存」，
   `getByRole('button',{name:'保存'})` 永远找不到。**这是 antd 的既定设计行为**，
   不是缺陷 → 用 `/保\s*存/`。
3. **`getByDisplayValue` 找不到 InputNumber 的值** —— 值在 React 状态里，
   `<input>` 上没有 `value` 属性。→ 改用 `getAllByRole('spinbutton')` 按序号取，
   **并另写一条「7 个输入框的数量与顺序固定」钉住序号**，否则改一次 JSX 顺序
   就会让「改时间邻近」的用例悄悄变成测「标签一致」而仍然全绿。
4. **手算浮点容差边界算错两次** —— ① `0.501+0.75` 我心算成 `1.001`，实测 `1.251`
   （忘了 0.501 里有 0.5）；② 「和恰好等于 1.001 判合法」实测
   `1.001000000000000112`，严格不等号判越界。**后者不是缺陷**：后端 Go 算同一式子
   同为 IEEE 754，会判同样结果 → **前后端自洽**。→ 容差测试只测「明确在内/明确超出」，
   并用**倍数于 eps 的 diff 表驱动**（0 / ±0.5eps / ±2eps）钉住判据等价于后端的
   严格不等号；写完 `toBeCloseTo` 先验算期望值。

**判据设计上的一个刻意选择**：`numInput` 用序号定位，因此另配「7 个、顺序固定」
那条测试。**序号选择器本身是脆弱的**，少那条兜底，未来的重构会让测试
「测着 A 却断言 B」且全绿 —— 这类缺陷比测试红更贵，因为它看起来是绿的。

### F4 期间新增的 OPEN 项

无。F4 未引入新的未验路径：后端端点在真进程里 11 条断言全覆盖
（含 400/422/边界/部分更新四种形态），前端 33 条用例覆盖渲染与交互。

### F5 实测记录（2026-10-04）

本批次两件事：① 补齐 §4.4.2 剩余的组件测试；② 实施 S5 明暗主题切换（ADR-021）。

#### 一、补测：§4.4.2 的 7 条里 4 条已被覆盖，F5 只补 3 条

| §4.4.2 要求 | 状态 |
|---|---|
| `EvidenceList` 渲染 0 条 | **已覆盖**（F2，`EvidenceList.test.tsx` 11 例） |
| `EvidenceList` 渲染 1 条 query 为空 | **已覆盖**（F3，`EvidenceLinks.test.tsx`） |
| `EvidenceList` 渲染 loki 证据 → 编码 | **F5 补**（既有只验「两个链接都在」，没验编码） |
| `Settings` 权重和为 0.8 | **已覆盖**（F4） |
| `Settings` merge < review | **已覆盖**（F4） |
| `IncidentDetail` 点「标记缓解」 | **F5 补**（此前零覆盖） |
| `IncidentDetail` 后端返 422 | **F5 补**（此前零覆盖） |

**发现既有覆盖有一处虚**：F3 的 loki 用例只断言「Grafana 与 Loki 两个链接都渲染出来了」，
**完全没验 query 进了 URL 之后变成什么**。§4.4.2 第 3 条要的正是编码。
→ 补的那条断言「解码后能 `JSON.parse` 且 `expr` 还原为原文」，
而不是断言「有没有 %22」这种具体形态。

#### 二、缺陷：流转失败后处置按钮被锁 3 秒（已修，见 §4.6.1）

补测 `IncidentDetail` 时抓到。**这是本批次唯一的产品缺陷**，其余 4 处都是测试写错。

| 路径 | 修前 | 修后 |
|---|---|---|
| 流转成功 | 156 ms | 148 ms |
| **流转失败（后端 422）** | **3141 ms** | **184 ms** |

归因过程（**三次才找对，记录下来**）：

1. 第一次归因「antd message 卸载触发 re-render，拖后了 `isPending` 的解除」——
   **对但不够**：换成组件内本地 state + `onSettled` 清理，**仍然 3.1 秒**。
   → 说明被拖后的不是某一个变量，而是**任意在 mutation 结束时触发的状态更新**。
2. 造最小替身做对照：同一个 mutation，只把 `message.error` 换成/不换。
   **有 message：4 秒未解除；无 message：立即解除**。→ 确认与 message 有关，与业务代码无关。
3. 真因：antd message 用 `ReactDOM.render` 挂到**独立 root**，
   挂载/卸载占住 React 调度通道，把紧随其后的更新拖到卸载那一刻。
   → 修法是**在 `onError` 里先 `setTransitioning(false)`、再 `message.error`**，
   让解锁排在 message 之前提交。

**修法不是把 3 秒改小**：把 `message.error(msg, 0.5)` 也能让数字好看，
但那是把缺陷藏起来 —— 用户仍会失去「读完错误再重试」的窗口。

**回归锁**：失败后 **1 秒内** loading 必须解除，且此时点另一个目标能发出第二个 POST。
只验「最终会恢复」会假通过 —— 修前 3.1 秒也会恢复。

#### 三、S5 明暗主题切换（ADR-021）

实现：`theme.ts` 拆成亮/暗两套 palette；`themeMode.tsx` 把 `palette` 变成**活引用**
（模块级可变对象，切换时重新赋值字段），**17 处内联色调用点一行未改**。

**关键取舍**：那 17 处若改成 token/CSS 变量，diff 会大到无法评审；
改成活引用则「主题切换」这件事被完全收在两个文件里。
代价是 `palette` 不再是纯常量 —— 故在 `themeMode.tsx` 里写明「不得再从 `./theme` 直接 import」。

亮色语义色**按白底重新校准**（实测暗底那套搬到白底只有 2.52~3.35:1，全部不达标），
亮色`text.tertiary` 定为 `#6b737d`（在两种卡片底上都 ≥ 4.5:1）。

**默认暗色 + localStorage 记忆**（ADR-021 裁定），Header 右侧图标切换。

#### 三之二、S5 真浏览器验收（截图 + 像素实测，10 PASS / 0 FAIL）

手册 §4.5.4 明说第 1、4 条「只能在真浏览器 / 截图里验，jsdom 读不到计算后的颜色」。
链路：真 PG 容器（`postgres:16-alpine` 一次性，起在 5461）→ `go run ./cmd/iip-server`（:8085）→ 造 4 批告警（每批 `accepted=1`）→ 静态+反代（:5175）→ 无头 Chrome 1600x1000。

**像素是**从截图像素里量出来的，不是从 `theme.ts` 抄的** ——
抄 theme.ts 只能证明「我配的值达标」，从截图量才能证明「浏览器真按那个值渲染了」。

| §4.5.4 判据 | 实测 |
|---|---|
| **1. 内联色真的变了** | header 暗 `#161b22` / 亮 `#f6f8fa`（16.25:1）；sider 暗 `#0d1117` / 亮 `#f6f8fa`（17.78:1）；content 暗 `#0d1117` / 亮 `#ffffff`（18.92:1） |
| 2. 刷新后保持 | 预置 light 后同 profile 再访问，仍为亮色（截图 84437 B，与暗色 83468 B 明显不同） |
| 3. 首次默认暗色 | 干净 profile 首访 = 暗色 |
| **4. 亮色对比度 ≥ 4.5:1** | 正文 `#1f2328` on `#ffffff` = **15.80:1** |

**截图字节数本身就是判据的一部分**：初版四张图全部 6989 B（= 同一张全白图），
修好后 38 KB / 84 KB 不等。→ 截图流程里「多张图字节数相同」是页面没渲染的可靠信号。

#### 三之三、修这条验收链路时踩的四个坑（都是「症状零报错」）

1. **自写静态服务漏了 `__init__` 的 `directory=DIST`** —— `SimpleHTTPRequestHandler` 静默退回 CWD，`/assets/*.js` 全部 404。症状与「资源真的不存在」一模一样。→ **先单独验 assets 的 200 与字节数**，别直接进截图。
2. **SPA fallback 判据写成「文件不存在就回 index.html」** —— 会把 `/assets/*.js` 也吞成一段 HTML，
   浏览器当模块解析失败、**不报错**，页面纯白。→ fallback 只对**无扩展名**路径生效。
3. **主题预置脚本的 localStorage key 写错** —— 我凭印象写 `iip-theme-mode`，
   真实是 `THEME_STORAGE_KEY = `'iip.theme'`。**判据要取自源码常量，不要凭印象写**。
4. **误判「`--virtual-time-budget` 有害」** —— 我先把它换成 `--timeout`，
   结果截到未渲染态（中心 `#ffffff`）。对照实验：
   `--timeout=15000` → 未渲染；`--virtual-time-budget=20000` → 已渲染。
   **虚拟时间恰恰是唯一能等到异步渲染的**（SPA 的请求在 load 之后才发出，`--timeout` 只等 load）。
   → 造对照实验，别凭「听起来危险」换参数。

另：`--virtual-time-budget` **不能设太大** —— 详情页有 12s `refetchInterval`，
预算太大时一直有 pending 任务，截不到稳定帧。取 9000。

#### 四、本批次的「测试写错而非产品缺陷」（4 处）

1. **antd 会在中文 Button 文字里插空格** —— 「转为已解决」的可访问名是「转为已解 决」。
   F4 在「保 存」上踩过一次，本轮在**五字**按钮上又踩一次（不是只有两字才插）。
   → 统一用 `/转为\s*已解决/` 这类宽松正则，**不去赌 antd 插在哪两个字之间**。
2. **两个 loki 链接的承载方式完全不同** —— 我一开始用同一个参数名去取：
   Grafana 是 `/explore?left=<整段 JSON>`（query 藏在 `queries[0].expr`），
   Loki 原生是 `?query=<LogQL>`。判据必须各写各的。
3. **`button.ant-btn.ant-btn-loading` 会把 Tabs 的 `ant-tabs-nav-more` 数进来** ——
   导致「点按钮之前就已有 4 个 loading」的假红。→ 只数 `ant-btn-primary` / `ant-btn-dangerous` 的处置按钮。
4. **漏删诊断文件** —— `__diag4.test.tsx` 留在仓库里，tsc 报 `TS6133: 'waitFor' is declared but its value is never read`，
   而 vitest 照跑全绿。**门禁 rc=2 才暴露**。→ 诊断文件必须用「遍历删除」而非逐个删。

#### 五、验收

| 项 | 结果 |
|---|---|
| tsc | rc=0 |
| 前端全量 | **114 用例全过**（F4 的 83 + 本轮 31） |
| Go | 无改动，未重跑真库 |

### F3 期间新增的 OPEN 项


无。F3 未引入新的未验路径：运行时配置在「启用/不启用」两侧都有真进程断言。

### F2 期间新增的 OPEN 项

| 编号 | 内容 | 处置 |
|---|---|---|
| OPEN-M5-6 | 「root 是符号链接」这条路径在本机 **SKIP**（Windows 无 `SeCreateSymbolicLink` 权限），只由等价的 `TestHandlerAcceptsNonCanonicalRoot` 部分覆盖 | 须在 `verify-images.py`（Linux 容器）补一条真 symlink 断言。F7 处理 |
| OPEN-M5-7 | `cmd/iip-server/main.go` 318 行 / `internal/config/config.go` 352 行，均超 AGENTS.md 的 300 行约束。**该超限自 M2 起存在**，M5 只拆了自己新增的部分，未顺手重构既有装配代码 | 不在 M5 收尾。剩余部分需独立里程碑处理；F7 门禁**不因此判红**（约束是「新增不加剧」，已在 §六 记录当前值） |

---

### F1 期间新增的 OPEN 项

| 编号 | 内容 | 处置 |
|---|---|---|
| OPEN-M5-4 | `vitest`/`jsdom`/Testing Library 为**新增 devDependency**，需在 CI 加 `pnpm test` job 才算进门禁；仓库无 remote，该 job 同样只能本地等价复现 | F7 处理。未加之前 §4.8 仍记 NOT DONE |
| OPEN-M5-5 | 组件覆盖率仅 `EvidenceList.tsx` 44%，其余 5 个页面/组件为 0 | **不追数字**。F5 补测试时自然上升；把覆盖率设成门禁阈值是错的（会诱导为凑数字写测试） |

---

## 九、端到端验证脚本

``bash
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
``

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
