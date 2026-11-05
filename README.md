# PJGoIoT 后端

「蓝牙设备数据面板」Android / iOS App 的后端服务：多用户账号、BLE 设备公共元数据、用户私有设备与增量同步。

- 语言 / 运行时：Go 1.22+
- HTTP 框架：Gin v1.10（仅 `internal/httpapi` 层依赖）
- 数据库：PostgreSQL 16（迁移 golang-migrate，查询 sqlc，不使用 ORM）
- 日志：标准库 `log/slog` 结构化日志
- 契约：[`api/openapi.yaml`](api/openapi.yaml)，契约版本 **0.7.0**（v0.8.0 新增 `expiresAt` 字段与 `410 RESYNC_REQUIRED` 语义）；服务版本 **v0.11.0**（双实例 + Caddy 负载均衡、优雅下线与滚动发布、可选 Postgres 共享限流）
  - 说明：`APP_VERSION` / README 的 **服务版本**与 OpenAPI 的 **契约版本**是两套号。v0.7.0/v0.7.1 只增加运维形态（`/healthz`、编排、备份脚本），不改业务契约，契约保持 0.6.0；v0.8.0 只新增响应字段（`expiresAt`）与新增错误码（`RESYNC_REQUIRED`、`TIMEOUT`），按“只加不删”原则升到 0.7.0；v0.9.0 不改业务契约（`/metrics` 新增 `route` 标签属运维观测面），契约保持 **0.7.0**；v0.10.0 只在 push 描述中补充事务/400/503 语义与 503 响应示例，不新增字段；v0.11.0 只增加部署形态与运维响应头（`X-Instance`）与可选共享限流，不改业务契约，契约保持 **0.7.0**。`/healthz` 见下文「部署编排」。

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

### 方式二：Docker Compose（Caddy + API + Postgres，生产形态）

```bash
cp .env.example .env    # 至少改 POSTGRES_PASSWORD 与 DOMAIN
docker compose up -d --build
```

- **Caddy**：`https://localhost`（本地自签，`curl -k`）；80 自动跳转 443；`deploy/Caddyfile`。
- **API**：不发布到宿主机，只能经 Caddy 访问；`/metrics` 被 Caddy 显式 404，避免公网暴露。
- **Postgres**：仅绑定 `127.0.0.1:55432`，持久卷 `pgdata`；API 等其 healthy 后启动并自动迁移。
- 运维细节（备份/恢复/清理/告警/回滚）见 [`docs/runbook.md`](docs/runbook.md)。

端口与安全默认值：

| 项 | 默认 | 说明 |
| --- | --- | --- |
| `DOMAIN` | `localhost` | 生产改成真实域名，并把 Caddyfile 的 `tls internal` 换成 ACME |
| API 宿主端口 | 不映射 | 外部只能走 Caddy（TLS + 安全头 + 限流） |
| Postgres 宿主端口 | `127.0.0.1:55432` | 不暴露公网 |
| `METRICS_ENABLED` | `true` | 仅容器内网可达，Caddy 拒绝 `/metrics` |
| `RUN_MIGRATIONS_ON_BOOT` | `true` | 启动即迁移；失败进程退出，不进入半可用 |

### 方式三：本地 Postgres

```bash
createdb pjgoiot
export DATABASE_URL='postgres://platojobs@127.0.0.1:5432/pjgoiot?sslmode=disable'
make migrate        # 建表 + seed
make dev-postgres   # Postgres 模式启动 API
```

不设置 `STORE` 时，只要 `DATABASE_URL` 非空就自动使用 Postgres；否则使用 fixture。

## 接口清单

所有接口前缀 `/api/v1`（`/metrics` 例外，位于根路径）。成功响应为裸 JSON，失败响应固定为：

```json
{"error":{"code":"NOT_FOUND","message":"资源不存在或无权访问"}}
```

| 方法 | 路径 | 鉴权 | 成功状态 | 说明 |
| --- | --- | --- | --- | --- |
| GET | `/health` | 否 | 200 | readiness：含存储 Ping，失败 503 |
| GET | `/healthz`（根路径） | 否 | 200 | liveness：只证明进程存活，不访问数据库 |
| POST | `/auth/anonymous` | 否 | 201 | 创建匿名用户，返回 `userId` 与不透明 `token` |
| POST | `/auth/login` | 否 | 501 | 预留账号密码登录，当前明确未实现 |
| POST | `/auth/logout` | Bearer | 204 | 吊销当前 token |
| POST | `/auth/pairing-code` | Bearer | 201 | 生成一次性配对码，`{code,expiresAt}`；10 分钟有效，新码使旧码失效 |
| POST | `/auth/pairing-code/claim` | 否 | 200 | 用配对码换取**同一 userId** 的新 token，`{userId,token}`；按 IP 独立限流 |
| GET | `/me/export` | Bearer | 200 | 导出账号全部数据，读数升序、游标分页，附件下载 |
| DELETE | `/me` | Bearer | 204 | 硬删除账号及设备/读数/变更日志/token/配对码；重复调用 401 |
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
| GET | `/metrics`（根路径） | 否 | 200 | Prometheus 文本指标；不受限流，`METRICS_ENABLED=false` 时 404 |

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

### 跨设备接管与合规（v0.6.0）

#### 一次性配对码

`pairing_codes`（迁移 `000007_pairing_codes`）：

| 列 | 类型 | 说明 |
| --- | --- | --- |
| `code_hash` | `text` PK | 配对码 SHA-256 十六进制哈希；**不存明文** |
| `user_id` | `uuid` nullable FK | 码所属用户，`ON DELETE CASCADE`；`NULL` 表示「错误尝试计数行」 |
| `attempts` | `smallint` | 失败次数，达到 5 次即删除该行（作废） |
| `expires_at` | `timestamptz` | 过期时间（签发后 10 分钟） |
| `created_at` | `timestamptz` | 签发时间 |

索引：`pairing_codes_user_id_idx (user_id)`、`pairing_codes_expires_at_idx (expires_at)`。
码表在 API 启动时与 `cmd/cleanup` 中按 `expires_at` 清理；down 迁移直接删表。

**生成**：`POST /auth/pairing-code`（Bearer）→ `201 {"code":"K7M2QX9P","expiresAt":"..."}`

- 8 位大写字母数字，字符集 `ABCDEFGHJKLMNPQRSTUVWXYZ23456789`（去掉易混的 `0/O/1/I`）；
- TTL 10 分钟；同一用户再次生成时，事务内先删除其全部旧码，**新码使旧码立即失效**；
- 码只在响应出现一次；日志不打印完整码。

**领取**：`POST /auth/pairing-code/claim`（无需鉴权）请求 `{"code":"K7M2QX9P"}` →

```json
{"userId":"4f0c2b1e-8a55-4c3f-9c1f-2b7a6d0e9a11","token":"pjiot_..."}
```

- 为该账号签发**新 token**（同一 `userId`，设备与读数天然共用）；**原 token 保留有效**，不吊销；
- 一次性：领取成功后删除码行，重放返回 404；
- 失败语义：不存在、已过期、已用过、格式错误统一 `404 NOT_FOUND`（不区分原因，避免枚举），
  响应只含错误体，不含账号任何信息；
- 防暴力：失败按提交的码哈希落库计数，同一码累计 5 次即作废；claim 单独按 IP 限流
  （默认 20 次/分钟、突发 10，`PAIRING_CLAIM_RATE_PER_MINUTE` / `PAIRING_CLAIM_BURST` 可调），超限 429；
- 码哈希比较使用 `crypto/subtle.ConstantTimeCompare`，避免时序侧信道。

#### 数据导出：`GET /me/export`

响应结构：

```json
{"user":{"id":"...","kind":"anonymous","createdAt":"..."},
 "devices":[...全部设备，含 tombstone...],
 "readings":[{"id":"...","platform":"android","externalId":"...","metric":"...",
              "value":21.5,"unit":"celsius","recordedAt":"...","deviceType":"..."}],
 "exportedAt":"...","nextCursor":"...","hasMore":false}
```

- `readings` 按 `recordedAt` **升序**（同一时刻按 `id` 升序）；`devices` 按 `updatedAt` 倒序；
- `?limit=` 默认 5000、最大 5000；`?cursor=` 传上一页 `nextCursor`，`hasMore=false` 时 `nextCursor` 为空串；
  单账号超过 5 万条时客户端按页拉取，服务端不做一次性全量拼装；
- 响应头 `Content-Disposition: attachment; filename="pjkotlinapp-export-<userId>.json"`；
- 非法 `limit` / `cursor` 返回 400；未认证 401。

#### 注销：`DELETE /me`

- 硬删除 `users` 行，外键 `ON DELETE CASCADE` 级联清理 `tokens`、`user_devices`、`readings`、
  `change_log`、`idempotency_keys`、`pairing_codes`；成功 `204`；
- 删除后该账号**所有 token 立即 401**；重复调用同样返回 **401**（幂等语义，不是 404）；
- 不可恢复，客户端必须二次确认。

#### 可观测性

**`X-Request-Id`**：请求带该头则原样透传，未带则生成 UUID v4；始终写入响应头。
结构化访问日志每行包含 `request_id`、`method`、`path`、`status`、`duration_ms`、`ip`，认证后附 `user_id`；
**不打印请求体与 token**（配对码也不打印）。

**`GET /metrics`**（根路径，无需鉴权，不受 API 限流影响；`METRICS_ENABLED=false` 时不注册该路由）：

