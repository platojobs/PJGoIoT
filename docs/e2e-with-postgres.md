# 真实存储客户端联调（Postgres e2e stack，v0.19.0）

Android / iOS 仓库的契约 e2e 默认跑 `STORE=fixture`，覆盖不到真实 SQL、迁移、
事务和 `statement_timeout` 行为。本页说明如何一条命令起「真实 Postgres + API」栈，
让客户端 e2e 指向它，以及需要清理的本地状态与两种存储的行为差异。

## 1. 前置条件

- Docker（能拉取 `postgres:16-alpine`）
- Go 1.22+
- `curl`、`python3`（脚本用于选端口与解析 JSON）

## 2. 启动 / 停止

```bash
# 启动：自动选空闲端口、起 Postgres、迁移到最新 version、启动 API、等 readiness
bash scripts/stack-for-e2e.sh
# stdout 最后一行（可直接 eval）：
# BASE_URL=http://127.0.0.1:63450

# 查看状态
bash scripts/stack-for-e2e.sh --status

# 停止：杀 API、删 Postgres 容器、清理 .e2e/
bash scripts/stack-for-e2e.sh --stop
```

人类可读日志全部走 stderr，stdout 只保留 `BASE_URL=...`，便于脚本消费：

```bash
BASE_URL="$(bash scripts/stack-for-e2e.sh 2>/dev/null | tail -1 | cut -d= -f2-)"
```

启动过程会：

1. 用 `python3` 申请空闲端口启动独立 Postgres 容器（`pjgoiot-e2e-pg-*`）；
   若容器启动时端口被并发抢走，会打印「端口 X 已被占用，自动换端口重试」并重试（最多 3 次）。
2. 等 `pg_isready`，执行 `cmd/migrate -direction up`（对 initdb 临时实例导致的连接重置做有限重试），
   再执行 `-version` 确认 `version=16 dirty=false`（以脚本实际输出的最新 version 为准）。
3. 编译并 `nohup` 启动 API：`STORE=postgres`、关闭限流、`APP_VERSION=0.19.0`；
   API 端口同样自动换端口重试。
4. 把 `DATABASE_URL`、`API_PID`、`BASE_URL` 等写入 `.e2e/stack.env`（已在 `.gitignore`）。
5. 任一步失败会自动清理半启动的容器/进程并以非 0 退出。

端口占用实测（用 `E2E_PG_PORT` / `E2E_API_PORT` 故意指向被占用的端口）：

```text
端口 65021 已被占用，自动换端口重试
启动 Postgres（第 2 次尝试，端口 51660）
Postgres 已就绪: 127.0.0.1:51660
端口 65022 已被占用，自动换端口重试
启动 API（第 2 次尝试，端口 51671）
API readiness 通过: http://127.0.0.1:51671
health: 200 pg=51660 api=51671
```

（macOS Docker Desktop 对 `-p` 端口占用不一定报错，因此脚本在启动前先做 connect+bind 预检，
避免把请求映射到错误的监听者上。）

> 注意：API 默认绑定 `127.0.0.1`。模拟器可直接访问（Android 模拟器用 `10.0.2.2`，
> iOS 模拟器用 `127.0.0.1`）；真机通过局域网访问目前不在本栈覆盖范围。

## 3. 写入最小可核实数据

```bash
bash scripts/seed-for-e2e.sh
```

脚本只使用**迁移 seed 中真实存在的型号**，不编造型号：

- 1 个匿名用户（token 存 `.e2e/seed.env`，重复执行复用，不会新增用户）
- 1 台 `user_devices`：`externalId=AA:BB:CC:00:00:02`，MAC 命中公共设备库
  `ESP32-C3-DevKitM-1`（Espressif Systems / `environmental_sensor`）
- 24 条 readings：`temperature_celsius` / `relative_humidity` / `pressure_hpa` × 8 个整点；
  客户端 id 由固定 UUIDv5 派生，重复 push 走 `ON CONFLICT DO NOTHING`
