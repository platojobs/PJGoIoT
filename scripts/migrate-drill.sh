#!/usr/bin/env bash
# migrate-drill.sh：迁移升级路径演练（可重复执行）。
#
# 三个场景：
#   1) 空库 0 -> 最新；
#   2) 构造 000001 时代的数据形态 -> 最新，抽查 platform/external_id/device_type/expires_at 回填；
#   3) 最新 -> 全部 down 到 version 1 -> up 到最新，验证业务行数不丢。
#
# 依赖：docker、go、python3。每次运行创建独立的 postgres 容器与三个临时库，
# 结束后删除容器，不污染本机数据库。可用 DRILL_PG_IMAGE 覆盖镜像。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

CONTAINER="${DRILL_PG_CONTAINER:-pjgoiot-drill-pg}"
IMAGE="${DRILL_PG_IMAGE:-postgres:16-alpine}"
DB_USER="${DRILL_PG_USER:-pjgoiot}"
DB_PASS="${DRILL_PG_PASSWORD:-drill-only}"
MIGRATE_BIN="$(mktemp -t pjgoiot-migrate.XXXXXX)"

cleanup() {
    docker rm -f "${CONTAINER}" >/dev/null 2>&1 || true
    rm -f "${MIGRATE_BIN}"
}
trap cleanup EXIT

if ! command -v docker >/dev/null 2>&1; then
    echo "缺少 docker，无法启动演练用 PostgreSQL" >&2
    exit 1
fi

PORT="$(python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1]);s.close()')"

echo "== 准备演练环境（镜像 ${IMAGE}，端口 ${PORT}）=="
docker rm -f "${CONTAINER}" >/dev/null 2>&1 || true
docker run -d --name "${CONTAINER}" \
    -e POSTGRES_USER="${DB_USER}" -e POSTGRES_PASSWORD="${DB_PASS}" \
    -p "127.0.0.1:${PORT}:5432" "${IMAGE}" >/dev/null

# 镜像启动阶段会先起一个临时实例再重启，pg_isready 可能在重启间隙短暂为真；
# 这里以真实查询为准，避免 CREATE DATABASE 撞上 "database system is shutting down"。
ready=0
for _ in $(seq 1 120); do
    if docker exec "${CONTAINER}" psql -U "${DB_USER}" -d postgres -tAc 'SELECT 1' >/dev/null 2>&1; then
        ready=1
        break
    fi
    sleep 0.5
done
if [ "$ready" -ne 1 ]; then
    echo "演练用 PostgreSQL 未就绪" >&2
    docker logs "${CONTAINER}" 2>&1 | tail -20 >&2
    exit 1
fi

create_db() {
    local db="$1"
    # 官方镜像初始化结束时会重启实例；CREATE 可能撞上重启窗口，重试即可。
    for _ in $(seq 1 30); do
        if docker exec "${CONTAINER}" psql -U "${DB_USER}" -d postgres -v ON_ERROR_STOP=1 \
            -c "CREATE DATABASE ${db};" >/dev/null 2>&1; then
            return 0
        fi
        sleep 1
    done
    return 1
}

for db in drill_empty drill_legacy drill_roundtrip; do
    create_db "${db}" || {
        echo "创建演练库 ${db} 失败" >&2
        docker logs "${CONTAINER}" 2>&1 | tail -20 >&2
        exit 1
    }
done

echo "== 编译 migrate =="
go build -o "${MIGRATE_BIN}" ./cmd/migrate

dsn() { echo "postgres://${DB_USER}:${DB_PASS}@127.0.0.1:${PORT}/$1?sslmode=disable"; }
migrate() { local db="$1"; shift; DATABASE_URL="$(dsn "${db}")" "${MIGRATE_BIN}" "$@"; }
psql_db() {
    local db="$1"; shift
    docker exec -i "${CONTAINER}" psql -U "${DB_USER}" -d "${db}" -v ON_ERROR_STOP=1 "$@"
}
version() {
    local db="$1"
    migrate "${db}" -version 2>&1 | sed -n 's/^version=/version=/p' || true
}
row_counts() {
    local db="$1" readings_sql="-1"
    if [[ "$(psql_db "${db}" -t -A -c "SELECT to_regclass('readings') IS NOT NULL;")" == "t" ]]; then
        readings_sql="(SELECT count(*) FROM readings)"
    fi
    psql_db "${db}" -t -A -F' ' -c "
        SELECT
            (SELECT count(*) FROM users) AS users,
            (SELECT count(*) FROM tokens) AS tokens,
            (SELECT count(*) FROM user_devices) AS user_devices,
            (SELECT count(*) FROM change_log) AS change_log,
            ${readings_sql} AS readings;" \
        | sed 's/^/  users tokens user_devices change_log readings = /'
}
fail() {
    echo "断言失败：$*" >&2
    exit 1
}