| 指标 | 类型 | 标签 | 说明 |
| --- | --- | --- | --- |
| `pjgoiot_http_requests_total` | counter | `method`,`status` | 请求总数 |
| `pjgoiot_http_request_duration_seconds_bucket/_sum/_count` | histogram | `method`,`le` | 延迟直方图（秒，桶 5ms–10s + Inf） |
| `pjgoiot_db_errors_total` | counter | — | 存储层错误计数 |
| `pjgoiot_users_approx` | gauge | — | 用户近似行数 |
| `pjgoiot_devices_approx` | gauge | — | 设备近似行数 |
| `pjgoiot_readings_approx` | gauge | — | 读数近似行数 |

近似行数读取 `pg_class.reltuples`，不做 `COUNT` 大表；不暴露可定位到个人的精确业务数据。
部署时建议只在监听内网或由反向代理限制访问的实例上暴露 `/metrics`。

### 部署编排与运维（v0.7.1）

#### 三个服务

| 服务 | 作用 | 关键设置 |
| --- | --- | --- |
| `caddy` | HTTPS 终止、反向代理、80→443 跳转 | 仅暴露 80/443；拒绝 `/metrics`；安全响应头；上游健康探针 `/healthz` |
| `api` | 业务服务 | distroless `nonroot`、`read_only`、不映射宿主端口、`depends_on postgres: service_healthy`、启动自动迁移 |
| `postgres` | 数据库 | 持久卷 `pgdata`、仅 `127.0.0.1:55432`、独立 `backend` 内部网络 |
| `cleanup` | 90 天读数 + 过期配对码清理 | `profiles: [maintenance]`，默认不随 `up` 启动 |

网络：`backend`（`internal: true`，仅 postgres/api/cleanup）与 `edge`（Caddy↔API）。
API 不直接暴露公网，`/metrics` 即使开启也只能在内网抓取；Prometheus 应在同一 Docker 网络或通过 SSH 隧道访问 `api:8080`。

`METRICS_ENABLED` 在 compose 中默认 `true`，但 **不发布 API 宿主端口**，且 Caddy 对 `/metrics` 直接返回 404。
如果 Prometheus 在宿主机之外，请用 SSH 隧道 / 内网专线，或在抓取入口加 basic auth；不要把 `/metrics` 直接暴露公网
（指标包含近似业务行数，虽不含个人数据，但会泄露容量与流量结构）。

#### liveness 与 readiness

| 端点 | 用途 | 行为 |
| --- | --- | --- |
| `GET /healthz` | liveness | 只返回进程状态，不访问数据库；容器/编排器判断是否重启 |
| `GET /api/v1/health` | readiness | 2 秒超时内 Ping 存储；失败 503，应摘流量但不要重启 |

镜像默认 `HEALTHCHECK` 用 `/healthz`；compose 的 `api1`/`api2` 通过 `HEALTHCHECK_URL` 覆盖为 readiness，供 `caddy` 的 `service_healthy` 使用。

#### 备份与恢复（可执行脚本）

```bash
# 备份：自定义格式 + gzip + UTC 时间戳 + SHA-256 + 按天保留
DATABASE_URL='postgres://...' BACKUP_DIR=./backups RETENTION_DAYS=14 \
  ./deploy/scripts/backup.sh

# 恢复：必须 --confirm；先校验 .sha256；恢复后检查迁移版本与表行数
DATABASE_URL='postgres://...' \
  ./deploy/scripts/restore.sh --confirm --file ./backups/pjgoiot-<ts>.dump.gz
```

- `BACKUP_MODE=host|docker`、`RESTORE_MODE=host|docker` 两种执行方式；
- 恢复是破坏性的（`pg_restore --clean --if-exists`），演练请用临时库；
- **本机已验证**：对真实 PostgreSQL 执行备份，再恢复到临时库 `pjgoiot_restoretest`，
  校验和通过、`schema_migrations` 为 `version=7 dirty=false`、表行数摘要与源库一致；
  保留策略删除超期文件也已实测。完整步骤见 [`docs/runbook.md`](docs/runbook.md)。

#### 监控与告警

- 抓取样例：[deploy/prometheus/scrape.yml](deploy/prometheus/scrape.yml)
- 告警规则：[deploy/prometheus/alerts.yml](deploy/prometheus/alerts.yml)（6 条：5xx 比例、P95、DB 错误、readiness、请求量为 0、指标缺失）
- 说明：健康告警依赖 blackbox-exporter；`/metrics` 指标与标签含义见上一节。

#### 保留策略（可配置）

API 不自动清理；必须通过 `cmd/cleanup` 定期调度。v0.8.0 起一次清理四类数据：

| 数据 | 默认保留 | 参数 / 环境变量 |
| --- | --- | --- |
| `readings` | 90 天 | `-retention-days`（`READING_RETENTION_DAYS` 仅 compose 使用） |
| `change_log` | 90 天 | `-change-log-retention-days` / `CHANGE_LOG_RETENTION_DAYS` |
| `idempotency_keys` | 7 天 | `-idempotency-retention-days` / `IDEMPOTENCY_RETENTION_DAYS` |
| 过期 token | 立即删除 | 固定按 `expires_at < now` |
| 过期配对码 | 立即删除 | 固定按 `expires_at < now` |

```bash
# 方式一：compose 的 maintenance profile（推荐，复用镜像与数据库网络）
docker compose --profile maintenance run --rm cleanup

# 方式二：宿主机直接跑（需 Go 或已构建的 /cleanup 二进制）
DATABASE_URL='postgres://...' go run ./cmd/cleanup                 # 默认 90/90/7 天 + 过期 token/配对码
DATABASE_URL='postgres://...' go run ./cmd/cleanup -dry-run        # 只打印时间边界
DATABASE_URL='postgres://...' go run ./cmd/cleanup \
  -retention-days 90 -change-log-retention-days 90 -idempotency-retention-days 7
```

调度示例（systemd timer / cron / Kubernetes CronJob）见
[`docs/runbook.md` §6](docs/runbook.md)；未配置调度时数据不会被清理。
每条 SQL 都带 30s 操作超时（`-operation-timeout`），避免大表清理卡住调度。
清理不影响 `user_devices`；`change_log` 被清理后旧游标会返回 410（见下一节）。

### 数据保留与游标失效（v0.8.0）

`GET /sync/changes?cursor=<n>` 的契约：

- `cursor=0` 表示客户端从未同步过，服务端从最老保留行开始返回，**永远不会**因保留期返回 410。
- 若 `cursor > 0` 且早于该用户当前最老保留 `seq` 减一，说明中间的行已被清理；此时返回：

```http
HTTP/1.1 410 Gone
Content-Type: application/json

{"error":{"code":"RESYNC_REQUIRED","message":"游标已过期，请全量重新同步"}}
```

- 客户端收到 410 必须**清空本地同步状态**，再以 `cursor=0` 全量拉取（或按设备重建），不能继续使用旧游标。
- 不允许静默返回空页：否则客户端会认为“没有新变更”并永久丢失被清理区间内的数据。
- 清理后 `change_log` 行数下降，`user_devices` / `readings` 不受影响。

端到端复现（本地栈）：

```bash
# 1) 匿名登录，push 3 台设备，记下 applied[].serverSeq = s1,s2,s3
# 2) 把 s1 的 server_ts 改成 100 天前
docker compose exec -T postgres psql -U pjgoiot -d pjgoiot \
  -c "UPDATE change_log SET server_ts = now() - interval '100 days' WHERE user_id='<uid>' AND seq=<s1>;"
# 3) 按 90 天保留清理
docker compose --profile maintenance run --rm cleanup
# 4) 旧游标 -> 410 RESYNC_REQUIRED；新游标 s1 -> 200 且剩 2 条
curl -sk "https://localhost/api/v1/sync/changes?cursor=$((s1-1))" -H "Authorization: Bearer $TOKEN"
curl -sk "https://localhost/api/v1/sync/changes?cursor=$s1"     -H "Authorization: Bearer $TOKEN"
```

本轮实测：旧游标得到 `410 / RESYNC_REQUIRED / 游标已过期，请全量重新同步`，新游标
返回 2 条，`change_log` 从 3 行降到 2 行，`user_devices` 保持 3 台。

**幂等键清理语义**（二选一，本轮选“按新请求处理”）：

- `idempotency_keys` 超过 7 天后被删除；同一 `Idempotency-Key` 再次到达时视为**全新请求**，
  不会返回 `409 IDEMPOTENCY_CONFLICT`，也不会命中旧响应缓存（响应中 `replayed=false`）。
- 由于设备写入本身是 LWW + 复合键幂等，重放同一设备数据不会产生重复设备；
  只有“清理后首次用同一 key 提交不同请求体”才会重新计算，而不复用旧缓存。

### token 生命周期（v0.8.0）

- 服务端签发 token 时写入 `tokens.expires_at`，默认 **180 天**，可用 `TOKEN_TTL_DAYS` 配置。
- `/auth/anonymous` 与 `/auth/pairing-code/claim` 的响应新增 `expiresAt`（RFC3339）：
  客户端应持久化并在过期前重新登录/配对；字段只加不删。
- 每次鉴权除校验哈希与吊销状态外，还会校验 `expires_at > now()`；过期后统一
  `401 {"error":{"code":"UNAUTHORIZED","message":"token 无效、已过期或已吊销"}}`。
- 401 语义不变：客户端**不得**静默调用 `/auth/anonymous` 换新账号（会造成 userId 漂移）。
- `cmd/cleanup` 按 `expires_at < now()` 物理删除过期 token 行；删除后行数下降，
  该 token 仍是 401（不存在等价于无效）。

边界行为：`expires_at` 恰好等于当前时刻视为已过期（SQL 用 `expires_at > now()`）；
库内 `expires_at` 与响应 `expiresAt` 一致（集成测试逐值比较到微秒）。

### 容量与压测结果（v0.8.0）

**机器规格**（本机实测，2026-11-05）：

