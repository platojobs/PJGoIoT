# PJGoIoT 后端

「蓝牙设备数据面板」Android / iOS App 的后端服务：多用户账号、BLE 设备公共元数据、用户私有设备与增量同步。

- 语言 / 运行时：Go 1.22+
- HTTP 框架：Gin v1.10（仅 `internal/httpapi` 层依赖）
- 数据库：PostgreSQL 16（迁移 golang-migrate，查询 sqlc，不使用 ORM）
- 日志：标准库 `log/slog` 结构化日志
- 契约：[`api/openapi.yaml`](api/openapi.yaml)，当前版本 **v0.3.1**（与 Android v0.3.1、iOS v0.3.0 联调对齐）

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
| GET | `/sync/changes?cursor=&limit=` | Bearer | 200 | 增量变更，`{changes,nextCursor,hasMore}` |
| POST | `/sync/push` | Bearer | 200 | 批量写入，`{applied,conflicts,nextCursor}` |

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

# iOS：externalId 为 CBPeripheral.identifier（UUID），mac 可省略
curl -s -X POST $BASE/sync/push \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "clientId": "ios-demo",
    "devices": [{
      "id": "22222222-2222-4222-8222-222222222222",
      "platform": "ios",
      "externalId": "E621E1F8-C36C-495A-93FC-0C247A3E6E5F",
      "alias": "iOS 连接的开发板",
      "updatedAt": "2026-11-05T01:00:00Z",
      "deleted": false
    }]
  }'

# 7. 拉取增量变更
curl -s "$BASE/sync/changes?cursor=0&limit=50" -H "Authorization: Bearer $TOKEN"

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
- **用户私有数据**（按 `user_id` 隔离）：`user_devices`、`change_log`、`idempotency_keys`、`tokens`。

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
| `APP_VERSION` | `0.3.1` | 健康接口与日志中展示的版本 |
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

seed 数据来源与边界：

- GATT 服务 UUID 取自 Bluetooth SIG Assigned Numbers 的标准服务（Cycling Speed and Cadence `0x1816`、Environmental Sensing `0x181A`、Heart Rate `0x180D`、Battery `0x180F`、Device Information `0x180A`、Body Composition `0x181B`、Weight Scale `0x181D`、Generic Access `0x1800`），以及 Nordic Semiconductor 官方文档中的 Nordic UART Service（`6e400001-b5a3-f393-e0a9-e50e24dcca9e`）。
- 固件版本只收录官方公开的参考版本：`nRF Connect SDK v2.9.0`、`ESP-IDF v5.2.2`；`released_at` 留空，未编造发布日期。
- 公共设备为厂商公开的开发板型号：`nRF52840 DK`、`nRF5340 DK`、`ESP32-C3-DevKitM-1`。
- 配件只放能核实名称的通用/官方在售项；具体 SKU 与量产设备固件号未编造，留空或以厂商渠道为准。

## 测试与验证

测试覆盖：

- `internal/httpapi`：httptest + Gin 表驱动；健康、公共设备、设备类型/详情/固件/配件/聚合、401/404、非法 MAC、CORS 预检、注销、push 参数校验（platform / externalId / 平台规则）、幂等重放、幂等键冲突、LWW 冲突、tombstone、changes 分页、跨用户越权 404、同用户 Android 与 iOS 双记录互不覆盖、按复合键单查（200/400/404、大小写规范化、删除后 404、跨用户 404、旧 MAC 路径兼容）、401 响应不下发新 token、限流。
- `internal/lww`：时钟兜底、服务端获胜、内容相同 noop 等判定。
- `internal/store/postgres`：`TEST_DATABASE_URL` 存在时运行真实数据库集成测试，覆盖 token 创建/校验/吊销、幂等重放与冲突、LWW、tombstone、分页、跨用户隔离、Android/iOS 复合键隔离、`GetUserDeviceByKey` 跨用户 404。
- `internal/store/postgres/migration_test.go`：临时数据库上先迁移到 version=2，写入旧 `(user_id, mac)` 记录与旧 change_log，再迁移到 version=3，验证回填、旧数据可读与 entity_id。

本机实际执行结果（2026-11-05）：

| 命令 | 结果 |
| --- | --- |
| `go build ./...` | 通过 |
| `go vet ./...` | 通过 |
| `go test ./...` | 通过（`internal/httpapi`、`internal/lww`、`internal/store/postgres` 均 ok） |
| `TEST_DATABASE_URL=... go test ./internal/store/postgres -run 'TestPostgresStoreIntegration|TestMigrationBackfillsLegacyDevices' -v` | 通过（本机真实 PostgreSQL，迁移到 version=3；含旧数据回填） |
| Postgres 模式 HTTP 验证 | 通过：health、匿名登录、公共设备、类型/详情/固件/配件、my/devices、push、幂等重放、409、LWW 冲突、key_taken、tombstone 404、跨用户 404、注销后 401、Android/iOS 双记录、旧客户端 mac-only 兼容、同复合键换 id 不重复 |
| fixture 模式新增接口验证 | 通过：`/device-types/{type}/catalog` 200 且含固件/配件、未知类型 404；`/my/devices/android/{mac}` 与 `/my/devices/ios/{uuid}` 200；非法 platform 400；旧 `/my/devices/{mac}` 兼容 |
| 401 契约验证 | 通过：无效 token 返回 401 `UNAUTHORIZED`，响应体仅有 `error`，不含 `token` / `userId` |
| 复合键数据库验证 | 通过：真实 PostgreSQL 中按 `(platform, externalId)` 查询 Android / iOS 成功，跨用户查询返回 `ErrNotFound` |
| `docker build` | 通过（GitHub Actions `docker-build` job，v0.2.0 提交，run 41881913964） |
| GitHub Actions | 通过（v0.2.0：gofmt、vet、build、单元测试、迁移、Postgres 集成测试、distroless 镜像构建；run 41881913964） |

集成测试运行方式：

```bash
TEST_DATABASE_URL='postgres://platojobs@127.0.0.1:5432/pjgoiot_test?sslmode=disable' \
  go test ./internal/store/postgres -run 'TestPostgresStoreIntegration|TestMigrationBackfillsLegacyDevices' -v
```

## 已知限制与下一步

- `/auth/login` 仍为 501，只有匿名账号；后续可加账号密码 / OAuth 与匿名账号绑定。
- token 无刷新与过期时间，仅支持吊销；如需长效会话可增加滚动续期，但必须保持「401 不自动重登」的契约。
- `/my/devices/{mac}` 仅适合 Android；iOS 使用 `/my/devices/{platform}/{externalId}` 单台查询（已提供），或使用 `/my/devices` 列表。
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
