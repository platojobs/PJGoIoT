#!/usr/bin/env bash
# 起一套只在本机运行的「真实 Postgres + API」联调栈，供 Android / iOS 客户端 e2e 使用。
#
# 行为：
#   - 用 Docker 起一个独立的 Postgres 容器（不占用 compose 栈的 55432 端口）；
#   - 自动挑选空闲端口；端口被占用时重试并如实打印换端口过程；
#   - 自动执行迁移到最新 version，并打印版本；
#   - 用本机 go 进程启动 API（STORE=postgres、关闭限流），等 readiness 通过；
#   - 把连接信息写入 .e2e/stack.env，stdout 最后一行输出 BASE_URL=...（可直接 eval）；
#   - `--stop` 停止 API、删除 Postgres 容器并清理 .e2e 状态。
#
# 依赖：docker、go、curl、python3。详细说明见 docs/e2e-with-postgres.md。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
STATE_DIR="${E2E_STATE_DIR:-$ROOT/.e2e}"
STATE_FILE="$STATE_DIR/stack.env"
PG_IMAGE="${E2E_PG_IMAGE:-postgres:16-alpine}"
PG_USER="pjgoiot"
PG_PASSWORD="pjgoiot-e2e-only"
PG_DB="pjgoiot_e2e"
APP_VERSION="${APP_VERSION:-0.22.0}"

log() { printf '%s\n' "$*" >&2; }

usage() {
	cat <<'EOF'
用法:
  scripts/stack-for-e2e.sh           启动 Postgres + API；stdout 输出 BASE_URL=...
  scripts/stack-for-e2e.sh --stop    停止并清理
  scripts/stack-for-e2e.sh --status  打印当前状态

环境变量:
  E2E_STATE_DIR   状态与日志目录（默认 <repo>/.e2e）
  E2E_PG_IMAGE    Postgres 镜像（默认 postgres:16-alpine）
  E2E_PG_PORT     仅用于验证端口冲突：首次尝试该端口，占用则自动换端口
  E2E_API_PORT    同上，作用于 API
  APP_VERSION     写入 API 的 APP_VERSION（默认 0.22.0）
EOF
}

port_is_busy() {
	local port="$1"
	python3 - "$port" <<'PY'
import socket
import sys

port = int(sys.argv[1])
probe = socket.socket()
probe.settimeout(0.3)
try:
    probe.connect(("127.0.0.1", port))
    print("busy")
    sys.exit(0)
except OSError:
    pass
finally:
    probe.close()

bind = socket.socket()
try:
    bind.bind(("127.0.0.1", port))
except OSError:
    print("busy")
else:
    print("free")
finally:
    bind.close()
PY
}

free_port() {
	python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
}

load_state() {
	if [[ -f "$STATE_FILE" ]]; then
		# shellcheck disable=SC1090,SC1091
		source "$STATE_FILE"
	fi
}

save_state() {
	mkdir -p "$STATE_DIR"
	{
		printf 'PG_CONTAINER=%q\n' "${PG_CONTAINER:-}"
		printf 'PG_PORT=%q\n' "${PG_PORT:-}"
		printf 'DATABASE_URL=%q\n' "${DATABASE_URL:-}"
		printf 'API_PID=%q\n' "${API_PID:-}"
		printf 'API_PORT=%q\n' "${API_PORT:-}"
		printf 'BASE_URL=%q\n' "${BASE_URL:-}"
		printf 'MIGRATION_VERSION=%q\n' "${MIGRATION_VERSION:-}"
	} >"$STATE_FILE"
}

