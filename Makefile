GO ?= go
APP_VERSION ?= 0.22.0
DATABASE_URL ?= postgres://platojobs@127.0.0.1:5432/pjgoiot?sslmode=disable

.PHONY: help dev dev-postgres test lint build migrate migrate-down migrate-version sqlc docker-build docker-up docker-down e2e-stack e2e-seed e2e-stop validate-dashboard

help: ## 显示可用命令
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-20s %s\n", $$1, $$2}'

dev: ## fixture 模式启动 API（无需数据库）
	STORE=fixture APP_VERSION=$(APP_VERSION) $(GO) run ./cmd/api

dev-postgres: ## Postgres 模式启动 API（需先 make migrate）
	STORE=postgres DATABASE_URL=$(DATABASE_URL) APP_VERSION=$(APP_VERSION) $(GO) run ./cmd/api

test: ## 运行全部测试（Postgres 集成测试需设置 TEST_DATABASE_URL）
	$(GO) test ./...

lint: ## gofmt 检查 + go vet
	@out=$$(gofmt -l . | grep -v '^internal/store/postgres/sqlcgen/' || true); \
	if [ -n "$$out" ]; then echo "以下文件未格式化:"; echo "$$out"; exit 1; fi
	$(GO) vet ./...

build: ## 编译全部包
	$(GO) build ./...

migrate: ## 执行数据库迁移到最新版本
	DATABASE_URL=$(DATABASE_URL) $(GO) run ./cmd/migrate -direction up

migrate-down: ## 回滚全部迁移（会删除表结构）
	DATABASE_URL=$(DATABASE_URL) $(GO) run ./cmd/migrate -direction down

migrate-version: ## 查看当前迁移版本
	DATABASE_URL=$(DATABASE_URL) $(GO) run ./cmd/migrate -version

sqlc: ## 重新生成 sqlc 代码（需先安装 sqlc）
	sqlc generate

docker-build: ## 构建 distroless 镜像
	docker build --build-arg APP_VERSION=$(APP_VERSION) -t pjgoiot-api:$(APP_VERSION) .

docker-up: ## 启动 Caddy + API + Postgres（见 docs/runbook.md）
	docker compose up --build

docker-down: ## 停止并清理容器
	docker compose down

e2e-stack: ## 启动真实 Postgres + API 联调栈（stdout 输出 BASE_URL）
	bash scripts/stack-for-e2e.sh

e2e-seed: ## 向 e2e 栈写入幂等的最小可核实数据
	bash scripts/seed-for-e2e.sh

e2e-stop: ## 停止并清理 e2e 栈
	bash scripts/stack-for-e2e.sh --stop

validate-dashboard: ## 校验 Grafana dashboard JSON 与引用的指标名
	bash scripts/validate-dashboard.sh
