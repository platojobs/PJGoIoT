# 服务端压测基线（v0.21.0）

> 本文件只记录**本机真实执行过**的数字。未执行的路径不列入结果表，只出现在「未覆盖」一节。
> 压测工具与 API 在同一台机器上运行，数字是**单机天花板**，不代表跨主机 + 真实网络的表现。

## 1. 环境

| 项 | 值 |
| --- | --- |
| 机器 | Apple M1 Max，10 核，32 GB 内存，macOS 27.2 |
| API | 单进程 `/tmp/pjgoiot-api`（`STORE=postgres`），Go 1.27.3 darwin/arm64，`APP_VERSION=0.21.0` |
| 数据库 | PostgreSQL 16.15（Homebrew），`shared_buffers=128MB`、`max_connections=100`、`work_mem=4MB`，与 API 同机 |
| 网络 | 全部 `127.0.0.1` 回环，无 TLS（直连 HTTP 端口，不经 Caddy） |
| 限流 | 压测轮：`RATE_LIMIT_PER_MINUTE=0` 等全部关闭；限流专项：单独实例，见 3.4 |
| 迁移 | 000001–000017 全部 up（`schema_migrations.version=17`） |
| 压测工具 | `cmd/loadgen`（本版新增 changes / scenes / feedback / catalog 场景），预热 3s 不计入统计 |

### 数据规模（分阶段，均为真实统计）

| 阶段 | 用户 | 设备 | 读数 | change_log | 库大小 |
| --- | --- | --- | --- | --- | --- |
| A 轮（7 条路径）结束时 | 10 | ≈5,400 | ≈303,000 | ≈225,000 | 626 MB |
| B 轮（push 拐点 + 多用户）结束后 | 18 | ≈21,000 | ≈1,993,000 | ≈1,071,000 | ≈1.7 GB |
| changes 长 backlog 专项 | 1（独立用户） | 1,475 × 100 | 1,475 × 200 | 69,325（该用户） | 同上 |

说明：push 每请求 100 台设备（`workerMACs` 按 worker 分配独立集合）+ 200 条读数；`changes` 每页 `limit=100`，游标按响应 `nextCursor` 推进，到底后回到 0。

## 2. 复现命令

```bash
# 1) 独立库 + 迁移
psql postgres://platojobs@127.0.0.1:5432/postgres -c "create database pjgoiot_load"
go run ./cmd/migrate -database-url 'postgres://platojobs@127.0.0.1:5432/pjgoiot_load?sslmode=disable' -direction up

# 2) 启动 API（关闭限流，打开指标）
STORE=postgres DATABASE_URL='postgres://platojobs@127.0.0.1:5432/pjgoiot_load?sslmode=disable' \
ADDR=':18080' APP_VERSION='0.21.0' RATE_LIMIT_PER_MINUTE=0 RATE_LIMIT_BURST=0 \
PAIRING_CLAIM_RATE_PER_MINUTE=0 PAIRING_CLAIM_BURST=0 \
FEEDBACK_USER_RATE_PER_MINUTE=0 FEEDBACK_USER_BURST=0 \
FEEDBACK_IP_RATE_PER_MINUTE=0 FEEDBACK_IP_BURST=0 \
RUN_MIGRATIONS_ON_BOOT=false METRICS_ENABLED=true LOG_LEVEL=warn \
go run ./cmd/api

# 3) 逐路径跑（A 轮，每项 30s + 3s 预热）
for s in devices push series changes scenes feedback catalog; do
  ./loadgen -base-url http://127.0.0.1:18080 -scenario $s -duration 30s -warmup 3s \
    -concurrency 50 -push-concurrency 50 -series-concurrency 50 -changes-concurrency 50 \
    -scenes-concurrency 50 -feedback-concurrency 20 -catalog-concurrency 50
done

# 4) push 拐点：同一个用户 10 / 50 / 100 并发
./loadgen -base-url http://127.0.0.1:18080 -token "$TOKEN" -scenario push \
  -duration 30s -warmup 3s -push-concurrency 100

# 5) push 多用户：5 个进程 × 各自匿名用户 × 10 并发（各进程自动注册）
for i in 1 2 3 4 5; do
  ./loadgen -base-url http://127.0.0.1:18080 -scenario push \
    -duration 30s -warmup 3s -push-concurrency 10 &
done; wait

# 6) changes 长 backlog：先同一 token 推 30s 造 69,325 条变更，再读
./loadgen -base-url http://127.0.0.1:18080 -token "$TOKEN" -scenario push -duration 30s -warmup 3s -push-concurrency 10
./loadgen -base-url http://127.0.0.1:18080 -token "$TOKEN" -scenario changes -duration 30s -warmup 3s -changes-concurrency 50
```

