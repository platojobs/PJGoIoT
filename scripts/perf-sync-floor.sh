#!/usr/bin/env bash
# 多家庭 /sync/changes 结构与性能对照（v0.24.0 起，v0.25.0 复测）。
#
# 前置：
#   1. 已创建空库并迁移到最新版本；
#   2. 已执行 scripts/perf-multi-household-seed.sql。
#
# 用法：
#   DB_URL='postgres://platojobs@127.0.0.1:5432/pjgoiot_perf24?sslmode=disable' \
#     scripts/perf-sync-floor.sh
#
# 输出：环境条件、floor 子查询在 1/4/8/16 个家庭下的 p50/p95、旧主查询与
# 新主查询在 cursor=0 / 中间游标下的 p50/p95 与 buffer 数（Markdown 表格）。
set -euo pipefail

DB_URL="${DB_URL:-postgres://platojobs@127.0.0.1:5432/pjgoiot_perf24?sslmode=disable}"
USER_ID="00000000-0000-0000-0000-000000000001"
PERSONAL_ID="10000000-0000-0000-0000-000000000001"
RUNS="${RUNS:-30}"
LEGACY_RUNS="${LEGACY_RUNS:-10}"

need() { command -v "$1" >/dev/null 2>&1 || { echo "缺少依赖: $1" >&2; exit 1; }; }
need psql
need jq

psql_q() { psql "$DB_URL" -v ON_ERROR_STOP=1 -tA -c "$1"; }

# 恢复目标用户加入全部 16 个家庭，保证后续测量在同一可见性集合上。
psql_q "DELETE FROM household_members WHERE user_id='$USER_ID' AND household_id <> '$PERSONAL_ID';" >/dev/null
psql_q "INSERT INTO household_members (household_id, user_id, role, joined_at)
        SELECT ('10000000-0000-0000-0000-'||lpad(g::text,12,'0'))::uuid, '$USER_ID', 'member',
               now() - interval '180 days'
        FROM generate_series(2,16) g
        ON CONFLICT DO NOTHING;" >/dev/null

set_memberships() {
	local h="$1"
	psql_q "DELETE FROM household_members WHERE user_id='$USER_ID' AND household_id <> '$PERSONAL_ID';" >/dev/null
	if [ "$h" -gt 1 ]; then
		psql_q "INSERT INTO household_members (household_id, user_id, role, joined_at)
		        SELECT ('10000000-0000-0000-0000-'||lpad(g::text,12,'0'))::uuid, '$USER_ID', 'member',
		               now() - interval '180 days'
		        FROM generate_series(2,$h) g
		        ON CONFLICT DO NOTHING;" >/dev/null
	fi
}

# run_sql <runs> <sql> -> 每行一次 Execution Time（毫秒）
run_sql() {
	local n="$1" sql="$2"
	for _ in $(seq 1 "$n"); do
		psql "$DB_URL" -v ON_ERROR_STOP=1 -tA -c "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) $sql" \
			| jq -r '.[0]["Execution Time"]'
	done
}

floor_sql="SELECT min(x) FROM (
  SELECT min(seq) AS x FROM change_log WHERE user_id = '$USER_ID'
  UNION ALL
  SELECT (SELECT min(seq) FROM change_log WHERE household_id = m.household_id) AS x
  FROM household_members m WHERE m.user_id = '$USER_ID'
) s"

legacy_sql() {
	printf "SELECT c.seq, c.entity, c.entity_id, c.op, c.payload, c.server_ts
		FROM change_log c
		WHERE (c.user_id = '%s' OR c.household_id IN (
			SELECT hm.household_id FROM household_members hm WHERE hm.user_id = '%s'))
		  AND c.seq > %s
		ORDER BY c.seq
		LIMIT 101" "$USER_ID" "$USER_ID" "$1"
}

new_sql() {
	printf "SELECT DISTINCT ON (x.seq) x.seq, x.entity, x.entity_id, x.op, x.payload, x.server_ts
		FROM (
		  (SELECT c.seq, c.entity, c.entity_id, c.op, c.payload, c.server_ts
		   FROM change_log c
		   WHERE c.user_id = '%s' AND (c.user_id, c.seq) > ('%s'::uuid, %s::bigint)
		   ORDER BY c.seq LIMIT 101)
		  UNION ALL
		  (SELECT c2.seq, c2.entity, c2.entity_id, c2.op, c2.payload, c2.server_ts
		   FROM (SELECT household_id FROM household_members WHERE user_id = '%s') m
		   CROSS JOIN LATERAL (
			SELECT seq, entity, entity_id, op, payload, server_ts
			FROM change_log
			WHERE household_id = m.household_id AND seq > %s
			ORDER BY seq LIMIT 101
		   ) c2)
		) x
		ORDER BY x.seq
		LIMIT 101" "$USER_ID" "$USER_ID" "$1" "$USER_ID" "$1"
}

