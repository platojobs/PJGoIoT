# PJGoIoT 后端

「蓝牙设备数据面板」Android / iOS App 的后端服务：多用户账号、BLE 设备公共元数据、用户私有设备与增量同步。

- 语言 / 运行时：Go 1.22+
- HTTP 框架：Gin v1.10（仅 `internal/httpapi` 层依赖）
- 数据库：PostgreSQL 16（迁移 golang-migrate，查询 sqlc，不使用 ORM）
- 日志：标准库 `log/slog` 结构化日志
- 契约：[`api/openapi.yaml`](api/openapi.yaml)，当前版本 **v0.5.1**（契约核对：四个读数路径与 push 读数字段齐全；补齐 metrics 的 400 响应与公共端点 `security: []`）

## 快速开始

### 方式一：fixture 模式（无需数据库）

```bash
make dev
# 等价于 STORE=fixture go run ./cmd/api
```

公共元数据读取 `fixtures/*.json`，用户与设备数据保存在内存中，进程重启即丢失。适合联调接口形状。

```bash
curl -s http://127.0.0.1:8080/api/v1/health
```

### 方式二：Docker Compose（Postgres + 迁移 + API）

```bash
docker compose up --build
```

- API：`http://127.0.0.1:8080`
- Postgres：`127.0.0.1:55432`（用户/密码/库名均为 `pjgoiot`）
- `migrate` 服务在数据库健康后执行迁移，API 等待迁移成功再启动。

### 方式三：本地 Postgres

```bash
createdb pjgoiot
export DATABASE_URL='postgres://platojobs@127.0.0.1:5432/pjgoiot?sslmode=disable'
make migrate        # 建表 + seed
make dev-postgres   # Postgres 模式启动 API
```

不设置 `STORE` 时，只要 `DATABASE_URL` 非空就自动使用 Postgres；否则使用 fixture。

## 接口清单

所有接口前缀 `/api/v1`。成功响应为裸 JSON，失败响应固定为：

```json
{"error":{"code":"NOT_FOUND","message":"资源不存在或无权访问"}}
```

| 方法 | 路径 | 鉴权 | 成功状态 | 说明 |
| --- | --- | --- | --- | --- |
| GET | `/health` | 否 | 200 | 健康检查，含存储 Ping；失败 503 |
| POST | `/auth/anonymous` | 否 | 201 | 创建匿名用户，返回 `userId` 与不透明 `token` |
| POST | `/auth/login` | 否 | 501 | 预留账号密码登录，当前明确未实现 |
| POST | `/auth/logout` | Bearer | 204 | 吊销当前 token |
| GET | `/devices/{mac}` | 否 | 200 | 公共设备元数据（厂商、型号、固件、GATT、兼容性） |
| GET | `/device-types` | 否 | 200 | 设备类型列表，`{"items":[...]}` |
| GET | `/device-types/{type}` | 否 | 200 | 按类型查询设备类型元数据，供 iOS 等无法获取 MAC 的端使用 |
| GET | `/device-types/{type}/catalog` | 否 | 200 | 按类型聚合元数据、固件、配件，`{deviceType,firmwares,accessories}` |
| GET | `/device-types/{type}/firmwares` | 否 | 200 | 指定类型的固件列表（历史端点，保持可用） |
| GET | `/device-types/{type}/accessories` | 否 | 200 | 指定类型的配件列表（历史端点，保持可用） |
| GET | `/my/devices` | Bearer | 200 | 当前用户未删除设备列表 |
| GET | `/my/devices/{mac}` | Bearer | 200 | 按 MAC 元数据查单台设备（仅 Android；iOS 用下一行接口） |
| GET | `/my/devices/{platform}/{externalId}` | Bearer | 200 | 按设备唯一键查单台设备，iOS 无需 MAC |
| GET | `/my/devices/{platform}/{externalId}/readings` | Bearer | 200 | 读数列表，`recordedAt` 倒序游标分页，`{items,nextCursor,hasMore}` |
| GET | `/my/devices/{platform}/{externalId}/readings/summary` | Bearer | 200 | 单指标聚合，`{metric,unit,count,min,max,avg,last,windowStart,windowEnd}` |
| GET | `/my/devices/{platform}/{externalId}/readings/series` | Bearer | 200 | UTC 对齐分桶序列，`{metric,unit,bucket,from,to,buckets[]}` |
| GET | `/my/devices/{platform}/{externalId}/readings/metrics` | Bearer | 200 | 指标清单，`{items:[{metric,unit,count,firstAt,lastAt}]}` |
| GET | `/sync/changes?cursor=&limit=` | Bearer | 200 | 增量变更，`{changes,nextCursor,hasMore}` |
| POST | `/sync/push` | Bearer | 200 | 批量写入设备与读数，`{applied,conflicts,nextCursor,appliedReadings,skippedReadings}` |

### 设备详情与市场字段（v0.3.2）

`UserDevice`（`/my/devices`、`/my/devices/{platform}/{externalId}`）在原有字段上新增：

| 字段 | 来源 | 缺失时 |
| --- | --- | --- |
| `deviceType` | 用户记录上报的 `deviceType`；缺失时按 `mac` 关联 `public_devices` | `null` |
| `vendor` | 优先 `public_devices.vendor`，回退 `device_types.vendor` | `null` |
| `firmwareVersion` | `public_devices.firmwareVersion`（即将上线/参考版本） | `null` |
| `compatibility` | 优先 `public_devices.compatibility`，回退 `device_types.compatibility` | `null` |

关联缺失（例如 iOS 未上报型号且无 MAC）时上述字段返回 `null`，接口仍为 200，不会 500。
`POST /sync/push` 的 `PushDevice` 新增可选 `deviceType`（服务端统一转小写，最长 64）；
客户端不上报且无法按 MAC 推导时，服务端沿用已存型号，避免更新把字段清空。

`GET /device-types/{type}/catalog` 的 `firmwares` 与 `accessories` 已补齐：

