#!/usr/bin/env bash
# 本地端到端验证「Prometheus 规则触发 -> Alertmanager 路由 -> webhook 收到」，
# 并顺带验证去重（同一告警重复提交只通知一次）与静默（silence 命中后不通知）。
#
# 依赖：docker（含 host.docker.internal 可解析，Docker Desktop 默认支持）、go。
# 不依赖真实 FCM/APNs、真实域名或生产告警通道；只验证到 webhook 收到。
#
# 用法：./scripts/verify-alerting.sh
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
mkdir -p "${ROOT}/tmp"
# Docker Desktop 的文件共享通常不含 /var/folders，临时目录放在仓库内（tmp/ 已在 .gitignore）。
TMP="$(mktemp -d "${ROOT}/tmp/verify-alerting.XXXXXX")"
WEBHOOK_PID=""
AM_NAME="pjgoiot-verify-alertmanager"
PROM_NAME="pjgoiot-verify-prometheus"

cleanup() {
  if [[ -n "${WEBHOOK_PID}" ]]; then
    kill "${WEBHOOK_PID}" >/dev/null 2>&1 || true
  fi
  docker rm -f "${AM_NAME}" "${PROM_NAME}" >/dev/null 2>&1 || true
  rm -rf "${TMP}"
}
trap cleanup EXIT

if ! command -v docker >/dev/null 2>&1; then
  echo "FAIL: 未找到 docker，无法运行本地告警链路验证"
  exit 1
fi
if ! docker info >/dev/null 2>&1; then
  echo "FAIL: docker 守护进程不可用"
  exit 1
fi

echo "== 1/5 构建 alert-webhook =="
cd "${ROOT}"
go build -o "${TMP}/alert-webhook" ./cmd/alert-webhook

echo "== 2/5 启动 alert-webhook（127.0.0.1:18099） =="
"${TMP}/alert-webhook" -addr 127.0.0.1:18099 -log-file "${TMP}/webhook.log" \
  >"${TMP}/webhook.out" 2>&1 &
WEBHOOK_PID=$!
disown "${WEBHOOK_PID}" 2>/dev/null || true
for _ in $(seq 1 20); do
  if curl -fsS http://127.0.0.1:18099/healthz >/dev/null 2>&1; then
    break
  fi
  sleep 0.3
done
curl -fsS http://127.0.0.1:18099/healthz >/dev/null

echo "== 3/5 渲染 Alertmanager/Prometheus 临时配置 =="
sed 's#http://alert-webhook:8099/alerts#http://host.docker.internal:18099/alerts#' \
  deploy/alertmanager/alertmanager.yml >"${TMP}/alertmanager.yml"
cat >"${TMP}/prometheus.yml" <<'YAML'
global:
  scrape_interval: 5s
  evaluation_interval: 5s
rule_files:
  - /etc/prometheus/verify-alerting.rules.yml
alerting:
  alertmanagers:
    - static_configs:
        - targets: ["host.docker.internal:19093"]
YAML
cp deploy/prometheus/verify-alerting.rules.yml "${TMP}/verify-alerting.rules.yml"

echo "== 4/5 启动 Alertmanager(19093) 与 Prometheus(19090) =="
docker run -d --rm --name "${AM_NAME}" \
  --add-host=host.docker.internal:host-gateway \
  -p 127.0.0.1:19093:9093 \
  -v "${TMP}:/etc/alertmanager:ro" \
  prom/alertmanager:v0.27.0 \
  --config.file=/etc/alertmanager/alertmanager.yml --log.level=warn >/dev/null

docker run -d --rm --name "${PROM_NAME}" \
  --add-host=host.docker.internal:host-gateway \
  -p 127.0.0.1:19090:9090 \
  -v "${TMP}:/etc/prometheus:ro" \
  prom/prometheus:v2.55.0 \
  --config.file=/etc/prometheus/prometheus.yml --log.level=warn >/dev/null

