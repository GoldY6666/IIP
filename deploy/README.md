# deploy

本目录用于存放本地联调环境编排文件（docker-compose、Prometheus / Alertmanager / Loki 配置）。

- 编排细节依据 `docs/IIP-M0详细开发计划.md` §T0.6。
- 本目录只放编排与配置，不得放入业务逻辑。

## ⚠️ 当前默认配置仅供内网 / 演示环境

**默认编排下没有任何鉴权**：任何能访问 `localhost:8080` 的人都能读写全部事件、
复核告警、改关联配置。默认配置只适用于本机联调与内网演示。

要放到内网以外（同事跨网段访问、反向代理后暴露）**必须**先启用鉴权，见
[「启用 UI 鉴权（M6 S4）」](#启用-ui-鉴权m6-s4)。

这不是「以后再说」的提示：M5 S3 交付静态托管时留下的条件义务是
「不做鉴权就必须在部署文档标注内网限定」，S4 把鉴权做出来后该标注
**依然保留**——因为「默认不启用」是刻意的向后兼容，任何人新起一个环境
时默认仍是无鉴权状态。

## 启用 UI 鉴权（M6 S4）

不配置即完全不开鉴权；配置了则`/api/v1/*` 全部需要携带请求头
`X-IIP-Token`。前端会自动带上，无需改任何前端代码。

```bash
# 1. 生成一个强随机 token（不要用人能猜到的口令）
python -c "import secrets; print(secrets.token_urlsafe(32))"

# 2. 取消 docker-compose.yml 里 IIP_AUTH_TOKEN 的注释并填入该值
#    （默认注释掉的就是「不启用」）

# 3. 重建 iip-server 容器使环境变量生效
docker compose -f deploy/docker-compose.yml --profile app up -d --force-recreate iip-server
```

验证是否真的生效：

```bash
# 应返回 401（未带 token）
curl -i http://localhost:8080/api/v1/alerts

# 应返回 200（带上正确的 token）
curl -i -H "X-IIP-Token: <你配的值>" http://localhost:8080/api/v1/alerts
```

三条边界，配置前值得知道：

| 路径 | 是否需要 token | 原因 |
|------|----------------|------|
| `/api/v1/*` | **需要** | 业务数据读写 |
| `/healthz` | 不需要 | 容器的 `healthcheck.test` 不可能带自定义头，拦了会导致服务被判不健康 |
| `/api/v1/auth/session` | 不需要 | 前端靠它拿 token，拦了就死锁（页面全白） |

token 只在启动日志里以「已启用」出现，**不回显值**；`/healthz` 与静态托管始终放行。

## 服务分组

`docker-compose.yml` 里有两组服务，用 `profiles` 隔离：

| 组 | 服务 | 镜像来源 | 默认是否启动 |
|----|------|----------|--------------|
| 依赖组 | postgres / redis / prometheus / alertmanager / loki | 官方镜像（版本钉死） | 是 |
| 应用组 | iip-server / iip-worker | 本项目构建 | 否（需 `--profile app`） |

默认 `up -d` 只起依赖组五个服务，与 T0.6 的验收标准（五服务全 healthy）一致。
需要连应用一起起时显式带 profile。

## 启动 UI（M5 起）

UI 由 `iip-server` 自带静态托管（`IIP_WEB_ROOT`），**与 API 同源同端口**：

| 用途 | 地址 |
|------|------|
| Web UI | <http://localhost:8080/> |
| API | <http://localhost:8080/api/v1> |
| 健康检查 | <http://localhost:8080/healthz> |

**首次使用必须先构建前端**——compose 不会替你做这件事：

```bash
cd web
pnpm install --frozen-lockfile
pnpm run build          # 产出 web/dist
cd ..
docker compose -f deploy/docker-compose.yml --profile app up -d
```

`web/dist` 是**挂载**进容器的（`../web/dist:/usr/share/iip/web:ro`），
不是烧进镜像：运行层是 distroless 没有 node，把 dist 烧进去会让镜像
从 14 MB 涨到 1 MB 以上，且前端每次改动都要重编 Go 镜像。

**没构建就启动会怎样**：服务**照常启动**（静态托管是可选能力，不影响 API），
但打开 UI 会看到 404 或「前端产物不完整，请先执行 pnpm run build」的 500。
这是刻意设计的——前端问题不该拖垮后端可用性。看到这两条提示就是忘了构建。

改前端后重新 `pnpm run build` 即可生效（`index.html` 不缓存，
带 hash 的 `assets/*` 会长期缓存，不需要手动清浏览器缓存）。

> `api/` 下的请求永远返回 JSON，不会被 SPA 回退吞掉。
> 前端用相对路径 `/api/v1` 调接口，所以不需要任何 API 地址配置，也没有跨域问题。

## 镜像构建

`iip-server` 与 `iip-worker` 由仓库根目录的 `Dockerfile` 多阶段构建产出，
同一个 Dockerfile 用 `--target` 区分两个镜像，builder 阶段共享 Go 模块缓存。

```bash
# 在仓库根执行（标签与 Makefile IMAGE_TAG / compose x-app-image / Chart appVersion 四处联动）
docker build --target server -t iip-server:0.2.0 .
docker build --target worker -t iip-worker:0.2.0 .

# 等价的一键方式（IMAGE_TAG 默认即 0.2.0）
make image-build
```

运行层是 `gcr.io/distroless/static-debian12:nonroot`：`CGO_ENABLED=0` 静态编译后
无动态库依赖，镜像内没有 shell / 包管理器，以非 root（UID 65532）运行。

## 资源限制与日志轮转（T7.5）

七个服务全部带 `deploy.resources.limits.memory` 与 json-file 日志轮转
（`max-size: 10m` × `max-file: 3`）。两个默认值都有来历：

- **只限 memory 不限 cpu**：cpu 限额在共享内核的开发机上容易把健康检查
  拖到超时（表现为随机 unhealthy，排查方向全错）；内存上限防的是
  「一个容器 OOM 拖垮整机」这个真会发生的事故。
- **日志必须轮转**：json-file 驱动默认永不删旧文件，联调实测 loki
  单日能写 1.2G。默认值只兜底；需要更长追溯的部署应改
  `deploy/docker-compose.yml` 的 `x-logging` 锚点，或换 journald / fluentd。

限量分布（与 `deploy/helm/iip` 的 `values.yaml` 同量级）：

| 档 | 服务 | memory |
|----|------|--------|
| 数据面 | postgres / prometheus / loki | 512M |
| 控制面 | redis / alertmanager / iip-server / iip-worker | 128–256M |

## 采集目标 TLS（T7.5）

worker 的四个采集器（prom / loki / k8s / trace）默认走系统根证书校验。
内网数据源用自签证书时二选一（都通过环境变量配给 worker）：

| 方式 | 变量 | 适用 |
|------|------|------|
| 提供 CA（推荐） | `IIP_EVIDENCE_TLS_CA_FILE=/path/ca.pem` | 自签 CA 可导出的一切场景 |
| 跳过校验（过渡） | `IIP_EVIDENCE_TLS_INSECURE=true` | 来不及铺 CA 的内网过渡期 |

行为边界（三条都不是默认值抄来的）：

1. **不给任何变量** = 与 T7.5 之前逐字节一致（系统根证书校验）；
2. **CA 文件给了却读不到 / 不是 PEM** = worker **启动即失败**，日志指明
   是哪个文件。刻意不静默降级——「配了 CA 却悄悄不校验」比启动失败
   危险得多；
3. **insecure 开启时** worker 启动日志打 WARN，且 insecure 优先于 CA
   （两个都配时先保连通）。

CA 文件要能被 worker 容器读到：compose 部署把它挂进容器目录
（如 `- ./tls/ca.pem:/config/tls/ca.pem:ro`）后设上面的路径。

## Helm chart（T7.5，简化版）

`deploy/helm/iip` 提供单集群内网部署路径，与 compose 同一套取舍：
默认值可跑、显式配置才启用。

```bash
helm lint deploy/helm/iip
helm template test deploy/helm/iip     # 只看渲染结果，不安装
helm install iip deploy/helm/iip       # 默认：内置单实例 PG
```

两条部署轨道：

- **内置 PG**（默认）：chart 生成单实例 StatefulSet + PVC。没有默认
  StorageClass 的集群必须显式设 `database.embeddedOpts.storageClassName`，
  否则 PVC 一直 Pending、PG Pod 起不来；
- **外置 PG**：`database.embedded=false` + 填 `database.external.*`。
  注意 external.password 是明文 values，会出现在 `helm template` 输出与
  Release 记录里，别提交进版本库。

采集器地址（`evidence.promURL` / `evidence.lokiURL`）**留空即不注册**，
与 worker 的运行时语义一致：chart 不假设你的集群里有 Prom / Loki。
K8s / trace 开关打开时地址必填，`helm install` 直接报错（把失败从
worker 启动期提前到安装期）。

刻意不做（写了跑不起来比不写更危险）：多副本 server 选主、PG 高可用、
Ingress/TLS 终止、HPA——这些属于集群层设施，理由见 values.yaml 顶部注释。

## 自监控指标与面板（T6.3 / T6.4）

`iip-server` 与 `iip-worker` 各自暴露 `/metrics`（Prometheus 文本格式）。两者是
**独立注册表**，同一个指标名不会在两个 target 上同时出现：

| 指标 | 出现位置 | 说明 |
|---|---|---|
| `iip_alert_ingest_total{result}` | server | `stored`/`duplicate`/`invalid`/`error` |
| `iip_alert_dedup_total{result}` | server | `hit`/`miss`/`race`（race 是 miss 的子集） |
| `iip_webhook_duration_seconds` | server | 端到端时长（含关联） |
| `iip_correlation_score{action}` | server | 关联分数直方图，桶步长 0.1 |
| `iip_incident_created_total` | server | 仅 `new` 档，不含合入 |
| `iip_evidence_collect_duration_seconds{source}` | **worker** | 单次采集耗时（含失败） |
| `iip_task_retry_total{reason}` | **worker** | `stale_reclaim`/`attempt_failed` |

三条接入步骤：

```bash
# 1. 起栈（观测组 + 应用组）。Prometheus 会按 deploy/prometheus/prometheus.yml
#    抓 iip-server:8080/metrics 与 iip-worker:9091/metrics
docker compose -f deploy/docker-compose.yml --profile app up -d

# 2. 确认两个 target 都是 UP（页面：Status → Targets）
#    只有 prometheus 自己 UP、两个 iip-* 是 DOWN 时，先看目标进程是否真的起了

# 3. 导入面板：Grafana → Dashboards → Import → 上传
#    deploy/grafana/iip-overview.json，数据源选你的 Prometheus
```

`/metrics` **不受 `IIP_AUTH_TOKEN` 保护**（Prometheus 不该持有业务 token；鉴权只作用于
`/api/v1/*`）。关掉指标的两个开关与各自的症状：

- `IIP_METRICS_ENABLED=false`（server）→ `/metrics` 路由不注册，返回 **404**，target DOWN；
- `IIP_WORKER_METRICS_ADDR` 置空（worker）→ 不监听，target DOWN。
  **端口被占用时 worker 只 WARN 不退出**（指标是旁路，不该拖死采集），
  所以「worker 健康、任务照跑、指标不可抓」是可能的状态，`up{job="iip-worker"}` 是唯一信号。

两条告警规则在 `deploy/prometheus/rules/iip-self.yml`（`IIPTargetDown`、
`IIPAlertIngestStalled`）。**它们不该路由回 iip-server**：`IIPAlertIngestStalled`
一旦送达 IIP，它自己就是一条入库，条件随即消失、下个周期自动 resolve。
自监控告警请配独立 receiver（邮件/Slack）。

改完面板或规则后跑静态核对（不需要 Docker / Prometheus / Grafana）：

```bash
python scripts/verify-observability.py        # 或 powershell scripts/tasks.ps1 verify-observability
```

它核对「指标名 / 标签名与取值 / 记录规则引用 / 抓取端口 / 面板结构」五类一致性。
**它不核对 PromQL 语法与渲染结果** —— 那需要 promtool 与 Grafana 实例，本仓库的
验证链路里都没有，所以「面板已验证可用」这句话目前给不出证据，只能说
「面板引用的名字与代码里的定义一致」。

> 历史：`deploy/prometheus/rules/highlatency.yml` 已改名为 `.yml.example`。
> 里面两条规则是 M0 用来验证告警链路的**恒真假规则**，接通真实指标后它们会
> 每 4 小时往库里灌一条 checkout 的假告警，并在面板上表现为无法解释的周期尖峰。

## 入口加固与两个「不做」（T6.6 / ADR-027）

三个 JSON 入口（告警 webhook、关联配置 PUT、事件评论 POST）现在共用一个
`readBody`，因此**同一类坏输入在任何入口的行为一致**：

| 输入 | 状态码 | 说明 |
|---|---|---|
| 请求体超限 | **413** | 上限未改（webhook 1 MiB 等）；此前评论入口误报 400 |
| 非 UTF-8（如 GBK） | **400** | 此前一路走到 PG 才报 `22021` ⇒ 503「存储不可用」。日志带 `first_invalid_offset` |
| 上游中途断开 | **400** | 客户端收不到响应，但服务端有日志可查 |

**注意这不会让 Alertmanager 停止重试** —— 它对任何非 2xx 都重试。本次改动的价值是
把「客户端编码错」和「服务端存储坏」分开，不是压住风暴。

### 为什么告警入口不做限流

风暴正是它唯一需要工作的场景，而 429 会让 Alertmanager 进入退避重试，
**最新的告警被排到退避队列后面** —— 最贵的时延恰好被放大。去风暴交给上游
（`group_wait`/`group_interval`），入口侧只做三件事：体量上限、编码校验、落库幂等。
若确实需要限流，正确位置是**反代/网关**（能返回 `Retry-After`、能按来源分桶），
不是应用进程内。

### 为什么不给 API 开 CORS

UI 与 API 由同一个 iip-server 同源提供（8080 静态托管 + `/api/v1/*`），
没有任何跨源需求。默认 `Access-Control-Allow-Origin: *` 会把「同源才可调用」
这个隐式防护直接拆掉。将来做前后端分离部署时，按**白名单来源**加，
并与 `IIP_AUTH_TOKEN` 一起评估。

## 常用命令

```bash
# 只起依赖组（五服务）
docker compose -f deploy/docker-compose.yml up -d

# 构建镜像 + 起全部服务
make app-up

# 查看状态（应用组会显示 healthy / running）
docker compose -f deploy/docker-compose.yml --profile app ps

# 停全部（保留 named volume 数据）
docker compose -f deploy/docker-compose.yml --profile app down

# 连数据一起删（reset）
docker compose -f deploy/docker-compose.yml --profile app down -v
```

## 需要知道的行为

（下面几条都是「看现象容易误判成别的问题」的那类，故单列。）

**1. 容器内连库走服务名，不是宿主机端口**

应用组的 `IIP_PG_URL` 指向 `postgres:5432`（compose 内部网络），
而不是宿主机的 `localhost:5433`。这意味着：

- 容器栈是完全自包含的，与宿主机上的绿色版 PostgreSQL 互不干扰；
- 但**容器看到的 `iip` 库与宿主机 `5433` 上的不是同一个库**，数据不互通。

**2. distroless 没有 shell，健康检查靠二进制自身**

运行层没有 `sh` / `curl` / `wget`，无法用 HTTP 探针。因此
`iip-server` 内置了 `--healthcheck` 模式（请求本机 `/healthz`，按结果退出 0/1），
compose 的 `healthcheck.test` 直接调用它。

`iip-worker` 不监听端口，无端口可探，它靠 polling 循环持续查库，
进程活着即正常工作，故**显式不配 healthcheck**——避免制造一个无意义的假探针。

排查容器内问题时，distroless 下无法 `docker exec` 进 shell。
需要调试时改为本地裸跑（见仓库根 `README.md` 的「无 Docker 的本地运行」一节），
或临时把 `Dockerfile` 的运行层换成 `alpine`。

**3. 静态托管不是「构建步骤」，是「挂载」**

`web/dist` 通过 volume 挂进容器，**不在镜像里**。因此：

- compose 不会替你构建前端（运行层是 distroless，没有 node）；
- 改了前端要重新 `pnpm run build`，但**不需要**重启容器——
  `index.html` 是 `no-cache`、带 hash 的 `assets/*` 走浏览器缓存，
  重新构建后刷新即可生效；
- 若前端代码更新但页面没变，先确认 `web/dist/index.html` 的时间戳是否更新了
  （这说明 build 真的跑了），再看浏览器网络面板里 `assets/*` 是否 304。

不启用静态托管（不设 `IIP_WEB_ROOT`）时，服务只提供 API，行为与引入 M5 之前完全一致——
这让「后端能跑」不需要「先构建前端」。