- `vendor`：固件发布方 / 配件厂商（未核实的记录为空字符串，不编造）；
- `compatibility`：型号级或配件兼容性（如 `platforms` / `features` / `deviceTypes`）；
- `accessories` 保留 `sku` 与 `name`，市场模块可据此按厂商与兼容性筛选。

### 可缺省字段语义与别名隔离（v0.3.3）

`POST /sync/push` 的 `alias`、`advertisedName`、`group` 均为可选字段，JSON 类型不变，语义如下：

| 上报形式 | 服务端行为 |
| --- | --- |
| 字段缺失（absent） | 视为「未上报」，保持原值，且不参与 LWW 内容比较 |
| `"group": ""` | **显式清除分组**，`user_devices.group` 置为空串 |
| 显式非空值 | 覆盖原值 |
| `"alias": ""` / `"advertisedName": ""` | 显式清空对应字段 |

`alias` 与 `advertisedName` 相互独立：

- `alias`：用户设置的别名，只在用户编辑时上报，服务端不会被广播名覆盖；
- `advertisedName`：设备广播名（BLE Local Name），客户端扫描时可覆盖上报，不会改动 `alias`；
- `UserDevice` 同时返回两者（`advertisedName` 为 v0.3.3 新增字段）。

### 读数链路（v0.4.0）

设备测量数据通过 `readings` 表持久化，App 不再只有本地缓存。

#### 表结构

`readings`（迁移 `000006_readings`）：

| 列 | 类型 | 说明 |
| --- | --- | --- |
| `id` | `uuid` | 主键，客户端生成；服务端按它幂等 |
| `user_id` | `uuid` | 归属用户，`ON DELETE CASCADE` |
| `platform` | `text` | `android` / `ios` |
| `external_id` | `text` | 设备唯一键（Android 大写 MAC，iOS UUID） |
| `metric` | `text` | 指标名，小写 snake_case，1–32 字符 |
| `value` | `double precision` | 测量值；CHECK 拒绝 NaN / ±Inf |
| `unit` | `text` | 单位，0–16 字符，空串表示无量纲 |
| `recorded_at` | `timestamptz` | 客户端测量时间 |
| `device_type` | `text` nullable | 写入时解析到的型号，可空 |
| `created_at` | `timestamptz` | 服务端写入时间 |

索引：`readings_user_device_metric_time_idx (user_id, platform, external_id, metric, recorded_at DESC)`。
down 迁移直接删除该表，不影响 `user_devices` 与公共元数据。

#### 写入：`POST /sync/push`

请求体新增可选 `readings` 数组（≤500 条/批），元素：

```json
{"id":"<uuid>","platform":"android","externalId":"AA:BB:CC:00:00:01",
 "metric":"temperature_celsius","value":21.5,"unit":"celsius",
 "recordedAt":"2026-11-05T00:00:00Z","deviceType":"nordic_uart"}
```

响应新增 `appliedReadings`（实际写入条数）与 `skippedReadings`（按 id 跳过条数）。
未提交 `readings` 时两字段恒为 0，其余字段与旧版本完全一致。

幂等与冲突：

| 情况 | 结果 |
| --- | --- |
| `id` 不存在 | 写入，`appliedReadings++` |
| `id` 已存在且内容一致 | 不写入，`skippedReadings++`，**不产生冲突** |
| `id` 已存在但内容不同 | 不写入，`skippedReadings++`，追加冲突 `{entity:"reading", reason:"duplicate_id", reading:{...}}` |
| 同批内重复 `id` | 按上述规则逐条判定 |

读数写入后不可变（不参与字段级 LWW），只能被 90 天保留策略清理。

校验规则（HTTP 与存储层共用 `internal/validate/reading.go`）：

| 字段 | 规则 | 违反时 |
| --- | --- | --- |
| `id` | UUID | 400 |
| `platform` / `externalId` | 沿用设备唯一键规则（`android` + MAC，`ios` + UUID） | 400 / `INVALID_MAC` |
| `metric` | 小写 snake_case，`^[a-z][a-z0-9_]*$`，1–32 字符 | 400 `BAD_REQUEST` |
| `value` | 有限浮点数；NaN / ±Inf 拒绝 | 400（JSON 解析失败为 `INVALID_JSON`/`BAD_REQUEST`；数据库 CHECK 兜底） |
| `unit` | 0–16 字符（允许空串） | 400 `BAD_REQUEST` |
| `recordedAt` | RFC3339；超前服务端 > 5 分钟拒绝 | 400 `BAD_REQUEST` |
| 批量 | `readings` ≤ 500，`devices` ≤ 100，两者不能同时为空 | 400 `BAD_REQUEST` |

**隐式设备**：读数引用的设备不要求在 `user_devices` 中存在。未注册设备按「隐式设备」接受写入
（便于先采数据、后补设备信息）；但在设备注册（或被 `/sync/push` 写入）之前，
`GET .../readings`、`.../summary`、`.../series`、`.../metrics` 均按设备不存在返回 404。

#### 查询：`GET /my/devices/{platform}/{externalId}/readings`

查询参数：`metric`（可选）、`from` / `to`（RFC3339，闭区间）、`limit`（1–500，默认 100）、`cursor`。

- 按 `recordedAt` **倒序**，同一时间按 `id` 倒序稳定排序；
- `nextCursor` 为不透明游标，`hasMore=false` 时为空串；
- 越权、设备已删除、设备不存在一律 404；非法游标/时间/limit 返回 400。

#### 聚合：`GET .../readings/summary?metric=&hours=24`

- `metric` 必填（缺失 400）；`hours` 默认 24，范围 1–2160（90 天）；
- 窗口为 `[now-hours, now]`，返回 `windowStart` / `windowEnd`；
- 无数据时 `count:0`，`min`/`max`/`avg`/`last` 均为 `null`，`unit` 为空串；
- 有数据时 `unit` 取窗口内最新一条的单位，`last` 为 `recordedAt` 最新的值。

#### 分桶序列与指标清单（v0.5.0）

**分桶序列**：`GET /my/devices/{platform}/{externalId}/readings/series?metric=&from=&to=&bucket=1m|1h|1d`

