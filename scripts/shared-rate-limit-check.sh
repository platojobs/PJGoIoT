#!/usr/bin/env bash
# 限流边界真跑：同一额度（3 次/分钟，burst=3）在两种后端下的跨实例表现。
#
#   memory  ：进程内令牌桶 —— 两个实例各自计数，总额度 = 3×2 = 6（已知限制）。
#   postgres：共享令牌桶 —— 两个实例共用一行计数，总额度 = 3（跨实例一致）。
#
# 结论会写进 docs/slo.md 的「容量假设」与 README 的「限流边界」。
#
# 用法：bash scripts/shared-rate-limit-check.sh
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

COMPOSE=(docker compose -f docker-compose.yml -f deploy/compose.multi-instance-test.yml)
API1="http://127.0.0.1:18081"
API2="http://127.0.0.1:18082"
PG_USER="${POSTGRES_USER:-pjgoiot}"
PG_DB="${POSTGRES_DB:-pjgoiot}"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

FAIL=0
pass() { printf '  [PASS] %s\n' "$1"; }
fail() { printf '  [FAIL] %s\n' "$1"; FAIL=$((FAIL + 1)); }
info() { printf '\n== %s ==\n' "$1"; }

wait_healthy() {
	local svc=$1 cid state
	for _ in $(seq 1 150); do
		cid="$("${COMPOSE[@]}" ps -q "$svc" 2>/dev/null || true)"
		if [[ -n "$cid" ]]; then
			state="$(docker inspect -f '{{.State.Health.Status}}' "$cid" 2>/dev/null || echo unknown)"
			[[ "$state" == "healthy" ]] && return 0
		fi
		sleep 1
	done
	return 1
}

recreate() {
	# 通过环境变量切换后端/额度；compose 会重建两个 API 容器。
	RATE_LIMIT_BACKEND="$1" RATE_LIMIT_PER_MINUTE=3 RATE_LIMIT_BURST=3 \
		"${COMPOSE[@]}" up -d api1 api2 >/dev/null 2>&1
	wait_healthy api1
	wait_healthy api2
}

probe() {
	local base=$1 out="$TMP/probe-body"
	curl -sS --max-time 10 -o "$out" -w '%{http_code}' "$base/api/v1/device-types" || true
}

# 交替请求两个实例各 4 次，输出每次的状态码。
burst_probe() {
	local i base code
	: >"$TMP/codes"
	for ((i = 0; i < 8; i++)); do
		if ((i % 2 == 0)); then base="$API1"; else base="$API2"; fi
		code="$(probe "$base")"
		printf '%s\t%s\n' "$base" "${code:-000}" >>"$TMP/codes"
	done
	cat "$TMP/codes"
}

info "场景 A：RATE_LIMIT_BACKEND=memory（进程内计数）"
recreate memory
CODES_A="$(burst_probe)"
printf '%s\n' "$CODES_A" | awk -F'\t' '{printf "  %s -> %s\n", $1, $2}'
ALLOWED_A="$(printf '%s\n' "$CODES_A" | awk -F'\t' '$2 == "200"' | wc -l | tr -d ' ')"
LIMITED_A="$(printf '%s\n' "$CODES_A" | awk -F'\t' '$2 == "429"' | wc -l | tr -d ' ')"
printf '  放行 %s 次、限流 %s 次\n' "$ALLOWED_A" "$LIMITED_A"
if [[ "$ALLOWED_A" == "6" ]]; then
	pass "memory 后端下双实例总额度=6（配置 3×实例数 2），确认进程内计数会放大额度"
else
	fail "memory 后端预期放行 6 次，实际 $ALLOWED_A"
fi

info "场景 B：RATE_LIMIT_BACKEND=postgres（共享令牌桶）"
"${COMPOSE[@]}" exec -T postgres psql -U "$PG_USER" -d "$PG_DB" -tA \
	-c "DELETE FROM rate_limit_buckets" >/dev/null
recreate postgres
CODES_B="$(burst_probe)"
printf '%s\n' "$CODES_B" | awk -F'\t' '{printf "  %s -> %s\n", $1, $2}'
ALLOWED_B="$(printf '%s\n' "$CODES_B" | awk -F'\t' '$2 == "200"' | wc -l | tr -d ' ')"
BOTH_429="$(printf '%s\n' "$CODES_B" | awk -F'\t' '$2 == "429" {print $1}' | sort -u | wc -l | tr -d ' ')"
printf '  放行 %s 次、两个实例都出现过 429 的实例数=%s\n' "$ALLOWED_B" "$BOTH_429"
if [[ "$ALLOWED_B" == "3" && "$BOTH_429" == "2" ]]; then
	pass "postgres 后端下双实例总额度=3，且两个实例都执行共享拒绝"
else
	fail "postgres 后端预期放行 3 次且两实例都拒绝，实际放行 $ALLOWED_B、拒绝实例数 $BOTH_429"
fi

# 恢复默认配置，避免把测试额度留给后续脚本。
RATE_LIMIT_BACKEND=memory RATE_LIMIT_PER_MINUTE=120 RATE_LIMIT_BURST=40 \
	"${COMPOSE[@]}" up -d api1 api2 >/dev/null 2>&1

printf '\n===== 汇总 =====\n'
if [[ "$FAIL" == "0" ]]; then
	printf '限流结论：memory=每实例独立额度（部署约束见 README）；postgres=跨实例共享额度。\n'
else
	printf '存在 %d 项失败。\n' "$FAIL"
fi
exit "$FAIL"