echo
echo "===== 场景 1：空库 0 -> 最新 ====="
START_VER="$(version drill_empty)"
echo "  起始：${START_VER:-none（空库）}"
migrate drill_empty -direction up
echo "  迁移后：$(version drill_empty)"
row_counts drill_empty

echo
echo "===== 场景 2：000001 时代数据 -> 最新 ====="
migrate drill_legacy -direction up -steps 1
echo "  迁移到：$(version drill_legacy)"

# 000001 形态：user_devices 只有 mac（无 platform/external_id/device_type），tokens 无 expires_at，
# change_log 的 payload 里只有 mac。
psql_db drill_legacy <<'SQL'
INSERT INTO users (id, kind, created_at) VALUES
    ('11111111-1111-4111-8111-111111111111', 'anonymous', '2025-12-01T00:00:00Z');

INSERT INTO tokens (id, user_id, token_hash, created_at) VALUES
    ('33333333-3333-4333-8333-333333333333', '11111111-1111-4111-8111-111111111111',
     'legacy-token-hash', '2025-12-01T00:00:00Z');

INSERT INTO user_devices (user_id, id, mac, alias, updated_at, server_seq) VALUES
    ('11111111-1111-4111-8111-111111111111', '22222222-2222-4222-8222-222222222222',
     'AA:BB:CC:00:00:01', 'legacy-device', '2025-12-02T00:00:00Z', 1);

INSERT INTO change_log (user_id, entity, entity_id, op, payload, server_ts) VALUES
    ('11111111-1111-4111-8111-111111111111', 'user_device', 'AA:BB:CC:00:00:01', 'upsert',
     '{"id":"22222222-2222-4222-8222-222222222222","mac":"AA:BB:CC:00:00:01","alias":"legacy-device"}',
     '2025-12-02T00:00:00Z');
SQL
echo "  写入 000001 形态数据完成：$(version drill_legacy)"
migrate drill_legacy -direction up
echo "  迁移到：$(version drill_legacy)"

DEVICE_FACTS="$(psql_db drill_legacy -t -A -c "
    SELECT platform || '|' || external_id || '|' || coalesce(device_type, 'NULL')
    FROM user_devices WHERE id = '22222222-2222-4222-8222-222222222222';")"
TOKEN_FACTS="$(psql_db drill_legacy -t -A -c "
    SELECT (expires_at = created_at + interval '180 days')::text
    FROM tokens WHERE token_hash = 'legacy-token-hash';")"
CHANGE_FACTS="$(psql_db drill_legacy -t -A -c "
    SELECT (payload ->> 'platform') || '|' || (payload ->> 'externalId') || '|' || entity_id
    FROM change_log WHERE entity = 'user_device';")"

echo "  回填抽查 user_devices(platform|external_id|device_type)：${DEVICE_FACTS}"
echo "  回填抽查 tokens(expires_at = created_at + 180d)：${TOKEN_FACTS}"
echo "  回填抽查 change_log(payload.platform|payload.externalId|entity_id)：${CHANGE_FACTS}"

[[ "${DEVICE_FACTS}" == "android|AA:BB:CC:00:00:01|nordic_uart" ]] || fail "device 回填不符：${DEVICE_FACTS}"
[[ "${TOKEN_FACTS}" == "true" ]] || fail "token expires_at 回填不符：${TOKEN_FACTS}"
[[ "${CHANGE_FACTS}" == "android|AA:BB:CC:00:00:01|11111111-1111-4111-8111-111111111111:android:AA:BB:CC:00:00:01" ]] \
    || fail "change_log 回填不符：${CHANGE_FACTS}"