- 宿主：macOS / Apple Silicon arm64，10 vCPU，32 GB RAM
- Docker Desktop VM：**2 vCPU / 4 GB**（`docker info` 报告 `NCPU=2`）
- 栈：`docker compose`（Caddy TLS → API → PostgreSQL 16），全部容器化
- 压测客户端：`cmd/loadgen` 在宿主机运行；`RATE_LIMIT_PER_MINUTE=0`（压测期间关闭限流，
  否则默认 120/min 会让结果全是 429）
- 数据规模：设备 100/用户、读数 2 000 条/用户（覆盖 90 天）、每批 push 100 设备 + 200 读数

命令：

```bash
RATE_LIMIT_PER_MINUTE=0 docker compose up -d api1 api2
go run ./cmd/loadgen -base-url https://localhost -duration 30s -warmup 3s
# 单场景：-scenario devices|push|series
```

真实结果（30s/场景，错误率按 HTTP >=400 或网络超时计）：

| 场景 | 并发 | 请求数 | RPS | p50 | p95 | p99 | 错误率 |
| --- | --- | --- | --- | --- | --- | --- | --- |
| `GET /my/devices` | 100 | 32 042 | 1 065.6 | 75 ms | 215 ms | 395 ms | 0.00% |
| `POST /sync/push`（100 设备 + 200 读数/批） | 50 | 1 598 | 51.5 | 801 ms | 2 005 ms | 2 818 ms | 0.00% |
| `GET /readings/series`（1d 桶，90 天） | 50 | 24 103 | 802.2 | 55 ms | 127 ms | 183 ms | 0.00% |

**p95 超过 200 ms 的定位（不是“写完文档就结束”）**：

- `GET /my/devices`：最初 100 并发 p95 为 220 ms。排查发现 `ListUserDevices` 对每台设备
  额外查询公共设备与设备类型（N+1，100 台 = 200 条 SQL）。改为**单条 LEFT JOIN**
  （`user_devices ⟕ public_devices ⟕ device_types`）后，`EXPLAIN (ANALYZE, BUFFERS)`
  实测执行 **0.25 ms**（100 行，走 `user_devices_user_id_updated_at_idx`）。
  剩余 p95 215 ms 是 **2 vCPU Docker VM 在 1 065 RPS 下的 CPU 饱和排队**：绕过 Caddy
  直连 API 复测 p95 为 256 ms（不降反升），说明瓶颈不在反向代理，也不在 SQL。
  50 并发时 p95 为 171 ms（<200 ms），可据此按机器规格推算容量。
- `POST /sync/push`：最初 50 并发 p50 4.9 s、p95 10 s（27 个客户端超时）。
  根因是每批约 900 条 SQL（逐设备查公共设备/型号/存在性/seq/变更日志 + 逐条读数插入），
  且为保持 LWW 对 100 台设备整批 `FOR UPDATE`，50 个并发事务在同一批行上排队。
  修正：
  1. 公共设备与设备类型改为每批 2 条批量查询；
  2. 已存设备改为每批 1 条 `IN (...)` 查询（去掉整批 `FOR UPDATE`，避免锁队列）；
  3. 变更 seq 用 `generate_series` 一次分配；
  4. 设备写入改为 `INSERT ... ON CONFLICT` / 单条 `UPDATE ... FROM (VALUES ...)` 批量执行；
  5. `change_log` 改为单条多行 INSERT；
  6. 读数改为单条多行 `INSERT ... ON CONFLICT (id) DO NOTHING RETURNING id`，仅冲突时才回查比较。
  每批 SQL 从约 900 条降到约 7 条，p95 从 10 s 降到 2.0 s、错误率 0%。
  仍有 p95 2 s 属于**写吞吐上限**：50 并发 × 每批约 400 行写 = 约 20 000 行/秒，
  已超出 2 vCPU/4 GB 本地 VM 的 PostgreSQL 写入能力；再压只能加 CPU 或分库，
  不是索引/查询写法问题（批量 UPDATE/INSERT 均已 EXPLAIN 验证走主键/唯一键）。
- `GET /readings/series`：单条聚合 SQL（`readings_user_platform_external_metric_recorded_idx`），
  p95 127 ms，未触发 200 ms 阈值。

> 说明：`cmd/loadgen` 的 push 场景按 worker 分配独立设备集合，模拟 50 个客户端各自推送；
> 若让 50 个并发反复写同一批 100 行，测到的是行锁排队而非服务容量，故不作为容量结论。

### 真实存储联调（v0.9.0）

客户端仓库此前都跑 `STORE=fixture`，覆盖不到真实 SQL、迁移、事务与超时。v0.9.0 提供一条命令
起「真实 Postgres + API」的本机栈，供 Android / iOS e2e 指向：

```bash
bash scripts/stack-for-e2e.sh        # 等价 make e2e-stack
# stdout 最后一行：BASE_URL=http://127.0.0.1:63450
bash scripts/seed-for-e2e.sh         # 幂等写入 1 台真实型号设备 + 24 条读数
bash scripts/stack-for-e2e.sh --status
bash scripts/stack-for-e2e.sh --stop # 等价 make e2e-stop
```

脚本行为：启动前用 connect+bind 预检端口，被占用时打印「端口 X 已被占用，自动换端口重试」
并重试（最多 3 次）；Docker 起独立 Postgres、迁移到最新 version（当前 `version=9`）、等 readiness、把
`DATABASE_URL/API_PID/BASE_URL` 写入 `.e2e/stack.env`；任一步失败自动清理并以非 0 退出。
stdout 只有 `BASE_URL=...`，人类日志走 stderr。可用 `E2E_PG_PORT` / `E2E_API_PORT`
强制首次尝试某端口来验证换端口路径（实测：65021/65022 被占用后自动切到 51660/51671）。

seed 只用迁移 seed 里**真实存在**的型号：公共设备 `aa:bb:cc:00:00:02` →
`ESP32-C3-DevKitM-1`（Espressif / `environmental_sensor`），24 条读数覆盖
`temperature_celsius` / `relative_humidity` / `pressure_hpa`，并校验 5 个 device_types、
固件 `ESP-IDF v5.2.2` 与配件 `USB-C 数据线`。重复执行不会新增用户/设备/读数（同 token、
设备 UPSERT、读数 `ON CONFLICT DO NOTHING`）。

本轮实测（真实 Postgres 容器 + 本机 API 进程）：

```text
== seed 第 1 次 ==
push 结果: applied=1 skippedReadings=0 conflicts=0
校验通过: user_devices=1 readings=24 metrics=pressure_hpa,relative_humidity,temperature_celsius device_types=5 firmware=ESP-IDF v5.2.2 accessory=USB-C 数据线 public_model=ESP32-C3-DevKitM-1

== seed 第 2 次（幂等） ==
复用已有 seed token（user=900e7f4a-01d8-4acb-8e99-017724fa9cd5）
push 结果: applied=1 skippedReadings=24 conflicts=0
校验通过: user_devices=1 readings=24 ...

== curl 全链路 ==
1) 匿名登录: {"userId":"164bd689-6f3e-487d-aa65-ddac2dda57ba","expiresAt":"2027-05-04T05:20:30.367973Z","hasToken":true}
2) push: {"applied":1,"skippedReadings":0,"conflicts":0,"serverSeq":[2]}
3) changes: {"changes":1,"nextCursor":2,"entities":["user_device"]}
4) series: {"metric":"temperature_celsius","bucket":"1d","buckets":2,"firstBucket":{"start":"2026-11-04T00:00:00Z","end":"2026-11-05T00:00:00Z","count":2,"min":21.5,"max":22.5,"avg":22,"last":22.5}}
5) metrics: {"items":[{"metric":"temperature_celsius","unit":"celsius","count":8}]}
6) export: {"user":"164bd689-6f3e-487d-aa65-ddac2dda57ba","devices":1,"readings":8,"hasMore":false}
7) 注销 DELETE /me: HTTP 204
8) 注销后访问: HTTP 401 {"error":{"code":"UNAUTHORIZED","message":"token 无效、已过期或已吊销"}}
```

客户端操作说明、需要清理的 token/游标/本地库，以及 fixture 与 postgres 的差异清单见
[`docs/e2e-with-postgres.md`](docs/e2e-with-postgres.md)。

### 压测门槛与阈值（v0.9.0）

`cmd/loadgen -smoke` 是 CI 的短跑模式：每场景 10s（2s 预热）、低并发，跑完按阈值断言，
**任一场景错误率 > 0 或 p95 超阈值即非 0 退出**，CI job 直接变红（没有 `|| true`）。

| 场景 | 并发 | 阈值（CI 断言） | 本机 smoke p95 | CI smoke p95（run 41974670052） | 取值依据 |
| --- | --- | --- | --- | --- | --- |
| `GET /my/devices` | 20 | **< 400 ms** | **28 ms**（20 421 请求，2 040.7 RPS） | **16 ms**（19 908 请求，1 986.4 RPS） | v0.8.0 100 并发 p95 215 ms 的约 1.9x，覆盖共享 runner 抖动 |
| `GET /readings/series`（1d/90 天） | 10 | **< 400 ms** | **15 ms**（14 207 请求，1 420.2 RPS） | **11 ms**（14 066 请求，1 405.8 RPS） | v0.8.0 50 并发 p95 127 ms 的约 3.1x，兼顾 CI 机器更慢 |
| `POST /sync/push`（100 设备+200 读数/批） | 5 | **< 4 000 ms** | **417 ms**（165 请求，16.2 RPS） | **313 ms**（184 请求，18.1 RPS） | v0.8.0 50 并发 p95 2 005 ms 的约 2x，保留写吞吐上限余量 |
| 错误率（全部场景） | — | **0%** | 0.00% | 0.00% | 不是性能抖动，任何 5xx/超时都视为回归 |

本地复跑（真实 Postgres 栈）：

