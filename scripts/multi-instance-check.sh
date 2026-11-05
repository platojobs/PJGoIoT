#!/usr/bin/env bash
# 多实例真跑校验：两个 API 实例 + 同一 PostgreSQL + Caddy 负载均衡。
#
# 覆盖：
#   1. 请求是否真的被 Caddy 轮询到两个实例（X-Instance 响应头）；
#   2. 并发 push 打到两个实例时 change_log 的 seq 是否严格有序、客户端游标是否漏读；
#   3. 同一 Idempotency-Key 在实例 A 成功后重放到实例 B 的行为是否一致；
#   4. 停掉一个实例时 Caddy 是否摘流、持续请求是否出现 5xx（目标 0）。
#
# 依赖：docker compose；本机 curl、python3。脚本会以 RATE_LIMIT_PER_MINUTE=0 启动实例，
# 避免负载校验被限流干扰（限流本身由 scripts/shared-rate-limit-check.sh 单独验证）。
#
# 用法：
#   bash scripts/multi-instance-check.sh          # 构建并启动栈、执行校验，结束后保留栈
#   NO_BUILD=1 bash scripts/multi-instance-check.sh
#   DOWN_AFTER=1 bash scripts/multi-instance-check.sh   # 结束后 docker compose down
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

COMPOSE=(docker compose -f docker-compose.yml -f deploy/compose.multi-instance-test.yml)
API1="http://127.0.0.1:18081"
API2="http://127.0.0.1:18082"
PUBLIC="https://localhost"
PG_USER="${POSTGRES_USER:-pjgoiot}"
PG_DB="${POSTGRES_DB:-pjgoiot}"

TMP="$(mktemp -d)"
trap '[[ -n "${POLLER_PID:-}" ]] && kill "$POLLER_PID" 2>/dev/null || true; rm -rf "$TMP"' EXIT
FAIL=0

pass() { printf '  [PASS] %s\n' "$1"; }
fail() { printf '  [FAIL] %s\n' "$1"; FAIL=$((FAIL + 1)); }
info() { printf '\n== %s ==\n' "$1"; }

# ---------- 基础设施 ----------
wait_healthy() {
	# 等容器 healthcheck 变成 healthy（healthcheck 走 readiness，含数据库 Ping）。
	local svc=$1 cid state
	for _ in $(seq 1 150); do
		cid="$("${COMPOSE[@]}" ps -q "$svc" 2>/dev/null || true)"
		if [[ -n "$cid" ]]; then
			state="$(docker inspect -f '{{.State.Health.Status}}' "$cid" 2>/dev/null || echo unknown)"
			[[ "$state" == "healthy" ]] && return 0
		fi
		sleep 1
	done
	printf '实例 %s 未在 150s 内就绪\n' "$svc" >&2
	return 1
}

wait_caddy() {
	for _ in $(seq 1 60); do
		if curl -k -sS --max-time 5 -o /dev/null "$PUBLIC/healthz" 2>/dev/null; then
			return 0
		fi
		sleep 1
	done
	printf 'Caddy 未就绪\n' >&2
	return 1
}

psql_q() {
	"${COMPOSE[@]}" exec -T postgres psql -U "$PG_USER" -d "$PG_DB" -tA -c "$1"
}

lb_probe() {
	# 通过 Caddy 发 n 次请求，输出每个实例被命中的次数。
	local n=$1 i hdr inst
	: >"$TMP/lb"
	for ((i = 0; i < n; i++)); do
		hdr="$TMP/lb-h"
		curl -k -sS --max-time 10 -D "$hdr" -o /dev/null "$PUBLIC/api/v1/device-types"
		inst="$(grep -i '^x-instance:' "$hdr" | tr -d '\r' | awk '{print $2}')"
		printf '%s\n' "${inst:-none}" >>"$TMP/lb"
	done
	sort "$TMP/lb" | uniq -c
}

info "启动双实例栈（RATE_LIMIT_PER_MINUTE=0）"
if [[ "${NO_BUILD:-0}" == "1" ]]; then
	RATE_LIMIT_PER_MINUTE=0 RATE_LIMIT_BURST=0 "${COMPOSE[@]}" up -d
else
	RATE_LIMIT_PER_MINUTE=0 RATE_LIMIT_BURST=0 "${COMPOSE[@]}" up -d --build
fi
wait_healthy api1
wait_healthy api2
wait_caddy

# ---------- 1. 负载均衡分布 ----------
info "1) Caddy 轮询是否真的落到两个实例"
LB_OUT="$(lb_probe 20)"
printf '%s\n' "$LB_OUT"
if printf '%s\n' "$LB_OUT" | grep -q 'api1' && printf '%s\n' "$LB_OUT" | grep -q 'api2'; then
	pass "20 次请求同时命中 api1 与 api2"
