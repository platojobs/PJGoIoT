#!/usr/bin/env bash
# 校验 Grafana dashboard JSON：
#   1) JSON 可解析，仪表盘/面板/查询必备字段齐全，panel id 不重复；
#   2) 所有 PromQL 引用的 pjgoiot_* 指标名必须能在 Go 代码中找到（防止文档与实现漂移）。
# probe_success 由 blackbox_exporter 提供，不在本仓库代码中，作为唯一豁免项。
#
# 用法：scripts/validate-dashboard.sh
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DASHBOARD="$ROOT/deploy/grafana/dashboard.json"

if [[ ! -f "$DASHBOARD" ]]; then
	echo "缺少 $DASHBOARD" >&2
	exit 1
fi

tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT

python3 - "$DASHBOARD" >"$tmp" <<'PY'
import json
import re
import sys

path = sys.argv[1]
with open(path, encoding="utf-8") as fh:
    data = json.load(fh)

errors = []
if not str(data.get("title", "")).strip():
    errors.append("缺少 title")
if "uid" not in data:
    errors.append("缺少 uid")
panels = data.get("panels")
if not isinstance(panels, list) or len(panels) < 8:
    errors.append(f"panels 至少需要 8 个，实际 {len(panels) if isinstance(panels, list) else '非数组'}")

ids = set()
names = set()
for index, panel in enumerate(panels or []):
    where = f"panels[{index}]"
    title = panel.get("title")
    if not str(title or "").strip():
        errors.append(f"{where} 缺少 title")
    if panel.get("type") not in {"timeseries", "stat", "gauge", "table", "barchart", "heatmap"}:
        errors.append(f"{where} type 异常: {panel.get('type')}")
    pos = panel.get("gridPos") or {}
    if not all(isinstance(pos.get(key), int) for key in ("h", "w", "x", "y")):
        errors.append(f"{where} gridPos 不完整")
    pid = panel.get("id")
    if not isinstance(pid, int):
        errors.append(f"{where} id 必须是整数")
    elif pid in ids:
        errors.append(f"{where} id 重复: {pid}")
    else:
        ids.add(pid)
    targets = panel.get("targets") or []
    if not targets:
        errors.append(f"{where} 没有 targets")
    for tidx, target in enumerate(targets):
        expr = str(target.get("expr") or "")
        if not expr.strip():
            errors.append(f"{where}.targets[{tidx}] expr 为空")
            continue
        if not str(target.get("refId") or "").strip():
            errors.append(f"{where}.targets[{tidx}] 缺少 refId")
        for name in re.findall(r"\b(pjgoiot_[a-z0-9_]+|probe_success)\b", expr):
            names.add(name)

if errors:
    for err in errors:
        print(f"dashboard 校验失败: {err}", file=sys.stderr)
    sys.exit(1)

for name in sorted(names):
    print(name)
PY

missing=0
while IFS= read -r metric; do
	[[ -z "$metric" ]] && continue
	if [[ "$metric" == "probe_success" ]]; then
		echo "跳过代码检索: ${metric}（blackbox_exporter 提供）"
		continue
	fi
	if ! grep -Rq --include='*.go' -- "$metric" "$ROOT/internal" "$ROOT/cmd"; then
		echo "指标在代码中不存在: ${metric}" >&2
		missing=1
	fi
done <"$tmp"

if [[ $missing -ne 0 ]]; then
	exit 1
fi
echo "dashboard 校验通过: JSON 结构有效，引用的指标名均存在于 Go 代码"
