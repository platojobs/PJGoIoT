# 服务端压测基线（v0.21.0 基线 + v0.22.0 下限优化 + v0.24.0 多家庭）

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

**结论**：`/sync/changes` 的成本由**全局 seq 位置**决定，而不仅是该用户自己的 backlog。v0.22.0 已按第 1 条完成下限查询改造，结果见 §3.6；第 2、3 条仍待办。

### 3.6 v0.22.0 可见性下限查询改造（同一库、同一用户、前后对比）

**根因（为什么原来的写法必须扫）**

可见性下限是「本人行 ∪ 所属家庭行」中的最小 seq。原 SQL 写成：

```sql
SELECT MIN(c.seq) FROM change_log c
WHERE c.user_id = $1 OR c.household_id IN (
  SELECT hm.household_id FROM household_members hm WHERE hm.user_id = $1)
```

`change_log` 上其实**早已存在** `change_log_user_seq_idx (user_id, seq)`（000001）与
`change_log_household_seq_idx (household_id, seq)`（000011），但 `OR` 把两个来源合并在一个
过滤条件里：优化器无法对「两个索引的并集」直接做 `MIN` 的 `LIMIT 1` 短路，只能选一条有序路径
（这里是 `change_log_pkey (seq)`）从 seq 最小端开始逐行 `Filter`，直到遇到第一条满足
`user_id/household_id` 条件的行。**代价因此正比于「全局最老行到该用户首条可见行」的距离**，
而不是该用户自己的数据量：全局行数越多，这段前缀越长，buffer 访问越多。实测计划中
`Rows Removed by Filter: 225,355`，即为一个用户的下限白白读了 22.5 万条索引项。

**改法：把并集拆成「各来源各取 MIN，再取整体 MIN」**

```sql
SELECT min(x) FROM (
  SELECT min(seq) AS x FROM change_log WHERE user_id = $1
  UNION ALL
  SELECT (SELECT min(seq) FROM change_log WHERE household_id = m.household_id) AS x
  FROM household_members m WHERE m.user_id = $1
) s
```

`MIN` 对集合并取最小值，等价于先对每个来源取最小值再取全局最小值——语义完全一致：
- 不改变 410 语义（仍以同一个下限值与 `cursor` 比较）；
- 不改变返回顺序（主查询 SQL 未动）；
- 不漏判可见性（本人行与全部家庭行仍是并集）。

每个分支都由 `(user_id, seq)` / `(household_id, seq)` 的有序前缀做 `Index Only Scan ... LIMIT 1`，
代价只与家庭数有关（本例 1 个家庭 → 2 个分支），与 `change_log` 全局行数无关。

**未选「物化每用户最老可见 seq」的原因**

物化表需要在写入时同步维护：一次 push 的家庭行要更新**所有家庭成员**的 floor（扇出），
清理任务删除旧行时 floor 又要前移；成员加入/退出、账号删除还需按历史重算；多实例下要与
push 走进同一事务或引入额外锁，否则会出现「行已写入但 floor 未更新」的漏判。
相比之下，索引探测是纯读、无写放大、无需回填，也不会因崩溃/并发产生不一致。
因此选择索引改写，而不是新增物化表（本次**不需要迁移**，索引在既有迁移中已具备）。

**同一环境 / 同一数据规模的实测**

环境：M1 Max 10 核 / 32 GB，PostgreSQL 16.15（Homebrew），库 `pjgoiot_load`；
`change_log` **1,112,155 行**、`user_devices` **21,600 台**、19 个家庭；代表性用户本人行 325,531、
家庭行 94,235。测法：`EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON)`，5 次预热后连续 30 次取
`Execution Time` 与 `shared hit+read` 中位数。

| 版本 | 下限查询形态 | p50 | p95 | buffers/次（中位） | 代表性计划 |
| --- | --- | --- | --- | --- | --- |
| 改造前 | `user_id OR household_id IN (...)` | **62.77ms** | **66.28ms** | **87,928** | `Index Scan using change_log_pkey`，`Rows Removed by Filter: 225,355` |
| 改造后 | 本人 MIN + 每家庭 MIN 的 `UNION ALL` | **0.35ms** | **0.56ms** | **10** | 两次 `Index Only Scan ... (actual rows=1)`，`Heap Fetches: 0` |

p95 从 66.28ms 降到 0.56ms（**约 118×**），buffer 访问从 87,928 降到 10（**约 8,800×**），
满足「≥100 万行 + ≥2 万设备下 p95 < 30ms」。

