#!/usr/bin/env bash
# 滚动发布演练：逐实例摘流量 → 等待退出 → 重启 → 等就绪 → 恢复流量。
# 全程通过 Caddy 持续打业务请求，统计“停机请求数”（非 200），目标 0。
#
# 零停机的依据：API 收到 SIGTERM 后先把 /api/v1/health 置为 503，Caddy 在
# health_interval=2s、max_fails=1 内把该实例摘掉；API 继续服务
# SHUTDOWN_DRAIN_SECONDS（默认 8s）后才退出，因此摘流前的在途请求仍能完成。
#
# 用法：
#   bash scripts/rolling-restart.sh                 # 构建并启动栈后演练
#   NO_BUILD=1 bash scripts/rolling-restart.sh
#
# 说明：脚本会用 RATE_LIMIT_PER_MINUTE=0 启动实例，避免持续压测触发业务限流；
# 这样被观测到的非 200 只可能来自实例切换本身。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

COMPOSE=(docker compose -f docker-compose.yml)
PUBLIC="https://localhost"
TMP="$(mktemp -d)"
cleanup() {
	# 先让后台压测 worker 退出，再删除临时目录，避免它们继续读写已删除文件。
	touch "$TMP/stop-load" 2>/dev/null || true
	if [[ -n "${L1:-}" ]]; then kill "$L1" 2>/dev/null || true; fi
	if [[ -n "${L2:-}" ]]; then kill "$L2" 2>/dev/null || true; fi
	wait 2>/dev/null || true
	rm -rf "$TMP"
}
trap cleanup EXIT

FAIL=0
pass() { printf '  [PASS] %s\n' "$1"; }
fail() { printf '  [FAIL] %s\n' "$1"; FAIL=$((FAIL + 1)); }
info() { printf '\n== %s ==\n' "$1"; }