else
	fail "请求没有同时命中两个实例（检查 Caddy upstream 配置）"
fi

# ---------- 2. 跨实例鉴权 + 幂等 ----------
info "2) 跨实例鉴权与幂等"
AUTH_JSON="$(curl -sS --max-time 10 -X POST "$API1/api/v1/auth/anonymous")"
TOKEN="$(printf '%s' "$AUTH_JSON" | python3 -c 'import json,sys; print(json.load(sys.stdin)["token"])')"
USER_ID="$(printf '%s' "$AUTH_JSON" | python3 -c 'import json,sys; print(json.load(sys.stdin)["userId"])')"
printf '  userId=%s（token 在 api1 创建）\n' "$USER_ID"

# 用 api1 签发的 token 直接访问 api2 的鉴权接口。
CODE_API2="$(curl -sS --max-time 10 -o "$TMP/me-api2.json" -w '%{http_code}' \
	-H "Authorization: Bearer $TOKEN" "$API2/api/v1/my/devices")"
if [[ "$CODE_API2" == "200" ]]; then
	pass "api1 签发的 token 在 api2 可用（token 存储在共享库）"
else
	fail "api1 签发的 token 在 api2 返回 $CODE_API2"
fi

IDEM_KEY="multi-idem-$(date +%s)-$$"
IDEM_ID="$(python3 -c 'import uuid; print(uuid.uuid4())')"
IDEM_MAC="$(python3 -c 'import random; print("BB:CC:%02X:%02X:%02X:%02X" % tuple(random.randrange(256) for _ in range(4)))')"
IDEM_TS="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
IDEM_BODY="$(printf '{"clientId":"v011-idem","devices":[{"id":"%s","mac":"%s","deviceType":"sensor","updatedAt":"%s"}]}' "$IDEM_ID" "$IDEM_MAC" "$IDEM_TS")"

CODE_A="$(curl -sS --max-time 15 -D "$TMP/idem-a.h" -o "$TMP/idem-a.json" -w '%{http_code}' \
	-X POST "$API1/api/v1/sync/push" -H "Authorization: Bearer $TOKEN" \
	-H "Content-Type: application/json" -H "Idempotency-Key: $IDEM_KEY" -d "$IDEM_BODY")"
CODE_B="$(curl -sS --max-time 15 -D "$TMP/idem-b.h" -o "$TMP/idem-b.json" -w '%{http_code}' \
	-X POST "$API2/api/v1/sync/push" -H "Authorization: Bearer $TOKEN" \
	-H "Content-Type: application/json" -H "Idempotency-Key: $IDEM_KEY" -d "$IDEM_BODY")"
REPLAYED="$(grep -i '^idempotent-replayed:' "$TMP/idem-b.h" | tr -d '\r' | awk '{print $2}')"
if [[ "$CODE_A" == "200" && "$CODE_B" == "200" && "$REPLAYED" == "true" ]] && cmp -s "$TMP/idem-a.json" "$TMP/idem-b.json"; then
	pass "同一 key/请求体：api1 首次成功，api2 重放返回 200 + Idempotent-Replayed，响应体一致"
else
	fail "幂等跨实例不一致（A=$CODE_A B=$CODE_B replayed=${REPLAYED:-未设置}）"
fi

# 同一 key、不同请求体必须 409。
IDEM_BODY2="${IDEM_BODY/\"deviceType\":\"sensor\"/\"deviceType\":\"meter\"}"
CODE_C="$(curl -sS --max-time 15 -o "$TMP/idem-c.json" -w '%{http_code}' \
	-X POST "$API2/api/v1/sync/push" -H "Authorization: Bearer $TOKEN" \
	-H "Content-Type: application/json" -H "Idempotency-Key: $IDEM_KEY" -d "$IDEM_BODY2")"
if [[ "$CODE_C" == "409" ]] && grep -q 'IDEMPOTENCY_CONFLICT' "$TMP/idem-c.json"; then
	pass "同一 key 不同请求体在实例 B 返回 409 IDEMPOTENCY_CONFLICT"
else
	fail "同一 key 不同请求体应返回 409，实际 $CODE_C"
fi

# ---------- 3. 并发 push + 游标不漏读 ----------
info "3) 并发 push（两个实例）+ 客户端游标不漏读"
python3 - "$TMP/devices" <<'PY'
import random
import sys
import uuid

with open(sys.argv[1], "w") as f:
    for _ in range(30):
        mac = "CC:DD:%02X:%02X:%02X:%02X" % tuple(random.randrange(256) for _ in range(4))
        f.write("%s\t%s\n" % (uuid.uuid4(), mac))