```bash
bash scripts/stack-for-e2e.sh
go run ./cmd/loadgen -base-url "$(grep BASE_URL .e2e/stack.env | cut -d= -f2-)" -smoke
# 可覆盖默认阈值：-max-p95-devices 400ms -max-p95-series 400ms -max-p95-push 4s -max-error-rate 0
```

CI 中的 `perf-smoke` job 会起 postgres service + API（`RATE_LIMIT_PER_MINUTE=0`）后执行
`go run ./cmd/loadgen -base-url http://127.0.0.1:18080 -smoke`；失败时额外打印 API 日志。
排障步骤见 [`docs/runbook.md`](docs/runbook.md) §7.10。

### SLO（v0.9.0）

| 编号 | SLI | 目标 | 对应告警（alerts.yml） |
| --- | --- | --- | --- |
| SLO-1 可用性 | 1 - 5xx/全部请求 | **≥ 99.5%/月**（错误预算 216 分钟） | `ApiAvailabilityBudgetBurn`、`ApiHigh5xxRate`、`ApiHealthCheckFailing`、`ApiRequestRateDroppedToZero`、`ApiMetricsAbsent` |
| SLO-2 设备列表延迟 | `GET /my/devices` p95 | **< 300 ms** | `ApiDevicesP95High` |
| SLO-3 读数序列延迟 | `GET .../readings/series` p95 | **< 200 ms** | `ApiSeriesP95High` |

- PromQL、错误预算算法、告警一一映射与限制见 [`docs/slo.md`](docs/slo.md)。
- 面板：[`deploy/grafana/dashboard.json`](deploy/grafana/dashboard.json)（请求量、错误率、
  p50/p95/p99、DB 错误、用户/设备/读数近似量、健康探针）。
- v0.9.0 给 HTTP 指标新增 `route` 标签（`pjgoiot_http_requests_total{method,route,status}`、
  直方图 `{method,route}`），中间件把 Gin 的通配路由还原成
  `/my/devices`、`/my/devices/:platform/:externalId/readings/series` 等模板，基数固定，
  SLO-2/3 才能按接口精确查询。
- dashboard 的 JSON 结构与指标名由 `scripts/validate-dashboard.sh` 校验（JSON 可解析 +
  每个 `pjgoiot_*` 指标必须在 Go 代码中存在），已接入 CI `lint-configs`。

### 事务边界与部分写入语义（v0.10.0）

`POST /sync/push` 是**整批单事务**：设备 upsert、读数写入、`change_log` 追加、幂等记录写入
都在同一个 `BeginTx` 内完成，失败即 `defer tx.Rollback()`。不存在“前 2 台已提交、第 3 台冲突”的
部分成功；客户端拿到 200 时，响应里的每一行都已落库（`unchanged` 除外，它表示客户端已是最新）。

| 结论项 | 语义 | 代码位置 |
| --- | --- | --- |
| 批内原子性 | 全部设备 + 读数 + `change_log` + 幂等记录同一事务，任一校验/约束/崩溃错误整批回滚 | [`postgres.go`](internal/store/postgres/postgres.go) `Store.Push` |
| 计数严格一致 | `applied` 只统计真正写入的行；`conflicts` 是服务端获胜的冲突；读数 `appliedReadings + skippedReadings == len(readings)` | [`readings.go`](internal/store/postgres/readings.go) `pushReadingsTx` |
| 写入行数校验 | 设备 INSERT/UPDATE 与 `change_log` INSERT 都读取 `RowsAffected`，与期望行数不符则整批回滚，绝不“报成功” | `writePendingDevices` |
| 批内重复键 | 同一批出现重复设备 `id` 或重复 `(platform, externalId)` → **400 BAD_REQUEST**，整批不写 | `Store.Push` 第一遍规范化；HTTP 层同步校验 |
| 同批重复读数 id | 第一条计 `appliedReadings`，后续条计 `skippedReadings`（内容不同再进 `conflicts`），一个 id 最多计一次 applied | `pushReadingsTx` 的 `credited` 集合 |
| 幂等记录 | 与业务写入同事务：崩溃/回滚不会留下幂等键，同 `Idempotency-Key` 重试按新请求处理；只有成功提交后重放才返回历史响应 | `Store.Push` |
| 变更日志 | 与设备写入同事务，不会出现“设备已更新但 `change_log` 缺失”；同用户 push 用事务级咨询锁串行化，`seq` 分配顺序 = 提交顺序，游标不会越过更小 seq | `LockIdempotencyKey("push:"+userID)` |
| 事务预算 | push 总预算 **10s**（含连接池排队），超时返回 503 `TIMEOUT` 并回滚 | `pushTimeout` |

崩溃窗口测试（真实 PG，注入点返回错误后验证零残留）：

```bash
TEST_DATABASE_URL='postgres://platojobs@127.0.0.1:5432/pjgoiot?sslmode=disable' \
  go test ./internal/store/postgres -run TestPushTransactionBoundaries -v
# 覆盖：devices_written / change_log_written / readings_written /
#       idempotency_written / before_commit 五个阶段，以及重复 id/键、读数校验失败、并发 push
```

### 故障恢复行为（v0.10.0）

数据库故障时 API 返回 **503 + 明确错误码**，客户端可重试；不会把可恢复故障伪装成 500。

| 场景 | 状态码 / code | 行为与可重试性 |
| --- | --- | --- |
| Postgres 停机 30s（`GET /my/devices`、`POST /sync/push`） | **503 `UNAVAILABLE`** | 连接被拒/重置/关闭；整批未提交，可重试 |
| `/healthz`（liveness） | **200** | 只证明进程存活，不访问数据库，供容器 liveness 探针使用 |
| `/api/v1/health`（readiness） | **503 `UNAVAILABLE`** | 含数据库 `Ping`，失败即摘流 |
| Postgres 重启 | 立即自愈 | `database/sql` 丢弃坏连接，重启后首次请求 200（实测 0–0.5s，无需重启 API） |
| `statement_timeout=100ms`（慢查询） | **503 `TIMEOUT`** | `SQLSTATE 57014` 显式映射；`pjgoiot_db_errors_total` +1/次 |
| 连接池耗尽（`DB_MAX_OPEN_CONNS=2`、高并发） | 排队，超预算 **503 `TIMEOUT`** | 鉴权查询 3s 预算、push 10s 预算；不是无限挂起 |

实测证据（本机 Docker `postgres:16-alpine` + 本机 API 进程，停机 36s）：

```text
== 停机窗口（30 次采样）==
my/devices {'503': 30}   push {'503': 30}
healthz {'200': 30}      api/health {'503': 30}
== 重启后 ==
恢复：health 200，用时 0s；GET /my/devices => 200（12.7ms），设备数据完好

== 慢查询（ALTER ROLE ... SET statement_timeout='100ms'）==
baseline 1.34s/1.32s/1.54s（200 OK）
100ms 下连续 3 次：status=503 code=TIMEOUT time≈0.10–0.17s
pjgoiot_db_errors_total 0 -> 3

== 连接池 = 2 ==
GET /my/devices 200 并发 8s：7330 请求，896.7 RPS，p95 540ms，错误率 0（排队后成功）
POST /sync/push 100 并发 8s：250 请求，113×200 + 137×503(TIMEOUT)，p95 10.2s，错误率 54.8%
```

连接池排障要点：`DB_MAX_OPEN_CONNS` 至少覆盖预期并发；503 `TIMEOUT` 应指数退避重试；
切勿通过调大 HTTP `ReadTimeout` 来“掩盖”鉴权查询无限等待（v0.10.0 已给鉴权查询加 3s 预算）。

### 迁移演练（v0.10.0）

`scripts/migrate-drill.sh` 每次创建独立 `postgres:16-alpine` 容器与三个临时库，结束后删除，
可重复执行；覆盖空库升级、000001 时代数据升级、down 到 1 再 up。真实输出：

```text
===== 场景 1：空库 0 -> 最新 =====
  起始：none（空库）
  迁移后：version=9 dirty=false
  users tokens user_devices change_log readings = 0 0 0 0 0

===== 场景 2：000001 时代数据 -> 最新 =====
  迁移到：version=1 dirty=false → version=9 dirty=false
  回填抽查 user_devices(platform|external_id|device_type)：android|AA:BB:CC:00:00:01|nordic_uart
  回填抽查 tokens(expires_at = created_at + 180d)：true
  回填抽查 change_log(payload.platform|payload.externalId|entity_id)：
    android|AA:BB:CC:00:00:01|11111111-...:android:AA:BB:CC:00:00:01

===== 场景 3：最新 -> down 到 1 -> up 到最新 =====
  down 前  users tokens user_devices change_log readings = 1 1 1 1 1
  回滚 8 步 users tokens user_devices change_log readings = 1 1 1 1 -1
  up 后    users tokens user_devices change_log readings = 1 1 1 1 0
  up 后重新推导字段：android|AA:BB:CC:00:00:02|environmental_sensor
```

已知边界：`000006` 的 down 会删除 `readings` 表，跨 version 1 的往返会丢读数（迁移语义，
不是数据损坏）；`users` / `tokens` / `user_devices` / `change_log` 行数保持不丢。
本演练发现并修复了 000004 对大写 MAC 的漏回填（新增 `000009_mac_case_backfill`）。

### curl 示例

