# IIP — iip-server / iip-worker 多阶段构建
#
# 用法（在仓库根执行）：
#   docker build --target server -t iip-server:0.0-m0 .
#   docker build --target worker -t iip-worker:0.0-m0 .
#
# 一个 Dockerfile 出两个镜像：builder 阶段共享 Go 模块缓存与依赖下载，
# 仅最后 COPY 的二进制不同，避免维护两份几乎相同的文件。
#
# 运行层用 distroless/static：CGO_ENABLED=0 静态编译后无任何动态库依赖，
# 不需要 shell、包管理器或 libc。代价是镜像内没有 sh/curl/wget，
# 因此健康检查由二进制自身的 --healthcheck 模式承担（见 cmd/iip-server）。

# ---------------------------------------------------------------- builder
FROM golang:1.23-alpine AS builder

# 静态编译：pgx 走纯 Go 路径，实测 linux/amd64 与 linux/arm64 均可 CGO_ENABLED=0。
ENV CGO_ENABLED=0 \
    GOOS=linux

# Go 模块代理。默认是官方源，保持生产/CI 行为不变；
# 但容器内直连 proxy.golang.org 在国内会超时（实测 connect refused），
# 此时用 --build-arg GOPROXY=https://goproxy.cn,direct 覆盖即可。
# 注意：必须在 go mod download 之前声明，否则那个 RUN 取不到值。
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY}

WORKDIR /src

# 先只 COPY go.mod / go.sum 再 download：依赖未变时这层直接命中缓存，
# 改代码不会触发重新下载模块。go.sum 缺失时 download 的报错信息足够明确。
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

# 复制源码。migrations/*.sql 必须一起进来——它被 embed.FS 编进二进制，
# 少了它 store.Migrate 会在运行时失败（编译期就会因 //go:embed 报错）。
COPY cmd/ ./cmd/
COPY internal/ ./internal/
COPY migrations/ ./migrations/

# 构建参数：默认产出 linux/amd64，可用 --build-arg 覆盖。
# 显式声明 TARGETOS/TARGETARCH 而非依赖宿主机，避免在 Windows 上误出 .exe。
ARG TARGETOS=linux
ARG TARGETARCH=amd64
ARG VERSION=dev

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    mkdir -p /out && \
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
      -o /out/iip-server ./cmd/iip-server && \
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
      -o /out/iip-worker ./cmd/iip-worker

# ------------------------------------------------------------- server 镜像
FROM gcr.io/distroless/static-debian12:nonroot AS server

COPY --from=builder /out/iip-server /usr/local/bin/iip-server

# 非 root（distroless 的 nonroot 用户 UID 65532）
USER nonroot:nonroot

EXPOSE 8080

# 注意：此处不能写 ENTRYPOINT 数组里带 --healthcheck，
# 健康检查由 compose 的 healthcheck.test 显式调用该 flag。
ENTRYPOINT ["/usr/local/bin/iip-server"]

# ------------------------------------------------------------- worker 镜像
FROM gcr.io/distroless/static-debian12:nonroot AS worker

COPY --from=builder /out/iip-worker /usr/local/bin/iip-worker

USER nonroot:nonroot

ENTRYPOINT ["/usr/local/bin/iip-worker"]
