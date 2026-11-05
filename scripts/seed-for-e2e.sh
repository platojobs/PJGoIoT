#!/usr/bin/env bash
# 向 scripts/stack-for-e2e.sh 起的真实 Postgres 栈写入最小可核实数据：
#   - 1 个用户（匿名登录复用 token，重复执行不会新增用户）
#   - 1 台用户设备：externalId=AA:BB:CC:00:00:02，MAC 关联到迁移 seed 的真实型号 ESP32-C3-DevKitM-1
#   - 24 条读数（3 个指标 × 8 个整点），id 用固定命名空间派生，重复执行不会重复插入
#   - 校验 device_types / firmwares / accessories 来自迁移 seed（数量与真实型号），不编造任何型号
#
# 幂等性来源：复用 .e2e/seed.env 中的 token；设备按 (platform, externalId) UPSERT；
# 读数按客户端 id 幂等（INSERT ... ON CONFLICT DO NOTHING）。
#
# 用法：先 scripts/stack-for-e2e.sh，再 scripts/seed-for-e2e.sh
# 环境变量：E2E_STATE_DIR / E2E_SEED_ANCHOR（固定读数时间锚点，默认 2026-10-01T00:00:00Z）
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
STATE_DIR="${E2E_STATE_DIR:-$ROOT/.e2e}"
STACK_FILE="$STATE_DIR/stack.env"
SEED_FILE="$STATE_DIR/seed.env"
ANCHOR="${E2E_SEED_ANCHOR:-2026-10-01T00:00:00Z}"
DEVICE_MAC_UPPER="AA:BB:CC:00:00:02"
DEVICE_MAC_LOWER="aa:bb:cc:00:00:02"

log() { printf '%s\n' "$*" >&2; }

if [[ ! -f "$STACK_FILE" ]]; then
	log "未找到 ${STACK_FILE}，请先执行 scripts/stack-for-e2e.sh"
	exit 1
fi
# shellcheck disable=SC1090,SC1091
source "$STACK_FILE"
if [[ -z "${BASE_URL:-}" ]]; then
	log "状态文件缺少 BASE_URL"
	exit 1
fi

TOKEN=""
USER_ID=""
if [[ -f "$SEED_FILE" ]]; then
	# shellcheck disable=SC1090,SC1091
	source "$SEED_FILE"
fi