```bash
BASE=http://127.0.0.1:8080/api/v1

# 1. 健康检查
curl -s $BASE/health          # readiness：含数据库 Ping，失败 503
curl -s http://127.0.0.1:8080/healthz   # liveness：只证明进程存活，不访问数据库

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

# 10. 跨设备接管：A 设备生成一次性配对码（10 分钟有效，新码使旧码失效）
curl -s -X POST $BASE/auth/pairing-code -H "Authorization: Bearer $TOKEN"

# 11. B 设备用码换取同一 userId 的新 token（无需鉴权；旧 token 仍有效）
curl -s -X POST $BASE/auth/pairing-code/claim \
  -H "Content-Type: application/json" \
  -d '{"code":"K7M2QX9P"}'
# 失败统一 404：{"error":{"code":"NOT_FOUND","message":"配对码无效或已过期"}}

# 12. 导出全部数据（附件下载；读数升序，超过 limit 时用 nextCursor 翻页）
curl -s -D - "$BASE/me/export?limit=1000" \
  -H "Authorization: Bearer $TOKEN" -o "pjkotlinapp-export.json"
# 下一页：curl -s "$BASE/me/export?limit=1000&cursor=<nextCursor>" -H "Authorization: Bearer $TOKEN"

# 13. 注销账号（硬删除全部数据；返回 204，之后原 token 401）
curl -i -X DELETE $BASE/me -H "Authorization: Bearer $TOKEN"

# 14. Prometheus 指标（根路径，无需鉴权，不受限流）
curl -s http://127.0.0.1:8080/metrics
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
│   ├── api/main.go               # API 启动、启动迁移、优雅关闭、-healthcheck
│   ├── cleanup/main.go           # 90 天读数清理（cron/CronJob/compose profile 调用）
│   └── migrate/main.go           # 内嵌迁移执行器
├── db/
│   ├── migrations/               # 000001 建表 … 000008 token 过期（go:embed）
│   └── queries/                  # sqlc 查询：auth / metadata / sync
├── deploy/
│   ├── Caddyfile                 # HTTPS 终止 / 反代 / 拒绝公网 /metrics
│   ├── grafana/dashboard.json    # 请求/错误/p50-p95-p99/DB/近似量/健康探针面板
│   ├── prometheus/               # alerts.yml（9 条规则，含 3 条 SLO）+ scrape.yml
│   └── scripts/                  # backup.sh / restore.sh（可执行、带校验和）
├── docs/
│   ├── runbook.md                # 部署清单、升级/回滚、备份恢复演练、故障排查
│   ├── e2e-with-postgres.md      # Android/iOS 真实存储联调与 fixture 差异
│   └── slo.md                    # SLO、PromQL、错误预算与告警映射
├── scripts/
│   ├── stack-for-e2e.sh          # 起真实 Postgres + API（自动换端口、迁移、输出 BASE_URL）
│   ├── seed-for-e2e.sh           # 幂等写入最小可核实数据
│   └── validate-dashboard.sh     # dashboard JSON 结构与指标名校验
├── fixtures/                     # 无数据库模式的公共元数据
├── internal/
│   ├── auth/token.go             # 不透明 token 生成与哈希
│   ├── config/config.go          # 环境变量配置
│   ├── httpapi/                  # Gin 路由、中间件、handler、校验、错误出口
│   ├── migrate/migrate.go        # 进程内迁移入口（供启动时自动迁移）
│   ├── lww/lww.go                # LWW 判定
│   ├── model/model.go            # 对外 JSON 领域模型
│   ├── store/                    # Store 接口 + fixture/postgres 实现
│   └── validate/                 # MAC / platform / externalId 规范化
├── Dockerfile                    # 多阶段 + distroless nonroot（api/migrate/cleanup）
├── docker-compose.yml            # Caddy + API + Postgres（+ cleanup profile）
├── .env.example                  # 部署环境变量样例（不含真实密码）
├── Makefile
├── sqlc.yaml
└── .github/workflows/ci.yml      # build-test / perf-smoke / docker-build / lint-configs
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
| `RATE_LIMIT_BACKEND` | `memory` | `memory`（进程内，默认）或 `postgres`（跨实例共享计数；要求 `STORE=postgres`） |
| `SHUTDOWN_DRAIN_SECONDS` | `5` | 收到 SIGTERM 后先把 readiness 置 503、继续服务的时间（compose 默认 8s） |
| `INSTANCE_ID` | 空 | 写入响应头 `X-Instance`，用于确认请求由哪个实例处理 |
| `PAIRING_CLAIM_RATE_PER_MINUTE` | `20` | 配对码领取接口的每 IP 每分钟令牌数；`<=0` 关闭该接口独立限流 |
| `PAIRING_CLAIM_BURST` | `10` | 配对码领取接口的每 IP 令牌桶容量 |
| `METRICS_ENABLED` | `true` | 是否注册根路径 `/metrics`；生产可关闭或仅内网暴露 |
| `RUN_MIGRATIONS_ON_BOOT` | `true` | postgres 模式下启动时执行迁移；失败进程非零退出（fixture 模式忽略） |
| `CORS_ALLOWED_ORIGINS` | `*` | 逗号分隔的允许来源，`*` 表示全部 |
| `LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error` |
| `TOKEN_TTL_DAYS` | `180` | 签发 token 的有效期（天）；过期后鉴权返回 401，`cmd/cleanup` 删除过期行 |
| `APP_VERSION` | `0.11.0` | 服务版本（健康接口与日志展示）；与 OpenAPI 契约版本 0.7.0 不是同一个号 |
| `DB_MAX_OPEN_CONNS` | `20` | postgres 连接池最大连接数；过小会在高并发下排队并在超时预算后返回 503 TIMEOUT |
| `DB_MAX_IDLE_CONNS` | `10` | 连接池最大空闲连接数，超过 `DB_MAX_OPEN_CONNS` 时自动收敛 |
| `HEALTHCHECK_URL` | 自动推导 | `-healthcheck` 模式请求的完整 URL |
| `TEST_DATABASE_URL` | 空 | 设置后运行 Postgres 集成测试 |
| `READING_RETENTION_DAYS` | `90` | **仅 cleanup 容器**：读数保留天数，映射到 `-retention-days` |
| `CHANGE_LOG_RETENTION_DAYS` | `90` | **仅 cleanup 容器/命令**：变更日志保留天数 |
| `IDEMPOTENCY_RETENTION_DAYS` | `7` | **仅 cleanup 容器/命令**：幂等键保留天数 |

> 清理工具不在 API 进程中运行，API 侧无需配置以上三个保留期变量。

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
- `000007_pairing_codes`：新增一次性配对码表：
  - `code_hash text PRIMARY KEY`、`user_id uuid NULL REFERENCES users ON DELETE CASCADE`、`attempts smallint`、`expires_at`、`created_at`；
  - `user_id IS NULL` 的行用于记录错误尝试，累计 5 次即删除；
  - 索引 `(user_id)` 与 `(expires_at)`，支持新码使旧码失效与 TTL 清理；
  - down 迁移 `DROP TABLE pairing_codes`，不影响用户与其他数据（迁移测试验证 up → down → up）。
- `000008_token_expiry`：v0.8.0 数据生命周期：
  - `tokens` 新增 `expires_at timestamptz NOT NULL DEFAULT (now() + interval '180 days')`，
    旧行回填 `created_at + interval '180 days'`；索引 `tokens_expires_at_idx (expires_at)` 供清理使用；
  - 新增 `change_log_server_ts_idx (server_ts)` 与 `idempotency_keys_created_at_idx (created_at)`，
    支撑按保留期的时间范围删除；
  - 只加列/索引，不加默认过期行为之外的语义；down 迁移按逆序删除索引与列，不影响其余数据。
- `000009_mac_case_backfill`：v0.10.0 迁移演练发现的回填缺陷修复：
  - 000004 用 `ud.mac = pd.mac` 精确比较，000001 时代写入的大写 MAC 永远拿不到 `device_type`；
  - 本次按 `lower(ud.mac) = pd.mac` 重新回填仍为 `NULL` 的记录，读路径的 `public_devices` 关联也改为大小写不敏感；
  - 幂等、只补空值，不改写已有型号；down 为空操作（该列由 000004 down 负责删除）。

seed 数据来源与边界：

- GATT 服务 UUID 取自 Bluetooth SIG Assigned Numbers 的标准服务（Cycling Speed and Cadence `0x1816`、Environmental Sensing `0x181A`、Heart Rate `0x180D`、Battery `0x180F`、Device Information `0x180A`、Body Composition `0x181B`、Weight Scale `0x181D`、Generic Access `0x1800`），以及 Nordic Semiconductor 官方文档中的 Nordic UART Service（`6e400001-b5a3-f393-e0a9-e50e24dcca9e`）。
- 固件版本只收录官方公开的参考版本：`nRF Connect SDK v2.9.0`、`ESP-IDF v5.2.2`；`released_at` 留空，未编造发布日期。
- 公共设备为厂商公开的开发板型号：`nRF52840 DK`、`nRF5340 DK`、`ESP32-C3-DevKitM-1`。
- 配件只放能核实名称的通用/官方在售项；具体 SKU 与量产设备固件号未编造，留空或以厂商渠道为准。
- 厂商信息只使用官方公开主体：固件 `Nordic Semiconductor` / `Espressif Systems`；配件 `Polar Electro` / `Espressif Systems`；通用 AA 电池无厂商，保持空字符串。
- 兼容性声明只来自可核实的 GATT 能力：`nordic_uart`（UART 透传）、`environmental_sensor`（温度/湿度/气压）；配件兼容性按适配型号标注（`deviceTypes`）。

## 测试与验证

测试覆盖：

- `internal/httpapi`：httptest + Gin 表驱动；健康、公共设备、设备类型/详情/固件/配件/聚合、401/404、非法 MAC、CORS 预检、注销、push 参数校验（platform / externalId / 平台规则 / deviceType / alias / advertisedName / group）、幂等重放、幂等键冲突、LWW 冲突、tombstone、changes 分页、跨用户越权 404、同用户 Android 与 iOS 双记录互不覆盖、按复合键单查（200/400/404、大小写规范化、删除后 404、跨用户 404、旧 MAC 路径兼容）、401 响应不下发新 token、限流；v0.3.2 新增 `TestDeviceMetadataEnrichment`：`UserDevice` 的 `deviceType`/`vendor`/`firmwareVersion`/`compatibility` 200 用例、catalog 固件与配件字段完整性、未注册型号与无 MAC 时降级为 `null` 且不返回 500；v0.3.3 新增 `TestPresenceSemantics`：`"group": ""` 显式清除、字段缺失保持原值、清空后再次同步不会让分组复活、上报 `advertisedName` 不改变 `alias` 且反之亦然；v0.4.0 新增 `TestReadingsPushIdempotency`（重复 id 计 skipped、同 id 不同内容计 `duplicate_id` 冲突且原值不变）、`TestReadingsPushOptional`（无 readings 时计数为 0、隐式设备写入与注册后可查）、`TestReadingsValidation`（metric/unit/value/recordedAt/批量上限/非法 externalId）、`TestReadingsListPagination`（倒序、游标翻页、metric 与时间过滤、非法参数、跨用户 404）、`TestReadingsSummary`（聚合值、无数据 null、hours 校验、跨用户 404）；v0.5.0 新增 `TestReadingsSeries`（1h/1d 的 UTC 对齐与跨整点/跨日边界、桶内 min/max/avg/last、空区间 `buckets: []`、8 类非法参数含「桶数超 500 提示更粗 bucket」、跨用户与不存在设备 404、unit 取窗口最新）、`TestReadingsMetrics`（排序与计数、多单位取最新、空数组、跨用户 404）；v0.6.0 新增 `TestPairingCodeFlow`（生成/字符集/TTL、新码使旧码失效、claim 换新 token 且原 token 保留、重放 404、跨用户、错误体不泄露账号）、`TestPairingCodeExpired`（过期统一 404）、`TestExportContentAndPagination`（内容与库内一致、升序、limit 分页与 nextCursor 连续性、非法 cursor/limit、Content-Disposition）、`TestDeleteMeIdempotent`（204、之后 401、重复调用 401、不影响其他用户）、`TestMetricsEndpoint`（文本可解析且含请求数/直方图/DB 错误/近似计数）、`TestMetricsBypassesRateLimit`（限流触发后 `/metrics` 仍 200）、`TestRequestIDAndLogs`（透传/生成 UUID、日志字段齐全且不含 token）；v0.7.0 新增 `TestHealthzLiveness`（`/healthz` 200、字段完整、不受限流影响，与 readiness 区分）。
- `internal/lww`：时钟兜底、服务端获胜、内容相同 noop 等判定；`deviceType`、`alias`、`advertisedName`、`group` 仅在客户端上报时参与内容比较（缺失保持原值不产生冲突）。
- `internal/store/postgres`：`TEST_DATABASE_URL` 存在时运行真实数据库集成测试，覆盖 token 创建/校验/吊销、幂等重放与冲突、LWW、tombstone、分页、跨用户隔离、Android/iOS 复合键隔离、`GetUserDeviceByKey` 跨用户 404，公共元数据补全与缺失降级（`deviceType` 由 MAC 推导、按型号回退厂商/兼容性、未注册型号返回 null），以及可缺省字段语义（别名与广播名隔离、`group: ""` 清除后缺省同步不复活）；v0.4.0 新增集成子测试「读数链路」：批量写入与计数、重复 id 幂等、同 id 不同内容冲突、NaN/Inf/未来时间/非法 metric 拒绝、数据库 CHECK 拒绝 NaN、倒序游标分页、非法游标、metric 过滤、跨用户与不存在设备 404、summary 聚合与无数据、90 天清理（历史数据被删且近期数据保留）；v0.5.0 新增集成子测试「读数分桶序列与指标清单」：1h 跨整点与 1d UTC 零点对齐、桶内 min/max/avg/last、分桶计数合计与原始条数一致、空区间 `[]`、非法 bucket / 超 90 天 / 超 500 桶、跨用户与不存在设备 404、指标清单排序与计数、空设备空数组；v0.6.0 新增 `TestPostgresAccountIntegration`：配对码新码失效旧码、claim 签发新 token 且原 token 保留、重放/过期 404、错 5 次作废（库内行删除）、跨用户配对、TTL 清理、导出含 tombstone 与升序分页（条数与库内 `COUNT` 一致、字段一致、非法游标）、`pg_class.reltuples` 近似计数、注销级联（`users/tokens/user_devices/change_log/idempotency_keys/readings/pairing_codes` 全部为 0）与重复注销 401。
- `internal/store/postgres/migration_test.go`：临时数据库上先迁移到 version=2，写入旧 `(user_id, mac)` 记录与旧 change_log，再迁移到最新 version=9，验证 000003 的键回填、000004 的 `device_type`/厂商回填、000005 的 `advertised_name` 默认值、000006 的 `readings` 表与索引、000007 的 `pairing_codes` 表、000008 的 `tokens.expires_at` 回填与三个新索引、000009 的大写 MAC 型号补齐；随后回滚到 version=6 验证 `pairing_codes` 被删除且用户数据完好，再回滚到 version=5 验证 `readings` 表被删除且设备数据完好，最后 up 回到 version=9。

本机实际执行结果（2026-11-05）：

| 命令 | 结果 |
| --- | --- |
| `go build ./...` | 通过 |
| `go vet ./...` | 通过 |
| `go test ./...` | 通过（`internal/httpapi`、`internal/lww`、`internal/store/postgres` 均 ok；`-v` 统计 40 个顶层测试，0 失败；需数据库的集成用例由下一行单独执行） |
| `TEST_DATABASE_URL=... go test ./internal/store/postgres -v` | 通过（本机真实 PostgreSQL，迁移到 version=8；含旧数据回填、sync 链路、读数、分桶序列与指标清单、配对码/导出/注销/近似计数、保留期清理与游标失效、down 回滚） |
| v0.3.2 字段测试 | 通过：`TestDeviceMetadataEnrichment`（列表/单查 200 字段、catalog 固件配件完整性、未注册型号与无 MAC 降级 null）、`TestDeviceTypeComparison`、集成测试「公共元数据补全与缺失降级」 |
| v0.3.3 语义测试 | 通过：`TestPresenceSemantics`（`"group": ""` 清除、缺失保持、清空后拉回为空、广播名与别名互不覆盖）、`TestPresenceOnlyComparison`、集成测试「可缺省字段语义与广播名隔离」、迁移测试 `advertised_name` 默认空串 |
| v0.4.0 读数测试 | 通过：`TestReadingsPushIdempotency`、`TestReadingsPushOptional`、`TestReadingsValidation`、`TestReadingsListPagination`、`TestReadingsSummary`、集成子测试「读数链路」（含数据库 CHECK 拒绝 NaN、90 天清理）、迁移测试 up→down→up |
| v0.5.0 分桶与指标清单测试 | 通过：`TestReadingsSeries`（UTC 对齐、跨整点/跨日、桶内聚合、空区间、非法参数、404）、`TestReadingsMetrics`（排序/计数/单位取最新/空数组/404）、集成子测试「读数分桶序列与指标清单」（真实 Postgres） |
| v0.6.0 配对/合规/可观测性测试 | 通过：`TestPairingCodeFlow`、`TestPairingCodeExpired`、`TestExportContentAndPagination`、`TestDeleteMeIdempotent`、`TestMetricsEndpoint`、`TestMetricsBypassesRateLimit`、`TestRequestIDAndLogs`；集成子测试「新码使旧码失效」「claim 签发新 token 且保留原 token」「重放返回 404」「过期码返回 404」「错 5 次作废」「跨用户配对」「TTL 清理」「导出设备含 tombstone」「导出读数分页与内容」「非法游标」「近似计数可读」「注销级联与幂等」 |
| 真实库迁移 | 通过：`cmd/migrate up` → version=8 dirty=false；`tokens.expires_at` 与 `change_log_server_ts_idx` / `idempotency_keys_created_at_idx` / `tokens_expires_at_idx` 按预期创建；迁移测试验证 000007/000008 up → down → up 且用户数据不受影响 |
| Postgres 模式 HTTP 验证 | 通过：health、匿名登录、公共设备、类型/详情/固件/配件、my/devices、push、幂等重放、409、LWW 冲突、key_taken、tombstone 404、跨用户 404、注销后 401、Android/iOS 双记录、旧客户端 mac-only 兼容、同复合键换 id 不重复 |
| v0.4.0 读数 HTTP 验证 | 通过（真实 PostgreSQL + `cmd/api`）：push `appliedReadings=2`；重复 id 不同值返回 `skippedReadings=1` 与 `entity=reading/reason=duplicate_id` 冲突且原值不变；列表倒序；summary `count/min/max/avg/last` 正确；无数据时数值为 `null`；未来时间 400 `BAD_REQUEST`；NaN 400；未知设备与跨用户 404 |
| fixture 模式新增接口验证 | 通过：`/device-types/{type}/catalog` 200 且含固件/配件、未知类型 404；`/my/devices/android/{mac}` 与 `/my/devices/ios/{uuid}` 200；非法 platform 400；旧 `/my/devices/{mac}` 兼容 |
| fixture 字段补全验证 | 通过：`/my/devices` 中 Android 设备返回 `nordic_uart / Nordic Semiconductor / nRF Connect SDK v2.9.0` 与 UART 兼容性；iOS 上报 `environmental_sensor` 返回厂商与温度/湿度/气压兼容性、`firmwareVersion=null`；未注册型号仅回显 `deviceType`，其余为 `null`；catalog 固件/配件均带 `vendor` 与 `compatibility` |
| fixture 语义验证 | 通过：上报 `advertisedName` 后 `alias` 与 `group` 不变；`"group": ""` 后分组为空；随后仅上报广播名（缺省 group），分组仍为空，未复活 |
| 401 契约验证 | 通过：无效 token 返回 401 `UNAUTHORIZED`，响应体仅有 `error`，不含 `token` / `userId` |
| 复合键数据库验证 | 通过：真实 PostgreSQL 中按 `(platform, externalId)` 查询 Android / iOS 成功，跨用户查询返回 `ErrNotFound` |
| v0.7.1 完整栈 `build` + `up -d` | **已验证**：`docker compose build` 成功（`pjgoiot-api:0.7.1`，43.9MB）；`up -d` 后 postgres/api 均 `healthy`，Caddy `Up`；60s 内每 5s 打印的 `docker compose ps` 12 次均正常；`down` 未加 `-v`，`pjgoiot_pgdata`/`caddy_data`/`caddy_config` 三个卷保留 |
| a) Caddy → readiness | **已验证**：`curl -k https://localhost/api/v1/health` → `HTTP 200`，`{"service":"pjgoiot-api","status":"ok","store":"postgres","version":"0.7.1"}`；`/healthz` 同样 200；响应含 HSTS 等安全头 |
| b) Caddy 拦截 `/metrics` | **已验证**：`curl -k -D - https://localhost/metrics` → `HTTP/2 404`，`content-length: 9`，body `not found` |
| c) 容器内 `/metrics` | **已验证**：在 `pjgoiot_edge` 网络内用 node 容器请求 `http://api:8080/metrics` → `HTTP 200`，含 `pjgoiot_http_requests_total{method="GET",status="200"} 12` |
| d) 非 root + 只读根文件系统 | **已验证**：`docker inspect` → `User=nonroot:nonroot`、`ReadonlyRootfs=true`；`docker top` 显示 UID `65532`；`docker cp` 写入报 `container rootfs is marked read-only`。distroless 无 `id` 命令（`exec: "id": executable file not found`），故用 `docker top`/inspect 取证 |
| e) `X-Request-Id` | **已验证**：带 `X-Request-Id: local-verify-123` 响应头原样回显；不带时返回生成的 UUID（如 `93317b48-1b74-4318-9d7b-70efedb6d862`） |
| f) 重启/恢复持久化 | **已验证**：写入设备后 `docker compose restart api`，`GET /my/devices` 仍返回同一设备；`down` 后再 `up` 数据仍在（Postgres 卷持久化） |
| g) 容器启动迁移 | **已验证**：`docker compose logs api` 两次启动均输出 `启动迁移完成 version=7 dirty=false` |
| v0.7.1 PromQL 语义校验 | **已验证**：`docker run --entrypoint /bin/promtool prom/prometheus:latest check rules /cfg/alerts.yml` → `SUCCESS: 6 rules found`；`check config /cfg/scrape.yml` → `SUCCESS: ... valid prometheus config file syntax` |
| v0.7.1 docker 模式备份/恢复 | **已验证**：`BACKUP_MODE=docker backup.sh` → `pjgoiot-20261105T031905Z.dump.gz`（8 193 bytes）+ SHA-256；`RESTORE_MODE=docker restore.sh --confirm` → 校验 `OK`、`version=7 dirty=f`、行数 `1/1/0/1/0`；恢复后 API `healthy` 且原设备仍可查 |
| v0.7.1 GitHub Actions | **通过**（run 41962617787，commit ef0c7af）：`构建与测试`、`构建镜像`、`配置与脚本校验` 均 success |
| v0.8.0 生命周期单元测试 | 通过：`TestAnonymousReturnsExpiresAt`（响应 `expiresAt` 约 180 天后）、`TestExpiredTokenReturns401`、`TestCursorExpiredReturns410`（`410 RESYNC_REQUIRED` + 指定 message）、`TestQueryTimeoutReturns503`（`503 TIMEOUT`）、`TestListLimitOutOfRange`（readings 501 / changes 501 / export 5001 → 400） |
| v0.8.0 保留期集成测试 | 通过（真实 Postgres）：`TestCleanupAndRetentionLifecycle`——`expiresAt` 与库内一致到微秒；3 条 change_log 老化 1 条后 cleanup 删除 1 行；旧游标 `seq0-1` → `ErrCursorExpired`，新游标 `seq0` 返回剩余 2 条且 `nextCursor=seq2`；`user_devices=3` / `readings=1` 不受影响；幂等键清理后同 key 重放 `replayed=false`（不 409）；过期 token `ErrUnauthorized`，`CleanupTokens` 后行数 0 |
| v0.8.0 端到端（真实 compose 栈） | 通过：匿名 `expiresAt=2027-05-04`；push 3 台设备得 `serverSeq=[246402,246403,246404]`；`cursor=0` 返回 3 条；cleanup 输出 `readings=1628 / change_log=1 / idempotency=0 / tokens=0`；旧游标 → `410 RESYNC_REQUIRED`；新游标返回 2 条；`/my/devices` 仍 3 台；过期 token → 401；token 清理后库内 0 行 |
| v0.8.0 压测 | 通过（真实 compose 栈，Docker Desktop VM 2 vCPU / 4 GB）：devices 100 并发 30s → 1 065.6 RPS、p50 75 / p95 215 / p99 395 ms、0% 错误；push 50 并发（100 设备+200 读数）→ 51.5 RPS、801 / 2 005 / 2 818 ms、0%；series 50 并发（1d/90 天）→ 802.2 RPS、55 / 127 / 183 ms、0%。详见「容量与压测结果」 |
| v0.8.0 GitHub Actions | **通过**（run 41969477796，commit b879f72）：`构建与测试`（含 `TestCleanupAndRetentionLifecycle`、迁移测试）、`构建镜像`、`配置与脚本校验`（cleanup/loadgen 编译与 `-h`、脚本 `bash -n`）均 success；tag `v0.8.0` 指向 b879f72 |
| v0.9.0 真实存储联调 | 通过（真实 Postgres 容器 + 本机 API）：栈自动选端口 65500/65504、迁移 `version=8 dirty=false`、readiness 通过；curl 全链路 匿名登录 → push → changes → series → metrics → export → 注销 204 → 再访问 401 |
| v0.9.0 seed 幂等 | 通过：连续执行两次，第二次复用 token，`skippedReadings=24`，`user_devices=1` / `readings=24` 不变；真实型号 `ESP32-C3-DevKitM-1`、`ESP-IDF v5.2.2`、`USB-C 数据线` 均由迁移 seed 校验 |
| v0.9.0 smoke 门槛 | 通过（真实 Postgres 栈，本机）：devices 20 并发 p95 28ms（阈值 400ms）、series 10 并发 p95 15ms（400ms）、push 5 并发 p95 417ms（4000ms），错误率 0%，`SMOKE PASS` |
| v0.9.0 dashboard 校验 | 通过：`bash scripts/validate-dashboard.sh` → JSON 结构有效（9 面板），`probe_success` 豁免，其余 `pjgoiot_*` 指标均在 Go 代码中存在 |
| v0.9.0 单元/集成测试 | 通过：`gofmt`/`go build`/`go vet`/`go test ./...` 全绿；真实 PostgreSQL 集成测试（`TEST_DATABASE_URL`）全绿；`TestMetricsEndpoint` 已断言 `route` 标签 |
| v0.9.0 GitHub Actions | **通过**（run 41974670052，commit c0f93ba）：`构建与测试`、`压测门槛（smoke）`（devices p95 16ms / series 11ms / push 313ms，错误率 0%，`SMOKE PASS`）、`构建镜像`、`配置与脚本校验`（含 dashboard 校验与新脚本 shellcheck）均 success |
| v0.10.0 事务边界与崩溃注入 | 通过（真实 PostgreSQL）：`TestPushTransactionBoundaries` 覆盖重复 id/键 400、读数校验失败整批回滚、`devices_written`/`change_log_written`/`readings_written`/`idempotency_written`/`before_commit` 五个崩溃注入点零残留、同 key 重试成功、`applied+skipped==len(readings)`、并发 push 游标无缺口 |
| v0.10.0 故障注入 | 通过（本机 Docker PG + 本机 API）：停机 36s 期间 `my/devices`/`push` 30/30 为 503 `UNAVAILABLE`，`/healthz` 30/30 为 200，`/api/v1/health` 30/30 为 503；重启后 0s 自愈；`statement_timeout=100ms` → 3 次 503 `TIMEOUT` 且 `db_errors_total` 0→3；池=2 时 devices 200 并发 7 330 请求 p95 540ms 0% 错误，push 100 并发 113×200 + 137×503 p95 10.2s |
| v0.10.0 迁移演练 | 通过：`scripts/migrate-drill.sh` 三场景可重复执行——空库→9；000001 数据→9 且 `platform/external_id/device_type/expires_at` 回填正确；down 8 步→1 再 up→9，`users/tokens/user_devices/change_log` 行数 1/1/1/1 不丢（readings 因 000006 down 归零，已知语义） |
| v0.10.0 GitHub Actions | **通过**（run 42007845297，commit c61e0c6）：`构建与测试`、`构建镜像`、`配置与脚本校验`（含 `scripts/migrate-drill.sh` 的 `bash -n`/shellcheck）、`压测门槛（smoke）`（devices p95 11ms / push 392ms / series 6ms，错误率 0%，`SMOKE PASS`）均 success；tag `v0.10.0` 指向 c61e0c6 |
| v0.11.0 双实例负载均衡 | 通过：`scripts/multi-instance-check.sh` 经 Caddy 打 20 次探测，`api1`/`api2` 各 10 次；两个实例共享同一 Postgres |
| v0.11.0 跨实例 seq 与游标 | 通过：30 个并发 push 打到两个实例，同时轮询 `/sync/changes`；最终 `change_log` 31 条与拉取集合完全一致，`seq` 严格递增、无缺口、无越过未提交变更 |
| v0.11.0 跨实例幂等 | 通过：同一 `Idempotency-Key` 在实例 A 首次 200，重放打到实例 B 返回 200 且 `Idempotent-Replayed: true`、响应体一致；同 key 不同 body → 409 `IDEMPOTENCY_CONFLICT` |
| v0.11.0 摘流行为 | 通过：`api1` 停止期间持续 160 个请求经 Caddy 全部 200（0 个 502），尾部只由 `api2` 服务；重启 `api1` 后重新接流量 |
| v0.11.0 滚动发布 | 通过：`scripts/rolling-restart.sh` 532 个请求全部 200，502=0、连接失败=0，停机请求数 **0**（`api1=239`、`api2=293`） |
| v0.11.0 并发迁移 | 通过：`scripts/migration-concurrency-check.sh` 外部持有 migrate advisory lock 5s，两个实例同时启动均在 5 261ms 后就绪，最终 `version=10 dirty=false`、`schema_migrations` 仅 1 行、5 张关键表存在 |
| v0.11.0 限流真相 | 通过：`scripts/shared-rate-limit-check.sh`，memory 后端双实例放行 6/分钟（2 倍额度）；postgres 后端全局只放行 3 个，两个实例均返回 429 |
| `docker build` | 通过（GitHub Actions `构建镜像` job，v0.6.0 提交 8a5e4bd，run 41951827338） |
| GitHub Actions | 通过（v0.6.0：gofmt、vet、build、单元测试、迁移到 version=7、Postgres 集成测试、distroless 镜像构建；run 41951827338） |
| OpenAPI 静态校验 | 通过（v0.8.0）：`npx @redocly/cli lint api/openapi.yaml` → valid，0 errors（12 条非阻断 warning，多为 operation-4xx-response 提示）；`expiresAt`、`410 CursorExpired`、`503 Timeout`、`RESYNC_REQUIRED`/`TIMEOUT` 均进入契约且 `$ref` 可解析 |