| 参数 | 必填 | 规则 |
| --- | --- | --- |
| `metric` | 是 | 小写 snake_case，1–32 字符 |
| `from` / `to` | 是 | RFC3339，闭区间，要求 `to > from`，跨度 ≤ 90 天 |
| `bucket` | 是 | `1m`、`1h`、`1d` 三选一 |

返回：

```json
{"metric":"temperature_celsius","unit":"celsius","bucket":"1h",
 "from":"2026-11-04T00:00:00Z","to":"2026-11-05T00:00:00Z",
 "buckets":[{"start":"2026-11-04T01:00:00Z","end":"2026-11-04T02:00:00Z",
             "count":12,"min":20.9,"max":21.8,"avg":21.35,"last":21.8}]}
```

- 桶边界按 **UTC** 对齐：`1m` 对齐整分钟、`1h` 对齐整点、`1d` 对齐 UTC 零点（`00:00:00Z`），
  与客户端本地时区、夏令时（DST）无关；`start` 为起点（含），`end = start + bucket`（不含）；
- 只返回 `count > 0` 的桶，按 `start` 升序；范围内无数据时 `buckets: []` 且 HTTP 200；
- `last` 为桶内 `recordedAt` 最大的值（同一时刻按 `id` 倒序取首条）；
- `unit` 取时间范围内最新一条读数的单位；
- 桶数量上限 **500**：参数会产生的桶数超出时返回 400，提示改用更粗的 `bucket` 或缩短范围；
- 非法参数（缺 `metric`/`from`/`to`/`bucket`、`to <= from`、跨度 > 90 天、非法 bucket）返回 400；
  越权、设备已删除或不存在统一 404。

**指标清单**：`GET /my/devices/{platform}/{externalId}/readings/metrics`

```json
{"items":[{"metric":"relative_humidity","unit":"%","count":42,
           "firstAt":"2026-11-01T00:00:00Z","lastAt":"2026-11-04T23:59:00Z"}]}
```

- 按 `lastAt` **倒序**（同一 `lastAt` 按 `metric` 升序）；
- `count` 为该指标读数总数，`firstAt` / `lastAt` 为最早 / 最晚的 `recordedAt`；
- `unit` 为该指标**最近一条**读数的单位；同一指标存在多个单位时以此为准（历史读数可能使用旧单位）；
- 设备已注册但未上报过读数时 `items: []` 且 HTTP 200；404 规则同上。

实现：分桶序列由 Postgres `date_bin` + `GROUP BY` 的**单条 SQL** 完成（`min`/`max`/`avg`/`count`
由聚合函数计算，`last` 与单位用 `ARRAY_AGG(... ORDER BY recorded_at DESC, id DESC)[1]` 取回），
不在应用层遍历读数。过滤条件与现有索引
`readings_user_device_metric_time_idx (user_id, platform, external_id, metric, recorded_at DESC)`
的前缀完全匹配，无需新增覆盖索引；90 天窗口上限保证了单次扫描量。

#### 保留策略（90 天）

服务端只保留 90 天读数。清理命令：

```bash
DATABASE_URL='postgres://...' go run ./cmd/cleanup                 # 默认删除 90 天前的数据
DATABASE_URL='postgres://...' go run ./cmd/cleanup -retention-days 90 -dry-run
```

建议通过 cron / Kubernetes CronJob / 云定时任务每天执行一次。清理逻辑为
`DELETE FROM readings WHERE recorded_at < now - retention`，不影响设备与变更日志。

### curl 示例

