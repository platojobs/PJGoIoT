# SLO 与错误预算（v0.9.0）

本文定义 PJGoIoT API 的 3 条 SLO、对应 PromQL、错误预算算法，以及与
[deploy/prometheus/alerts.yml](../deploy/prometheus/alerts.yml) 的告警映射。
Grafana 面板见 [deploy/grafana/dashboard.json](../deploy/grafana/dashboard.json)。

指标全部来自 `GET /metrics`（`METRICS_ENABLED=true`，默认开启）：

| 指标 | 类型 | 标签 | 说明 |
| --- | --- | --- | --- |
| `pjgoiot_http_requests_total` | counter | `method`、`route`、`status` | `route` 是低基数路由模板（如 `/api/v1/my/devices/:platform/:externalId/readings/series`），v0.9.0 新增 |
| `pjgoiot_http_request_duration_seconds` | histogram | `method`、`route` | 桶：10ms/25/50/100/250/500/1s/2.5s/5s/10s |
| `pjgoiot_db_errors_total` | counter | 无 | 存储层错误累计 |
| `pjgoiot_users_approx` / `pjgoiot_devices_approx` / `pjgoiot_readings_approx` | gauge | 无 | 表行数近似值，每 30s 刷新 |
| `probe_success{job="pjgoiot-api-health"}` | gauge | `job` | blackbox_exporter 对 `/api/v1/health` 的探测结果，1=可用 |

> `route` 标签由 `routeLabel` 在中间件里归一化，基数等于静态路由数（约 15 个），
> 不会因为设备 MAC / 用户 ID 增长而膨胀。Gin 的 `/my/devices/*key` 通配路由已在中间件中
> 还原为 devices / readings / series 三个模板，否则 SLO2/SLO3 无法区分。

## 1. 三条 SLO

| 编号 | SLI | 目标 | 观测窗口 | 用户可感知含义 |
| --- | --- | --- | --- | --- |
| SLO-1 可用性 | `1 - 5xx请求数 / 全部请求数` | **≥ 99.5%** | 自然月 | 5xx（含 503 `TIMEOUT`、410 属 4xx 不计）越少越好；每月不可用时间 ≤ 3h36m |
| SLO-2 设备列表延迟 | `GET /api/v1/my/devices` 的 p95 | **< 300ms** | 滚动 30 天（5m 粒度） | 打开设备列表不卡顿 |
| SLO-3 读数序列延迟 | `GET .../readings/series` 的 p95 | **< 200ms** | 滚动 30 天（5m 粒度） | 90 天 / 1d 桶的图表加载迅速 |

延迟 SLI 以服务端直方图为准（不含公网 RTT）；客户端 e2e 另有压测门槛，见 README。

### SLO-1 PromQL

```promql
# 当前 1 小时 5xx 比例（燃烧率 = 该值 / 0.005）
sum(rate(pjgoiot_http_requests_total{status=~"5.."}[1h]))
  /
clamp_min(sum(rate(pjgoiot_http_requests_total[1h])), 0.001)

# 当前 5 分钟可用性 SLI（Grafana 面板用 $__rate_interval）
1 - (
  sum(rate(pjgoiot_http_requests_total{status=~"5.."}[$__rate_interval]))
    /
  clamp_min(sum(rate(pjgoiot_http_requests_total[$__rate_interval])), 0.001)
)

# 自然月剩余错误预算（0~1，负数为已超支）
1 - (
  (sum(increase(pjgoiot_http_requests_total{status=~"5.."}[30d]))
     / clamp_min(sum(increase(pjgoiot_http_requests_total[30d]), 0.001))
  ) / 0.005
)
```

### SLO-2 PromQL（设备列表 p95 < 0.3s）

```promql
histogram_quantile(0.95,
  sum by (le) (rate(pjgoiot_http_request_duration_seconds_bucket{
    method="GET", route="/api/v1/my/devices"
  }[5m]))
) > 0.3
```

### SLO-3 PromQL（序列 p95 < 0.2s）