集成测试运行方式：

```bash
TEST_DATABASE_URL='postgres://platojobs@127.0.0.1:5432/pjgoiot_test?sslmode=disable' \
  go test ./internal/store/postgres -run 'TestPostgresStoreIntegration|TestMigrationBackfillsLegacyDevices' -v
```

## 多实例部署（v0.11.0）

compose 默认起两个 API 实例 `api1`、`api2`，共享同一个 Postgres；Caddy 在两者之间做
轮询负载均衡（`lb_policy round_robin`），并用主动健康检查（`/api/v1/health`，2s 间隔）
自动摘除不健康实例。两个实例都在内部 `backend` 网络里，只有 Caddy 暴露 80/443。

```bash
docker compose up -d --build
docker compose ps                       # api1/api2/postgres 均 healthy，caddy Up
curl -k https://localhost/api/v1/health # 响应头 X-Instance: api1 或 api2
```

- `X-Instance` 来自 `INSTANCE_ID`，用于确认实际服务实例；不参与业务契约。
- 两个实例启动时都会执行迁移，golang-migrate 使用 PG advisory lock 串行化；
  实测第二个实例会等待锁而不是并发执行（见 runbook §9.10）。
- 迁移必须先于新代码完成（前滚设计）；滚动发布脚本会逐实例摘流、重启并等待健康。
- 会话不落进程：token、幂等键、`change_log` 都在 Postgres，任何实例都能处理任意请求。
- 限流默认仍是**进程内计数**，双实例等于 2 倍额度；需要全局额度时设置
  `RATE_LIMIT_BACKEND=postgres`（见下节）。