```bash
BASE=http://127.0.0.1:8080/api/v1

# 1. 健康检查
curl -s $BASE/health

# 2. 匿名登录
AUTH=$(curl -s -X POST $BASE/auth/anonymous)
TOKEN=$(echo "$AUTH" | sed -E 's/.*"token":"([^"]+)".*/\1/')
echo "$AUTH"

# 3. 公共设备元数据（MAC 大小写均可）
curl -s $BASE/devices/AA:BB:CC:00:00:01

# 4. 设备类型 / 固件 / 配件（iOS 无法拿 MAC 时用 /device-types/{type}）
curl -s $BASE/device-types
curl -s $BASE/device-types/nordic_uart
curl -s $BASE/device-types/nordic_uart/firmwares
curl -s $BASE/device-types/heart_rate_monitor/accessories

# 4b. 聚合端点：一次拿到类型元数据 + 固件 + 配件（不依赖 MAC）
curl -s $BASE/device-types/environmental_sensor/catalog

# 5. 我的设备（401 示例与 200 示例）
curl -i $BASE/my/devices
curl -s $BASE/my/devices -H "Authorization: Bearer $TOKEN"

# 5b. 按设备唯一键查单台（iOS：platform=ios + CBPeripheral.identifier）
curl -s "$BASE/my/devices/ios/E621E1F8-C36C-495A-93FC-0C247A3E6E5F" \
  -H "Authorization: Bearer $TOKEN"
# Android 也可以按 (android, MAC) 查询；旧 MAC 路径仍可用
curl -s "$BASE/my/devices/android/AA:BB:CC:00:00:01" -H "Authorization: Bearer $TOKEN"
curl -s "$BASE/my/devices/AA:BB:CC:00:00:01" -H "Authorization: Bearer $TOKEN"

# 6. 推送设备（带幂等键）
# Android：platform=android，externalId 为大写 MAC，mac 可选（会自动补全）
curl -s -X POST $BASE/sync/push \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: demo-1" \
  -d '{
    "clientId": "android-demo",
    "devices": [{
      "id": "11111111-1111-4111-8111-111111111111",
      "platform": "android",
      "externalId": "AA:BB:CC:00:00:01",
      "alias": "我的开发板",
      "group": "默认",
      "rules": {"notify": true},
      "scanRecords": [{"rssi": -50}],
      "updatedAt": "2026-11-05T01:00:00Z",
      "deleted": false
    }]
  }'

# iOS：externalId 为 CBPeripheral.identifier（UUID），mac 可省略；deviceType 可选（如 environmental_sensor）
curl -s -X POST $BASE/sync/push \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "clientId": "ios-demo",
    "devices": [{
      "id": "22222222-2222-4222-8222-222222222222",
      "platform": "ios",
      "externalId": "E621E1F8-C36C-495A-93FC-0C247A3E6E5F",
      "deviceType": "environmental_sensor",
      "advertisedName": "Polar H10",
      "alias": "iOS 连接的开发板",
      "updatedAt": "2026-11-05T01:00:00Z",
      "deleted": false
    }]
  }'

# 7. 拉取增量变更
curl -s "$BASE/sync/changes?cursor=0&limit=50" -H "Authorization: Bearer $TOKEN"

# 7b. 推送读数（与设备同批或只推读数均可；按 id 幂等）
curl -s -X POST $BASE/sync/push \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: readings-1" \
  -d '{
    "clientId": "android-demo",
    "readings": [{
      "id": "44444444-4444-4444-8444-444444444444",
      "platform": "android",
      "externalId": "AA:BB:CC:00:00:01",
      "metric": "temperature_celsius",
      "value": 21.5,
      "unit": "celsius",
      "recordedAt": "2026-11-05T01:00:00Z"
    }]
  }'

# 7c. 查询读数（recordedAt 倒序，limit 1–500，默认 100）
curl -s "$BASE/my/devices/android/AA:BB:CC:00:00:01/readings?metric=temperature_celsius&limit=50" \
  -H "Authorization: Bearer $TOKEN"

# 7d. 聚合最近 24 小时（metric 必填；无数据时数值字段为 null）
curl -s "$BASE/my/devices/android/AA:BB:CC:00:00:01/readings/summary?metric=temperature_celsius&hours=24" \
  -H "Authorization: Bearer $TOKEN"

# 7e. 按 UTC 整点分桶（bucket=1m|1h|1d；跨度 ≤ 90 天、桶数 ≤ 500）
curl -s "$BASE/my/devices/android/AA:BB:CC:00:00:01/readings/series?metric=temperature_celsius&from=2026-11-04T00:00:00Z&to=2026-11-05T00:00:00Z&bucket=1h" \
  -H "Authorization: Bearer $TOKEN"

# 7f. 指标清单（按 lastAt 倒序；unit 取最近一条读数的单位）
curl -s "$BASE/my/devices/android/AA:BB:CC:00:00:01/readings/metrics" \
  -H "Authorization: Bearer $TOKEN"

# 7g. 清理 90 天前读数（也可用 -dry-run 先看时间边界）
# DATABASE_URL='postgres://...' go run ./cmd/cleanup -retention-days 90

# 8. 删除设备（只写 tombstone，不物理删除）
curl -s -X POST $BASE/sync/push \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"clientId":"android-demo","devices":[{"id":"11111111-1111-4111-8111-111111111111","platform":"android","externalId":"AA:BB:CC:00:00:01","updatedAt":"2026-11-05T02:00:00Z","deleted":true}]}'

# 9. 注销
curl -i -X POST $BASE/auth/logout -H "Authorization: Bearer $TOKEN"
```

错误码：`BAD_REQUEST`、`INVALID_MAC`、`INVALID_JSON`、`UNAUTHORIZED`、`NOT_FOUND`、`METHOD_NOT_ALLOWED`、`IDEMPOTENCY_CONFLICT`、`PAYLOAD_TOO_LARGE`、`RATE_LIMITED`、`NOT_IMPLEMENTED`、`UNAVAILABLE`、`INTERNAL`。客户端应基于 `code` 分支，`message` 仅用于展示。

## 架构

```
Android / iOS App
   │  HTTP /api/v1
   ▼
Gin Engine（internal/httpapi）
   ├─ 全局中间件：request id → recover → slog 访问日志 → CORS → 每 IP 限流 → 统一错误出口
   ├─ 路由分组：/auth  /devices  /device-types  /my  /sync
   └─ handler：ShouldBindUri / ShouldBindJSON / ShouldBindQuery + validator
        │
        ▼
store.Store 接口（internal/store）
   ├─ fixture：fixtures JSON + 内存用户/设备/变更（无数据库本地开发）
   └─ postgres：database/sql + pgx + sqlc 生成查询，Push 单事务
        │
        ▼
PostgreSQL
```

数据分两层：

- **公共元数据**（全局共享，按 MAC 或 type 查询）：`device_types`、`gatt_services`、`public_devices`、`firmwares`、`accessories`。
- **用户私有数据**（按 `user_id` 隔离）：`user_devices`、`readings`、`change_log`、`idempotency_keys`、`tokens`。

设备唯一键：

- `user_devices` 的唯一约束是 `(user_id, platform, external_id)`，主键仍是 `(user_id, id)`。
- Android：`platform=android`，`external_id` 为规范化大写 MAC；`mac` 元数据自动填充为小写。
- iOS：`platform=ios`，`external_id` 为 `CBPeripheral.identifier`（UUID，服务端统一大写）；`mac` 可为空。
- 因此同一台设备在 Android 与 iOS 上不会被错误合并，也不会互相覆盖；不同平台的同型号设备是两条独立记录。

所有私有查询都带 `user_id` 条件；访问他人资源统一返回 404，不泄露资源是否存在。

### 与 iOS 端一致性（docs/consistency.md）

本后端与 iOS 端仓库中的 `docs/consistency.md` 保持同一套约定，联调时以以下规则为准：