复现命令（同一 DSN 下替换用户 id 即可）：

```bash
psql "$DB" -c "EXPLAIN (ANALYZE, BUFFERS)
SELECT min(x) FROM (
  SELECT min(seq) AS x FROM change_log WHERE user_id = '$UID'
  UNION ALL
  SELECT (SELECT min(seq) FROM change_log WHERE household_id = m.household_id) AS x
  FROM household_members m WHERE m.user_id = '$UID') s"
```

**未验证的部分**：多家庭（>10 个）用户的 p95 未单独压测；探测分支数随家庭数线性增长，
但每个分支仍是 O(log n)，预计远低于 30ms。跨主机网络与真实生产数据量（>1 亿行）未测。


### 3.7 已知拐点小结

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

## 5. v0.24.0：多家庭 `/sync/changes` 与场景长稳

> 本节数字全部来自本机真实 PostgreSQL；数据集、脚本与原始输出可复跑。
> 原始测量结果：[v0.24.0-sync-floor.md](perf-results/v0.24.0-sync-floor.md)；
> 数据集：[perf-multi-household-seed.sql](../scripts/perf-multi-household-seed.sql)；
> 测量脚本：[perf-sync-floor.sh](../scripts/perf-sync-floor.sh)。

### 5.1 环境与数据规模

| 项 | 值 |
| --- | --- |
| 机器 | Apple M1 Max，10 核，32 GB，macOS 27.x（与 API、PostgreSQL 同机） |
| PostgreSQL | 16.15 (Homebrew)，`shared_buffers=128MB`、`work_mem=4MB`、`effective_cache_size=4GB` |
| 目标用户 | 1 个，加入 **16 个家庭**（1 个个人家庭 + 15 个共享家庭） |
| 家庭内容 | 每个家庭 3 个房间 + 20 台设备（共 320 台，全部归目标用户） |
| 填充用户 | 2,000 个（只用于制造全局 `change_log` 体积） |
| `change_log` | **1,080,100 行 / 357 MB**；目标用户可见行约 10 万，集中在全局 seq 60 万之后 |
| 索引 | `(user_id, seq)`、`(household_id, seq)`、`(seq)`、`(server_ts)`；**本轮未新增迁移/索引** |

复现：

```bash
psql postgres://platojobs@127.0.0.1:5432/postgres -c "create database pjgoiot_perf24"
go run ./cmd/migrate -database-url 'postgres://platojobs@127.0.0.1:5432/pjgoiot_perf24?sslmode=disable' -direction up
psql 'postgres://platojobs@127.0.0.1:5432/pjgoiot_perf24?sslmode=disable' \
  -v ON_ERROR_STOP=1 -f scripts/perf-multi-household-seed.sql
DB_URL='postgres://platojobs@127.0.0.1:5432/pjgoiot_perf24?sslmode=disable' scripts/perf-sync-floor.sh
```

### 5.2 可见性下限：家庭数从 1 增到 16

查询保持 v0.22.0 的「本人 MIN + 每家庭 MIN 的 UNION ALL」，每分支都是
`Index Only Scan ... (actual rows=1)`（30 次 `EXPLAIN ANALYZE` 的实际执行耗时）：

| 家庭数 | p50 (ms) | p95 (ms) | 均值 (ms) | max (ms) | shared buffers |
| --- | --- | --- | --- | --- | --- |
| 1 | 0.405 | 0.632 | 0.481 | 1.486 | 9 |
| 4 | 0.470 | 0.700 | 0.510 | 0.859 | 18 |
| 8 | 0.475 | 0.754 | 0.538 | 1.098 | 30 |
| 16 | 0.718 | 1.035 | 0.739 | 1.315 | 54 |

结论：

- buffer 数随家庭数近似线性增长（每家庭约 3 个 buffer），因为每个家庭各做一次
  `LIMIT 1` 的索引探测；但**墙钟耗时是亚线性的**：家庭数 ×16，p50 仅 ×1.77、p95 仅 ×1.64，
  绝对值仍 ≤ 1.1 ms。每个分支是 O(log N) 的索引下降，与 `change_log` 全局 108 万行无关。
- 查询结构不需要再改：把 16 个家庭压成一个聚合查询反而会退化为全表/位图扫描。
  若未来单用户家庭数继续上升（远超 16），可考虑按 user 缓存 floor 或物化每家庭 floor，
  但当前 1 ms 量级不值得引入写放大与一致性成本。