### 滚动发布

```bash
bash scripts/rolling-restart.sh        # 逐实例摘流 → stop → start → 等健康 → 归还流量
```

脚本在全程持续打请求（经 Caddy，约 20 req/s），每个实例执行：

1. `docker compose stop <instance>`：SIGTERM 后 readiness 立即返回 503，Caddy 健康检查
   把它摘除；进程仍继续服务 `SHUTDOWN_DRAIN_SECONDS`（compose 默认 8s）后退出；
2. `docker compose start <instance>`，等待 healthy；
3. 继续观察 3s，确认该实例重新接流量。

实测 532 个请求全部 200（0 个 502、0 个连接失败），停机请求数为 **0**。

### 限流边界

| 后端 | 计数范围 | 双实例实际额度 | 代价 |
| --- | --- | --- | --- |
| `memory`（默认） | 单进程 | **2× `RATE_LIMIT_PER_MINUTE`** | 无额外依赖；额度随实例数线性放大 |
| `postgres` | 全实例共享 | 1× | 每个限流请求 1 次 UPSERT；高并发同一 IP 会在单行上排队；DB 故障时**降级为进程内限流**并打 warn 日志 |

`postgres` 后端使用 `rate_limit_buckets` 表（迁移 `000010`）做令牌桶，单条
`INSERT ... ON CONFLICT ... RETURNING` 原子扣减；每 5 分钟惰性清理 10 分钟未更新的桶。

