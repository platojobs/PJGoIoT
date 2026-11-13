# PJGoIoT

「蓝牙设备数据面板」Android / iOS App 的后端服务：多用户账号、BLE 设备公共元数据、用户私有设备与增量同步。

- 语言 / 运行时：Go 1.22+
- HTTP 框架：Gin v1.10（仅 `internal/httpapi` 层依赖）
- 数据库：PostgreSQL 16（迁移 golang-migrate，查询 sqlc，不使用 ORM）
- 日志：标准库 `log/slog` 结构化日志
- 当前版本：服务 `v0.26.0`，契约 `0.21.0`，迁移 `000001`–`000020`
- 对外契约以 [`api/openapi.yaml`](api/openapi.yaml) 为准；服务版本（`APP_VERSION`）与契约版本是两套号，
  契约只增不删，变更历史见 `git log` 与 tag。

## 快速开始

### 方式一：fixture 模式（无需数据库）

```bash
make dev          # 等价于 STORE=fixture go run ./cmd/api
curl -s http://127.0.0.1:8080/api/v1/health
```

公共元数据读取 `fixtures/*.json`，用户与设备数据在内存中，进程重启即丢失。

### 方式二：Docker Compose（Caddy + API + Postgres，生产形态）

```bash
cp .env.example .env    # 至少改 POSTGRES_PASSWORD 与 DOMAIN
docker compose up -d --build
```

