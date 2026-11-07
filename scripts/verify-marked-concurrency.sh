#!/usr/bin/env bash
# 在真实 Postgres + 真实 HTTP 服务上验证「两个客户端同时操作同一台设备」的 markedAt 行为。
#
# 覆盖：
#   1. 两个客户端（同 platform + externalId、不同客户端设备 id）并发交错 push：
#      一个微秒时间戳、一个毫秒时间戳，最终落库取 LWW 较大者，且无重复 change_log 条目；
#   2. 显式 null（取消）与字段缺省（保持）交错：缺省不会清掉标记；null 后旧 updatedAt 再标记仍 conflict；
#   3. 回传服务端返回的时间戳做幂等重放：unchanged，不新增 change_log；
#   4. 这些写入不影响 /sync/changes 游标推进（含 HTTP 生成的其他 entity 与直接插入的未知 entity）。
#
# 用法：
#   scripts/verify-marked-concurrency.sh              # 自动起 e2e 栈，结束后清理
#   BASE_URL=... DATABASE_URL=... scripts/verify-marked-concurrency.sh   # 复用已有栈
#
# 依赖：bash、go、docker、curl、python3、psql（自动起栈时）。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
STARTED=0

if [[ -z "${BASE_URL:-}" || -z "${DATABASE_URL:-}" ]]; then
	log_file="$(mktemp)"
	if ! "$ROOT/scripts/stack-for-e2e.sh" >"$log_file"; then
		tail -20 "$log_file" >&2
		rm -f "$log_file"
		exit 1
	fi
	STARTED=1
	# shellcheck disable=SC1091
	source "$ROOT/.e2e/stack.env"
	rm -f "$log_file"
fi

cleanup() {
	if [[ "$STARTED" == "1" ]]; then
		"$ROOT/scripts/stack-for-e2e.sh" --stop >/dev/null 2>&1 || true
	fi
}
trap cleanup EXIT

export BASE_URL DATABASE_URL

python3 - "$@" <<'PY'
import datetime as dt
import json
import os
import subprocess
import sys
import uuid
import urllib.error
import urllib.request
from concurrent.futures import ThreadPoolExecutor

BASE = os.environ["BASE_URL"].rstrip("/")
DB = os.environ["DATABASE_URL"]
EXT = "AA:BB:CC:00:00:91"
MAC = "aa:bb:cc:00:00:91"
PLATFORM = "android"
ENTITY_ID = None  # 首次 push 后按 user id 拼出

rows = []
failures = []


def check(step, expected, observed):
    ok = expected == observed
    rows.append({"step": step, "expected": expected, "observed": observed, "ok": ok})
    if not ok:
        failures.append(f"{step}: 期望 {expected!r}，实际 {observed!r}")


def http(method, path, body=None, token=None):
    data = None if body is None else json.dumps(body).encode()
    req = urllib.request.Request(BASE + path, data=data, method=method)
    if data is not None:
        req.add_header("Content-Type", "application/json")
    if token:
        req.add_header("Authorization", "Bearer " + token)
    try:
        with urllib.request.urlopen(req, timeout=10) as resp:
            raw = resp.read()
            return resp.status, json.loads(raw) if raw else {}
    except urllib.error.HTTPError as e:
        raw = e.read()
        try:
            return e.code, json.loads(raw) if raw else {}
        except json.JSONDecodeError:
            return e.code, {"raw": raw.decode(errors="replace")}


def psql(sql):
    out = subprocess.check_output(
        ["psql", DB, "-v", "ON_ERROR_STOP=1", "-tA", "-c", sql],
        text=True,
    )
    return out.strip()


def count_entries():
    return int(psql(
        "SELECT count(*) FROM change_log WHERE entity='user_device' "
        f"AND entity_id='{ENTITY_ID}'"
    ))


def marked_value():
    """按唯一键读取当前 marked_at；客户端不同 id 会把服务端主键替换掉，不能按 id 查。"""
    return psql(
        "SELECT coalesce(to_char(marked_at AT TIME ZONE 'UTC', "
        "'YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"'), 'null') "
        f"FROM user_devices WHERE platform='{PLATFORM}' AND external_id='{EXT}'")


def device_payload(dev_id, updated_at, marked="__absent__", alias="E2E 并发设备"):
    d = {
        "id": dev_id,
        "platform": PLATFORM,
        "externalId": EXT,
        "mac": MAC,
        "alias": alias,
        "advertisedName": "ESP32-C3-DevKitM-1",
        "rules": {},
        "scanRecords": [],
        "updatedAt": updated_at,
    }
    if marked != "__absent__":
        d["markedAt"] = marked
    return d


def push(devices):
    return http("POST", "/api/v1/sync/push", {"devices": devices}, TOKEN)


def status_for(resp, dev_id):
    """返回 (status, reason)；status ∈ applied/unchanged/conflict/error。"""
    code, body = resp
    if code != 200:
        return "http_error", str(body)
    for a in body.get("applied") or []:
        if a.get("id") == dev_id:
            return a.get("status"), ""
    for c in body.get("conflicts") or []:
        if c.get("id") == dev_id:
            return "conflict", c.get("reason")
    return "missing", ""