- 设备唯一键：`(platform, externalId)`；`platform` 只能是 `android` / `ios`。
- Android `externalId` = 大写 MAC（冒号分隔）；公共设备接口 `/devices/{mac}` 仍按小写 MAC 查询。
- iOS `externalId` = `CBPeripheral.identifier`（UUID，大写），不依赖 MAC。
- `mac` 字段只作为可选元数据，禁止任何一端把 `mac` 当作唯一键。
- `entityId` = `<userId>:<platform>:<externalId>`，由服务端派生；客户端不要自行拼接或解析。
- 契约只增不删：新增了 `/device-types/{type}/catalog` 聚合端点与 `/my/devices/{platform}/{externalId}` 单台查询，历史端点保持不变。
- 设备详情不再必须依赖 MAC：公共元数据用 `/device-types/{type}`，用户私有数据用 `/my/devices/{platform}/{externalId}`。
- v0.3.2 起 `UserDevice` 直接返回 `deviceType` / `vendor` / `firmwareVersion` / `compatibility`，iOS/Android 详情页无需自行拼装公共元数据；缺失时为 `null`。
- v0.3.3 起 `UserDevice` 增加 `advertisedName`；分组删除请上报 `"group": ""`，不要在本地省略该字段。
- v0.4.0 起读数走 `POST /sync/push` 的 `readings`（按 `id` 幂等）与 `/my/devices/{platform}/{externalId}/readings[/summary]`；
  读数不要求在 `user_devices` 中先注册，但查询以设备存在为前提。
- `401` 语义见下节；iOS 端已确认不在 401 后自动匿名重登。
- 字段命名以 `api/openapi.yaml` 为准；若与 iOS 端 `docs/consistency.md` 有出入，需同步修改两份文档并在此处记录。

### 账号与鉴权

- 不使用全局 API Key，也不使用 JWT。
- token 为不透明随机串：`pjiot_` + 32 字节随机数的 base64url 编码。
- 数据库只存 SHA-256 哈希；`tokens.revoked_at` 支持吊销（`/auth/logout`）。
- 私有接口用 `Authorization: Bearer <token>`，服务端按 token 哈希查出 `userId` 后再执行查询。
- 服务端不会自动签发、轮换或续期 token；只有显式调用 `/auth/anonymous` 才会创建新账号与新 token。

### 401 语义（客户端必须遵守）

`401` 是明确的「token 失效」信号：token 缺失、无效或已被吊销。契约如下：

1. 客户端应提示用户重新登录，并保留本地待同步数据；不得静默丢弃或覆盖。
2. **不得**在收到 401 后自动调用 `POST /auth/anonymous`。自动重登会创建新的 `userId`，导致
   原账号的设备列表与 `sync` 游标失联，出现「数据消失 / 重复同步 / 跨账号串数据」。
3. 401 响应只包含 `{"error":{"code":"UNAUTHORIZED","message":"..."}}`，不会下发新 token，也不返回 `userId`。
4. 重新登录是用户显式操作；登录成功后客户端应重新拉取 `/my/devices` 与从 `cursor=0` 或本地保存的游标继续同步。
5. 匿名账号一旦丢失 token 且无其他登录方式，旧数据不可恢复；这是匿名模式的固有限制，客户端应在 UI 明确提示。

### 同步模型

- `user_devices` 唯一约束 `(user_id, platform, external_id)`，主键 `(user_id, id)`，`id` 为客户端生成的 UUID。
- `platform` 取值 `android` / `ios`；Android 的 `external_id` 为大写 MAC，iOS 为 `CBPeripheral.identifier`；`mac` 仅作为可选元数据。
- `change_log(user_id, seq BIGSERIAL, entity, entity_id, op, payload, server_ts)` 是增量来源，`seq` 单调递增。
- `entity_id` 由复合键派生：`<userId>:<platform>:<externalId>`；旧变更日志在 000003 迁移中回填为同一形式。
- 字段级 LWW：
  - 客户端 `updatedAt` 严格更新 → 接受；
  - 客户端时间超前服务端 2 分钟以上 → 以服务端时间兜底（`clock_clamped`）；
  - 时间相同且内容一致 → `unchanged`，不写库；
  - 否则服务端获胜，落败数据放入 `conflicts`（原因 `server_newer` / `clock_clamped` / `key_taken`），不静默覆盖。
- 删除只写 tombstone：`deleted=true` 时写 `deleted_at` 与 `op=delete`，不物理删除；`/my/devices` 列表与单查默认过滤已删除记录，`/sync/changes` 仍会返回删除事件。
- `POST /sync/push` 支持 `Idempotency-Key`（同用户下唯一，≤128 字符）：
  - 相同 key + 相同请求体 → 返回历史响应，并带 `Idempotent-Replayed: true`；
  - 相同 key + 不同请求体 → 409 `IDEMPOTENCY_CONFLICT`。
- `GET /sync/changes` 游标为上次返回的 `nextCursor`，`limit` 默认 100、最大 500，`hasMore` 表示是否还有下一页。

### 为什么用 Gin

路由层在项目中途从标准库 `net/http` 迁移到 Gin，收益是具体的：

1. **参数绑定与校验**：`ShouldBindUri` / `ShouldBindJSON` / `ShouldBindQuery` 把路径、请求体、查询参数统一解析，配合 `binding:"required,min=1,max=100,dive"`、`uuid` 等 validator 标签，把 MAC 格式、UUID、长度、数组上限等校验声明在 DTO 上，避免每个 handler 重复手写解析与判断。校验失败统一映射 400，错误出口单一。
2. **中间件复用**：request id、panic 恢复、slog 访问日志、CORS、每 IP 限流、鉴权、统一错误处理都以 `gin.HandlerFunc` 组合，顺序显式；标准库需要在每个 handler 外包一层 `http.Handler` 或自己维护 context 传递。
3. **分组路由**：`/api/v1/auth`、`/api/v1/sync` 等分组直接映射资源层级，`requireAuth()` 可以只挂在 `/my`、`/sync` 组上，减少逐个接口挂鉴权的遗漏风险。
4. **团队与岗位匹配度**：Gin 是国内 Go 后端最常用的 Web 框架之一，新成员上手和交接成本低，招聘时也更常见；相比自研路由约定，代码风格更统一。
5. **边界可控**：框架依赖只存在于 `internal/httpapi`，领域模型、LWW 判定、存储、sqlc 查询均不依赖 Gin；未来若要替换 Web 框架，业务层不受影响。

选择 Gin 不是因为“流行”，而是上述绑定校验、中间件与分组能力能直接减少重复代码和鉴权遗漏；同时限制其使用范围，避免框架侵入业务层。

## 目录结构