PY

: >"$TMP/seqs"
: >"$TMP/cursor"
: >"$TMP/poll-errors"
echo 0 >"$TMP/cursor"

poll_once() {
	local base=$1 cursor resp
	cursor="$(cat "$TMP/cursor")"
	resp="$(curl -sS --max-time 10 -H "Authorization: Bearer $TOKEN" \
		"$base/api/v1/sync/changes?cursor=$cursor&limit=100" || true)"
	if [[ -z "$resp" ]]; then
		printf 'poll %s cursor=%s empty\n' "$base" "$cursor" >>"$TMP/poll-errors"
		return 0
	fi
	printf '%s' "$resp" | python3 -c '
import json
import sys

data = json.load(sys.stdin)
for ch in data.get("changes") or []:
    payload = ch.get("payload") or {}
    # change_log 的 entity_id 是 user:platform:mac，设备 UUID 在 payload.id。
    entity_id = payload.get("id", "") if isinstance(payload, dict) else ""
    print("%s\t%s" % (ch.get("seq"), entity_id))
open(sys.argv[1], "w").write(str(data.get("nextCursor") or 0))
' "$TMP/cursor" >>"$TMP/seqs"
}

poll_loop() {
	while [[ ! -f "$TMP/stop-poller" ]]; do
		poll_once "$API1"
		poll_once "$API2"
		sleep 0.05
	done
}

poll_loop &
POLLER_PID=$!

push_one() {
	local base=$1 id=$2 mac=$3 out=$4 ts body
	ts="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
	body="$(printf '{"clientId":"v011-multi","devices":[{"id":"%s","mac":"%s","deviceType":"sensor","updatedAt":"%s"}]}' \
		"$id" "$mac" "$ts")"
	curl -sS --max-time 20 -o "$out" -w '%{http_code}' \
		-X POST "$base/api/v1/sync/push" \
		-H "Authorization: Bearer $TOKEN" \
		-H "Content-Type: application/json" \
		-d "$body"
}

# 用独立子 shell 承载所有 push，避免与后台 poller 混在同一个 wait 里。
pusher() {
	local i=0 base id mac
	while IFS=$'\t' read -r id mac; do
		if ((i % 2 == 0)); then base="$API1"; else base="$API2"; fi
		(
			code="$(push_one "$base" "$id" "$mac" "$TMP/push-$id.json" || true)"
			printf '%s\n' "${code:-000}" >"$TMP/code-$id"
		) &
		i=$((i + 1))
		if ((i % 6 == 0)); then wait; fi
	done <"$TMP/devices"
	wait
}
pusher &
PUSHER_PID=$!
wait "$PUSHER_PID"

# 所有 push 都应在各自实例成功。
BAD_PUSH=0
while IFS=$'\t' read -r id _; do
	if [[ "$(cat "$TMP/code-$id")" != "200" ]]; then BAD_PUSH=$((BAD_PUSH + 1)); fi
done <"$TMP/devices"

touch "$TMP/stop-poller"
wait "$POLLER_PID" || true

# 把游标推到最新（最多轮询 10 次直到 cursor 不变）。
for _ in $(seq 1 10); do
	before="$(cat "$TMP/cursor")"
	poll_once "$API1"
	if [[ "$(cat "$TMP/cursor")" == "$before" ]]; then break; fi
	sleep 0.2
done

# 数据库中的权威 seq 列表。
: >"$TMP/db-seqs"
psql_q "SELECT seq FROM change_log WHERE user_id = '$USER_ID' ORDER BY seq" >"$TMP/db-seqs"
python3 - "$TMP/devices" "$TMP/push-" "$TMP/applied-ids" <<'PY'
import json
import sys

with open(sys.argv[3], "w") as out:
    for line in open(sys.argv[1]):
        dev_id = line.split("\t")[0]
        try:
            data = json.load(open(sys.argv[2] + dev_id + ".json"))
        except (OSError, ValueError):
            continue
        for item in data.get("applied") or []:
            out.write("%s\n" % item.get("id", dev_id))
PY

if python3 - "$TMP/seqs" "$TMP/db-seqs" "$TMP/applied-ids" <<'PY'
import sys

fetched = [line.rstrip("\n").split("\t") for line in open(sys.argv[1]) if line.strip()]
db_seqs = [int(line) for line in open(sys.argv[2]) if line.strip()]
applied = set(line.strip() for line in open(sys.argv[3]) if line.strip())

seqs = [int(row[0]) for row in fetched]
ids = set(row[1] for row in fetched if row[1])

ok = True
if len(seqs) != len(set(seqs)):
    print("  [FAIL] 客户端游标返回了重复 seq")
    ok = False
