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
#
# **Go 版本不在这里定权威，而是由 go.mod 的 toolchain 行决定**（2026-10-12 修）。
#
# 原先这里硬编码 `golang:1.23-alpine`，而 go.mod 早已是 `go 1.26.0` /
# `toolchain go1.26.9`，构建直接失败于
# `go.mod requires go >= 1.26.0 (running go 1.23.12)`。
# 该缺陷潜伏了两个里程碑：CI 用 `go-version-file: go.mod` 自动跟随所以一直绿，
# 只有容器链路红，而容器链路恰好又被「Docker 不可用」掩盖。
#
# **为什么不直接写 FROM golang:1.26-alpine**（最直观的修法，实测行不通）：
# 本机镜像源不通（`registry-1.docker.io` context deadline exceeded，实测 17s 超时），
# `golang:1.26-alpine` 拉不下来，构建会从「版本不匹配」变成「镜像拉不到」——
# 两种都红，但后者把问题从「代码写错」变成「环境不通」，更难定位。
#
# **采用的解法：基础镜像只当「有 go 二进制」的载体，实际工具链由 go.mod 决定。**
# Go 1.21+ 的toolchain 机制会读 go.mod 的 `toolchain` 行，自动下载并切换到
# go1.26.9。实测（golang:1.23-alpine + goproxy.cn）：自举成功、go mod download rc=0。
# 代价是构建多一次下载；换来的是**版本永远跟着 go.mod 走，不会再漂移**。
FROM golang:1.23-alpine AS builder

# 静态编译：pgx 走纯 Go 路径，实测 linux/amd64 与 linux/arm64 均可 CGO_ENABLED=0。
ENV CGO_ENABLED=0 \
    GOOS=linux

# **必须 auto，不能 local**：`local` 会禁止工具链切换，直接报
# `go.mod requires go >= 1.26.0 (running go 1.23.12; GOTOOLCHAIN=local)`。
# 而 auto 才让上面那段「版本由 go.mod 决定」成立。
# 实测 golang:1.23-alpine 镜像内 GOTOOLCHAIN 默认就是 auto，此处显式写出只为不依赖默认值。
ENV GOTOOLCHAIN=auto

# Go 模块代理。**默认用 goproxy.cn**，不是官方源。
#
# 改默认值而非只留注释说明的原因：官方源在国内容器内是**实测不通**的 ——
# `dial tcp 142.250.198.81:443: connect: connection refused`。
# 留官方源作默认值意味着「开箱即用的构建必然失败」，而失败点在下载工具链，
# 报错（`go: download go1.26.9: ... connection refused`）完全看不出是代理选错。
#
# 生产环境若需官方源：`--build-arg GOPROXY=https://proxy.golang.org,direct`。
# 注意：必须在 go mod download 之前声明，否则那个 RUN 取不到值。
ARG GOPROXY=https://goproxy.cn,direct
ENV GOPROXY=${GOPROXY}

WORKDIR /src

# 先只 COPY go.mod / go.sum 再 download：依赖未变时这层直接命中缓存，
# 改代码不会触发重新下载模块。go.sum 缺失时 download 的报错信息足够明确。
COPY go.mod go.sum ./

# **工具链自举 + 版本一致性校验（2026-10-12 新增）**
#
# 第一步 `go mod download` 会触发 Go 的 toolchain 机制：读到 go.mod 的
# `toolchain go1.26.9` 就自动下载并切换到该版本。第二步校验切换**真的发生了**。
#
# 为什么校验必须放在 download **之后**：download 才是触发自举的动作，
# 放在它之前量到的必然是基础镜像的旧版本（go1.23.12），会误报不一致。
#
# 为什么仍要校验（既然 auto 会自动切换）：自举失败时报的是
# `go: download go1.26.9: ... connection refused` —— 说的是下载失败，
# 而调用方真正要判断的是「这次构建用的编译器对不对」。校验把这件事变成显式断言：
# 版本不符即构建失败，且报错直接指出 go.mod 要求什么、实际用了什么。
RUN --mount=type=cache,target=/go/pkg/mod \
    set -eu; \
    go mod download; \
    want="$(awk '/^toolchain /{print $2; exit}' go.mod)"; \
    got="go$(go env GOVERSION | sed 's/^go//')"; \
    if [ "$want" != "$got" ]; then \
        echo "构建中止：实际使用的 Go 工具链与 go.mod 不一致"; \
        echo "  go.mod toolchain = $want"; \
        echo "  实际工具链= $got"; \
        echo "常见真因：GOPROXY 不可达导致工具链自举失败（当前 ${GOPROXY}）"; \
        exit 1; \
    fi; \
    echo "工具链已自举并对齐：$got"

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
