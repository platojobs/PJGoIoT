#!/usr/bin/env bash
# shellcheck disable=SC2317,SC2329  # trap 回调由信号触发，静态分析会误判为不可达/未调用
# 并发迁移验证：两个 API 进程同时启动、共享同一个空数据库。
#
# 目的：证明 golang-migrate 的数据库级 advisory lock 让“只有一个实例真正执行迁移，
# 另一个实例等待锁后看到已是最新版本”，而不是并发建表互相踩踏。
#
# 做法：
#   1. 新建空库；
#   2. 先由外部会话持有 golang-migrate 使用的同一把 advisory lock（5s），
#      确保两个 API 都必须在迁移入口等待；
#   3. 同时启动两个 cmd/api（RUN_MIGRATIONS_ON_BOOT=true）；
#   4. 断言两者都在锁释放后完成启动（耗时 >= 3.5s），且最终 version=10 dirty=false、
#      schema_migrations 只有一行。
#
# 依赖：docker compose（提供 postgres）、go、python3、curl。
#
# 用法：bash scripts/migration-concurrency-check.sh
#       KEEP_DB=1 bash scripts/migration-concurrency-check.sh   # 保留测试库
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

COMPOSE=(docker compose)
PG_USER="${POSTGRES_USER:-pjgoiot}"
PG_PORT="${POSTGRES_PORT:-55432}"
DB="${MIGRATION_CHECK_DB:-pjgoiot_migconc}"
DSN="postgres://${PG_USER}:${POSTGRES_PASSWORD:-pjgoiot-local-only}@127.0.0.1:${PG_PORT}/${DB}?sslmode=disable"

TMP="$(mktemp -d)"
cleanup() {
	# 结束可能仍在运行的 API 进程。
	if [[ -n "${API1_PID:-}" ]]; then kill "$API1_PID" 2>/dev/null || true; fi
	if [[ -n "${API2_PID:-}" ]]; then kill "$API2_PID" 2>/dev/null || true; fi
	wait 2>/dev/null || true
	rm -rf "$TMP"
}
trap cleanup EXIT

FAIL=0
pass() { printf '  [PASS] %s\n' "$1"; }
fail() { printf '  [FAIL] %s\n' "$1"; FAIL=$((FAIL + 1)); }
info() { printf '\n== %s ==\n' "$1"; }

admin_q() {
	"${COMPOSE[@]}" exec -T postgres psql -U "$PG_USER" -d postgres -tA -c "$1"
}
db_q() {
	"${COMPOSE[@]}" exec -T postgres psql -U "$PG_USER" -d "$DB" -tA -c "$1"
}
now_ms() { python3 -c 'import time; print(int(time.time() * 1000))'; }

info "准备空数据库 $DB"
"${COMPOSE[@]}" up -d postgres >/dev/null
for _ in $(seq 1 60); do
	admin_q 'SELECT 1' >/dev/null 2>&1 && break
	sleep 1
done
admin_q "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = '$DB'" >/dev/null 2>&1 || true
admin_q "DROP DATABASE IF EXISTS $DB" >/dev/null
admin_q "CREATE DATABASE $DB" >/dev/null
pass "已创建空库 $DB"

# golang-migrate 的锁 id = crc32("public\x00schema_migrations\x00<db>") * 1486364155 (uint32)
LOCK_ID="$(python3 - "$DB" <<'PY'
import sys
import zlib

db = sys.argv[1]
data = ("public\x00schema_migrations\x00" + db).encode()
print((zlib.crc32(data) * 1486364155) & 0xFFFFFFFF)
PY
)"
printf '  golang-migrate advisory lock id=%s\n' "$LOCK_ID"

info "构建 cmd/api 并持有迁移锁 5s"
go build -o "$TMP/api" ./cmd/api

# 外部会话取得同一把锁后休眠 5s；两个 API 启动时会在此等待。
"${COMPOSE[@]}" exec -T postgres psql -U "$PG_USER" -d "$DB" -v ON_ERROR_STOP=1 \
	-c "SELECT pg_advisory_lock($LOCK_ID);" -c "SELECT pg_sleep(5);" >/dev/null &
HOLDER_PID=$!

# 确认锁已被持有（返回 f 表示 try_lock 失败）。
for _ in $(seq 1 100); do
	if [[ "$(db_q "SELECT pg_try_advisory_lock($LOCK_ID)")" == "f" ]]; then break; fi
	sleep 0.1