### 5.3 主查询：旧 OR/IN 与新分支结构

旧实现 `WHERE user_id = $1 OR household_id IN (...) ORDER BY seq LIMIT` 在客户端首次同步
（`cursor=0`）时会从全局 seq 起点扫描到该用户第一条可见行。16 个家庭、limit=100 的实测：

| 游标 | 查询 | 样本 | p50 (ms) | p95 (ms) | max (ms) | shared buffers |
| --- | --- | --- | --- | --- | --- | --- |
| 0 | 旧 OR/IN | 10 | 100.046 | **104.904** | 141.490 | 16,282 |
| 0 | 新分支结构 | 10 | 0.778 | **2.402** | 5.904 | 121 |
| 640000 | 旧 OR/IN | 10 | 0.194 | 0.386 | 0.554 | 10 |
| 640000 | 新分支结构 | 10 | 2.761 | 5.509 | 8.429 | 282 |

新结构（v0.24.0 实装）：本人分支 + 每个家庭分支各自用复合索引取 `cursor` 之后的前
`limit+1` 行，外层 `DISTINCT ON (seq)` 去重并按 `seq` 归并截断。语义等价性：

- 本人分支覆盖 `user_id = 当前用户` 的全部行；
- 家庭分支覆盖 `household_id = 当前用户任一家庭` 的行；
- 两条分支同时命中的行（个人家庭、本人写给共享家庭）由 `seq` 唯一键去重；
- 每个分支最多取 `limit+1` 行，足以覆盖全局前 `limit+1` 条可见变更（任一条全局前 N 的
  可见行必然属于某个分支的前 N 行）。

**代价与取舍（不夸大）**：

- 首次同步：p95 从 104.9 ms 降到 2.4 ms（约 44×），buffer 从 16,282 降到 121（约 135×）。
- 游标已接近尾部时：旧结构 p95 0.386 ms 反而更快，新结构 p95 5.5 ms。
  原因是旧结构沿 `(seq)` 主键顺序扫描，当前数据集里该用户可见行在游标后很密集；
  一旦用户长时间未同步、而其他用户在此期间写了大量变更，旧结构会退化为「跨大量无关行」
  的扫描（`cursor=0` 即该退化形态的最坏例），新结构的成本上界只与「家庭数 × limit」有关。
- 因此 v0.24.0 选择有界的最坏情况：正常增量仍远低于 30 ms SLO，首次/回流同步不再出现
  100 ms 级尖刺。若未来要同时压低尾部增量延迟，可考虑按游标位置在两种结构间路由，
  但会引入两套语义与测试成本，本轮不做。

### 5.4 未验证

- 单用户家庭数 > 16（如 100 个家庭）的 floor 与主查询未压测；分支数继续增长时的拐点未知。
- 真实生产量级（`change_log` > 1 亿行、跨主机网络、TLS）未测；本轮为单机回环。
- 新主查询在 `limit=500` 时（分窗上限）p95 会上升到约 17 ms（16 家庭、cursor=0、单次
  `EXPLAIN ANALYZE`），仍低于 30 ms，但未做 30 次采样与端到端压测。

## 6. v0.24.0 场景路径长稳压测（GET /scenes 与场景写入）

> 原始结果：[v0.24.0-scenes-soak.md](perf-results/v0.24.0-scenes-soak.md)；
> 压测后追加的 `/sync/changes` 端到端结果：[v0.24.0-changes-endpoint.md](perf-results/v0.24.0-changes-endpoint.md)；
> 复现脚本：[scenes-soak.sh](../scripts/scenes-soak.sh)。

### 6.1 环境、数据与负载

| 项 | 值 |
| --- | --- |
| 机器 | 同 §5.1（Apple M1 Max，10 核，32 GB；API、PostgreSQL、压测进程同机） |
| API | 单进程 `/tmp/pjgoiot-perf/api`，`STORE=postgres`，`APP_VERSION=0.24.0`，`ADDR=127.0.0.1:18080` |
| 连接池 | `DB_MAX_OPEN_CONNS=20`、`DB_MAX_IDLE_CONNS=10`（默认值，未调大） |
| 限流 | 全部关闭（`RATE_LIMIT_PER_MINUTE=0` 等） |
| 数据 | `pjgoiot_perf24`；**150 个场景**（每轮预置 50 个，共 3 轮）、16 个家庭、**420 台用户设备**（含此前 smoke 轮次残留）；`change_log` 开始时 1,080,100 行 |
| 负载方式 | GET /scenes 与 PUT /scenes 各**连续 10 分钟**、40 并发、1 分钟分窗；每 10 秒采样连接池与 `pg_stat_activity` |
| 采样 | 121 个点（约 20 分钟，含两次 10 分钟） |
| 写入副作用 | 场景写入产生 1,179,244 条 `change_log` 记录；结束时 `change_log` 共 **2,259,444 行 / 1,167 MB**（自动清理后 `n_dead_tup=0`） |