```
PJGoIoT/
├── api/openapi.yaml              # 对外契约
├── cmd/
│   ├── api/main.go               # API 启动、优雅关闭、-healthcheck
│   ├── cleanup/main.go           # 90 天读数清理（cron/CronJob 调用）
│   └── migrate/main.go           # 内嵌迁移执行器
├── db/
│   ├── migrations/               # 000001 建表、000002 seed（go:embed）
│   └── queries/                  # sqlc 查询：auth / metadata / sync
├── fixtures/                     # 无数据库模式的公共元数据
├── internal/
│   ├── auth/token.go             # 不透明 token 生成与哈希
│   ├── config/config.go          # 环境变量配置
│   ├── httpapi/                  # Gin 路由、中间件、handler、校验、错误出口
│   ├── lww/lww.go                # LWW 判定
│   ├── model/model.go            # 对外 JSON 领域模型
│   ├── store/                    # Store 接口 + fixture/postgres 实现
│   └── validate/                 # MAC / platform / externalId 规范化
├── Dockerfile                    # 多阶段 + distroless
├── docker-compose.yml            # Postgres + migrate + api
├── Makefile
├── sqlc.yaml
└── .github/workflows/ci.yml
```

## 环境变量

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `ADDR` | `:8080` | 监听地址 |
| `STORE` | 自动 | `fixture` 或 `postgres`；有 `DATABASE_URL` 时默认 postgres |
| `DATABASE_URL` | 空 | PostgreSQL 连接串，postgres 模式必填 |
| `FIXTURES_DIR` | `./fixtures` | fixture 模式数据目录 |
| `RATE_LIMIT_PER_MINUTE` | `120` | 每 IP 每分钟令牌数；`<=0` 关闭限流 |
| `RATE_LIMIT_BURST` | `40` | 每 IP 令牌桶容量 |
| `CORS_ALLOWED_ORIGINS` | `*` | 逗号分隔的允许来源，`*` 表示全部 |
| `LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error` |
| `APP_VERSION` | `0.5.1` | 健康接口与日志中展示的版本 |
| `HEALTHCHECK_URL` | 自动推导 | `-healthcheck` 模式请求的完整 URL |
| `TEST_DATABASE_URL` | 空 | 设置后运行 Postgres 集成测试 |

## 迁移与 seed

迁移通过 `cmd/migrate` 执行，SQL 内嵌进二进制，镜像中无需额外文件：

```bash
make migrate                                  # up 到最新
make migrate-version                          # 查看版本
make migrate-down                             # 回滚全部（会删表）
go run ./cmd/migrate -direction up -steps 1   # 只前进一步
```

- `000001_init`：建表（`users`、`tokens`、`device_types`、`gatt_services`、`public_devices`、`firmwares`、`accessories`、`user_devices`、`change_log`、`idempotency_keys`）。
- `000002_seed`：公共元数据 seed。
- `000003_platform_external_id`：设备唯一键从 `(user_id, mac)` 迁移为 `(user_id, platform, external_id)`：
  - 旧记录回填 `platform=android`、`external_id=upper(mac)`；
  - `mac` 改为可空，仅作为元数据；
  - 旧的 `change_log` 记录回填 `payload.platform` / `payload.externalId` 与新的 `entity_id`；
  - 迁移不破坏已有数据，旧记录迁移后仍可按 MAC 读取；
  - down 迁移有损（iOS 的 external_id 会被回填到 mac），仅用于本地回滚。
- `000004_metadata_enrichment`：字段扩展，只加列、带默认值，不修改既有列语义：
  - `user_devices` 新增可空 `device_type`，并按 `mac` 关联 `public_devices` 回填旧记录（未命中保持 `NULL`）；
  - `device_types` 新增 `compatibility`，仅回填 `nordic_uart` 与 `environmental_sensor` 两个已核实型号；
  - `firmwares` 新增 `vendor` / `compatibility`，回填 `Nordic Semiconductor`、`Espressif Systems`，兼容性沿用同型号已核实的公共设备声明；
  - `accessories` 新增 `vendor` / `compatibility`，只回填 Polar Electro / Espressif Systems 与适配型号，通用电池厂商留空；
  - 新增索引 `user_devices_user_device_type_idx (user_id, device_type)`；
  - down 迁移删除新增列与索引，不影响其余数据。
- `000005_alias_advertised_name`：区分用户别名与设备广播名：
  - `user_devices` 新增 `advertised_name text NOT NULL DEFAULT ''`，旧记录默认空串，`alias` 原值保留；
  - 分组清除不需要改表：既有 `group` 列已为 `NOT NULL DEFAULT ''`，空串即表示未分组；
  - 通过 `COMMENT ON COLUMN` 固化三列语义；down 迁移只删除 `advertised_name`。
- `000006_readings`：新增设备读数表：
  - 主键 `id uuid`（客户端生成，服务端按 id 幂等）；列 `user_id, platform, external_id, metric, value, unit, recorded_at, device_type, created_at`；
  - 索引 `readings_user_device_metric_time_idx (user_id, platform, external_id, metric, recorded_at DESC)`；
  - `value` 有 CHECK 约束拒绝 NaN / ±Inf；`device_type` 可空；
  - down 迁移 `DROP TABLE readings`，不触碰其他表（已在迁移测试中验证 up → down → up）。

seed 数据来源与边界：

- GATT 服务 UUID 取自 Bluetooth SIG Assigned Numbers 的标准服务（Cycling Speed and Cadence `0x1816`、Environmental Sensing `0x181A`、Heart Rate `0x180D`、Battery `0x180F`、Device Information `0x180A`、Body Composition `0x181B`、Weight Scale `0x181D`、Generic Access `0x1800`），以及 Nordic Semiconductor 官方文档中的 Nordic UART Service（`6e400001-b5a3-f393-e0a9-e50e24dcca9e`）。
- 固件版本只收录官方公开的参考版本：`nRF Connect SDK v2.9.0`、`ESP-IDF v5.2.2`；`released_at` 留空，未编造发布日期。
- 公共设备为厂商公开的开发板型号：`nRF52840 DK`、`nRF5340 DK`、`ESP32-C3-DevKitM-1`。
- 配件只放能核实名称的通用/官方在售项；具体 SKU 与量产设备固件号未编造，留空或以厂商渠道为准。
- 厂商信息只使用官方公开主体：固件 `Nordic Semiconductor` / `Espressif Systems`；配件 `Polar Electro` / `Espressif Systems`；通用 AA 电池无厂商，保持空字符串。
- 兼容性声明只来自可核实的 GATT 能力：`nordic_uart`（UART 透传）、`environmental_sensor`（温度/湿度/气压）；配件兼容性按适配型号标注（`deviceTypes`）。