stop_stack() {
	load_state
	local stopped=0
	if [[ -n "${API_PID:-}" ]] && kill -0 "$API_PID" 2>/dev/null; then
		log "停止 API (pid=$API_PID)"
		kill "$API_PID" 2>/dev/null || true
		for _ in $(seq 1 40); do
			kill -0 "$API_PID" 2>/dev/null || break
			sleep 0.25
		done
		kill -9 "$API_PID" 2>/dev/null || true
		stopped=1
	fi
	if [[ -n "${PG_CONTAINER:-}" ]] && docker container inspect "$PG_CONTAINER" >/dev/null 2>&1; then
		log "删除 Postgres 容器 $PG_CONTAINER"
		docker rm -f "$PG_CONTAINER" >/dev/null 2>&1 || true
		stopped=1
	fi
	rm -rf "$STATE_DIR"
	if [[ $stopped -eq 1 ]]; then
		log "e2e 栈已停止，状态目录已清理: $STATE_DIR"
	else
		log "没有发现运行中的 e2e 栈；状态目录已清理: $STATE_DIR"
	fi
}

status_stack() {
	load_state
	if [[ -n "${BASE_URL:-}" ]] && curl -fsS --max-time 2 "$BASE_URL/api/v1/health" >/dev/null 2>&1; then
		log "运行中: $BASE_URL (postgres 127.0.0.1:${PG_PORT:-?}, 迁移 ${MIGRATION_VERSION:-?})"
		return 0
	fi
	log "未发现可用的 e2e 栈（${STATE_FILE}）"
	return 1
}

cleanup_on_error() {
	local rc
	rc=$?
	if [[ $rc -ne 0 ]]; then
		log "启动失败（exit=${rc}），清理已创建的资源"
		stop_stack >/dev/null 2>&1 || true
	fi
	exit "$rc"
}

start_postgres() {
	local attempt name port forced
	forced="${E2E_PG_PORT:-}"
	for attempt in 1 2 3; do
		if [[ "$attempt" == "1" && -n "$forced" ]]; then
			port="$forced"
		else
			port="$(free_port)"
		fi
		if [[ "$(port_is_busy "$port")" == "busy" ]]; then
			log "端口 ${port} 已被占用，自动换端口重试"
			continue
		fi
		name="pjgoiot-e2e-pg-$$-$attempt"
		log "启动 Postgres（第 $attempt 次尝试，端口 ${port}）"
		if docker run -d --rm --name "$name" \
			-e "POSTGRES_USER=$PG_USER" \
			-e "POSTGRES_PASSWORD=$PG_PASSWORD" \
			-e "POSTGRES_DB=$PG_DB" \
			-p "127.0.0.1:$port:5432" \
			"$PG_IMAGE" >/dev/null 2>"$STATE_DIR/pg.err"; then
			PG_CONTAINER="$name"
			PG_PORT="$port"
			DATABASE_URL="postgres://$PG_USER:$PG_PASSWORD@127.0.0.1:$port/${PG_DB}?sslmode=disable"
			save_state
			break
		fi
		if grep -qi 'port is already allocated\|address already in use' "$STATE_DIR/pg.err"; then
			log "端口 $port 已被占用，自动换端口重试"
			docker rm -f "$name" >/dev/null 2>&1 || true
			continue
		fi
		cat "$STATE_DIR/pg.err" >&2
		log "Postgres 容器启动失败"
		return 1
	done
	if [[ -z "${PG_CONTAINER:-}" ]]; then
		log "连续 3 次未能获得可用端口启动 Postgres"
		return 1
	fi

	log "等待 Postgres readiness（容器 ${PG_CONTAINER}）"
	for _ in $(seq 1 120); do
		if docker exec "$PG_CONTAINER" pg_isready -U "$PG_USER" -d "$PG_DB" >/dev/null 2>&1; then
			log "Postgres 已就绪: 127.0.0.1:$PG_PORT"
			return 0
		fi
		sleep 0.5
	done
	log "Postgres 在 60s 内未就绪"
	return 1
}