if [[ -n "${TOKEN:-}" ]] && [[ "$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $TOKEN" "$BASE_URL/api/v1/my/devices")" == "200" ]]; then
	log "复用已有 seed token（user=${USER_ID}）"
else
	log "创建匿名用户"
	login="$(curl -fsS -X POST "$BASE_URL/api/v1/auth/anonymous")"
	TOKEN="$(python3 -c 'import json,sys; print(json.load(sys.stdin)["token"])' <<<"$login")"
	USER_ID="$(python3 -c 'import json,sys; print(json.load(sys.stdin)["userId"])' <<<"$login")"
	{
		printf 'TOKEN=%q\n' "$TOKEN"
		printf 'USER_ID=%q\n' "$USER_ID"
	} >"$SEED_FILE"
fi

payload="$(python3 - "$ANCHOR" "$DEVICE_MAC_UPPER" "$DEVICE_MAC_LOWER" <<'PY'
import datetime as dt
import json
import sys
import uuid

anchor = dt.datetime.fromisoformat(sys.argv[1].replace("Z", "+00:00"))
mac_upper, mac_lower = sys.argv[2], sys.argv[3]
ns = uuid.NAMESPACE_URL

def det(name: str) -> str:
    return str(uuid.uuid5(ns, "https://pjgoiot.example/e2e/seed/" + name))

def iso(value: dt.datetime) -> str:
    return value.astimezone(dt.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")

device = {
    "id": det("device/esp32-c3"),
    "platform": "android",
    "externalId": mac_upper,
    "mac": mac_lower,
    "alias": "E2E 环境传感器",
    "advertisedName": "ESP32-C3-DevKitM-1",
    "rules": {},
    "scanRecords": [],
    "updatedAt": iso(anchor + dt.timedelta(hours=7)),
}
series = [
    ("temperature_celsius", "celsius", lambda i: 22.0 + i * 0.25),
    ("relative_humidity", "percent", lambda i: 48.0 + i * 0.5),
    ("pressure_hpa", "hpa", lambda i: 1012.0 + i * 0.1),
]
readings = []
for metric, unit, value in series:
    for i in range(8):
        at = anchor + dt.timedelta(hours=i)
        readings.append({
            "id": det(f"reading/{metric}/{i}/{sys.argv[1]}"),
            "platform": "android",
            "externalId": mac_upper,
            "metric": metric,
            "value": round(value(i), 2),
            "unit": unit,
            "recordedAt": iso(at),
        })
print(json.dumps({"devices": [device], "readings": readings}))
PY
)"

response="$(curl -fsS -X POST "$BASE_URL/api/v1/sync/push" \
	-H "Authorization: Bearer $TOKEN" \
	-H "Content-Type: application/json" \
	-d "$payload")"
python3 -c '
import json, sys
body = json.loads(sys.argv[1])
print("push 结果: applied=%d skippedReadings=%d conflicts=%d"
      % (len(body.get("applied") or []), body.get("skippedReadings", 0), len(body.get("conflicts") or [])))
' "$response" >&2

# 从真实 API 读回并校验，避免只看 push 响应自证成功。
tmp="$STATE_DIR/seed-verify"
mkdir -p "$tmp"
curl -fsS -H "Authorization: Bearer $TOKEN" "$BASE_URL/api/v1/my/devices" >"$tmp/devices.json"
curl -fsS -H "Authorization: Bearer $TOKEN" \
	"$BASE_URL/api/v1/my/devices/android/$DEVICE_MAC_UPPER/readings?limit=500" >"$tmp/readings.json"
curl -fsS "$BASE_URL/api/v1/device-types" >"$tmp/device-types.json"
curl -fsS "$BASE_URL/api/v1/device-types/environmental_sensor/catalog" >"$tmp/catalog.json"
curl -fsS "$BASE_URL/api/v1/devices/$DEVICE_MAC_LOWER" >"$tmp/public-device.json"

python3 - "$tmp" <<'PY'
import json
import sys

base = sys.argv[1]

def load(name):
    with open(f"{base}/{name}", encoding="utf-8") as fh:
        return json.load(fh)

devices = load("devices.json")["items"]
if len(devices) != 1:
    raise SystemExit(f"user_devices 期望 1 台，实际 {len(devices)}")
device = devices[0]
if device["externalId"] != "AA:BB:CC:00:00:02":
    raise SystemExit(f"externalId 异常: {device['externalId']}")
if device.get("deviceType") != "environmental_sensor":
    raise SystemExit(f"deviceType 未关联到真实型号: {device.get('deviceType')}")
if device.get("vendor") != "Espressif Systems":
    raise SystemExit(f"vendor 异常: {device.get('vendor')}")

readings = load("readings.json")["items"]
metrics = sorted({item["metric"] for item in readings})
if len(readings) != 24:
    raise SystemExit(f"readings 期望 24 条，实际 {len(readings)}")
if metrics != ["pressure_hpa", "relative_humidity", "temperature_celsius"]:
    raise SystemExit(f"metrics 异常: {metrics}")

types = load("device-types.json")["items"]
if len(types) != 5:
    raise SystemExit(f"device_types 期望 5 个，实际 {len(types)}")

catalog = load("catalog.json")
firmwares = catalog["firmwares"]
accessories = catalog["accessories"]
if not any(fw["version"] == "ESP-IDF v5.2.2" for fw in firmwares):
    raise SystemExit(f"未找到官方固件 ESP-IDF v5.2.2: {firmwares}")
if not any(acc["name"] == "USB-C 数据线" for acc in accessories):
    raise SystemExit(f"未找到真实配件 USB-C 数据线: {accessories}")

public = load("public-device.json")
if public["model"] != "ESP32-C3-DevKitM-1":
    raise SystemExit(f"公共设备型号异常: {public['model']}")

print("校验通过: user_devices=1 readings=24 metrics=%s device_types=%d "
      "firmware=%s accessory=%s public_model=%s"
      % (",".join(metrics), len(types), firmwares[0]["version"], accessories[0]["name"], public["model"]))
PY

log "seed 完成: BASE_URL=$BASE_URL device=$DEVICE_MAC_UPPER (ESP32-C3-DevKitM-1)"
log "状态文件: ${SEED_FILE}（TOKEN 仅本机测试使用）"