## 测试与验证

测试覆盖：

- `internal/httpapi`：httptest + Gin 表驱动；健康、公共设备、设备类型/详情/固件/配件/聚合、401/404、非法 MAC、CORS 预检、注销、push 参数校验（platform / externalId / 平台规则 / deviceType / alias / advertisedName / group）、幂等重放、幂等键冲突、LWW 冲突、tombstone、changes 分页、跨用户越权 404、同用户 Android 与 iOS 双记录互不覆盖、按复合键单查（200/400/404、大小写规范化、删除后 404、跨用户 404、旧 MAC 路径兼容）、401 响应不下发新 token、限流；v0.3.2 新增 `TestDeviceMetadataEnrichment`：`UserDevice` 的 `deviceType`/`vendor`/`firmwareVersion`/`compatibility` 200 用例、catalog 固件与配件字段完整性、未注册型号与无 MAC 时降级为 `null` 且不返回 500；v0.3.3 新增 `TestPresenceSemantics`：`"group": ""` 显式清除、字段缺失保持原值、清空后再次同步不会让分组复活、上报 `advertisedName` 不改变 `alias` 且反之亦然；v0.4.0 新增 `TestReadingsPushIdempotency`（重复 id 计 skipped、同 id 不同内容计 `duplicate_id` 冲突且原值不变）、`TestReadingsPushOptional`（无 readings 时计数为 0、隐式设备写入与注册后可查）、`TestReadingsValidation`（metric/unit/value/recordedAt/批量上限/非法 externalId）、`TestReadingsListPagination`（倒序、游标翻页、metric 与时间过滤、非法参数、跨用户 404）、`TestReadingsSummary`（聚合值、无数据 null、hours 校验、跨用户 404）；v0.5.0 新增 `TestReadingsSeries`（1h/1d 的 UTC 对齐与跨整点/跨日边界、桶内 min/max/avg/last、空区间 `buckets: []`、8 类非法参数含「桶数超 500 提示更粗 bucket」、跨用户与不存在设备 404、unit 取窗口最新）、`TestReadingsMetrics`（排序与计数、多单位取最新、空数组、跨用户 404）。
- `internal/lww`：时钟兜底、服务端获胜、内容相同 noop 等判定；`deviceType`、`alias`、`advertisedName`、`group` 仅在客户端上报时参与内容比较（缺失保持原值不产生冲突）。
- `internal/store/postgres`：`TEST_DATABASE_URL` 存在时运行真实数据库集成测试，覆盖 token 创建/校验/吊销、幂等重放与冲突、LWW、tombstone、分页、跨用户隔离、Android/iOS 复合键隔离、`GetUserDeviceByKey` 跨用户 404，公共元数据补全与缺失降级（`deviceType` 由 MAC 推导、按型号回退厂商/兼容性、未注册型号返回 null），以及可缺省字段语义（别名与广播名隔离、`group: ""` 清除后缺省同步不复活）；v0.4.0 新增集成子测试「读数链路」：批量写入与计数、重复 id 幂等、同 id 不同内容冲突、NaN/Inf/未来时间/非法 metric 拒绝、数据库 CHECK 拒绝 NaN、倒序游标分页、非法游标、metric 过滤、跨用户与不存在设备 404、summary 聚合与无数据、90 天清理（历史数据被删且近期数据保留）；v0.5.0 新增集成子测试「读数分桶序列与指标清单」：1h 跨整点与 1d UTC 零点对齐、桶内 min/max/avg/last、分桶计数合计与原始条数一致、空区间 `[]`、非法 bucket / 超 90 天 / 超 500 桶、跨用户与不存在设备 404、指标清单排序与计数、空设备空数组。
- `internal/store/postgres/migration_test.go`：临时数据库上先迁移到 version=2，写入旧 `(user_id, mac)` 记录与旧 change_log，再迁移到最新 version=6，验证 000003 的键回填、000004 的 `device_type`/厂商回填、000005 的 `advertised_name` 默认值、000006 的 `readings` 表与索引；随后回滚到 version=5 验证 `readings` 表被删除且设备数据完好，再 up 回到 version=6。

本机实际执行结果（2026-11-05）：

