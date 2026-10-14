# IIP M0 — 本地开发任务入口
# Windows 下若未安装 make，请使用等价的 scripts/tasks.ps1（同名函数）。
# 依赖版本与命令依据 docs/IIP-M0详细开发计划.md §2 T0.1。

SHELL := /bin/sh
GO    ?= go
PNPM  ?= pnpm

# 本地 PG（docker）默认连接串：与 T0.2 验证命令保持一致
PG_URL ?= postgres://postgres:dev@localhost:5432/iip?sslmode=disable
PG_CONTAINER ?= iip-pg
COMPOSE ?= docker compose -f deploy/docker-compose.yml

# 镜像标签与构建目标：一个 Dockerfile 出两个镜像（--target server / worker）
# 2026-10-14 T7.6：0.0-m1 -> 0.1.0。此前该值自 M2 起停滞在 0.0-m1（落后两代），
# 与 compose / verify-images.py / Helm chart 三处漂移；发版时五处必须同步改。
IMAGE_TAG ?= 0.1.0
IMAGE_PREFIX ?= iip

.PHONY: help deps compose-up compose-down run-server run-worker test lint migrate-up migrate-down web-dev pg-up pg-down tidy build image-build image-ls app-up app-down verify-images verify-runtime check-binaries run-local

help: ## 列出全部可用目标
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

deps: ## 拉取全部依赖（Go modules + 前端 pnpm）
	$(GO) mod download
	cd web && $(PNPM) install

compose-up: ## 起 deploy/docker-compose.yml 五服务（postgres/redis/prometheus/alertmanager/loki）
	$(COMPOSE) up -d

compose-down: ## 停止并移除五服务（保留 named volume 数据）
	$(COMPOSE) down

image-build: ## 构建 iip-server / iip-worker 镜像（多阶段，需先有 Docker daemon）
	docker build --target server -t $(IMAGE_PREFIX)-server:$(IMAGE_TAG) .
	docker build --target worker -t $(IMAGE_PREFIX)-worker:$(IMAGE_TAG) .

image-ls: ## 列出本项目镜像
	docker images "$(IMAGE_PREFIX)-server" "$(IMAGE_PREFIX)-worker"

app-up: image-build ## 构建镜像并起全部服务（含 iip-server/iip-worker）
	$(COMPOSE) --profile app up -d

app-down: ## 停止并移除全部服务（保留 named volume 数据）
	$(COMPOSE) --profile app down

verify-images: ## 容器链路验证：构建镜像 + 起服务 + 逐项断言 + 出报告 + 清理（需 Docker daemon）
	python scripts/verify-images.py

verify-runtime: ## 应用链路验证：真实 PG 从零建库 -> 迁移 -> 探活 -> 心跳 -> 退出（不需 Docker）
	python scripts/verify-runtime.py

check-binaries: ## 交叉编译 linux/amd64 并校验真静态链接（distroless 前提）
	python scripts/check-binaries.py

run-local: ## 无 Docker 的本地全栈启动（绿色版 PG + server + Vite）
	python scripts/run-local-nodocker.py

run-server: ## 运行 iip-server（需先 pg-up 且已设置 IIP_PG_URL）
	$(GO) run ./cmd/iip-server

run-worker: ## 运行 iip-worker
	$(GO) run ./cmd/iip-worker

test: ## 运行全部单测（带 race）
	$(GO) test -race -coverprofile=coverage.out ./...

lint: ## 运行 golangci-lint
	golangci-lint run ./...

migrate-up: ## 对本地 PG 执行迁移（up）
	migrate -path migrations -database "$(PG_URL)" up

migrate-down: ## 回滚全部迁移（down-all，验证可回滚）
	migrate -path migrations -database "$(PG_URL)" down-all

web-dev: ## 启动前端 dev server（5173）
	cd web && $(PNPM) dev

build: ## 编译二进制到 bin/
	$(GO) build -o bin/iip-server ./cmd/iip-server
	$(GO) build -o bin/iip-worker ./cmd/iip-worker

tidy: ## 整理 go.mod / go.sum
	$(GO) mod tidy

pg-up: ## 起本地 PG 容器（iip-pg）
	docker run --name $(PG_CONTAINER) -e POSTGRES_PASSWORD=dev -e POSTGRES_DB=iip -p 5432:5432 -d postgres:16-alpine

pg-down: ## 删除本地 PG 容器（含数据）
	docker rm -f $(PG_CONTAINER)
