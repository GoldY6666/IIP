# deploy

本目录用于存放本地联调环境编排文件（docker-compose、Prometheus / Alertmanager / Loki 配置）。

- 编排细节依据 `docs/IIP-M0详细开发计划.md` §T0.6。
- 本目录只放编排与配置，不得放入业务逻辑。

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
# 在仓库根执行
docker build --target server -t iip-server:0.0-m0 .
docker build --target worker -t iip-worker:0.0-m0 .

# 等价的一键方式
make image-build
```

运行层是 `gcr.io/distroless/static-debian12:nonroot`：`CGO_ENABLED=0` 静态编译后
无动态库依赖，镜像内没有 shell / 包管理器，以非 root（UID 65532）运行。

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