- 校验 5 个 `device_types`、官方固件 `ESP-IDF v5.2.2`、配件 `USB-C 数据线`、公共设备型号

幂等性实测（同一栈连续执行两次）：

```text
== seed 第 1 次 ==
push 结果: applied=1 skippedReadings=0 conflicts=0
校验通过: user_devices=1 readings=24 metrics=pressure_hpa,relative_humidity,temperature_celsius device_types=5 firmware=ESP-IDF v5.2.2 accessory=USB-C 数据线 public_model=ESP32-C3-DevKitM-1

== seed 第 2 次（幂等） ==
复用已有 seed token（user=900e7f4a-01d8-4acb-8e99-017724fa9cd5）
push 结果: applied=1 skippedReadings=24 conflicts=0
校验通过: user_devices=1 readings=24 ...
```

读数时间锚点默认 `2026-10-01T00:00:00Z`，可在 90 天保留窗口内改动：

```bash
E2E_SEED_ANCHOR=2026-11-01T00:00:00Z bash scripts/seed-for-e2e.sh
```

改锚点会新增一组幂等 id（旧组仍保留），适合验证“跨窗口”场景。

## 4. 真实 curl 全链路（本轮实测输出）

```text
1) 匿名登录: {"userId":"164bd689-6f3e-487d-aa65-ddac2dda57ba","expiresAt":"2027-05-04T05:20:30.367973Z","hasToken":true}
2) push: {"applied":1,"skippedReadings":0,"conflicts":0,"serverSeq":[2]}
3) changes: {"changes":1,"nextCursor":2,"entities":["user_device"]}
4) series: {"metric":"temperature_celsius","bucket":"1d","buckets":2,"firstBucket":{"start":"2026-11-04T00:00:00Z","end":"2026-11-05T00:00:00Z","count":2,"min":21.5,"max":22.5,"avg":22,"last":22.5}}
5) metrics: {"items":[{"metric":"temperature_celsius","unit":"celsius","count":8}]}
6) export: {"user":"164bd689-6f3e-487d-aa65-ddac2dda57ba","devices":1,"readings":8,"hasMore":false}
7) 注销 DELETE /me: HTTP 204
8) 注销后访问: HTTP 401 {"error":{"code":"UNAUTHORIZED","message":"token 无效、已过期或已吊销"}}
```

同一栈的 `GET /metrics` 已能按 route 区分（v0.10.0 新增 `route` 标签）：

```text
pjgoiot_http_requests_total{method="GET",route="/api/v1/my/devices",status="200"} 3
pjgoiot_http_requests_total{method="GET",route="/api/v1/my/devices",status="401"} 1
pjgoiot_http_requests_total{method="GET",route="/api/v1/my/devices/:platform/:externalId/readings",status="200"} 2
pjgoiot_http_requests_total{method="GET",route="/api/v1/my/devices/:platform/:externalId/readings/metrics",status="200"} 1
pjgoiot_http_requests_total{method="GET",route="/api/v1/my/devices/:platform/:externalId/readings/series",status="200"} 1
pjgoiot_http_requests_total{method="GET",route="/api/v1/health",status="200"} 1
```

## 5. 指向该栈（客户端仓库）

### Android

1. 启动栈后取 `BASE_URL`，把测试构建的 base URL 指向：
   - 模拟器：`http://10.0.2.2:<API_PORT>`（10.0.2.2 是宿主机 loopback 映射）
   - 单元测试/Robolectric：直接 `http://127.0.0.1:<API_PORT>`
2. 通过 `local.properties` / Gradle 属性 / `BuildConfig` 注入，不要改生产默认值。
3. 清理本地状态：

```bash
# 清 token、游标、Room/SQLite、DataStore
adb shell pm clear <your.package>
# 若 token 走 Keystore 且怀疑残留，彻底卸载重装
adb uninstall <your.package>
```