now = dt.datetime.now(dt.timezone.utc)
base = (now - dt.timedelta(seconds=60)).replace(microsecond=0)


def ts(seconds, micros=0):
    return (base + dt.timedelta(seconds=seconds, microseconds=micros)).strftime(
        "%Y-%m-%dT%H:%M:%S.%fZ")


T0 = ts(0)
TM = ts(10, 123456)   # 微秒客户端
TL = ts(10, 123000)   # 毫秒客户端（更旧）
TA = ts(20)           # 缺省 markedAt
TN = ts(30)           # 显式 null
TABS2 = ts(35)        # 取消后再来缺省
TOLD = ts(25)         # 旧时间戳再标记
TNEW = ts(50, 777000) # 新时间戳再标记

# 0) 建立独立用户与设备
code, login = http("POST", "/api/v1/auth/anonymous")
if code != 201:
    print("创建匿名用户失败:", code, login)
    sys.exit(1)
TOKEN = login["token"]
UID = login["userId"]
ENTITY_ID = f"{UID}:{PLATFORM}:{EXT}"

id0 = str(uuid.uuid4())
code, body = push([device_payload(id0, T0)])
check("建立基线设备（无标记）", "applied", status_for((code, body), id0)[0])
base_count = count_entries()
check("基线 change_log 条目数", 1, base_count)

# 1) 两个客户端并发交错：微秒 vs 毫秒
id_micro = str(uuid.uuid4())
id_milli = str(uuid.uuid4())
rounds = 10
for i in range(rounds):
    with ThreadPoolExecutor(max_workers=2) as ex:
        # 奇偶轮交换提交顺序，覆盖两种交错：微秒先到 / 毫秒先到。
        if i % 2 == 0:
            r_micro = ex.submit(push, [device_payload(id_micro, TM, TM)])
            r_milli = ex.submit(push, [device_payload(id_milli, TL, TL)])
        else:
            r_milli = ex.submit(push, [device_payload(id_milli, TL, TL)])
            r_micro = ex.submit(push, [device_payload(id_micro, TM, TM)])
        resp_micro, resp_milli = r_micro.result(), r_milli.result()

    s_micro, why_micro = status_for(resp_micro, id_micro)
    s_milli, why_milli = status_for(resp_milli, id_milli)
    if s_micro not in ("applied", "unchanged"):
        check(f"并发第 {i+1} 轮 微秒客户端", "applied/unchanged", s_micro)
    if s_milli not in ("applied", "conflict", "unchanged"):
        check(f"并发第 {i+1} 轮 毫秒客户端", "applied/conflict", s_milli)
    if s_milli == "conflict" and why_milli not in ("server_newer", "clock_clamped"):
        check(f"并发第 {i+1} 轮 毫秒客户端冲突原因", "server_newer", why_milli)

final_marked = marked_value()
check("并发结束后落库为微秒值", TM, final_marked)
delta = count_entries() - base_count
check("并发合并新增 change_log 条目数 ∈ {1,2}", "1 或 2", "1 或 2" if delta in (1, 2) else delta)
dups = psql(
    "SELECT count(*) FROM (SELECT payload->>'markedAt', payload->>'updatedAt' "
    "FROM change_log WHERE entity='user_device' "
    f"AND entity_id='{ENTITY_ID}' GROUP BY 1,2 HAVING count(*) > 1) s")
check("无同一操作重复条目", 0, int(dups))

# 并发中毫秒值只可能落库一次（若它先赢得锁），之后应全部 conflict
milli_entries = int(psql(
    "SELECT count(*) FROM change_log WHERE entity='user_device' "
    f"AND entity_id='{ENTITY_ID}' AND (payload->>'markedAt')::timestamptz = timestamptz '{TL}'"))
check("毫秒时间戳条目数 ≤ 1", "≤1", "≤1" if milli_entries <= 1 else milli_entries)

# 2) 显式 null 与字段缺省交错
before = count_entries()
id_absent = str(uuid.uuid4())
code, body = push([device_payload(id_absent, TA)])  # markedAt 缺省
check("缺省 markedAt 写入", "applied", status_for((code, body), id_absent)[0])
check("缺省不会清掉已有标记", TM, marked_value())
check("缺省写入新增 1 条", before + 1, count_entries())

before = count_entries()
id_null = str(uuid.uuid4())
code, body = push([device_payload(id_null, TN, None)])  # 显式 null
check("显式 null 取消标记", "applied", status_for((code, body), id_null)[0])
check("取消后 marked_at 为 NULL", "null", marked_value())
check("取消写入新增 1 条", before + 1, count_entries())

before = count_entries()
code, body = push([device_payload(id_absent, TABS2)])  # 取消后再缺省
check("取消后缺省写入", "applied", status_for((code, body), id_absent)[0])
check("缺省不会把 NULL 误判为重新标记", "null", marked_value())
check("取消后缺省新增 1 条", before + 1, count_entries())

