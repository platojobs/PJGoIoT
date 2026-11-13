# 性能基线（v0.26.0）

本机单点、单进程 API（除标注外），M1 Max / 10 核 / 32 GB / PostgreSQL 16.15（`shared_buffers=128MB`、
`work_mem=4MB`、`effective_cache_size=4GB`）。数字是单机天花板，不是生产容量承诺。
复现：`scripts/perf-sync-floor.sh`、`scripts/scenes-soak.sh`、`cmd/loadgen`、`scripts/verify-marked-concurrency.sh`。

## 1. 容量（A 轮，每路径 50 并发，feedback 20）

| 路径 | RPS | p50 | p95 | p99 | 错误率 |
| --- | --- | --- | --- | --- | --- |
| `GET /my/devices` | 5,773.9 | 8ms | 16ms | 23ms | 0 |
| `GET /readings/series`（90 天 / 1d，2k 读数） | 4,766.0 | 8ms | 20ms | 77ms | 0 |
| `POST /sync/push`（100 设备 + 200 读数，同一用户） | 42.4 | 1.053s | 2.042s | 2.752s | 0.08%（1×503） |
| `GET /sync/changes`（backlog 仅 100 条） | 136.7 | 345ms | 597ms | 761ms | 0 |
| `GET /scenes`（50 个场景） | 7,174.5 | 6ms | 13ms | 18ms | 0 |
| `PUT /scenes/{id}`（字段级 LWW） | 2,097.7 | 22ms | 43ms | 57ms | 0 |
| `POST /feedback` | 12,749.3 | 1ms | 3ms | 10ms | 0 |
| `GET /feedback/mine` | 11,443.6 | 1ms | 5ms | 13ms | 0 |
| `GET /catalog/device-types`（`If-None-Match`） | 12,388.2 | 4ms | 7ms | 11ms | 0（304 全命中） |

## 2. push 拐点（同一用户）

| 并发 | RPS | p50 | p95 | p99 | 错误率 |
| --- | --- | --- | --- | --- | --- |
| 10 | 48.9 | 203ms | 218ms | 227ms | 0 |
| 50 | 42.4 | 1.053s | 2.042s | 2.752s | 0.08% |
| 100 | 41.5 | 2.148s | 4.463s | 5.8s | 3.99% |

## 3. `/sync/changes`（108 万行 `change_log`、16 家庭、limit=100）

| 版本 / 场景 | p95 | buffers |
| --- | --- | --- |
| 可见性 floor，16 家庭（迁移 20 后） | 0.869ms | 54 |
| 主查询 cursor=0，旧 OR/IN | 104.793ms | 16,282 |
| 主查询 cursor=0，新分支结构 | 2.985ms | 121 |
| 同一库迁移 19→20 前后（A/B，同一脚本） | 5.708ms → 4.179ms | 121 → 121 |
| 可见性下限改造（111 万行，改造前 → 后） | 66.28ms → 0.56ms | 87,928 → 10 |

## 4. 其他

| 项 | 结果 |
| --- | --- |
| 场景长稳（40 并发 × 各 10 分钟） | GET p95 20ms、PUT p95 37ms，0 错误、全 200；连接池 20/20 饱和无 503 |
| push 多用户（5 × 10 并发） | ≈158.9 RPS，API CPU 2.38 核；吞吐随用户数近似线性 |
| 备份 / 恢复（138MB 库） | dump 9.96s、restore 15.40s，15 张表校验和一致 |
| 停机自愈 | Postgres 停 36s，恢复后首个请求即成功，无需重启 API |
| 滚动发布 | 双实例滚动，停机请求数 0 |

## 结论

1. 读路径余量充足（p95 ≤ 20ms）；瓶颈是按用户串行化的 `push`（advisory lock，
   ≈42–49 RPS/用户，超 3s 预算返回 503 `TIMEOUT`），这是设计取舍，不是 DB 上限。
2. 多家庭下 `/sync/changes` 的重点是避免 OR/IN 全量扫描：新分支结构把 cursor=0 从 ≈105ms
   降到 ≈3–4ms，buffers 从 16k 降到 121；迁移 19/20 未改变执行计划与 buffers。
3. 以上均为数据库/单机层面；公网 RTT、客户端渲染、真实 FCM/APNs 投递不在基线内。