| 命令 | 结果 |
| --- | --- |
| `go build ./...` | 通过 |
| `go vet ./...` | 通过 |
| `go test ./...` | 通过（`internal/httpapi`、`internal/lww`、`internal/store/postgres` 均 ok；`-v` 统计 25 个顶层测试、132 个含子测试用例，0 失败） |
| `TEST_DATABASE_URL=... go test ./internal/store/postgres -run 'TestPostgresStoreIntegration|TestMigrationBackfillsLegacyDevices' -v` | 通过（本机真实 PostgreSQL，迁移到 version=6；含旧数据、字段回填、读数链路、分桶序列与指标清单、down 回滚） |
| v0.3.2 字段测试 | 通过：`TestDeviceMetadataEnrichment`（列表/单查 200 字段、catalog 固件配件完整性、未注册型号与无 MAC 降级 null）、`TestDeviceTypeComparison`、集成测试「公共元数据补全与缺失降级」 |
| v0.3.3 语义测试 | 通过：`TestPresenceSemantics`（`"group": ""` 清除、缺失保持、清空后拉回为空、广播名与别名互不覆盖）、`TestPresenceOnlyComparison`、集成测试「可缺省字段语义与广播名隔离」、迁移测试 `advertised_name` 默认空串 |
| v0.4.0 读数测试 | 通过：`TestReadingsPushIdempotency`、`TestReadingsPushOptional`、`TestReadingsValidation`、`TestReadingsListPagination`、`TestReadingsSummary`、集成子测试「读数链路」（含数据库 CHECK 拒绝 NaN、90 天清理）、迁移测试 up→down→up |
| v0.5.0 分桶与指标清单测试 | 通过：`TestReadingsSeries`（UTC 对齐、跨整点/跨日、桶内聚合、空区间、非法参数、404）、`TestReadingsMetrics`（排序/计数/单位取最新/空数组/404）、集成子测试「读数分桶序列与指标清单」（真实 Postgres） |
| 真实库迁移 | 通过：`cmd/migrate up` → version=6 dirty=false；`readings` 表、索引与 CHECK 约束按预期创建；历史 000003~000005 回填保持正确（`advertised_name` 默认空串、AA 电池厂商为空） |
| Postgres 模式 HTTP 验证 | 通过：health、匿名登录、公共设备、类型/详情/固件/配件、my/devices、push、幂等重放、409、LWW 冲突、key_taken、tombstone 404、跨用户 404、注销后 401、Android/iOS 双记录、旧客户端 mac-only 兼容、同复合键换 id 不重复 |
| v0.4.0 读数 HTTP 验证 | 通过（真实 PostgreSQL + `cmd/api`）：push `appliedReadings=2`；重复 id 不同值返回 `skippedReadings=1` 与 `entity=reading/reason=duplicate_id` 冲突且原值不变；列表倒序；summary `count/min/max/avg/last` 正确；无数据时数值为 `null`；未来时间 400 `BAD_REQUEST`；NaN 400；未知设备与跨用户 404 |
| fixture 模式新增接口验证 | 通过：`/device-types/{type}/catalog` 200 且含固件/配件、未知类型 404；`/my/devices/android/{mac}` 与 `/my/devices/ios/{uuid}` 200；非法 platform 400；旧 `/my/devices/{mac}` 兼容 |
| fixture 字段补全验证 | 通过：`/my/devices` 中 Android 设备返回 `nordic_uart / Nordic Semiconductor / nRF Connect SDK v2.9.0` 与 UART 兼容性；iOS 上报 `environmental_sensor` 返回厂商与温度/湿度/气压兼容性、`firmwareVersion=null`；未注册型号仅回显 `deviceType`，其余为 `null`；catalog 固件/配件均带 `vendor` 与 `compatibility` |
| fixture 语义验证 | 通过：上报 `advertisedName` 后 `alias` 与 `group` 不变；`"group": ""` 后分组为空；随后仅上报广播名（缺省 group），分组仍为空，未复活 |
| 401 契约验证 | 通过：无效 token 返回 401 `UNAUTHORIZED`，响应体仅有 `error`，不含 `token` / `userId` |
| 复合键数据库验证 | 通过：真实 PostgreSQL 中按 `(platform, externalId)` 查询 Android / iOS 成功，跨用户查询返回 `ErrNotFound` |
| `docker build` | 通过（GitHub Actions `构建镜像` job，v0.5.1 提交 ca0d5fd，run 41947830444） |
| GitHub Actions | 通过（v0.5.1：gofmt、vet、build、单元测试、迁移到 version=6、Postgres 集成测试、distroless 镜像构建；run 41947830444） |
| OpenAPI 静态校验 | 通过：`npx @redocly/cli lint api/openapi.yaml` → valid，0 errors（10 条非阻断 warning）；四个读数路径、push 的 `readings` 与 `appliedReadings`/`skippedReadings`、所有 `$ref` 均可解析 |

集成测试运行方式：

```bash
TEST_DATABASE_URL='postgres://platojobs@127.0.0.1:5432/pjgoiot_test?sslmode=disable' \
  go test ./internal/store/postgres -run 'TestPostgresStoreIntegration|TestMigrationBackfillsLegacyDevices' -v
```

## 已知限制与下一步

- `/auth/login` 仍为 501，只有匿名账号；后续可加账号密码 / OAuth 与匿名账号绑定。
- token 无刷新与过期时间，仅支持吊销；如需长效会话可增加滚动续期，但必须保持「401 不自动重登」的契约。
- `/my/devices/{mac}` 仅适合 Android；iOS 使用 `/my/devices/{platform}/{externalId}` 单台查询（已提供），或使用 `/my/devices` 列表。
- 读数保留 90 天由 `cmd/cleanup` 负责，服务内不做后台定时清理；生产环境需自行配置 cron / CronJob。
- 读数查询要求设备已存在于 `user_devices`；隐式设备写入的读数在注册前不可查。
- 读数支持单指标聚合（`min/max/avg/last`）、UTC 对齐的 `1m/1h/1d` 分桶序列，以及设备维度指标清单；跨指标合并统计按需再扩展。
- 限流为单进程内存令牌桶，多实例部署时各实例独立计数；需要全局限流时再引入共享存储。
- fixture 模式的用户与设备数据在重启后丢失，仅用于本地联调与测试。
- 公共元数据列表未分页；当前数据量小，后续数据增长后再加分页。
- 删除时 payload 采用本次 push 提交的字段；客户端删除前应提交完整业务字段，或仅依赖 `deletedAt`。
- seed 只有开发板参考数据，尚未接入真实量产设备型号、固件发布列表与官方配件 SKU；需要厂商/产品提供可核实来源后再补。
- 尚未做 Android 端到端联调、压测与可观测性接入（指标/Tracing）。
- 云部署（服务器、域名、HTTPS、托管数据库、密钥管理）需用户决定，仓库不包含也不擅自开通。

## 开发命令

```bash
make help          # 查看命令
make dev           # fixture 模式启动
make test          # go test ./...
make lint          # gofmt + go vet
make build         # go build ./...
make migrate       # 迁移到最新
make sqlc          # 重新生成 sqlc 代码（需安装 sqlc）
make docker-build  # 构建 distroless 镜像
make docker-up     # compose 启动
```
