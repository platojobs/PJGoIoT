#!/usr/bin/env bash
# 场景路径长稳压测（v0.24.0）。
#
# 对 GET /scenes（列表读）与 PUT /scenes（场景写入）各持续压测 duration，
# 每分钟输出一个分窗（p50/p95/p99、RPS、错误率、状态码），并每 10 秒采样
# 应用连接池（/metrics 的 pjgoiot_db_pool_*）与服务端 pg_stat_activity，
# 用于观察连接池占用、等待与超时行为。
#
# 用法（应用已启动，且 token 对应真实用户）：
#   BASE_URL=http://127.0.0.1:18080 \
#   TOKEN=... \
#   DATABASE_URL='postgres://...' \
#   DURATION=15m WINDOW=1m CONCURRENCY=40 \
#   scripts/scenes-soak.sh
#
# 结果默认写入 docs/perf-results/v0.24.0-scenes-soak.md；原始采样 CSV 与
# loadgen 报告保留在同目录 /tmp 下（脚本退出前会说明路径）。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BASE_URL="${BASE_URL:-http://127.0.0.1:18080}"
TOKEN="${TOKEN:?需要 TOKEN}"
DATABASE_URL="${DATABASE_URL:-}"
DURATION="${DURATION:-15m}"
WINDOW="${WINDOW:-1m}"
CONCURRENCY="${CONCURRENCY:-40}"
OUT="${OUT:-$ROOT/docs/perf-results/v0.24.0-scenes-soak.md}"
SAMPLE_INTERVAL="${SAMPLE_INTERVAL:-10}"

need() { command -v "$1" >/dev/null 2>&1 || { echo "缺少依赖: $1" >&2; exit 1; }; }
need curl
need psql
need python3
need go

workdir="$(mktemp -d)"
sampler_pid=""
cleanup() {
	if [ -n "$sampler_pid" ] && kill -0 "$sampler_pid" 2>/dev/null; then
		kill "$sampler_pid" 2>/dev/null || true
		wait "$sampler_pid" 2>/dev/null || true
	fi
}
trap cleanup EXIT

mkdir -p "$(dirname "$OUT")"
mkdir -p "$ROOT/docs/perf-results"
csv="$workdir/pool.csv"
report="$workdir/loadgen.md"
api_metrics="$workdir/metrics-before.txt"
api_metrics_after="$workdir/metrics-after.txt"

metric() { # <file> <metric name>
	awk -v name="$2" '$1 == name {print $2}' "$1" | head -1
}

echo "时间,pool_open,pool_in_use,pool_idle,pool_max,pool_wait_total,pool_wait_seconds,pg_total,pg_active" >"$csv"
curl -sS "$BASE_URL/metrics" >"$api_metrics"

sampler() {
	local start
	start=$(date +%s)
	while :; do
		local now ts m p_open p_use p_idle p_max p_wait p_wait_sec pg_total pg_active
		now=$(date +%s)
		ts=$((now - start))
		# /metrics 本身要查一次数据库；池被打满时可能排队，给较长超时并重试。
		for _ in 1 2 3; do
			m=$(curl -sS --max-time 30 "$BASE_URL/metrics" 2>/dev/null || true)
			p_open=$(printf '%s' "$m" | awk '$1 == "pjgoiot_db_pool_open" {print $2}' | head -1)
			[ -n "$p_open" ] && break
			sleep 1
		done
		p_use=$(printf '%s' "$m" | awk '$1 == "pjgoiot_db_pool_in_use" {print $2}' | head -1)
		p_idle=$(printf '%s' "$m" | awk '$1 == "pjgoiot_db_pool_idle" {print $2}' | head -1)
		p_max=$(printf '%s' "$m" | awk '$1 == "pjgoiot_db_pool_max_open" {print $2}' | head -1)
		p_wait=$(printf '%s' "$m" | awk '$1 == "pjgoiot_db_pool_wait_total" {print $2}' | head -1)
		p_wait_sec=$(printf '%s' "$m" | awk '$1 == "pjgoiot_db_pool_wait_seconds_total" {print $2}' | head -1)
		pg_total="-"; pg_active="-"
		if [ -n "$DATABASE_URL" ]; then
			IFS=',' read -r pg_total pg_active <<<"$(psql "$DATABASE_URL" -tA -F ',' -c \
				"select count(*), count(*) filter (where state='active') from pg_stat_activity where datname=current_database();" 2>/dev/null || echo '-,-')"
		fi
		printf '%s,%s,%s,%s,%s,%s,%s,%s,%s\n' \
			"$ts" "${p_open:--}" "${p_use:--}" "${p_idle:--}" "${p_max:--}" "${p_wait:--}" "${p_wait_sec:--}" "$pg_total" "$pg_active" >>"$csv"
		sleep "$SAMPLE_INTERVAL"
	done
}

sampler &
sampler_pid=$!