run_migrations() {
	local attempt version
	log "执行迁移到最新 version"
	# 刚启动的 postgres 容器会先跑 initdb 的临时实例，pg_isready 可能短暂为 true；
	# 此时连接会被重置，故对迁移做有限重试（迁移本身按版本幂等）。
	for attempt in 1 2 3 4 5; do
		if DATABASE_URL="$DATABASE_URL" go run ./cmd/migrate -direction up >&2; then
			break
		fi
		if [[ "$attempt" == "5" ]]; then
			log "迁移连续 5 次失败"
			return 1
		fi
		log "迁移连接失败（第 $attempt 次），2s 后重试"
		sleep 2
	done
	version="$(DATABASE_URL="$DATABASE_URL" go run ./cmd/migrate -version)"
	MIGRATION_VERSION="${version#version=}"
	MIGRATION_VERSION="${MIGRATION_VERSION%% *}"
	MIGRATION_VERSION="${MIGRATION_VERSION%% dirty*}"
	log "迁移完成: version=$MIGRATION_VERSION"
	save_state
}

start_api() {
	log "编译 API 二进制"
	go build -o "$STATE_DIR/api" ./cmd/api

	local attempt port pid forced
	forced="${E2E_API_PORT:-}"
	for attempt in 1 2 3; do
		if [[ "$attempt" == "1" && -n "$forced" ]]; then
			port="$forced"
		else
			port="$(free_port)"
		fi
		if [[ "$(port_is_busy "$port")" == "busy" ]]; then
			log "端口 ${port} 已被占用，自动换端口重试"
			continue
		fi
		log "启动 API（第 $attempt 次尝试，端口 ${port}）"
		nohup env STORE=postgres \
			DATABASE_URL="$DATABASE_URL" \
			RUN_MIGRATIONS_ON_BOOT=false \
			RATE_LIMIT_PER_MINUTE=0 \
			RATE_LIMIT_BURST=0 \
			PAIRING_CLAIM_RATE_PER_MINUTE=0 \
			PAIRING_CLAIM_BURST=0 \
			METRICS_ENABLED=true \
			APP_VERSION="$APP_VERSION" \
			ADDR="127.0.0.1:$port" \
			"$STATE_DIR/api" >"$STATE_DIR/api.log" 2>&1 &
		pid=$!
		API_PID="$pid"
		API_PORT="$port"
		BASE_URL="http://127.0.0.1:$port"
		save_state

		for _ in $(seq 1 120); do
			if curl -fsS --max-time 2 "$BASE_URL/api/v1/health" >/dev/null 2>&1; then
				log "API readiness 通过: $BASE_URL"
				return 0
			fi
			if ! kill -0 "$pid" 2>/dev/null; then
				break
			fi
			sleep 0.5
		done

		if grep -qi 'address already in use' "$STATE_DIR/api.log"; then
			log "端口 $port 已被占用，自动换端口重试"
			kill "$pid" 2>/dev/null || true
			API_PID=""
			BASE_URL=""
			save_state
			continue
		fi
		log "API 未通过 readiness，最后 30 行日志:"
		tail -n 30 "$STATE_DIR/api.log" >&2 || true
		return 1
	done
	log "连续 3 次未能获得可用端口启动 API"
	return 1
}

main() {
	case "${1:-}" in
	--stop)
		stop_stack
		return 0
		;;
	--status)
		status_stack
		return $?
		;;
	-h | --help)
		usage
		return 0
		;;
	"") ;;
	*)
		log "未知参数: $1"
		usage >&2
		return 2
		;;
	esac

	if [[ -f "$STATE_FILE" ]]; then
		load_state
		if [[ -n "${API_PID:-}" ]] && kill -0 "$API_PID" 2>/dev/null; then
			log "已有运行中的 e2e 栈: ${BASE_URL:-未知}"
			log "先执行 scripts/stack-for-e2e.sh --stop，或复用该栈"
			return 1
		fi
		rm -rf "$STATE_DIR"
	fi

	trap cleanup_on_error EXIT
	mkdir -p "$STATE_DIR"
	start_postgres
	run_migrations
	start_api

	trap - EXIT
	log "e2e 栈已就绪"
	log "状态文件: $STATE_FILE"
	log "下一步: scripts/seed-for-e2e.sh"
	printf 'BASE_URL=%s\n' "$BASE_URL"
}

main "$@"