复现：

```bash
# API 与数据库准备好后（真实 token 对应 pjgoiot_perf24 中的用户）
BASE_URL=http://127.0.0.1:18080 \
TOKEN='<token>' \
DATABASE_URL='postgres://platojobs@127.0.0.1:5432/pjgoiot_perf24?sslmode=disable' \
DURATION=10m WINDOW=1m CONCURRENCY=40 SAMPLE_INTERVAL=10 \
scripts/scenes-soak.sh
```

### 6.2 客户端结果（整段 10 分钟）

| 场景 | 并发 | 请求数 | RPS | p50 | p95 | p99 | 错误率 | 状态码 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| GET /scenes | 40 | 2,170,840 | 3,617.9 | 10 ms | **20 ms** | **39 ms** | 0.00% | 200 全部 |
| PUT /scenes | 40 | 1,106,619 | 1,844.3 | 20 ms | **37 ms** | **48 ms** | 0.00% | 200 全部 |

### 6.3 分窗稳定性（1 分钟窗口）

10 个完整窗口内：

- GET：p95 在 **17–28 ms** 之间，p99 在 28–69 ms 之间；窗口 1–3 略高（p95 20→28 ms），
  之后稳定在 17–18 ms，未随时间劣化。
- PUT：p95 稳定在 **36–37 ms**，p99 46–50 ms；10 个窗口无漂移。

没有出现「跑一段时间后延迟爬升」的现象；两段压测期间均无错误、无非 2xx。

### 6.4 连接池占用与超时行为

| 指标 | 实测 |
| --- | --- |
| 连接池上限 | 20 |
| `in_use` 平均 / 最大 | 19.8 / 20 |
| `open` 平均 / 最大 | 19.9 / 20 |
| 池等待次数增量（`pjgoiot_db_pool_wait_total`） | 6,626,377 |
| 池等待耗时增量（`pjgoiot_db_pool_wait_seconds_total`） | 22,700.749 s |
| 单次获取平均排队 | ≈ 3.4 ms（22,700.749 / 6,626,377） |
| 服务端连接（含脚本自身 psql）平均 / 最大 | 21.0 / 22 |
| 服务端 `active` 平均 / 最大 | 13.9 / 22 |

结论：

- 40 并发下单进程的连接池**全程打满**（`in_use=20/20`），请求主要时间花在等连接；
  但排队是**有界**的（平均 3.4 ms），因此 P95/P99 仍保持在 20/39 ms（读）与 37/48 ms（写）。
- **没有观察到超时或 503**：客户端 10 s 超时未触发、错误率 0、全部 200；
  压测期间 `/metrics`（自身也要查库）121/121 次采样成功（30 s 超时 + 重试），说明
  控制面在池饱和时仍可取得数据。
- 这说明当前上限不在数据库容量，而在 `DB_MAX_OPEN_CONNS=20`；提高池大小可能提升吞吐，
  但本机 `max_connections=100` 且生产可能多实例共享，**未做调大后的对照实验**，不将其作为结论。
- 需要说明：API 开启了请求日志，20 分钟压测写出约 400 MB 日志；延迟包含这部分 I/O 开销。

### 6.5 压测后追加：`/sync/changes` 端到端

压测把 `change_log` 从 108 万行推到 **226 万行**（目标用户可见约 120 万行）后，
用 20 并发、1 分钟、10 秒分窗复测 `GET /sync/changes?limit=100`：

| 场景 | 并发 | 请求数 | RPS | p50 | p95 | p99 | 错误率 |
| --- | --- | --- | --- | --- | --- | --- | --- |
| GET /sync/changes | 20 | 78,446 | 1,307.1 | 11 ms | **35 ms** | **82 ms** | 0.00% |

分窗上 RPS 从 2,134 递减到 905、p50 从 4 ms 升到 20 ms：worker 的游标从 0 逐步推进到
大 backlog 深处，后续每页都要在 120 万条可见行里走索引并返回 100 条 JSONB；这是
「深游标 + 大 backlog」的真实成本，不是查询结构随家庭数劣化。