echo "开始长稳压测: DURATION=${DURATION} (GET/PUT 各一次), WINDOW=${WINDOW}, 并发=${CONCURRENCY}"
loadgen_bin="$workdir/loadgen"
(cd "$ROOT" && go build -o "$loadgen_bin" ./cmd/loadgen)
"$loadgen_bin" \
	-base-url "$BASE_URL" \
	-token "$TOKEN" \
	-scenario scenes \
	-duration "$DURATION" \
	-warmup 10s \
	-window "$WINDOW" \
	-scenes-concurrency "$CONCURRENCY" \
	-report "$report"
curl -sS "$BASE_URL/metrics" >"$api_metrics_after"

cleanup
sampler_pid=""

python3 - "$csv" "$report" "$OUT" "$DURATION" "$WINDOW" "$CONCURRENCY" <<'PY'
import sys, collections

csv_path, report_path, out_path, duration, window, conc = sys.argv[1:7]
rows = []
with open(csv_path, encoding="utf-8") as f:
    header = f.readline().strip().split(",")
    for line in f:
        parts = line.strip().split(",")
        if len(parts) != len(header):
            continue
        rec = dict(zip(header, parts))
        try:
            rec["t"] = int(rec["时间"])
        except ValueError:
            continue
        rows.append(rec)

def num(rec, key):
    try:
        return float(rec[key])
    except (KeyError, ValueError):
        return 0.0

report = open(report_path, encoding="utf-8").read().strip()

out = []
out.append("# 场景路径长稳压测结果（v0.24.0）")
out.append("")
out.append("| 参数 | 值 |")
out.append("| --- | --- |")
out.append(f"| GET /scenes 持续时长 | {duration} |")
out.append(f"| PUT /scenes 持续时长 | {duration} |")
out.append(f"| 分窗长度 | {window} |")
out.append(f"| 并发 | {conc} |")
out.append(f"| 采样间隔 | {len(rows) and max(1, rows[1]['t'] - rows[0]['t']) or 0} s |")
out.append(f"| 采样点数 | {len(rows)} |")
out.append("")
out.append("## 压测汇总与分窗（客户端视角）")
out.append("")
out.append(report)
out.append("")
out.append("## 连接池与服务端连接采样")
out.append("")

if not rows:
    out.append("未采集到样本。")
else:
    in_use = [num(r, "pool_in_use") for r in rows]
    opens = [num(r, "pool_open") for r in rows]
    waits_totals = [num(r, "pool_wait_total") for r in rows]
    wait_secs = [num(r, "pool_wait_seconds") for r in rows]
    pools = [num(r, "pool_max") for r in rows]
    pg_active = [num(r, "pg_active") for r in rows if r.get("pg_active") not in ("-", None)]
    pg_total = [num(r, "pg_total") for r in rows if r.get("pg_total") not in ("-", None)]
    out.append("| 指标 | 值 |")
    out.append("| --- | --- |")
    out.append(f"| 连接池上限 | {int(max(pools)) if pools else 0} |")
    out.append(f"| in_use 平均 / 最大 | {sum(in_use)/len(in_use):.1f} / {max(in_use):.0f} |")
    out.append(f"| open 平均 / 最大 | {sum(opens)/len(opens):.1f} / {max(opens):.0f} |")
    out.append(f"| 池等待次数增量 | {int(max(waits_totals)-min(waits_totals)) if waits_totals else 0} |")
    out.append(f"| 池等待耗时增量 | {max(wait_secs)-min(wait_secs):.3f} s |")
    if pg_total:
        out.append(f"| 服务端连接（含本脚本）平均 / 最大 | {sum(pg_total)/len(pg_total):.1f} / {max(pg_total):.0f} |")
        out.append(f"| 服务端 active 平均 / 最大 | {sum(pg_active)/len(pg_active):.1f} / {max(pg_active):.0f} |")
    out.append("")
    out.append("| 时间段 (s) | in_use 平均 | in_use 最大 | pool_open 最大 | 池等待次数增量 | 服务端 active 最大 |")
    out.append("| --- | --- | --- | --- | --- | --- |")
    buckets = collections.OrderedDict()
    for r in rows:
        b = r["t"] // 60 * 60
        buckets.setdefault(b, []).append(r)
    prev_wait = None
    for b in sorted(buckets):
        group = buckets[b]
        # 每个分钟桶取该桶最后一条的累计等待次数，与上一桶比较得到增量。
        last_wait = num(group[-1], "pool_wait_total")
        delta = "" if prev_wait is None else str(int(last_wait - prev_wait))
        prev_wait = last_wait
        o = [num(x, "pool_in_use") for x in group]
        op = [num(x, "pool_open") for x in group]
        pa = [num(x, "pg_active") for x in group if x.get("pg_active") not in ("-", None)]
        out.append(f"| {b} | {sum(o)/len(o):.1f} | {max(o):.0f} | {max(op):.0f} | {delta} | {max(pa) if pa else '-'} |")
    out.append("")
    out.append("说明：`pool_wait_total` 是 database/sql 因池满排队等待的累计次数；")
    out.append("`in_use` 达到上限且等待次数持续增长时，P99 会明显抬升。")

open(out_path, "w", encoding="utf-8").write("\n".join(out) + "\n")
print(f"结果写入 {out_path}")
print(f"原始采样 CSV: {csv_path}")
print(f"loadgen 报告: {report_path}")
PY