wait_for_http() {
  local url="$1"
  for _ in $(seq 1 30); do
    if curl -fsS "${url}" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  return 1
}
wait_for_http http://127.0.0.1:19090/-/ready || { echo "FAIL: Prometheus 未就绪"; exit 1; }
wait_for_http http://127.0.0.1:19093/-/ready || { echo "FAIL: Alertmanager 未就绪"; exit 1; }

count_records() {
  local name="$1"
  if [[ ! -f "${TMP}/webhook.log" ]]; then
    echo 0
    return
  fi
  grep -c "\"alertname\":\"${name}\"" "${TMP}/webhook.log" || true
}

wait_for_count() {
  local name="$1" want="$2" timeout="${3:-60}"
  for _ in $(seq 1 $((timeout / 2))); do
    if [[ "$(count_records "${name}")" -ge "${want}" ]]; then
      return 0
    fi
    sleep 2
  done
  return 1
}

post_alert() {
  local body="$1"
  curl -fsS -X POST http://127.0.0.1:19093/api/v2/alerts \
    -H 'Content-Type: application/json' -d "${body}" >/dev/null
}

NOW="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

echo "== 5/5 验证规则触发 -> webhook =="
if ! wait_for_count VerifyAlertingPipeline 1 90; then
  echo "FAIL: 规则已触发但 webhook 未收到 VerifyAlertingPipeline"
  echo "--- prometheus 活动告警 ---"
  curl -fsS http://127.0.0.1:19090/api/v1/alerts || true
  echo "--- webhook 输出 ---"
  cat "${TMP}/webhook.out" || true
  exit 1
fi
echo "PASS: 规则触发后 webhook 收到 VerifyAlertingPipeline"

echo "== 验证去重：同一告警重复提交只通知一次 =="
DEDUP_BODY='[{"labels":{"alertname":"DedupProbe","severity":"info","service":"verify","env":"local"},"annotations":{"summary":"dedup probe"},"startsAt":"'"${NOW}"'"}]'
post_alert "${DEDUP_BODY}"
post_alert "${DEDUP_BODY}"
sleep 15
dedup_count="$(count_records DedupProbe)"
if [[ "${dedup_count}" -ne 1 ]]; then
  echo "FAIL: 重复提交两次应只通知一次，实际 ${dedup_count} 次"
  exit 1
fi
echo "PASS: 重复告警去重生效（webhook 收到 ${dedup_count} 次）"

echo "== 验证静默：silence 命中后不再通知 =="
ENDS_AT="$(date -u -v+1H +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -d '+1 hour' +%Y-%m-%dT%H:%M:%SZ)"
SILENCE_BODY='{"matchers":[{"name":"alertname","value":"SilenceProbe","isRegex":false,"isEqual":true}],"startsAt":"'"${NOW}"'","endsAt":"'"${ENDS_AT}"'","createdBy":"verify-script","comment":"verify"}'
curl -fsS -X POST http://127.0.0.1:19093/api/v2/silences \
  -H 'Content-Type: application/json' -d "${SILENCE_BODY}" >/dev/null

SILENCE_ALERT_BODY='[{"labels":{"alertname":"SilenceProbe","severity":"info","service":"verify","env":"local"},"annotations":{"summary":"silence probe"},"startsAt":"'"${NOW}"'"}]'
post_alert "${SILENCE_ALERT_BODY}"
sleep 20
silence_count="$(count_records SilenceProbe)"
if [[ "${silence_count}" -ne 0 ]]; then
  echo "FAIL: 静默命中后仍收到 ${silence_count} 次通知"
  exit 1
fi
echo "PASS: 静默生效（webhook 收到 0 次）"

echo "== webhook 收到的记录样例 =="
cat "${TMP}/webhook.log"
echo
echo "ALL PASS: 规则触发 -> Alertmanager -> webhook 链路在本地可用；未验证真实电话/短信/邮件通道。"