buffers() {
	local sql="$1"
	psql "$DB_URL" -v ON_ERROR_STOP=1 -tA -c "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) $sql" \
		| jq -r '.[0].Plan["Shared Hit Blocks"] + .[0].Plan["Shared Read Blocks"] + .[0].Plan["Shared Dirtied Blocks"]'
}

echo "## 环境"
echo
echo '| 项 | 值 |'
echo '| --- | --- |'
echo "| 数据库 | $(psql_q "select current_database()") |"
echo "| PostgreSQL | $(psql_q "select version()" | cut -d, -f1) |"
echo "| change_log 行数 | $(psql_q "select count(*) from change_log") |"
echo "| change_log 大小 | $(psql_q "select pg_size_pretty(pg_total_relation_size('change_log'))") |"
echo "| CPU | $(sysctl -n machdep.cpu.brand_string 2>/dev/null || uname -m) / $(sysctl -n hw.ncpu 2>/dev/null || nproc) 核 |"
echo "| 内存 | $(sysctl -n hw.memsize 2>/dev/null | awk '{printf "%.1f GiB", $1/1024/1024/1024}' || echo unknown) |"
echo "| shared_buffers | $(psql_q "show shared_buffers") |"
echo "| work_mem | $(psql_q "show work_mem") |"
echo "| effective_cache_size | $(psql_q "show effective_cache_size") |"
echo

echo "## 可见性下限（本人 MIN + 每家庭 MIN）"
echo
echo '| 家庭数 | 样本 | p50 (ms) | p95 (ms) | 均值 (ms) | max (ms) | buffers |'
echo '| --- | --- | --- | --- | --- | --- | --- |'
tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT
for h in 1 4 8 16; do
	set_memberships "$h"
	f="$tmpdir/floor_$h"
	run_sql "$RUNS" "$floor_sql" | sort -n >"$f"
	vals="$(awk '{a[n++]=$1;s+=$1;if($1>m)m=$1}
		END{idx=int((n-1)*50/100); p50=a[idx]; idx=int((n-1)*95/100); p95=a[idx];
		printf "%.3f %.3f %.3f %.3f", p50, p95, s/n, m}' "$f")"
	read -r p50 p95 avg max <<<"$vals"
	b="$(buffers "$floor_sql")"
	printf '| %d | %d | %s | %s | %s | %s | %s |\n' "$h" "$RUNS" "$p50" "$p95" "$avg" "$max" "$b"
done

echo
echo "## 主查询：旧 OR/IN 与新分支配对（16 个家庭，limit=100）"
echo
echo '| 游标 | 查询 | 样本 | p50 (ms) | p95 (ms) | max (ms) | buffers |'
echo '| --- | --- | --- | --- | --- | --- | --- |'
set_memberships 16
for cursor in 0 640000; do
	f="$tmpdir/legacy_$cursor"
	run_sql "$LEGACY_RUNS" "$(legacy_sql "$cursor")" | sort -n >"$f"
	vals="$(awk '{a[n++]=$1;if($1>m)m=$1}
		END{idx=int((n-1)*50/100); p50=a[idx]; idx=int((n-1)*95/100); p95=a[idx];
		printf "%.3f %.3f %.3f", p50, p95, m}' "$f")"
	read -r p50 p95 max <<<"$vals"
	printf '| %s | 旧 OR/IN | %d | %s | %s | %s | %s |\n' "$cursor" "$LEGACY_RUNS" "$p50" "$p95" "$max" "$(buffers "$(legacy_sql "$cursor")")"

	f="$tmpdir/new_$cursor"
	run_sql "$LEGACY_RUNS" "$(new_sql "$cursor")" | sort -n >"$f"
	vals="$(awk '{a[n++]=$1;if($1>m)m=$1}
		END{idx=int((n-1)*50/100); p50=a[idx]; idx=int((n-1)*95/100); p95=a[idx];
		printf "%.3f %.3f %.3f", p50, p95, m}' "$f")"
	read -r p50 p95 max <<<"$vals"
	printf '| %s | 新分支结构 | %d | %s | %s | %s | %s |\n' "$cursor" "$LEGACY_RUNS" "$p50" "$p95" "$max" "$(buffers "$(new_sql "$cursor")")"
done

echo
echo "说明：cursor=0 代表客户端首次全量同步，旧结构会从全局 seq 起点扫描到该用户"
echo "第一条可见行；新结构每个分支直接按复合索引定位，成本与全局历史量无关。"