echo
echo "===== 场景 3：最新 -> down 到 1 -> up 到最新 ====="
migrate drill_roundtrip -direction up
echo "  迁移到：$(version drill_roundtrip)"

psql_db drill_roundtrip <<'SQL'
INSERT INTO users (id, kind, created_at) VALUES
    ('aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa', 'anonymous', '2026-01-01T00:00:00Z');

INSERT INTO tokens (id, user_id, token_hash, created_at, expires_at) VALUES
    ('bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb', 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa',
     'roundtrip-token', '2026-01-01T00:00:00Z', '2026-06-30T00:00:00Z');

INSERT INTO user_devices (user_id, id, platform, external_id, mac, alias, updated_at, server_seq, device_type) VALUES
    ('aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa', 'cccccccc-cccc-4ccc-8ccc-cccccccccccc',
     'android', 'AA:BB:CC:00:00:02', 'aa:bb:cc:00:00:02', 'roundtrip-device',
     '2026-01-02T00:00:00Z', 7, 'environmental_sensor');

INSERT INTO change_log (user_id, entity, entity_id, op, payload, server_ts) VALUES
    ('aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa', 'user_device',
     'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa:android:AA:BB:CC:00:00:02', 'upsert',
     '{"id":"cccccccc-cccc-4ccc-8ccc-cccccccccccc","platform":"android","externalId":"AA:BB:CC:00:00:02"}',
     '2026-01-02T00:00:00Z');

INSERT INTO readings (id, user_id, platform, external_id, metric, value, unit, recorded_at, device_type) VALUES
    ('dddddddd-dddd-4ddd-8ddd-dddddddddddd', 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa',
     'android', 'AA:BB:CC:00:00:02', 'temperature_celsius', 21.5, 'celsius',
     '2026-01-02T00:00:00Z', 'environmental_sensor');
SQL

BEFORE="$(psql_db drill_roundtrip -t -A -F',' -c "
    SELECT (SELECT count(*) FROM users), (SELECT count(*) FROM tokens),
           (SELECT count(*) FROM user_devices), (SELECT count(*) FROM change_log);")"
echo "  down 前业务行数（users,tokens,user_devices,change_log）= ${BEFORE}"
row_counts drill_roundtrip

LATEST="$(version drill_roundtrip | sed -n 's/.*version=\([0-9][0-9]*\).*/\1/p')"
STEPS=$((LATEST - 1))
migrate drill_roundtrip -direction down -steps "${STEPS}"
echo "  回滚 ${STEPS} 步到：$(version drill_roundtrip)"
row_counts drill_roundtrip

migrate drill_roundtrip -direction up
echo "  重新迁移到：$(version drill_roundtrip)"
row_counts drill_roundtrip

AFTER="$(psql_db drill_roundtrip -t -A -F',' -c "
    SELECT (SELECT count(*) FROM users), (SELECT count(*) FROM tokens),
           (SELECT count(*) FROM user_devices), (SELECT count(*) FROM change_log);")"
echo "  up 后业务行数（users,tokens,user_devices,change_log）= ${AFTER}"
[[ "${BEFORE}" == "${AFTER}" ]] || fail "回滚往返后业务行数变化：${BEFORE} != ${AFTER}"

DEVICE_AFTER="$(psql_db drill_roundtrip -t -A -c "
    SELECT platform || '|' || external_id || '|' || coalesce(device_type, 'NULL')
    FROM user_devices WHERE id = 'cccccccc-cccc-4ccc-8ccc-cccccccccccc';")"
echo "  up 后重新推导字段：${DEVICE_AFTER}"
[[ "${DEVICE_AFTER}" == "android|AA:BB:CC:00:00:02|environmental_sensor" ]] \
    || fail "回滚往返后字段未正确推导：${DEVICE_AFTER}"

echo
echo "演练完成：三个场景均通过。"
echo "注意：000006 的 down 会删除 readings 表，因此跨 version 1 的往返会丢失读数（迁移语义，非数据损坏）；"
echo "      users/tokens/user_devices/change_log 的行数在往返后保持不丢。"