实测（`scripts/shared-rate-limit-check.sh`，额度 3/分钟、burst 3）：

- `memory`：两个实例合计放行 6 个请求，第 7 个起 429 → 确认为 2 倍额度；
- `postgres`：两个实例合计只放行 3 个，两个实例都返回 429 → 全局额度生效。

取值建议：

- 单实例，或已有前置网关/云 WAF 做全局限流 → `memory` 即可；
- 多实例且限流是硬约束（防刷、防配额滥用）→ `postgres`；接受每请求一次数据库写，
  并监控 `pjgoiot_db_errors_total`；Redis 未引入（多一个需要高可用与持久化运维的组件）。
- `RATE_LIMIT_PER_MINUTE<=0` 仍然完全关闭限流（压测用）。

## 已知限制与下一步

- `/auth/login` 仍为 501，只有匿名账号；后续可加账号密码 / OAuth 与匿名账号绑定。
- token 已有有效期（默认 180 天）但**没有刷新/滚动续期**；客户端应在 `expiresAt` 前重新登录或配对，且不得在 401 后静默换新匿名账号（会造成 userId 漂移）。
- `/my/devices/{mac}` 仅适合 Android；iOS 使用 `/my/devices/{platform}/{externalId}` 单台查询（已提供），或使用 `/my/devices` 列表。
- 读数、`change_log`、幂等键与过期 token 的保留期删除均由 `cmd/cleanup` 负责，API 内不做后台定时清理；生产环境需自行配置 cron / systemd timer / CronJob，否则数据无限增长。
- `change_log` 被清理后，落后超过保留期的客户端会收到 `410 RESYNC_REQUIRED`；客户端必须实现“清本地 + `cursor=0` 全量拉取”，否则会卡在 410。
- `scripts/stack-for-e2e.sh` 绑定 `127.0.0.1`，覆盖 Android/iOS 模拟器；真机经局域网访问尚未验证。
- `/metrics` 的 `route` 标签依赖 Gin 路由模板；若新增通配路由，需要同步扩展 `routeLabel`，否则会退化为通配模板（不影响正确性，只降低可分性）。
- 读数查询要求设备已存在于 `user_devices`；隐式设备写入的读数在注册前不可查。
- 读数支持单指标聚合（`min/max/avg/last`）、UTC 对齐的 `1m/1h/1d` 分桶序列，以及设备维度指标清单；跨指标合并统计按需再扩展。
- 限流默认是单进程内存令牌桶，多实例部署时各实例独立计数（N 个实例约 N 倍额度）；需要全局限流时设置 `RATE_LIMIT_BACKEND=postgres`，其单行热点与数据库故障降级语义见「限流边界」。
- fixture 模式的用户与设备数据在重启后丢失，仅用于本地联调与测试。
- 公共元数据列表未分页；当前数据量小，后续数据增长后再加分页。
- 删除时 payload 采用本次 push 提交的字段；客户端删除前应提交完整业务字段，或仅依赖 `deletedAt`。
- seed 只有开发板参考数据，尚未接入真实量产设备型号、固件发布列表与官方配件 SKU；需要厂商/产品提供可核实来源后再补。
- 尚未做 Android 端到端联调；后端 HTTP 压测已完成（见「容量与压测结果」），但未覆盖 App 侧重试/弱网行为。可观测性已提供 `/metrics`（请求量/延迟/DB 错误/近似计数）与 `X-Request-Id`，Tracing 仍待接入。
- 配对码为 8 位（约 40 bit 熵）且仅 10 分钟有效，配合每 IP 限流与错 5 次作废；如需更高强度可加长码或增加二次验证。
- `/metrics` 的业务计数为 `pg_class.reltuples` 近似值（依赖 autovacuum/ANALYZE），非精确行数；不应作为计费或审计依据。
- 完整栈已在本地 `docker compose up` 实测（Caddy 自签 HTTPS、非 root、只读根文件系统、启动迁移、卷持久化、docker 模式备份恢复均通过）；**真实域名 / ACME 证书 / 云主机仍未验证**，仓库未开通任何云账号。
- distroless 镜像没有 shell、`id`、`curl`，容器内排查请用 `docker top` / `docker inspect` / `docker compose exec api /api -healthcheck`；健康检查已使用二进制自带的 `-healthcheck`，无需额外工具。
- 备份脚本默认 `host` 模式，需要本机有 `pg_dump`/`pg_restore`；容器内执行用 `BACKUP_MODE=docker`/`RESTORE_MODE=docker`，本轮已实测。
- 告警规则已用容器内 `promtool check rules` + `check config` 做过语义校验；阈值仍需按真实流量基线调整。压测为 30s/场景的短时压测，未做数小时长稳压、故障注入与多实例集群验证。
- systemd timer 仅提供示例，未在目标主机安装验证。

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