4. 从 fixture 切到真实栈时，**必须同时清游标**：fixture 的 `seq` 从 1 重新计数，
   把旧游标带到 postgres 会被判为过期并返回 `410 RESYNC_REQUIRED`。

### iOS

1. 模拟器 base URL 用 `http://127.0.0.1:<API_PORT>`；若使用 `https` 自签证书需在
   URLSession 信任链里放行测试证书（本栈是纯 HTTP，无需 ATS 例外；
   若要访问局域网真机地址，需 `NSAllowsLocalNetworking`）。
2. 清理本地状态：

```bash
xcrun simctl uninstall booted <bundle-id>   # 清 UserDefaults/CoreData/SQLite/URLCache
xcrun simctl keychain booted reset          # Keychain 里的 token 不会随卸载必清
```

3. 若 App 内实现了「清除本地数据 / 重新登录」调试入口，优先用它（同时清 token + cursor + 本地缓存）。

### 清空服务端数据

- 简单：`--stop` 后再启动（容器删除，数据库全新）。
- 保留栈但清表：

```bash
source .e2e/stack.env
docker exec "$PG_CONTAINER" psql -U pjgoiot -d pjgoiot_e2e \
  -c 'TRUNCATE readings, user_devices, change_log, idempotency_keys, pairing_codes, tokens, users RESTART IDENTITY CASCADE'
```

清理后重新执行 `seed-for-e2e.sh`（旧 seed token 会 401，脚本自动重新匿名登录）。

## 6. fixture 与 postgres 的差异清单

| 维度 | `STORE=fixture` | `STORE=postgres`（本栈） | 客户端 e2e 要注意 |
| --- | --- | --- | --- |
| 持久化 | 进程内存，API 重启即清空 | 容器内磁盘（本栈随容器删除） | fixture 下“重启后仍存在”类断言必然失败 |
| 迁移 / schema | 无 SQL、无迁移版本 | golang-migrate 8 个版本，启动前已迁移 | 唯一约束、外键、级联删除才会真实生效 |
| 并发 / 事务 | 全局互斥锁串行 | 真并发 + 事务 | fixture 看不出竞态、死锁、部分写入 |
| 幂等键 | 内存 map，`Cleanup` 后失效 | `idempotency_keys` 表，保留 7 天（可配） | 重放窗口与重启行为不同 |
| `change_log` 保留 / 410 | 有 410 语义，但无 90 天保留调度 | 真实保留 + `cmd/cleanup`，旧 cursor 返回 410 `RESYNC_REQUIRED` | 想测 410 必须 postgres + 手动清理 |
| LWW | 内存比较 `updatedAt` | SQL `ON CONFLICT ... WHERE updated_at < ...` | 相同 `updatedAt` 的边界行为要用 postgres 验证 |
| token 过期 | 内存校验 `expiresAt`，重启即丢 | `tokens.expires_at`，`TOKEN_TTL_DAYS` + `cmd/cleanup` | 过期/吊销后 401 需在真实栈验证 |
| 超时保护 | 无（纯内存，几乎不可能超时） | `statement_timeout`/context 3s，超时 503 `TIMEOUT` | 客户端需按 503 重试，而不是当 500 |
| 列表排序 / limit | 内存切片，行为接近 | SQL `ORDER BY` + limit 校验（readings 500 / export 5000） | 最大页边界只能在 postgres 测 |
| 性能 | 常数级，无意义 | 受索引、连接池、数据量影响 | 压测数字只以 postgres 为准 |

## 7. 已知限制

- 栈绑定 `127.0.0.1`，覆盖模拟器；**真机通过局域网**访问未纳入本轮验证。
- 脚本用 Docker 起 Postgres，本机原生 Postgres 也可手工用同一 `DATABASE_URL` 跑迁移+API。
- seed 的读数锚点是固定日期，超出 90 天保留窗口后会被 `cmd/cleanup` 清掉；
  届时用 `E2E_SEED_ANCHOR` 换一个近期时间点。