done

START_MS="$(now_ms)"
STORE=postgres DATABASE_URL="$DSN" ADDR=127.0.0.1:19081 \
	RUN_MIGRATIONS_ON_BOOT=true METRICS_ENABLED=false SHUTDOWN_DRAIN_SECONDS=0 \
	APP_VERSION=0.14.0 "$TMP/api" >"$TMP/api1.log" 2>&1 &
API1_PID=$!
STORE=postgres DATABASE_URL="$DSN" ADDR=127.0.0.1:19082 \
	RUN_MIGRATIONS_ON_BOOT=true METRICS_ENABLED=false SHUTDOWN_DRAIN_SECONDS=0 \
	APP_VERSION=0.14.0 "$TMP/api" >"$TMP/api2.log" 2>&1 &
API2_PID=$!

READY1_MS=""
READY2_MS=""
for _ in $(seq 1 600); do
	now="$(now_ms)"
	if [[ -z "$READY1_MS" ]] && [[ "$(curl -sS --max-time 1 -o /dev/null -w '%{http_code}' "http://127.0.0.1:19081/api/v1/health" 2>/dev/null || true)" == "200" ]]; then
		READY1_MS="$now"
	fi
	if [[ -z "$READY2_MS" ]] && [[ "$(curl -sS --max-time 1 -o /dev/null -w '%{http_code}' "http://127.0.0.1:19082/api/v1/health" 2>/dev/null || true)" == "200" ]]; then
		READY2_MS="$now"
	fi
	[[ -n "$READY1_MS" && -n "$READY2_MS" ]] && break
	if ! kill -0 "$API1_PID" 2>/dev/null || ! kill -0 "$API2_PID" 2>/dev/null; then break; fi
	sleep 0.1
done

wait "$HOLDER_PID" || true

if [[ -z "$READY1_MS" || -z "$READY2_MS" ]]; then
	fail "至少一个 API 未能就绪（api1 日志：$(tail -3 "$TMP/api1.log" | tr '\n' ' ')）"
else
	ELAPSED1=$((READY1_MS - START_MS))
	ELAPSED2=$((READY2_MS - START_MS))
	printf '  api1 就绪耗时=%sms，api2 就绪耗时=%sms（锁被外部持有 5s）\n' "$ELAPSED1" "$ELAPSED2"
	if ((ELAPSED1 >= 3500 && ELAPSED2 >= 3500)); then
		pass "两个实例都在迁移锁上等待，未并发执行建表"
	else
		fail "就绪耗时不足，可能没有等待迁移锁（api1=${ELAPSED1}ms api2=${ELAPSED2}ms）"
	fi
fi

VERSION_LINE="$(db_q "SELECT version::text || ' dirty=' || dirty::text FROM schema_migrations" || true)"
ROWS="$(db_q 'SELECT count(*) FROM schema_migrations' || true)"
printf '  最终迁移状态：%s（schema_migrations 行数=%s）\n' "$VERSION_LINE" "$ROWS"
if [[ "$VERSION_LINE" == "10 dirty=false" && "$ROWS" == "1" ]]; then
	pass "最终 version=10 dirty=false，迁移版本表只有一行（无并发写坏）"
else
	fail "迁移终态异常：%s rows=%s" "$VERSION_LINE" "$ROWS"
fi

TABLES="$(db_q "SELECT count(*) FROM information_schema.tables WHERE table_schema='public' AND table_name IN ('users','user_devices','change_log','idempotency_keys','rate_limit_buckets')" || true)"
printf '  关键表存在数量=%s/5\n' "$TABLES"
if [[ "$TABLES" == "5" ]]; then
	pass "关键业务表与 rate_limit_buckets 均已创建"
else
	fail "关键表缺失（应有 5 张，实际 $TABLES）"
fi

printf '\n===== 汇总 =====\n'
if [[ "$FAIL" == "0" ]]; then
	printf '并发迁移安全：两个实例同时启动时由 advisory lock 串行化，只有一个执行迁移。\n'
else
	printf '存在 %d 项失败。\n' "$FAIL"
fi

if [[ "${KEEP_DB:-0}" != "1" ]]; then
	admin_q "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = '$DB'" >/dev/null 2>&1 || true
	admin_q "DROP DATABASE IF EXISTS $DB" >/dev/null || true
fi
exit "$FAIL"