- **Caddy**：`https://localhost`（本地自签，`curl -k`）；80 跳 443；配置见 `deploy/Caddyfile`。
- **API**：不发布到宿主机，只能经 Caddy 访问；`/metrics` 被 Caddy 显式 404。
- **Postgres**：仅绑定 `127.0.0.1:55432`，持久卷 `pgdata`；API 等其 healthy 后启动并自动迁移。
- 备份/恢复/清理/告警/回滚见 [`docs/runbook.md`](docs/runbook.md)。

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
make migrate          # 建表 + seed
make dev-postgres     # Postgres 模式启动 API
```

不设置 `STORE` 时，`DATABASE_URL` 非空即使用 Postgres，否则用 fixture。

## 接口总览

前缀 `/api/v1`（`/healthz`、`/metrics` 在根路径）。成功返回裸 JSON；失败固定为
`{"error":{"code":"...","message":"..."}}`。字段、可空性、枚举与响应示例一律看
[`api/openapi.yaml`](api/openapi.yaml)。

| 分组 | 路径 | 说明 |
| --- | --- | --- |
| 健康 | `GET /api/v1/health`、`GET /healthz` | readiness（含 DB Ping，失败 503）/ liveness（不看 DB） |
| 账号 | `POST /auth/anonymous`、`/auth/login`(501)、`/auth/logout`、`/auth/refresh` | 匿名账号、滚动刷新 token |
| 会话与配对 | `GET /sessions`、`DELETE /sessions/{id}`、`POST /auth/pairing-code`、`POST /auth/pairing-code/claim` | 多设备登录；配对码 10 分钟一次性 |
| 账号数据 | `POST /me/deletion-request`、`DELETE /me`、`POST /me/export`、`GET /me/export/{id}[/download]` | 二次确认硬删除；异步导出 |
| 公共元数据 | `GET /devices/{mac}`、`/device-types[/{type}[/catalog|firmwares|accessories]]`、`GET /catalog/device-types` | 厂商、型号、GATT、固件、配件；目录支持 `If-None-Match`/304 |
| 我的设备 | `GET /my/devices`、`GET|PATCH /my/devices/{platform}/{externalId}`、`GET /my/devices/{mac}` | 本人 + 家庭共享；`{mac}` 仅 Android |
| 读数 | `GET /my/devices/{platform}/{externalId}/readings[/summary|series|metrics]` | 游标分页、聚合、分桶序列、指标清单 |
| 同步 | `GET /sync/changes`、`POST /sync/push` | 增量拉取；批量写设备与读数（单事务） |
| 家庭 | `/households` 及成员、房间、邀请码、转让、退出 | 成员可读，跨家庭 404 |
| 反馈 | `POST /feedback`、`GET /feedback/mine`、`GET|PATCH /feedback/{id}` | 按账号 + IP 双维度限流；运营入口 `/admin/feedback` |
| 场景 | `GET|POST /scenes`、`GET|PUT|DELETE /scenes/{id}` | 触发条件 + 动作，字段级 LWW |
| 推送 | `POST /push/devices`、`DELETE /push/devices/{deviceId}`、`POST /admin/alerts` | FCM/APNs 投递队列 |
| 目录运营 | `PUT|DELETE /admin/catalog/device-types/{type}` | admin 凭据；未配置 `ADMIN_TOKEN` 时整组 404 |

## 关键语义

### 设备唯一键

- 唯一约束 `(user_id, platform, external_id)`，主键 `(user_id, id)`，`id` 为客户端生成的 UUID。
- Android：`external_id` 为大写 MAC；iOS：`CBPeripheral.identifier`（大写 UUID）。
- `mac` 只是可选元数据，任何一端都不得把它当唯一键；同型号跨平台是两条独立记录。
- `entityId` = `<userId>:<platform>:<externalId>`，由服务端派生，客户端不要拼接或解析。
- 所有私有查询都带 `user_id`；访问他人资源统一 404。

### 同步与字段级 LWW

- `change_log(user_id, seq BIGSERIAL, entity, entity_id, op, payload, server_ts)` 是增量来源，
  `entity` 含 `user_device` / `scene` / `household` / `room`；客户端应忽略未知 entity。
- `updatedAt` 严格更新才接受；客户端时间超前服务端 2 分钟以上按服务端时间兜底（`clock_clamped`）；
  时间相同且内容一致为 `unchanged`；否则服务端获胜，落败数据进 `conflicts`
  （`server_newer` / `clock_clamped` / `key_taken`），不静默覆盖。
- 可空字段三态：**缺省 = 保持原值**、**`null` = 显式清空**、**有值 = 更新**；比较前截断到微秒。
- 删除写 tombstone（`deleted_at` + `op=delete`），列表过滤、`/sync/changes` 仍可见。
- `POST /sync/push` 单事务：设备、读数、`change_log`、幂等键要么全成功要么全回滚；
  支持 `Idempotency-Key`（同 key 同 body 回放返回历史响应，不同 body 409 `IDEMPOTENCY_CONFLICT`）。
- `GET /sync/changes`：`limit` 默认 100、最大 500；游标用 `nextCursor`；`change_log` 被清理后
  落后客户端收到 `410 RESYNC_REQUIRED`，必须清本地并从 `cursor=0` 全量重拉。

### 设备标记与在线状态

- `markedAt`（可空）：缺省保持、`null` 取消标记、RFC3339 标记；标记与取消都写 `change_log`。
- `lastSeenAt`（只读、UTC、微秒、可空）：服务端最后一次**接受**该设备读数上报的接收时间，
  `null` 表示从未上报。
- `presence`（只读三态）：`unknown` = `lastSeenAt` 为 null；`online` = `now - lastSeenAt <= 15m`；
  `offline` = 曾上报但超窗，长时间无活动自然离线，无单独下线事件。
- 活动只来自被接受的 `readings`（用服务端接收时间，不是 `recordedAt`），与读数同事务推进，
  提交后 `/my/devices` 立即可见；`presence` 在每次响应时按当前时间重算（`/sync/changes` 同样）。
- 读写都限本人设备行；客户端上报 `lastSeenAt`/`presence` 会被忽略，不参与 LWW。

### 401 与 token 生命周期

- token 为不透明串（`pjiot_` + 32 字节随机数 base64url），库里只存 SHA-256；默认 180 天。
- `POST /auth/refresh` 滚动换新并立即使旧 token 失效；`GET /sessions`、`DELETE /sessions/{id}`
  管理多设备会话。服务端不自动轮换。
- 401 表示 token 缺失/无效/已吊销：客户端应提示重新登录并保留待同步数据，
  **不得自动调用 `/auth/anonymous`**（会换 `userId`，导致设备与游标失联）。

### 数据保留

`cmd/cleanup` 清理读数（默认 90 天）、`change_log`（90 天）、幂等键（7 天）、过期 token，
以及推送投递（7 天）与失效设备（30 天）。API 进程内不做定时清理，生产必须安装
systemd timer / cron，否则数据无限增长（见 runbook）。

## 数据库迁移

SQL 内嵌进二进制，镜像无需额外文件：

```bash
make migrate                                  # up 到最新
make migrate-version                          # 查看版本
make migrate-down                             # 回滚全部（会删表）
go run ./cmd/migrate -direction up -steps 1   # 只前进一步
```

| 版本 | 主题 |
| --- | --- |
| 000001–000002 | 建表 + 公共元数据 seed |
| 000003 | 设备唯一键改为 `(platform, external_id)`，回填旧记录与 `change_log` |
| 000004–000005 | 元数据字段扩展、`advertisedName` 与 `group` 语义 |
| 000006–000007 | `readings`、一次性配对码 |
| 000008–000009 | token 过期与清理索引、MAC 大小写回填修正 |
| 000010 | 共享限流桶 `rate_limit_buckets` |
| 000011–000012 | 家庭/成员/房间/邀请码，及邀请码管理 |
| 000013–000014 | 反馈与状态流转、审计 |
| 000015 | 设备类型目录与版本 |
| 000016 | 账号删除令牌与导出任务 |
| 000017 | 场景上云 |
| 000018 | 推送设备与告警投递队列 |
| 000019 | `user_devices.marked_at` |
| 000020 | `user_devices.last_seen_at`（在线状态） |

所有迁移 up/down 成对；`scripts/migrate-drill.sh` 做空库、历史库、down→up 三场景演练。
seed 只收录可核实的公开数据（Bluetooth SIG GATT、厂商开发板与公开固件版本），不编造 SKU 与发布日期。

## 测试与 CI

```bash
make lint                                                     # gofmt + go vet
TEST_DATABASE_URL='postgres://platojobs@127.0.0.1:5432/pjgoiot_test?sslmode=disable' go test ./...
npx --yes @redocly/cli@latest lint api/openapi.yaml           # 契约 0 error
bash scripts/stack-for-e2e.sh && bash scripts/seed-for-e2e.sh # 真实栈 e2e（需同一 shell）
```

CI 四个 job：`构建与测试`（含真实 PG 集成与契约校验）、`配置与脚本校验`、`压测门槛（smoke）`、`构建镜像`。

## 部署与运维

compose 默认两个 API 实例 `api1`/`api2` 共享同一 Postgres，Caddy 轮询并主动健康检查；
响应头 `X-Instance` 标识实例（非业务契约）。所有状态在 Postgres，任意实例可处理任意请求。

```bash
bash scripts/rolling-restart.sh   # 逐实例摘流 → 重启 → 等健康 → 归还流量
```

- 迁移采用前滚设计，两实例同时启动由 migrate advisory lock 串行化。
- 限流默认进程内（多实例 = N 倍额度）；需要全局额度时设 `RATE_LIMIT_BACKEND=postgres`
  （要求 `STORE=postgres`，每请求一次 UPSERT，DB 故障时降级本地并打 warn）。
- 观测：`/metrics`（请求量/延迟/DB 错误/连接池/近似行数）、`X-Request-Id`、
  `deploy/prometheus/alerts.yml` 9 条规则（含 3 条 SLO：可用性 99.5%、p95 延迟、错误预算）。
- 备份 `deploy/scripts/backup.sh`、恢复 `restore.sh`、清理 `cmd/cleanup`；安装与回滚步骤见
  [`docs/runbook.md`](docs/runbook.md)。

### tag 纪律

已推送的 tag 禁止移动、删除或强制重指。发布后发现问题就打递增版本（如 `v0.13.1`），
旧 tag 保持原样；只在 tag 尚未推送时允许本地重打。发布的 tag 必须是 CI 全绿的提交，
用 annotated tag 打在 `main` 上，message 写版本与主题。

## 压测与验证结果

本机 M1 Max / 32 GB / PostgreSQL 16，单机回环，数字是单机天花板。原始数据、复现命令与结论见
[`docs/perf-results/summary.md`](docs/perf-results/summary.md)，完整基线见
[`docs/perf-baseline.md`](docs/perf-baseline.md)。

| 场景 | 结果 |
| --- | --- |
| `/sync/changes` 108 万行、16 家庭 | floor p95 0.87ms / 54 buffers；主查询 cursor=0 p95 4.18ms / 121 buffers |
| `/sync/changes` 可见性下限（111 万行） | 改造后 p95 0.56ms、10 buffers（改造前 66.28ms、87,928 buffers） |
| `GET/PUT /scenes` 各 10 分钟、40 并发 | p95 20ms / 37ms，0 错误、全 200；连接池 20/20 饱和但无 503 |
| `POST /sync/push` 拐点 | 单用户 50 并发可用；5 用户合计约 158.9 RPS |
| 备份恢复 | 138MB 库 dump 9.96s、restore 15.40s，15 张表校验和一致 |

标记并发、在线状态等行为验证见同一份 summary；上线阻塞项见 [`docs/preflight.md`](docs/preflight.md)。

## 已知限制

- 场景已上云，但两端 App 仍以本地 SQLite/Room 为执行源；首次同步、冲突合并、`deviceMissing` UI 未在真实客户端验证。
- **真实 FCM/APNs 投递与真机收通知未验证**（无凭据/设备）；无凭据时只做 dry-run。若产品承诺离线提醒，需补齐凭据。
- `ADMIN_TOKEN` 支持当前 + 上一把密钥的窗口化轮换，但密钥下发与吊销仍依赖部署侧环境变量。
- 真实域名/ACME、Alertmanager 生产部署、systemd/cron 生产安装、真机与商店上架均未执行。
- 两端 App 尚未接入 `/auth/refresh`，也未切到 `/scenes`、`/catalog/device-types`。
- 家庭结构变更（`household`/`room`）按成员逐个写 `change_log`，写入量随成员数放大；
  邀请码领取等成员变动需客户端主动拉 `/households*` 兜底。
- 两位成员扫描同一台物理设备会产生两条独立记录，不做跨账号设备指纹合并。
- `cmd/loadgen` 的 changes/scenes/feedback/catalog 场景未纳入 CI `-smoke`（短跑造数据、缺阈值）。
- `/metrics` 的业务计数是 `pg_class.reltuples` 近似值，不能用于计费或审计。
- 公共元数据（设备类型、固件、配件）列表不分页，按全量返回。
- 读数查询要求设备已存在；隐式设备写入的读数在注册前不可查。
- `scripts/stack-for-e2e.sh` 绑 `127.0.0.1`，覆盖模拟器；真机经局域网访问未验证。
- `/auth/login` 仍为 501，只支持匿名账号。

## 目录结构

```
PJGoIoT/
├── api/openapi.yaml              # 对外契约（唯一权威）
├── cmd/{api,cleanup,migrate,loadgen,alert-webhook}/
├── db/migrations/                # 000001–000020，go:embed
├── db/queries/                   # sqlc 查询
├── deploy/                       # Caddyfile、prometheus、alertmanager、grafana、systemd、cron、备份脚本
├── docs/                         # runbook / preflight / perf 摘要与基线
├── fixtures/                     # fixture 模式公共元数据
├── internal/
│   ├── httpapi/                  # Gin 路由、中间件、handler、校验、错误出口
│   ├── store/                    # Store 接口 + fixture / postgres 实现
│   ├── lww/                      # LWW 判定
│   ├── model/                    # 对外 JSON 领域模型
│   └── auth/ config/ migrate/ validate/
├── scripts/                      # 迁移演练、e2e 栈、滚动发布、限流/告警/压测校验
├── Dockerfile                    # 多阶段 + distroless nonroot
└── docker-compose.yml
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
| `RATE_LIMIT_BACKEND` | `memory` | `memory`（进程内）或 `postgres`（跨实例共享；要求 `STORE=postgres`） |
| `SHUTDOWN_DRAIN_SECONDS` | `5` | SIGTERM 后先置 readiness 503、继续服务的时间（compose 默认 8s） |
| `INSTANCE_ID` | 空 | 写入响应头 `X-Instance` |
| `PAIRING_CLAIM_RATE_PER_MINUTE` | `20` | 配对码领取每 IP 令牌数；`<=0` 关闭 |
| `PAIRING_CLAIM_BURST` | `10` | 配对码领取令牌桶容量 |
| `FEEDBACK_USER_RATE_PER_MINUTE` | `10` | 反馈提交按账号令牌数 |
| `FEEDBACK_USER_BURST` | `5` | 反馈提交按账号桶容量 |
| `FEEDBACK_IP_RATE_PER_MINUTE` | `30` | 反馈提交按 IP 令牌数 |
| `FEEDBACK_IP_BURST` | `10` | 反馈提交按 IP 桶容量 |
| `ADMIN_TOKEN` | 空 | 运营接口凭据（`X-Admin-Token`）；为空整组 404 |
| `ADMIN_TOKEN_PREVIOUS` | 空 | 轮换期间的上一把密钥；需同时设 `ADMIN_TOKEN_ROTATED_AT` |
| `ADMIN_TOKEN_ROTATED_AT` | 空 | 新密钥生效时间（RFC3339，建议 UTC） |
| `ADMIN_TOKEN_ROTATION_WINDOW_SECONDS` | `86400` | 上一把密钥保留窗口（秒） |
| `METRICS_ENABLED` | `true` | 是否注册 `/metrics` |
| `RUN_MIGRATIONS_ON_BOOT` | `true` | postgres 模式启动时迁移；失败非零退出 |
| `CORS_ALLOWED_ORIGINS` | `*` | 逗号分隔来源 |
| `LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error` |
| `TOKEN_TTL_DAYS` | `180` | token 有效期（天） |
| `APP_VERSION` | `0.26.0` | 服务版本（健康接口与日志）；契约版本见 openapi |
| `DB_MAX_OPEN_CONNS` | `20` | 连接池上限；过小会在超时预算后返回 503 `TIMEOUT` |
| `DB_MAX_IDLE_CONNS` | `10` | 连接池最大空闲连接数 |
| `HEALTHCHECK_URL` | 自动 | `-healthcheck` 模式请求的 URL |
| `TEST_DATABASE_URL` | 空 | 设置后运行 Postgres 集成测试 |
| `READING_RETENTION_DAYS` | `90` | 仅 cleanup：读数保留天数 |
| `CHANGE_LOG_RETENTION_DAYS` | `90` | 仅 cleanup：变更日志保留天数 |
| `IDEMPOTENCY_RETENTION_DAYS` | `7` | 仅 cleanup：幂等键保留天数 |
| `PUSH_WORKER_ENABLED` | `true` | postgres 模式是否启动投递 worker |
| `PUSH_WORKER_INTERVAL_SECONDS` | `5` | worker 轮询间隔 |
| `PUSH_WORKER_BATCH` | `50` | 每轮抢占条数（1–500） |
| `PUSH_MAX_ATTEMPTS` | `5` | 可重试错误最大尝试次数（1–20） |
| `PUSH_BACKOFF_BASE_SECONDS` | `30` | 退避基数 `base × 2^(attempt-1)`，上限 30 分钟 |
| `PUSH_SENDING_STALE_SECONDS` | `300` | `sending` 超时回收（≥60） |
| `FCM_SERVER_KEY` | 空 | FCM legacy key；为空 Android dry-run |
| `FCM_ENDPOINT` | 空 | 覆盖 FCM 地址（测试/代理） |
| `APNS_KEY_FILE` / `APNS_KEY_PEM` | 空 | APNs `.p8` 路径或内联 PEM；缺失 iOS dry-run |
| `APNS_KEY_ID` / `APNS_TEAM_ID` / `APNS_TOPIC` | 空 | APNs Key ID / Team ID / Bundle ID；任一缺失即 dry-run |
| `APNS_ENDPOINT` | 空 | 覆盖 APNs 地址（默认正式环境，可指 sandbox） |
| `PUSH_DELIVERY_RETENTION_DAYS` | `7` | 仅 cleanup：推送投递保留天数 |
| `PUSH_DEVICE_RETENTION_DAYS` | `30` | 仅 cleanup：失效推送设备保留天数 |

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