wait_healthy() {
	local svc=$1 cid state
	for _ in $(seq 1 180); do
		cid="$("${COMPOSE[@]}" ps -q "$svc" 2>/dev/null || true)"
		if [[ -n "$cid" ]]; then
			state="$(docker inspect -f '{{.State.Health.Status}}' "$cid" 2>/dev/null || echo unknown)"
			[[ "$state" == "healthy" ]] && return 0
		fi
		sleep 1
	done
	printf '实例 %s 未在 180s 内就绪\n' "$svc" >&2
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

lb_probe() {
	local n=$1 i hdr inst
	for ((i = 0; i < n; i++)); do
		hdr="$TMP/probe-h"
		curl -k -sS --max-time 10 -D "$hdr" -o /dev/null "$PUBLIC/api/v1/device-types"
		inst="$(grep -i '^x-instance:' "$hdr" | tr -d '\r' | awk '{print $2}')"
		printf '%s\n' "${inst:-none}"
	done
}

load_worker() {
	# 每 100ms 一个业务请求，记录 时间戳/状态码/实例。
	local worker=$1 out=$2 i=0 code inst hdr
	: >"$out"
	while [[ ! -f "$TMP/stop-load" ]]; do
		hdr="$TMP/h-$worker"
		code="$(curl -k -sS --max-time 10 -D "$hdr" -o /dev/null -w '%{http_code}' \
			"$PUBLIC/api/v1/device-types" || true)"
		inst="$(grep -i '^x-instance:' "$hdr" | tr -d '\r' | awk '{print $2}')"
		printf '%s\t%s\t%s\n' "$(date +%s)" "${code:-000}" "${inst:-none}" >>"$out"
		i=$((i + 1))
		sleep 0.1
	done
}

info "启动双实例栈（滚动演练期间关闭业务限流）"
if [[ "${NO_BUILD:-0}" == "1" ]]; then
	RATE_LIMIT_PER_MINUTE=0 RATE_LIMIT_BURST=0 "${COMPOSE[@]}" up -d
else
	RATE_LIMIT_PER_MINUTE=0 RATE_LIMIT_BURST=0 "${COMPOSE[@]}" up -d --build
fi
wait_healthy api1
wait_healthy api2
wait_caddy

CID1="$("${COMPOSE[@]}" ps -q api1)"
DRAIN="$(docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "$CID1" | grep '^SHUTDOWN_DRAIN_SECONDS=' | cut -d= -f2 || true)"
printf '  SHUTDOWN_DRAIN_SECONDS=%s（SIGTERM 后继续服务时长）\n' "${DRAIN:-8}"

info "开始持续请求（2 个 worker，约 20 req/s，经 Caddy）"
load_worker 1 "$TMP/load-1.log" &
L1=$!
load_worker 2 "$TMP/load-2.log" &
L2=$!
sleep 3

for svc in api1 api2; do
	info "滚动重启 $svc"
	started="$(date +%s)"
	# SIGTERM → readiness 503 → Caddy 摘流 → drain 窗口继续服务 → 进程退出。
	"${COMPOSE[@]}" stop "$svc" >/dev/null
	stopped="$(date +%s)"

	# 记录停流窗口内仍落到该实例的请求数（应只发生在 drain 早期，且全部成功）。
	served_during_stop="$(awk -F'\t' -v s="$started" -v e="$stopped" -v n="$svc" \
		'$1 >= s && $1 <= e && $3 == n' "$TMP/load-1.log" "$TMP/load-2.log" | wc -l | tr -d ' ')"
	printf '  SIGTERM 后 %ss 内该实例仍处理 %s 个请求（drain 窗口），随后退出\n' \
		"$((stopped - started))" "$served_during_stop"

	# 摘流确认：另一个实例必须承载全部请求，且不应出现 5xx。
	"${COMPOSE[@]}" start "$svc" >/dev/null
	wait_healthy "$svc"
	sleep 3

	instances="$(lb_probe 10 | sort -u | tr '\n' ' ')"
	if printf '%s' "$instances" | grep -q "$svc"; then
		pass "$svc 重启后重新接流量（近 10 次命中：${instances}）"
	else
		fail "$svc 重启后未被 Caddy 重新纳入（近 10 次命中：${instances}）"
	fi
done

touch "$TMP/stop-load"
wait "$L1" || true
wait "$L2" || true

info "滚动发布结果"
cat "$TMP/load-1.log" "$TMP/load-2.log" >"$TMP/load-all.log"
TOTAL="$(wc -l <"$TMP/load-all.log" | tr -d ' ')"
BAD="$(awk -F'\t' '$2 != "200"' "$TMP/load-all.log" | wc -l | tr -d ' ')"
BAD_502="$(awk -F'\t' '$2 == "502"' "$TMP/load-all.log" | wc -l | tr -d ' ')"
BAD_000="$(awk -F'\t' '$2 == "000"' "$TMP/load-all.log" | wc -l | tr -d ' ')"
PER_INSTANCE="$(awk -F'\t' '{c[$3]++} END {for (k in c) printf "%s=%d ", k, c[k]}' "$TMP/load-all.log")"
printf '  总请求=%s 非 200=%s（502=%s 连接失败=%s）\n' "$TOTAL" "$BAD" "$BAD_502" "$BAD_000"
printf '  实例分布：%s\n' "$PER_INSTANCE"

if [[ "$BAD" == "0" ]]; then
	pass "滚动重启全程 $TOTAL 次请求全部 200，停机请求数 = 0"
else
	fail "滚动重启期间出现 $BAD 次非 200（502=$BAD_502 连接失败=$BAD_000）"
fi

printf '\n===== 汇总 =====\n'
if [[ "$FAIL" == "0" ]]; then
	printf '滚动发布成功：两个实例依次重启，零停机请求。\n'
else
	printf '存在 %d 项失败。\n' "$FAIL"
fi
exit "$FAIL"
