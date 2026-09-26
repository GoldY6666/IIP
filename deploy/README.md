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

## 两个需要知道的行为

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