## 3. 结果

### 3.1 A 轮：每条路径 50 并发（feedback 20 并发）

| 路径 | 并发 | 请求数 | RPS | p50 | p95 | p99 | 错误率 | 状态码 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| `GET /my/devices` | 50 | 173,260 | 5,773.9 | 8ms | 16ms | 23ms | 0.00% | 200 |
| `GET /readings/series`（90 天，1d bucket，1 设备 2k 读数） | 50 | 143,020 | 4,766.0 | 8ms | 20ms | 77ms | 0.00% | 200 |
| `POST /sync/push`（100 设备 + 200 读数/请求，**同一用户**） | 50 | 1,322 | 42.4 | 1.053s | 2.042s | 2.752s | 0.08% | 200×1,321、503×1 |
| `GET /sync/changes`（`limit=100`，用户仅 100 条变更） | 50 | 4,142 | 136.7 | 345ms | 597ms | 761ms | 0.00% | 200 |
| `GET /scenes`（50 个场景） | 50 | 215,292 | 7,174.5 | 6ms | 13ms | 18ms | 0.00% | 200 |
| `PUT /scenes/{id}`（字段级 name，LWW 真实写入） | 50 | 62,982 | 2,097.7 | 22ms | 43ms | 57ms | 0.00% | 200 |
| `POST /feedback` | 20 | 382,490 | 12,749.3 | 1ms | 3ms | 10ms | 0.00% | 201 |
| `GET /feedback/mine?limit=20` | 20 | 343,325 | 11,443.6 | 1ms | 5ms | 13ms | 0.00% | 200 |
| `GET /catalog/device-types`（带 `If-None-Match`） | 50 | 371,679 | 12,388.2 | 4ms | 7ms | 11ms | 0.00% | **304 全部命中** |

API 进程 CPU（`ps -o time=` 采样，含 3s 预热窗口）：

| 场景 | 并发 | CPU 秒 / 窗口 33s | 折算核数 | CPU/请求 |
| --- | --- | --- | --- | --- |
| `POST /sync/push`（单用户） | 10 | 21.51 | 0.65 | ≈14.6ms |
| `GET /sync/changes`（69,325 条 backlog） | 50 | 6.19 | 0.19 | ≈1.24ms |
| `POST /sync/push`（5 用户 × 10 并发合计） | 50 | 78.48 | 2.38 | ≈16.5ms |

### 3.2 push 拐点（同一用户，设备+读数批量写入）

| 并发 | 请求数 | RPS | p50 | p95 | p99 | 错误率 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 10 | 1,475 | 48.9 | 203ms | 218ms | 227ms | 0.00% | 队列长度 10 |
| 50 | 1,322 | 42.4 | 1.053s | 2.042s | 2.752s | 0.08% | 1 次 503（3s 预算超时） |
| 100 | 1,353 | 41.5 | 2.148s | 4.463s | 5.8s | 3.99% | 54 次 503，日志 `duration_ms=3000` |

**结论**：同一用户的 push 被 `"push:"+userID` advisory lock 串行化，吞吐稳定在 **≈42–49 RPS**（约 4,200–4,900 设备行/s、8,400–9,800 读数行/s），增加并发只增加排队；100 并发时大量请求超过 `pushTimeout=3s` 预算，返回 503 `TIMEOUT`。这是**按用户串行**的预期行为，不是 DB 容量上限。

### 3.3 push 多用户（5 进程 × 各自用户 × 10 并发，共 50 并发）

| 分片 | 请求数 | RPS | p50 | p95 | p99 | 错误率 |
| --- | --- | --- | --- | --- | --- | --- |
| 1 | 950 | 31.4 | 295ms | 570ms | 744ms | 0.00% |
| 2 | 948 | 31.3 | 297ms | 550ms | 693ms | 0.00% |
| 3 | 962 | 31.8 | 291ms | 556ms | 732ms | 0.00% |
| 4 | 947 | 31.3 | 289ms | 596ms | 749ms | 0.00% |
| 5 | 960 | 31.7 | 294ms | 555ms | 706ms | 0.00% |