before = count_entries()
id_old = str(uuid.uuid4())
code, body = push([device_payload(id_old, TOLD, TOLD)])
s_old, why_old = status_for((code, body), id_old)
check("取消后用旧 updatedAt 再标记", "conflict", s_old)
check("旧 updatedAt 冲突原因", "server_newer", why_old)
check("旧 updatedAt 不覆盖 NULL", "null", marked_value())
check("旧 updatedAt 不新增条目", before, count_entries())

before = count_entries()
code, body = push([device_payload(id_old, TNEW, TNEW)])
check("新 updatedAt 再标记", "applied", status_for((code, body), id_old)[0])
check("新时间戳落库", TNEW, marked_value())
check("新标记新增 1 条", before + 1, count_entries())

# 3) 回传服务端值做幂等重放
code, page = http("GET", "/api/v1/my/devices", None, TOKEN)
check("读取设备列表", 200, code)
item = next(x for x in page["items"] if x["externalId"] == EXT)
echo = {
    "id": item["id"],
    "platform": item["platform"],
    "externalId": item["externalId"],
    "mac": item.get("mac", ""),
    "alias": item["alias"],
    "advertisedName": item["advertisedName"],
    "group": item["group"],
    "rules": item["rules"],
    "scanRecords": item["scanRecords"],
    "updatedAt": item["updatedAt"],
    "markedAt": item["markedAt"],
}
if item.get("deviceType"):
    echo["deviceType"] = item["deviceType"]

before = count_entries()
r1 = push([echo])
check("回传原值第 1 次", "unchanged", status_for(r1, echo["id"])[0])
r2 = push([echo])
check("回传原值第 2 次", "unchanged", status_for(r2, echo["id"])[0])
check("回放不新增 change_log", before, count_entries())
check("回放后标记值不变", TNEW, marked_value())

# 4) 游标推进：HTTP 生成 scene entity + DB 插入未知 entity
unknown_seq = int(psql(
    "WITH ins AS (INSERT INTO change_log (user_id, entity, entity_id, op, payload) "
    f"VALUES ('{UID}', 'legacy_unknown', 'legacy:unknown', 'upsert', '{{\"legacy\":true}}') "
    "RETURNING seq) SELECT seq FROM ins"))
scene_body = {
    "name": "E2E 并发游标校验",
    "trigger": {
        "kind": "device_offline",
        "device": {"platform": PLATFORM, "externalId": EXT},
        "offlineSeconds": 120,
    },
    "actions": [{"kind": "log", "message": "离线", "level": "warn"}],
}
code, scene = http("POST", "/api/v1/scenes", scene_body, TOKEN)
check("创建场景（其他 entity）", 201, code)
scene_seq = int(psql(
    "SELECT max(seq) FROM change_log WHERE entity='scene' AND user_id='" + UID + "'"))
check("未知 entity 在场景之前", True, unknown_seq < scene_seq)

seqs = []
entities = set()
cursor = 0
pages = 0
stalled = False
while True:
    code, pg = http("GET", f"/api/v1/sync/changes?limit=10&cursor={cursor}", None, TOKEN)
    if code != 200:
        check("拉取 /sync/changes", 200, code)
        break
    pages += 1
    for ch in pg["changes"]:
        seqs.append(ch["seq"])
        entities.add(ch["entity"])
    nxt = pg["nextCursor"]
    if pg.get("hasMore"):
        if nxt <= cursor:
            stalled = True
        cursor = nxt
        if pages > 50:
            stalled = True
            break
        continue
    if nxt < cursor:
        stalled = True
    cursor = nxt
    break

check("分页未停滞", False, stalled)
check("seq 严格递增", True, all(b > a for a, b in zip(seqs, seqs[1:])))
check("包含 user_device", True, "user_device" in entities)
check("包含 scene（HTTP 其他 entity）", True, "scene" in entities)
check("包含 legacy_unknown（未知 entity）", True, "legacy_unknown" in entities)
check("未知 entity 之后仍推进到 scene", True, scene_seq in seqs)
code, tail = http("GET", f"/api/v1/sync/changes?limit=10&cursor={cursor}", None, TOKEN)
check("尾部空页", 0, len(tail["changes"]))
check("尾部游标不倒退", cursor, tail["nextCursor"])

# 输出 markdown 报告
print("## 场景与断言\n")
print("| 步骤 | 期望 | 实际 | 结果 |")
print("| --- | --- | --- | --- |")
for r in rows:
    exp = str(r["expected"]).replace("|", "\\|")
    obs = str(r["observed"]).replace("|", "\\|")
    print(f"| {r['step']} | {exp} | {obs} | {'PASS' if r['ok'] else 'FAIL'} |")
print()
print(f"- 用户：`{UID}`")
print(f"- 设备：`platform={PLATFORM}`、`externalId={EXT}`、`entity_id={ENTITY_ID}`")
print(f"- 并发轮次：{rounds}（每轮两个客户端同时 push）")
print(f"- 最终 marked_at：`{marked_value()}`")
print(f"- change_log 最终条目数：{count_entries()}")
print(f"- /sync/changes 页数：{pages}，seq 条数：{len(seqs)}")
print()
print(f"结论：{'全部通过' if not failures else '存在失败'}")
if failures:
    for f in failures:
        print("FAIL:", f)
    sys.exit(1)
PY