if seqs != sorted(seqs):
    print("  [FAIL] 客户端游标返回的 seq 非严格递增")
    ok = False
if sorted(seqs) != sorted(db_seqs):
    missing = set(db_seqs) - set(seqs)
    extra = set(seqs) - set(db_seqs)
    print("  [FAIL] 客户端漏读/多读：missing=%s extra=%s" % (sorted(missing), sorted(extra)))
    ok = False
missing_ids = applied - ids
if missing_ids:
    print("  [FAIL] 已 applied 的设备未出现在变更日志：%s" % sorted(missing_ids))
    ok = False
print("  [INFO] push 成功=%d，拉取变更=%d，数据库变更=%d，严格递增=%s" %
      (len(applied), len(seqs), len(db_seqs), seqs == sorted(seqs)))
sys.exit(0 if ok else 1)
PY
then
	pass "并发 push 后 seq 严格递增，客户端游标与数据库变更集合完全一致（无漏读）"
else
	fail "并发 push 的 seq/游标一致性校验失败"
fi
if [[ "$BAD_PUSH" == "0" ]]; then
	pass "30 次 push 在两个实例上全部返回 200"
else
	fail "有 $BAD_PUSH 次 push 未返回 200"
fi

# ---------- 4. 摘流：停 api1 期间持续请求 ----------
info "4) 停掉 api1：Caddy 摘流与 5xx 计数"
traffic_loop() {
	local n=$1 out=$2 i code inst hdr
	: >"$out"
	for ((i = 0; i < n; i++)); do
		hdr="$TMP/traffic-h"
		code="$(curl -k -sS --max-time 10 -D "$hdr" -o /dev/null -w '%{http_code}' \
			"$PUBLIC/api/v1/device-types" || true)"
		inst="$(grep -i '^x-instance:' "$hdr" | tr -d '\r' | awk '{print $2}')"
		printf '%s\t%s\t%s\n' "$(date +%s)" "${code:-000}" "${inst:-none}" >>"$out"
		sleep 0.1
	done
}

traffic_loop 160 "$TMP/drain.log" &
LOAD_PID=$!
sleep 2
# docker compose stop 会发送 SIGTERM：实例先把 readiness 置 503（Caddy 摘流），
# 再继续服务 SHUTDOWN_DRAIN_SECONDS=8s，最后退出。
"${COMPOSE[@]}" stop api1 >/dev/null
wait "$LOAD_PID" || true

TOTAL_REQ="$(wc -l <"$TMP/drain.log" | tr -d ' ')"
BAD_REQ="$(awk -F'\t' '$2 != "200"' "$TMP/drain.log" | wc -l | tr -d ' ')"
BAD_502="$(awk -F'\t' '$2 == "502"' "$TMP/drain.log" | wc -l | tr -d ' ')"
LAST_INST="$(tail -20 "$TMP/drain.log" | awk -F'\t' '$2 == "200" {print $3}' | sort -u | tr '\n' ' ')"
printf '  停流窗口内请求=%s 非 200=%s 其中 502=%s；尾部命中实例=%s\n' \
	"$TOTAL_REQ" "$BAD_REQ" "$BAD_502" "$LAST_INST"
if [[ "$BAD_REQ" == "0" ]]; then
	pass "停掉一个实例期间 $TOTAL_REQ 次请求全部 200（Caddy 在 drain 窗口内完成摘流）"
else
	fail "停掉一个实例期间出现 $BAD_REQ 次非 200（502=$BAD_502）"
fi
if ! printf '%s' "$LAST_INST" | grep -q 'api1'; then
	pass "api1 停止后流量只落在 api2"
else
	fail "api1 已停止但仍有请求命中 api1"
fi

info "5) 恢复 api1 并确认重新接流量"
"${COMPOSE[@]}" start api1 >/dev/null
wait_healthy api1
# Caddy 每 2s 做一次健康检查；等一个检查周期再统计。
sleep 3
LB_OUT2="$(lb_probe 10)"
printf '%s\n' "$LB_OUT2"
if printf '%s\n' "$LB_OUT2" | grep -q 'api1'; then
	pass "api1 重启后重新加入负载均衡"
else
	fail "api1 重启后未被 Caddy 重新纳入负载均衡"
fi

printf '\n===== 汇总 =====\n'
if [[ "$FAIL" == "0" ]]; then
	printf '全部通过：双实例负载均衡、seq 有序、幂等跨实例、停实例零 5xx。\n'
else
	printf '存在 %d 项失败，请查看上方 [FAIL]。\n' "$FAIL"
fi

if [[ "${DOWN_AFTER:-0}" == "1" ]]; then
	"${COMPOSE[@]}" down
fi

exit "$FAIL"