结论：在 226 万行 / 120 万可见行的数据集上，端到端 p95 35 ms、0 错误；比空库/小 backlog
时的 12 ms 高，但仍是**有界**的（每分支 `limit+1` 行），没有回到旧 OR/IN 的 100 ms 级。
本轮没有承诺端到端 p95 < 30 ms；该值受数据规模与并发共同影响。

### 6.6 结论与边界

- 场景读/写路径在 40 并发、各 10 分钟下稳定：0 错误、P95 20/37 ms、P99 39/48 ms，无漂移。
- 单进程的瓶颈是连接池（20/20 饱和）；排队平均 3.4 ms，未触发请求超时或 503。
- `/sync/changes` 在 226 万行、120 万可见行下 p95 35 ms，仍远低于旧结构的 100 ms 级尖刺。
- 本轮未调整任何索引、迁移或查询结构；v0.24.0 无 schema 变更。

### 6.7 未验证（不要当作已成立）

- 并发 > 40、单段 > 20 分钟、多 API 实例、跨主机 + TLS 下的场景长稳与连接池行为。
- 主动把排队时间推到请求超时预算（10 s）以上、观察 503 `TIMEOUT` 的边界；本轮池等待
  平均只有 3.4 ms，未触发。
- 场景数上千时的 `GET /scenes` 列表与 JSONB 解码成本（本轮 150 个场景，未到上千）。
- `change_log` 增长到 226 万行后，cleanup 保留期与磁盘增长的实际影响（本轮未跑 cleanup）。
- 调大 `DB_MAX_OPEN_CONNS` 后的吞吐/延迟对照，以及它与生产 `max_connections` 的关系。

## 7. v0.25.0：`user_devices.marked_at` 后的 `/sync/changes` 复测

> 目标：确认新增可空列 `marked_at` 没有拖差 `/sync/changes`。沿用 v0.24.0 的多家庭数据集
> 与同一脚本来复测，数字口径完全可比。原始结果：
> [v0.25.0-sync-floor.md](perf-results/v0.25.0-sync-floor.md)。

### 7.1 数据集与差异

| 项 | 值 |
| --- | --- |
| 数据库 | `pjgoiot_perf25`（v0.24.0 同款 seed，仅迁移多了一版） |
| 迁移版本 | 19（新增 `000019_device_marked_at`） |
| `change_log` | 1,080,000 行 / 357 MB（与 v0.24.0 一致） |
| 用户 / 家庭 / 成员 / 房间 / 设备 | 2,033 / 16 / 46 / 48 / 320 |
| 目标用户 | 1 个，16 个家庭 |
| 索引 | 未新增；`marked_at` 是普通可空列，不参与 `change_log` 过滤 |

复现：

```bash
psql postgres://platojobs@127.0.0.1:5432/postgres -c "create database pjgoiot_perf25"
go run ./cmd/migrate -database-url 'postgres://platojobs@127.0.0.1:5432/pjgoiot_perf25?sslmode=disable' -direction up
psql 'postgres://platojobs@127.0.0.1:5432/pjgoiot_perf25?sslmode=disable' \
  -v ON_ERROR_STOP=1 -f scripts/perf-multi-household-seed.sql
DB_URL='postgres://platojobs@127.0.0.1:5432/pjgoiot_perf25?sslmode=disable' scripts/perf-sync-floor.sh
```

### 7.2 可见性下限（与 v0.24.0 对照）

| 家庭数 | v0.24.0 p95 (ms) | v0.25.0 p95 (ms) | 变化 | buffers (24→25) |
| --- | --- | --- | --- | --- |
| 1 | 0.632 | 0.579 | -8.4% | 9 → 9 |
| 4 | 0.700 | 0.621 | -11.3% | 18 → 18 |
| 8 | 0.754 | 0.752 | -0.3% | 30 → 30 |
| 16 | 1.035 | 0.850 | -17.9% | 54 → 54 |

buffer 数完全不变（floor 查询不读取设备行），p95 的下降属于同机测量的正常波动。
**结论：新字段没有改变可见性下限的查询计划与成本。**

### 7.3 主查询（16 家庭，limit=100）

| 游标 | 查询 | v0.24.0 p95 (ms) | v0.25.0 p95 (ms) | buffers (24→25) |
| --- | --- | --- | --- | --- |
| 0 | 旧 OR/IN | 104.904 | 104.793 | 16,282 → 16,282 |
| 0 | 新分支结构 | 2.402 | 2.985 | 121 → 121 |
| 640000 | 旧 OR/IN | 0.386 | 0.255 | 10 → 10 |
| 640000 | 新分支结构 | 5.509 | 5.804 | 282 → 271 |