合计 **≈158.9 RPS**（≈15,900 设备行/s、31,800 读数行/s），API CPU 2.38 核（10 核机器）。即：**吞吐随用户数近似线性放大，直至 API CPU 成为瓶颈**；单用户上限与全局限流无关。

### 3.4 多实例限流（`RATE_LIMIT_BACKEND=postgres`）

配置：`RATE_LIMIT_PER_MINUTE=20`、`RATE_LIMIT_BURST=5`，4 个实例（18081/18082 = postgres，18083/18084 = memory 对照组），同一 IP（127.0.0.1）连续 40 次 `GET /my/devices`，在两个实例间交替发送，测试前清空 `rate_limit_buckets`。

| 后端 | 实例数 | 允许 | 429 | 结论 |
| --- | --- | --- | --- | --- |
| postgres | 2 | 5 | 35 | 额度**共享**，不随实例数放大 |
| memory（对照） | 2 | 9 | 31 | 每进程独立桶，接近 2× 额度 |

postgres 后端写入的单行 `rate_limit_buckets`（key=`127.0.0.1`）在测试后残余 `tokens≈0.02`，证明两次实例读写的是同一个桶。

### 3.5 changes：游标与 backlog 的真实代价

| 场景 | 用户 backlog | 并发 | RPS | p50 | p95 | p99 |
| --- | --- | --- | --- | --- | --- | --- |
| 仅 100 条变更（A 轮） | 100 | 50 | 136.7 | 345ms | 597ms | 761ms |
| 同一 token 造 69,325 条变更后 | 69,325 | 50 | 165.1 | 256ms | 495ms | 672ms |

单查询微基准（psql `EXPLAIN (ANALYZE, BUFFERS)`，同一库）：

- 在 ≈387k 行 `change_log`、`cursor=1000` 时，`/sync/changes` 的主查询计划为 `Index Scan using change_log_pkey`，扫 **72,542 shared buffers（含 15,386 物理读）**，单查询 71–86ms。
- 在 ≈1.11M 行 `change_log`、用户首条变更位于 seq≈1,071,256 时，**可见性下限**查询（`MIN(seq)`）warm 执行 **347.7ms**、命中 **148,539 buffer + 84,734 物理读**；即每个 `/sync/changes` 请求都要先付这笔固定成本。

**结论**：`/sync/changes` 的成本由**全局 seq 位置**决定，而不仅是该用户自己的 backlog。上线前需要：

1. 把 `MIN(seq)` 与主查询拆成 `household_id IS NULL` / 家庭成员两个分支（或在 SQL 层用 `UNION ALL`），让 `(user_id, seq)` 索引可用；
2. 上线后按 `change_log` 总量设置清理/分区，否则新用户的首次同步耗时会随全局行数线性上升；
3. 客户端 `limit` 从 100 起，不要一次性拉大页。

本轮**未改代码**（v0.21.0 不做功能变更），以上为待办项。

### 3.6 已知拐点小结

| 拐点 | 现象 | 证据 |
| --- | --- | --- |
| 单用户 push 并发 > ≈10 | 吞吐不涨，p99 随并发线性上升；100 并发开始 503 | 3.2 表 + 日志 `duration_ms=3000` |
| `change_log` 全局行数增长 | 新用户每次增量同步先付 `MIN(seq)` 全表前缀扫描 | 3.5 微基准（347ms/233k buffers @1.11M 行） |
| API 单进程 CPU ≈2.4 核 | 多用户 push 到 ≈159 RPS 时接近 1/4 机器 CPU；再叠加其它路径会互相挤占 | 3.3 CPU 采样（同机压测，含负载端开销） |
| 反馈写入 | 30s 写入 38 万行、库增长快（本机 1.7 GB 中反馈占大头）；`cleanup` 的保留期策略需按此估算 | 3.1 表 + `cmd/cleanup` 默认 90 天 |

## 4. 未覆盖（不要当作已验证）

- 跨主机网络、TLS（Caddy）、真实域名与 CDN 的延迟；本轮全部为回环 HTTP。
- 多 API 实例 + 负载均衡下的吞吐（仅在限流专项起了 2 个 postgres 实例，未做混合负载）。
- 数小时长稳、内存增长、连接池耗尽曲线（本轮最长 30s/项）。
- `GET /scenes` 未分页：只测了 50 个场景；场景数上千时的列表与 JSONB 解码成本未测。
- WebSocket/SSE、推送通道：服务端目前没有此能力，未测。
- 备份恢复、迁移往返、硬杀重启属于可用性而非性能，结果见 `docs/preflight.md`。