```promql
histogram_quantile(0.95,
  sum by (le) (rate(pjgoiot_http_request_duration_seconds_bucket{
    method="GET", route="/api/v1/my/devices/:platform/:externalId/readings/series"
  }[5m]))
) > 0.2
```

延迟 SLO 的合规率（30 天内 5 分钟窗口满足阈值的比例）：

```promql
avg_over_time((
  histogram_quantile(0.95,
    sum by (le) (rate(pjgoiot_http_request_duration_seconds_bucket{
      method="GET", route="/api/v1/my/devices"
    }[5m]))
  ) <= bool 0.3
)[30d:5m])
```

## 2. 错误预算算法

以 30 天月（43,200 分钟）为例：

```
错误预算分钟 = 窗口分钟 × (1 - SLO)
SLO-1: 43200 × 0.5% = 216 分钟（3h36m）
燃烧率 burn = 当前错误率 / (1 - SLO)
剩余预算 = 1 - 窗口内错误率 / (1 - SLO)
```

| 燃烧率 | 含义 | 216 分钟预算耗尽时间 | 处置 |
| --- | --- | --- | --- |
| 1x | 恰好按预算消耗 | 30 天 | 正常 |
| 2x | 错误率 1% | 15 天 | `ApiAvailabilityBudgetBurn` 触发，开始排查 |
| 10x | 错误率 5% | 约 1.5 天 | `ApiHigh5xxRate` 触发（critical），立即处置 |
| 100x | 错误率 50% | 约 7 小时 | 熔断/回滚，通知用户 |

延迟 SLO 同理，以窗口内“p95 超阈值时长占比”折算；当前通过
`ApiDevicesP95High` / `ApiSeriesP95High` 的 10 分钟持续条件发现劣化。
CI 的 smoke 门槛（devices 400ms、series 400ms、push 4000ms）是**发布闸门**，
比线上 SLO 更宽松（2–3x 余量），只用于拦截明显回归，不代替线上 SLO。

## 3. 告警映射（与 alerts.yml 一一对应）

| 告警 | 对应 SLO | 角色 | 触发条件（摘要） |
| --- | --- | --- | --- |
| `ApiAvailabilityBudgetBurn` | SLO-1 | 主告警（快速燃烧） | 1h 5xx 比例 > 1%（2x 燃烧率）持续 10m |
| `ApiHigh5xxRate` | SLO-1 | 灾难性燃烧 | 5m 5xx 比例 > 5%（10x）持续 5m |
| `ApiHealthCheckFailing` | SLO-1 | 可用性佐证 | `probe_success=0` 持续 2m |
| `ApiRequestRateDroppedToZero` | SLO-1 | 流量入口故障 | 5m 内请求量为 0 |
| `ApiMetricsAbsent` | SLO-1 | 观测盲区 | `absent(pjgoiot_http_requests_total)` 持续 5m |
| `ApiDevicesP95High` | SLO-2 | 主告警 | devices p95 > 0.3s 持续 10m |
| `ApiSeriesP95High` | SLO-3 | 主告警 | series p95 > 0.2s 持续 10m |
| `ApiP95LatencyHigh` | SLO-2 / SLO-3 | 全局护栏 | 全接口 p95 > 2s 持续 5m |
| `ApiDBErrorsIncreasing` | SLO-1 | 根因信号 | 10m 内 `pjgoiot_db_errors_total` 增长 |

值班顺序：先看 SLO-1 主告警 → 看 `ApiDBErrorsIncreasing` 与 API 日志 `request_id`；
延迟告警 → 看 Grafana「各 route P95」与 CI perf-smoke 最近一次结果。

## 4. 已知限制

- `histogram_quantile` 是桶插值，p95 在桶边界（250ms/500ms）附近存在误差，阈值判断以趋势为准。
- 低流量时（<1 req/s）5 分钟窗口样本过少，SLO 计算会抖动；告警使用 1h 窗口缓解。
- `/metrics` 与 404 仍会进入 `pjgoiot_http_requests_total`（route 为 `/metrics` 或 `unmatched`），
  如需纯粹业务口径，可在 PromQL 分母中加 `route!~"/metrics|unmatched"`。