`change_log` 查询只投影 `entity/payload`，设备行是 payload 的一部分，`marked_at`
不参与 WHERE/ORDER BY，因此执行计划与 buffer 命中不变；±25% 属于 10 次采样的抖动。
绝对值仍在 3–6 ms 量级，离 30 ms SLO 有充分余量。**结论：无性能回退。**

### 7.4 未验证

- 标记密集写入（大量设备同时 mark/unmark）下的 `change_log` 写放大与 payload 体积增长未压测。
- 设备 `marked_at` 高频变更对 `/sync/changes` 端到端（HTTP + JSON 编解码）的影响未单独测量；
  本轮只测数据库层 SQL。

## 8. v0.26.0：`user_devices.last_seen_at` 后的 `/sync/changes` 复测

> 目标：确认新增可空列 `last_seen_at`（在线状态来源）没有拖差 `/sync/changes`。
> 沿用 `pjgoiot_perf25` 的 16 家庭数据集，在同一库上做迁移前/后 A/B：
> 先回滚到 19 跑一轮，再迁移到 20 跑同一脚本，两组均 60 次 floor + 30 次主查询采样。

### 8.1 数据集与差异

| 项 | 值 |
| --- | --- |
| 数据库 | `pjgoiot_perf25` |
| 迁移版本 | 19（before）→ 20（after，新增 `000020_device_presence`） |
| `change_log` | 1,080,000 行 / 357 MB |
| 用户 / 家庭 / 成员 / 房间 / 设备 | 2,033 / 16 / 46 / 48 / 320 |
| 目标用户 | 1 个，16 个家庭 |
| 索引 | 未新增；`last_seen_at` 是 `user_devices` 上的普通可空列，不参与 `change_log` 过滤 |

复现：

```bash
DB_URL='postgres://platojobs@127.0.0.1:5432/pjgoiot_perf25?sslmode=disable' \
  RUNS=60 LEGACY_RUNS=30 scripts/perf-sync-floor.sh          # before（version=19）
go run ./cmd/migrate -database-url 'postgres://platojobs@127.0.0.1:5432/pjgoiot_perf25?sslmode=disable' -direction up
DB_URL='postgres://platojobs@127.0.0.1:5432/pjgoiot_perf25?sslmode=disable' \
  RUNS=60 LEGACY_RUNS=30 scripts/perf-sync-floor.sh          # after（version=20）
```

### 8.2 可见性下限（before → after）

| 家庭数 | 迁移前 version=19 p95 (ms) | 迁移后 version=20 p95 (ms) | 变化 | buffers (19→20) |
| --- | --- | --- | --- | --- |
| 1 | 0.734 | 0.726 | -1.1% | 9 → 9 |
| 4 | 0.701 | 0.819 | +16.8% | 18 → 18 |
| 8 | 1.256 | 0.755 | -39.9% | 30 → 30 |
| 16 | 0.905 | 0.869 | -4.0% | 54 → 54 |

floor 查询只读 `change_log` 与 `household_members`，buffer 数完全一致；p95 差异属于同机测量抖动。
**结论：新列没有改变可见性下限的查询计划与成本。**

### 8.3 主查询（16 家庭，limit=100）

| 游标 | 查询 | 迁移前 p95 (ms) | 迁移后 p95 (ms) | buffers (19→20) |
| --- | --- | --- | --- | --- |
| 0 | 旧 OR/IN | 131.178 | 122.711 | 16,282 → 16,282 |
| 0 | 新分支结构 | 5.708 | 4.179 | 121 → 121 |
| 640000 | 旧 OR/IN | 0.366 | 0.434 | 10 → 10 |
| 640000 | 新分支结构 | 7.135 | 5.896 | 271 → 271 |

`/sync/changes` 的 SQL 只读 `change_log`，`last_seen_at` 只出现在 `payload` 里（且本查询不解析 payload），
执行计划与 buffer 命中完全不变。**结论：无性能回退。**

### 8.4 未验证

- 新增的 handler 侧 `presence` 重算只对 `user_device` 行做一次 JSON 解码/编码（每页 ≤100 行），
  未单独做端到端 HTTP 压测；本轮数字仍是数据库层 SQL。
- 读数高频上报带来的 `user_device` 变更写放大与 `change_log` 增长速度未压测。

