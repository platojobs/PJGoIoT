# PJGoIoT 后端

「蓝牙设备数据面板」Android / iOS App 的后端服务：多用户账号、BLE 设备公共元数据、用户私有设备与增量同步。

- 语言 / 运行时：Go 1.22+
- HTTP 框架：Gin v1.10（仅 `internal/httpapi` 层依赖）
- 数据库：PostgreSQL 16（迁移 golang-migrate，查询 sqlc，不使用 ORM）
- 日志：标准库 `log/slog` 结构化日志
- 契约：[`api/openapi.yaml`](api/openapi.yaml)，契约版本 **0.21.0**（v0.26.0 新增设备在线状态 `lastSeenAt` / `presence`）；服务版本 **v0.26.0**（设备在线状态契约落地）
  - 说明：`APP_VERSION` / README 的 **服务版本**与 OpenAPI 的 **契约版本**是两套号。v0.7.0/v0.7.1 只增加运维形态（`/healthz`、编排、备份脚本），不改业务契约，契约保持 0.6.0；v0.8.0 只新增响应字段（`expiresAt`）与新增错误码（`RESYNC_REQUIRED`、`TIMEOUT`），按“只加不删”原则升到 0.7.0；v0.9.0 不改业务契约（`/metrics` 新增 `route` 标签属运维观测面），契约保持 **0.7.0**；v0.10.0 只在 push 描述中补充事务/400/503 语义与 503 响应示例；v0.11.0 只增加部署形态与运维响应头（`X-Instance`）与可选共享限流，契约保持 **0.7.0**；v0.12.0 新增家庭、成员、房间、邀请码接口与设备芯片字段，契约升到 **0.8.0**；v0.13.0 补齐成员移除/退出、邀请码管理与房间整段重排，`Household` 增加 `roomCount`，契约升到 **0.9.0**；v0.14.0 新增 `POST /households/{id}/transfer-ownership`、`HouseholdMember.isSelf` 与房间重排的 `ROOM_SET_MISMATCH`，契约升到 **0.10.0**（无 schema 迁移）；v0.15.0 把家庭/房间结构变更写入 `change_log`（新增 `entity=household` / `room` 及对应 payload），契约升到 **0.11.0**（无 schema 迁移）；v0.16.0 新增意见反馈提交与本人列表、`FEEDBACK_RATE_LIMITED` 错误码，契约升到 **0.12.0**（新增迁移 `000013_feedback`）；v0.17.0 新增反馈详情、用户关闭、运营受理入口与状态机、`FEEDBACK_INVALID_TRANSITION` 错误码，契约升到 **0.13.0**（新增迁移 `000014_feedback_status_workflow`）；v0.18.0 新增设备类型目录 `GET /catalog/device-types` 与运营侧 PUT/DELETE、ETag/304 协商缓存，契约升到 **0.14.0**（新增迁移 `000015_device_type_catalog`）；v0.19.0 新增 `POST /me/deletion-request` + `DELETE /me`（一次性令牌二次确认、单事务硬删除、owner 有成员 409）与异步个人数据导出 `POST /me/export`、`GET /me/export/{id}`、一次性下载链接，契约升到 **0.15.0**（新增迁移 `000016_account_deletion_export`）；v0.20.0 把客户端本地场景（触发条件 + 动作）搬到服务端，新增 `GET|POST /scenes`、`GET|PUT|DELETE /scenes/{sceneId}`、字段级 LWW 更新、`change_log` 的 `scene` entity 以及场景纳入账号删除与导出，契约升到 **0.16.0**（新增迁移 `000017_scenes`）。v0.21.0 **不做功能变更**，只做压测容量基线（[`docs/perf-baseline.md`](docs/perf-baseline.md)）与故障恢复 / 上线前清单演练（[`docs/preflight.md`](docs/preflight.md)），契约保持 **0.16.0**。v0.22.0 修复清单中三项实证问题：`/sync/changes` 可见性下限拆成索引探测（p95 66.28ms → 0.56ms）、导出任务启动对账（`failed` + `failureReason`）、token 滚动刷新与会话吊销（`POST /auth/refresh`、`GET /sessions`、`DELETE /sessions/{id}`），契约升到 **0.17.0**（无 schema 迁移）。v0.23.0 打通服务端推送（`POST /push/devices`、`DELETE /push/devices/{deviceId}`、告警同事务入队、后台 worker、FCM/APNs 适配层与无凭据 dry-run）、`GET /scenes` 可选 `limit`/`cursor`（不传参数行为不变）、Alertmanager 与 webhook 接收端、备份/清理的 systemd timer 与容器 cron；契约升到 **0.18.0**（新增迁移 `000018_push_and_alerts`）。v0.24.0 做三项产品化：`/sync/changes` 可见性下限与主查询改为按分支索引探测 + `DISTINCT ON`（多家庭 16 户 p95 < 1.1ms，主查询 cursor=0 p95 104.9ms → 2.4ms，见 [`docs/perf-baseline.md`](docs/perf-baseline.md)）、场景读/写长稳压测脚本与结果（[`docs/perf-results/`](docs/perf-results/)）、admin 双密钥轮换（`ADMIN_TOKEN_PREVIOUS` + 轮换窗口，到期自动淘汰旧密钥）；契约升到 **0.19.0**（无 schema 迁移）。v0.25.0 把「设备标记」做成正式同步字段：`UserDevice` 新增可空 `markedAt`，`POST /sync/push` 设备支持「缺省=保持 / null=取消标记 / RFC3339=标记」，标记与取消都写 `change_log` 并按微秒截断参与字段级 LWW；契约升到 **0.20.0**（新增迁移 `000019_device_marked_at`）。v0.25.1 **不做功能变更**：用真实 HTTP + 真实 Postgres 验证两个客户端并发操作同一设备（微秒/毫秒交错、null 与缺省交错、幂等回放、`/sync/changes` 游标推进），结果见 [`docs/perf-results/v0.25.1-marked-concurrency.md`](docs/perf-results/v0.25.1-marked-concurrency.md)；契约保持 **0.20.0**，无 schema 迁移。v0.26.0 补齐设备在线状态的契约缺口：`UserDevice` 新增只读 `lastSeenAt`（UTC、微秒、可空）与三态 `presence`（`unknown`/`online`/`offline`），由服务端在接受的读数上报时维护、按固定 15 分钟窗口实时推导，并随 `change_log` 增量同步；`lastSeenAt=null`（从未上报）与超窗 `offline` 可区分；契约升到 **0.21.0**（新增迁移 `000020_device_presence`）。`/healthz` 见下文「部署编排」。

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
| POST | `/auth/refresh` | Bearer | 200 | 滚动刷新 token；旧 token 立即失效，返回新 `token` 与 `expiresAt` |
| GET | `/sessions` | Bearer | 200 | 列出当前账号有效登录会话；`current=true` 标记本次会话 |
| DELETE | `/sessions/{id}` | Bearer | 204 | 吊销指定会话；跨用户/不存在 404，重复吊销幂等 |
| POST | `/auth/pairing-code` | Bearer | 201 | 生成一次性配对码，`{code,expiresAt}`；10 分钟有效，新码使旧码失效 |
| POST | `/auth/pairing-code/claim` | 否 | 200 | 用配对码换取**同一 userId** 的新 token，`{userId,token}`；按 IP 独立限流 |
| POST | `/me/deletion-request` | Bearer | 201 | 签发一次性删除令牌 `{token,expiresAt}`；10 分钟有效，只存哈希，再次申请作废旧令牌 |
| DELETE | `/me` | `X-Deletion-Token` | 204 | 二次确认后硬删除账号及全部个人数据；令牌缺失 400、错误 403、过期 410、owner 有成员 409、重放 404 |
| POST | `/me/export` | Bearer | 202 | 创建异步导出任务，返回任务 id；客户端轮询状态 |
| GET | `/me/export/{id}` | Bearer | 200 | 查询导出状态；ready 时返回 10 分钟一次性下载链接 |
| GET | `/me/export/{id}/download` | 下载令牌 | 200 | 下载导出 JSON；一次性，过期/已用 410，令牌错误 403 |
| GET | `/devices/{mac}` | 否 | 200 | 公共设备元数据（厂商、型号、固件、GATT、兼容性） |
| GET | `/device-types` | 否 | 200 | 设备类型列表，`{"items":[...]}` |
| GET | `/device-types/{type}` | 否 | 200 | 按类型查询设备类型元数据，供 iOS 等无法获取 MAC 的端使用 |
| GET | `/device-types/{type}/catalog` | 否 | 200 | 按类型聚合元数据、固件、配件，`{deviceType,firmwares,accessories}` |
| GET | `/device-types/{type}/firmwares` | 否 | 200 | 指定类型的固件列表（历史端点，保持可用） |
| GET | `/device-types/{type}/accessories` | 否 | 200 | 指定类型的配件列表（历史端点，保持可用） |
| GET | `/my/devices` | Bearer | 200 | 本人 + 所在家庭共享的设备列表 |
| GET | `/my/devices/{mac}` | Bearer | 200 | 按 MAC 元数据查单台设备（仅 Android；iOS 用下一行接口） |
| GET | `/my/devices/{platform}/{externalId}` | Bearer | 200 | 按设备唯一键查单台设备，iOS 无需 MAC |
| PATCH | `/my/devices/{platform}/{externalId}` | Bearer | 200 | 改 `alias`/`roomId`/`householdId`（仅设备 owner；三态语义） |
| GET | `/my/devices/{platform}/{externalId}/readings` | Bearer | 200 | 读数列表，`recordedAt` 倒序游标分页，`{items,nextCursor,hasMore}` |
| GET | `/my/devices/{platform}/{externalId}/readings/summary` | Bearer | 200 | 单指标聚合，`{metric,unit,count,min,max,avg,last,windowStart,windowEnd}` |
| GET | `/my/devices/{platform}/{externalId}/readings/series` | Bearer | 200 | UTC 对齐分桶序列，`{metric,unit,bucket,from,to,buckets[]}` |
| GET | `/my/devices/{platform}/{externalId}/readings/metrics` | Bearer | 200 | 指标清单，`{items:[{metric,unit,count,firstAt,lastAt}]}` |
| GET | `/sync/changes?cursor=&limit=` | Bearer | 200 | 增量变更，`{changes,nextCursor,hasMore}`；范围=本人+家庭 |
| POST | `/sync/push` | Bearer | 200 | 批量写入设备与读数，`{applied,conflicts,nextCursor,appliedReadings,skippedReadings}` |
| POST | `/feedback` | Bearer | 201 / 200 | 提交意见反馈；同内容重复提交返回同一 `feedbackId`（200 重放）；超限 429 `FEEDBACK_RATE_LIMITED` |
| GET | `/feedback/mine?cursor=&limit=` | Bearer | 200 | 本人反馈列表，`{items,nextCursor,hasMore}`；只含本人数据，不含 `contact`/`diagnostics` |
| GET | `/households` | Bearer | 200 | 我所在的家庭列表（含角色/成员数/设备数/房间数） |
| POST | `/households` | Bearer | 201 | 创建家庭，创建者为 owner |
| GET | `/households/{id}` | Bearer | 200 | 家庭详情（成员可读，跨家庭 404） |
| PATCH | `/households/{id}` | Bearer | 200 | 改家庭名（仅 owner，member 403） |
| DELETE | `/households/{id}` | Bearer | 204 | 删家庭（仅 owner；个人家庭 403；设备回个人家庭） |
| GET | `/households/{id}/members` | Bearer | 200 | 成员列表（成员可读；每行含 `isSelf` 标记当前账号本人） |
| DELETE | `/households/{id}/members/{userId}` | Bearer | 204 | 移除成员（仅 owner；不能移除自己/唯一成员；设备回该成员个人家庭） |
| POST | `/households/{id}/leave` | Bearer | 204 | 成员退出；owner 有其他成员 409，个人家庭 400，owner 独退删家庭 |
| POST | `/households/{id}/transfer-ownership` | Bearer | 200 | 转让所有权（仅 owner；目标是本家庭成员的 `userId`，非成员 404，个人家庭 400） |
| POST | `/households/{id}/invitations` | Bearer | 201 | 签发一次性邀请码（仅 owner；TTL 10 分钟） |
| GET | `/households/{id}/invitations` | Bearer | 200 | 邀请码管理列表（仅 owner；只有哈希前缀/状态/时间，绝不返回明文） |
| DELETE | `/households/{id}/invitations/{invitationId}` | Bearer | 204 | 撤销未使用的邀请码（仅 owner） |
| POST | `/households/invitations/claim` | Bearer | 200 | 用邀请码加入家庭；无效/过期/已用 404 |
| GET | `/households/{id}/rooms` | Bearer | 200 | 房间列表（成员可读） |
| POST | `/households/{id}/rooms` | Bearer | 201 | 创建房间（仅 owner） |
| PUT | `/households/{id}/rooms/order` | Bearer | 200 | 整段重排（仅 owner；必须覆盖全部房间，原子写 0..n-1，集合不一致 409） |
| PATCH | `/households/{id}/rooms/{roomId}` | Bearer | 200 | 改房间名/排序（仅 owner） |
| DELETE | `/households/{id}/rooms/{roomId}` | Bearer | 204 | 删房间（仅 owner；设备 `roomId` 置 null） |
| GET | `/metrics`（根路径） | 否 | 200 | Prometheus 文本指标；不受限流，`METRICS_ENABLED=false` 时 404 |

### 家庭、房间与成员（v0.12.0 引入，v0.13.0 补齐管理与排序，v0.14.0 补所有权转让，v0.15.0 纳入增量同步）

对齐米家的「账号 → 家庭 → 房间 → 设备」结构：设备仍由写入它的账号拥有，家庭只决定可见范围与房间归类。

**数据模型与回填**（迁移 `000011_households` 建表并回填；`000012_household_invitation_management` 为邀请码管理补 `id`/索引，均含 down 脚本）：

| 表 | 关键字段 | 说明 |
| --- | --- | --- |
| `households` | `id`、`name`、`owner_user_id`、`is_personal`、`created_at` | 每个账号自动一个 `is_personal=true` 的个人家庭；部分唯一索引保证每账号至多一个 |
| `household_members` | `household_id`、`user_id`、`role(owner/member)`、`joined_at` | 主键 `(household_id,user_id)`，owner 也是成员行 |
| `household_rooms` | `id`、`household_id`、`name`、`sort_order`、`created_at` | 删除房间不影响设备 |
| `household_invitations` | `code_hash`(PK)、`id`、`household_id`、`invited_by_user_id`、`attempts`、`expires_at`、`created_at` | 只存哈希；`household_id=NULL` 的行只用于猜码失败计数；`id`（迁移 `000012`）供管理端撤销，不参与领取逻辑 |
| `user_devices` | 新增 `household_id`、`room_id`（均可空，`ON DELETE SET NULL`） | 旧设备回填到各自账号的个人家庭，`room_id` 保持 NULL（未分配） |
| `change_log` | 新增 `household_id` | 旧变更回填到写入者个人家庭 |

迁移对既有数据自动回填：为每个 `users` 行插入「我的家」个人家庭与 owner 成员行，再把该账号的设备与历史变更归入该家庭。
已验证 `up → down → up`：down 后 4 张家庭表与 `user_devices` 的 2 个新列全部移除，再 up 后回填结果一致。

**角色权限矩阵**（跨家庭访问一律 **404**，不暴露资源是否存在）：

| 操作 | owner | member | 非成员 |
| --- | --- | --- | --- |
| 读取家庭/成员/房间 | 200 | 200 | 404 |
| 读取家庭内全部设备与读数 | 200 | 200 | 404 |
| 修改家庭名、删除家庭 | 200/204 | **403** | 404 |
| 签发邀请码 | 201 | **403** | 404 |
| 列出/撤销邀请码 | 200/204 | **403** | 404 |
| 创建/修改/删除房间 | 200/201/204 | **403** | 404 |
| 整段重排房间 | 200 | **403** | 404 |
| 移除成员 | 204 | **403**（含移除自己） | 404 |
| 转让所有权 | 200 | **403** | 404 |
| 退出家庭 | 见下节 | 204 | 404 |
| 修改**自己的**设备 alias/room/household | 200 | 200 | — |
| 修改**他人**的设备 | 404（设备不属于自己） | 404 | 404 |
| 删除个人家庭 | **403**（个人家庭不可删） | — | — |

**成员移除与退出规则**：

| 操作 | 调用者 | 结果 |
| --- | --- | --- |
| `DELETE /households/{id}/members/{userId}` | owner 移除他人 | 204；该成员设备回到其个人家庭、`room_id` 置 NULL |
| 同上 | owner 移除自己 | **400**（`BAD_REQUEST`，不能用移除接口退群，请走 leave） |
| 同上 | member 移除任何人（含自己） | **403** |
| 同上 | 目标不是该家庭成员 | **404** |
| 同上 | 家庭是 `is_personal=true` | **400**（个人家庭不可变） |
| `POST /households/{id}/leave` | member 自己退出 | 204；设备回个人家庭；成员行删除 |
| 同上 | owner 且还有其他成员 | **409**（`CONFLICT`，message 明确为「家庭还有其他成员，请先转让所有权或移除其他成员后再退出」） |
| 同上 | owner 且是唯一成员 | 204；整个非个人家庭被删除（成员/房间/邀请码级联） |
| 同上 | 退出个人家庭 | **400**（个人家庭不可退出） |
| 同上 | 非成员 | **404** |

- 退出/移除都把设备归还到**该成员自己的**个人家庭，而不是留在原家庭；设备与读数不删除。
- 错误响应沿用统一结构，`409` 的 `message` 为可读中文，客户端可直接展示。

**所有权转让（v0.14.0）**：

- `POST /households/{id}/transfer-ownership`，body `{"userId":"<目标成员>"}`；仅 owner 可调用。
- 事务内一次性完成：原 owner 降为 `member`、目标升为 `owner`、`households.owner_user_id` 同步更新，
  提交前对 `households` 行加 `FOR UPDATE` 行锁，保证任意时刻**恰好一个 owner**（不会出现两个 owner 或零 owner）。
- **并发**：多笔同时转让被行锁串行化，只有第一笔成功；后到者角色已变，返回 **403**。
- 错误语义：目标不是该家庭成员 → **404**（与跨家庭 404 一致，不泄露用户是否存在）；
  目标是自己 → **400**；个人家庭 → **400**；非 owner 调用 → **403**。
- 转让后原 owner 成为普通成员：读接口仍 200，结构性写（改名/删家庭/建房/签发邀请码/移除成员）一律 403，
  并可正常 `POST /households/{id}/leave` 退出。

**owner 想退出时的产品流程**（务必按此顺序，避免 409）：

1. `GET /households/{id}/members` 选一个成员；
2. `POST /households/{id}/transfer-ownership` 转让；
3. 再以普通成员身份 `POST /households/{id}/leave`。

若家庭里只有 owner 一人，直接 `leave` 即可（家庭会被删除）；若不想转让，就先
`DELETE /households/{id}/members/{userId}` 把其他成员全部移除，再 `leave`。


**邀请码安全设计**：
- 8 位、32 字符表（去掉 `0/O/1/I`），约 40 bit 熵；与配对码复用同一生成/规范化/哈希实现；
- **只存 SHA-256 哈希**，明文仅在 `201` 响应中出现一次；日志只允许输出前 2 位掩码；
- **一次性**：领取成功即删除该行，重放走「不存在」分支返回 404；TTL 10 分钟；
- 同一码累计 **5 次失败**即作废（对不存在的码也落库计数，防止无限猜码）；
- 领取接口复用现有每 IP 限流中间件（`429`），签发接口也受限流；
- 签发新码不会使旧码失效，旧码仍按其自身 TTL 到期；加入家庭是幂等的（已是成员重复领取返回 200）。

**邀请码管理**（仅 owner）：

- `GET /households/{id}/invitations` 返回 `{items:[...]}`，每行：`id`、`codeHashPrefix`（SHA-256 哈希前 8 位）、`status`（`active` / `expired` / `locked`）、`attempts`、`expiresAt`、`createdAt`；按 `createdAt` 倒序。
- **绝不返回明文**，也不返回完整哈希；`codeHashPrefix` 只用于运维侧在日志/工单中对应到某一批次，无法还原码。
- `status` 是读取时按当前时间计算的：`expiresAt` 已过 → `expired`；`attempts >= 5` → `locked`；否则 `active`。已领用的码在领取成功时即被删除，不会出现在列表里。
- `DELETE /households/{id}/invitations/{invitationId}` 物理删除该行（撤销未使用的码），返回 204；对已不存在/其他家庭的 `id` 返回 404；已被领取的码撤销同样是 404。
- `household_id=NULL` 的猜码失败计数行不会出现在列表里。

**房间排序与重排冲突**：

- `PATCH /households/{id}/rooms/{roomId}` 支持 `name` 与 `sortOrder`（单房间增量更新）；`sortOrder` 非负整数，越界 400。
- 推荐客户端使用 **整段顺序提交**：`PUT /households/{id}/rooms/order`，body `{"roomIds":["<roomA>","<roomB>",...]}`，服务端在一个事务里按数组下标写 `sort_order = 0..n-1`。
- 冲突处理采用「整表快照 + 严格校验」：请求必须**恰好覆盖该家庭的全部房间**，缺项、多项、重复 id 一律 **409**，响应体 `error.code` 为可机读的 **`ROOM_SET_MISMATCH`**（v0.14.0 起），`message` 仅作展示；客户端按 code 判断后重新 `GET /households/{id}/rooms` 再整段重提，不要解析 message 文本。
- 事务内先 `SELECT ... FOR UPDATE` 锁定该家庭房间行，再逐行更新，因此并发重排不会产生交错序号；两个并发请求按行锁串行，后提交者覆盖前者的顺序（last write wins），不会出现同一序号两条房间。
- 单房间 `PATCH sortOrder` 只改一行，不做整段重排；并发插入/拖拽时仍建议改用 `PUT .../rooms/order`，避免多个客户端各自算出的序号互相插空。
- 家庭列表 `GET /households` 的 `roomCount` 与 `GET /households/{id}/rooms` 同步变化，客户端首页刷新即可看到。

**删除家庭的级联行为**（仅 owner、需客户端二次确认）：
- 成员行、房间、邀请码随家庭级联删除；
- 家庭内每台设备回到**其各自 owner** 的个人家庭，`room_id` 置 NULL，设备与读数本身不删除；
- 家庭内的设备历史变更保留，但 `household_id` 置 NULL，原成员不再能看到；结构变更行按接收者各自保留（含一条 `household delete`）。
- 个人家庭不可删除（403）。

**家庭计数**：`GET /households` 的每个家庭返回 `memberCount`（含 owner）、`deviceCount`（该家庭内 `household_id` 指向它的设备数）、`roomCount`（房间数），供客户端首页直接展示，无需再逐个家庭拉详情。计数为读取时实时统计，不做缓存。

**同步可见范围的取舍与已知限制**：
- `GET /sync/changes` 的可见范围从「本人」扩展为「本人 + 本人所在家庭」，`seq` 仍全局单调递增；
  `entityId` 仍由设备 owner 的 userId 派生，因此同一条共享变更对不同成员呈现相同的 `entityId`。
- `POST /sync/push` **只匹配并写入调用者自己的设备行**，不会改写其他成员的记录，因此不存在“两个成员同时改同一设备互相覆盖”；
  代价是：若两位成员各自扫描同一台物理设备，会产生两条独立记录（已知限制，后续可按外部设备指纹合并）。
- 设备 alias/room/household 的修改通过 `PATCH /my/devices/{platform}/{externalId}` 会写入 `change_log`，家庭成员可见。
- 家庭内设备视为共享**只读**：成员可看、可读读数，但不能编辑他人设备（404），不能管理家庭结构（403）。

**家庭结构变更进入增量同步（v0.15.0）**：

| entity | entityId | op | payload |
| --- | --- | --- | --- |
| `household` | `<householdId>` | `upsert` / `delete` | `id`、`name`、`ownerUserId`、`role`（**接收者本人**角色）、`memberCount`、`deleted` |
| `room` | `<roomId>` | `upsert` / `delete` | `id`、`householdId`、`name`、`sortOrder`、`deleted` |

- 写入范围：创建家庭、改名/删除家庭、创建/改名/删除/重排房间、成员加入/移除/退出、所有权转让，全部与业务写入在**同一事务**内提交。
  整段重排会为每间房各发一条 `upsert`；删除家庭只发一条 `household delete`（成员/房间/邀请码随之消失，客户端整段移除即可）。
  设备归属变更复用已有的 `user_device` 变更，**不重复写**结构变更。
- **可见性（按「当时属于该家庭」）**：结构变更按写入时刻的成员集合**逐人**各写一行（`user_id=接收者`、`household_id` 留空）。因此
  成员被移除/退出后不再收到离开之后的结构变更；已经写入的历史结构行也不会因为退出而消失（不会误报 410）；
  非成员完全看不到。
- **不回放历史**：新成员加入后不会收到加入之前的结构变更，首次基线请用 `GET /households`（全量）。
  这一点与设备变更不同（设备变更由 `household_id` 共享可见），是刻意取舍：结构变更只在“发生那一刻”推给当时在册的人。
- **向后兼容**：客户端必须忽略未知 `entity` 并照常推进 `nextCursor`；payload 字段只加不删。
- **90 天保留与 410**：结构变更同样受 `change_log` 保留期约束，`cursor` 早于最老 `seq` 时返回 `410 RESYNC_REQUIRED`。

### 意见反馈（v0.16.0 提交与列表，v0.17.0 状态流转）

客户端此前只能把反馈写入本地待发队列并标注「未发送」；v0.16.0 补上服务端接收与本人查询，
v0.17.0 再补上详情、用户关闭与运营受理，使 `submitted → received → closed` 有真实路径。

**提交 `POST /feedback`**：

```json
{
  "kind": "bug",
  "body": "添加设备后偶发闪退",
  "contact": "user@example.com",
  "context": {
    "platform": "android",
    "appVersion": "1.2.3",
    "buildType": "release",
    "osVersion": "Android 14",
    "deviceModel": "Pixel 7"
  },
  "diagnostics": "java.lang.NullPointerException at DeviceListFragment"
}
```

- `kind` 枚举：`bug` / `feature` / `experience` / `other`；`body` 去首尾空白后不能为空，最长 4000 字符；
  `context` 五个字段必填（`platform` 只能是 `android` / `ios`），`contact` ≤ 200、`diagnostics` ≤ 8000，均可省略。
- 成功返回 `201 {feedbackId, createdAt}`。
- **幂等**：优先使用请求头 `Idempotency-Key`（≤128 字符）；未携带时服务端对**规范化后的请求体**
  （字段去首尾空白、空串视为未提供）做 SHA-256 作为幂等键。同一账号重复提交同一内容返回同一个
  `feedbackId`（第二次为 `200` + `Idempotent-Replayed: true`），不会产生第二条记录；
  同一 `Idempotency-Key` 用于不同内容返回 `409 IDEMPOTENCY_CONFLICT`。存储层由 `(user_id, idem_key)`
  唯一索引兜底，`INSERT ... ON CONFLICT DO NOTHING` 后回读既有行。
- **限流**：账号与 IP 双维度同时生效，默认账号 `10/min`（桶容量 5）、IP `30/min`（桶容量 10），
  每维度独立计数；任一超限返回 `429 FEEDBACK_RATE_LIMITED` 并带 `Retry-After: 60`，不静默丢弃。
  参数可分别用 `FEEDBACK_USER_RATE_PER_MINUTE` / `FEEDBACK_USER_BURST` /
  `FEEDBACK_IP_RATE_PER_MINUTE` / `FEEDBACK_IP_BURST` 覆盖；`RATE_LIMIT_BACKEND=postgres` 时与全局限流共用共享桶。

**查看 `GET /feedback/mine?status=&cursor=&limit=`**：

- 只返回当前 token 对应账号的数据，按 `createdAt` 倒序游标分页（默认 20，最大 100），
  响应 `{items,nextCursor,hasMore}`；状态 `submitted` / `received` / `closed`。
- 响应项不含 `contact` 与 `diagnostics`：联系方式属于个人信息，诊断文本体积大且仅用于服务端排查。
- 自 v0.17.0 起支持 `?status=submitted|received|closed` 过滤；空串不过滤，未知值返回 `400 BAD_REQUEST`，
  游标语义不变。
- 联系方式不会写入日志明文。

**详情 `GET /feedback/{feedbackId}`（v0.17.0）**：

- 只返回本人反馈；不存在或属于他人一律 `404 NOT_FOUND`，不泄露资源是否存在。
- 返回 `kind`、`body`、`status`、`createdAt`、`updatedAt` 与 `statusMessage`（给用户看的中文说明，
  如「已提交，等待处理」「已受理，工作人员正在处理」「已关闭」）；**不回显** `contact` 与 `diagnostics`。

**关闭 `PATCH /feedback/{feedbackId}`（v0.17.0）**：

- 请求体必须是 `{"status":"closed"}`；用户侧只能关闭，提交其他目标状态返回 `400 BAD_REQUEST`。
- 允许 `submitted → closed` 与 `received → closed`；已关闭的反馈再次 PATCH 返回
  `409 FEEDBACK_INVALID_TRANSITION`，不允许回退或重复关闭。

**状态机**：`submitted → received → closed`，允许 `submitted → closed`；`received` 可回退到 `submitted`
（误受理）。其余跳转（如 `closed → received` / `closed → submitted`）一律 `409 FEEDBACK_INVALID_TRANSITION`。

**运营受理入口（v0.17.0，内部接口，不在公开客户端 SDK 中）**：

- `GET /admin/feedback?status=&cursor=&limit=`：跨用户查询，按状态与时间过滤，游标语义与用户列表一致；
  响应含 `contact`、`diagnostics` 与 `internalNote`，**不得暴露给终端用户**。
- `PATCH /admin/feedback/{feedbackId}`：请求体 `{"status":"submitted|received|closed","note":"..."}`，
  `note` 最长 2000 字符、可为空；内部备注只在运营视图回显，用户侧接口不会出现。
  目标状态与当前状态相同时返回 `200` 幂等 no-op（不改 `updatedAt`、不追加审计、忽略本次备注）。
- **鉴权**：独立凭据 `X-Admin-Token`，由环境变量 `ADMIN_TOKEN` 配置；与客户端 Bearer token 完全独立，
  两者不可互相替代。比较前对两侧取 SHA-256 摘要再用 `crypto/subtle.ConstantTimeCompare`，
  避免按长度提前返回。
- **密钥轮换**：`ADMIN_TOKEN_PREVIOUS` + `ADMIN_TOKEN_ROTATED_AT`（RFC3339）+
  `ADMIN_TOKEN_ROTATION_WINDOW_SECONDS`（默认 86400）可在轮换窗口内同时接受新旧两把密钥；
  窗口结束后上一把自动失效，未知密钥仍返回 `401`。详见 `docs/runbook.md` 的「Admin 凭据轮换」。
- **默认关闭**：未配置 `ADMIN_TOKEN` 时整组接口返回 `404`（与不存在的路由不可区分，不暴露存在性）；
  配置后凭据缺失或错误返回 `401 UNAUTHORIZED`。
- **审计**：每次真实状态变更在同一事务内写一条 `feedback_status_events` 记录
  （`actor_type`、`actor_id`、`from_status`、`to_status`、`note`、`created_at`）。
  共享 admin token 没有个人身份，因此 admin 变更统一记 `actor_type=admin` / `actor_id=admin`；
  用户关闭记 `actor_type=user` / `actor_id=<userID>`。`feedback.internal_note` 只保存最新一条备注，
  历史备注以审计表为准。

**数据落点**：现有 `user_devices` / `households` / `readings` 都不承载自由文本反馈，因此新增独立表
`feedback`（迁移 `000013`），索引 `(user_id, created_at DESC, id)` 与唯一索引 `(user_id, idem_key)`。
v0.17.0 的 `000014_feedback_status_workflow` 补 `internal_note` 列、`(status, created_at DESC, id)`
索引与审计表 `feedback_status_events`（`feedback_id` 外键 `ON DELETE CASCADE`）。
`feedback.updated_at` 与 status 的 `CHECK` 约束在 `000013` 已存在，`000014` 不重复添加。
账号注销时 `ON DELETE CASCADE` 一并删除。

### 设备类型目录（v0.18.0）

两端 App 的「产品 / 市场」不再各自维护设备类型常量，改为读取服务端目录。

**读取 `GET /catalog/device-types`（公开，匿名可读）**：

- 响应 `{ version, generatedAt, items[] }`，每项含 `type`、`displayName`、`description`、
  `gattServices`（服务 UUID + 名称）、`metrics`（`key` / `unit` / `characteristicUuid`）、
  `limitations`、`compatibility`、`updatedAt`。
- 鉴权可选：不带 token 也能读；带有效 Bearer token 时按 `(userId, 目录版本)` 记录一次访问
  （`device_type_catalog_accesses`，同版本去重），用于后续灰度；token 无效也不会 401。
- 协商缓存：响应带 `ETag`（`"catalog-v<version>"`）与 `Last-Modified`；`If-None-Match` 命中
  返回 `304`，未带时按 `If-Modified-Since` 比较（秒级）。任意管理端写入都会自增版本，
  因此 ETag 一定变化。

**运营管理（内部接口，不在公开客户端 SDK 中）**：

- `PUT /admin/catalog/device-types/{type}`：整项替换（字段全部必填），只能更新已存在项；
  `DELETE /admin/catalog/device-types/{type}`：软删除（`deleted_at`）。
- 与 `/admin/feedback` 共用 `X-Admin-Token`；未配置 `ADMIN_TOKEN` 时 404，凭据错误 401，
  类型不存在/已删除 404。写目录、版本自增与审计（`device_type_catalog_audits`）同事务完成。

**真值来源与一致性**：

- 唯一真值表在 [`internal/devicecatalog/catalog.go`](internal/devicecatalog/catalog.go)，
  GATT 服务、可解析指标、来源特征 UUID 与限制说明只在这里维护；
- `go run ./cmd/cataloggen` 生成迁移 `000015_device_type_catalog`（含 seed）与 down；
- 真实 PG 测试 `TestPostgresCatalogDeviceTypesIntegration` 把接口返回的 metrics / 特征 UUID /
  GATT 服务与真值表逐项比对，手改迁移或文档漂移会直接测试失败。

### 场景上云（v0.20.0）

两端 App 的本地场景（触发条件 + 动作 + 启用状态 + 冷却）此前只存在本机 SQLite/Room，
换机、重装、多设备登录即丢失，也不在账号删除与导出范围内。v0.20.0 把它们搬到服务端：

- `GET /scenes`、`POST /scenes`、`GET /scenes/{sceneId}`、`PUT /scenes/{sceneId}`、
  `DELETE /scenes/{sceneId}`，全部限定本人；跨用户或已软删统一 404。
- 软删除只写 `deleted_at`，同时向 `change_log` 写 `entity=scene`、`op=delete` 的 tombstone；
  `scene` 的 `household_id` 恒为 NULL，只对本人可见，其他用户的 `changes` 不会看到。
- 响应与 `change_log` payload 都是完整 `Scene`（含 `version`、`deviceMissing`、`deletedAt`），
  可直接重建客户端状态。

**存储形态**：一个场景就是客户端可直接执行的完整规则包，因此用单行 `scenes` 表 +
`trigger` / `actions` 两个 JSONB 列，而不是 `scene_rules` / `scene_actions` 子表。
理由：子表会引入没有业务含义的子级 LWW 时间戳；`change_log` 需要一次 payload 完整重建；
客户端本地也是整体读写。取舍与迁移注释见
[`000017_scenes.up.sql`](db/migrations/000017_scenes.up.sql)。

**字段级 LWW**：`name`、`enabled`、`cooldown_seconds`、`trigger`、`actions` 各有独立更新时间戳，
与设备元数据同一套语义。`PUT` 中出现的每个字段都必须自带 `updatedAt`：

- 客户端时间严格更新 → 应用该字段；
- 时间相同且内容一致 → 幂等 no-op（不改 `version`、不写 `change_log`）；
- 时间相同但内容不同或客户端更旧 → 服务端较新值静默获胜（不返回冲突）；
- 客户端时间超前 2 分钟以上 → 按服务端当前时间兜底。

并发修改不同字段互不覆盖；整个请求没有任何字段被应用时不增加 `version`。

字段时间戳在比较前统一截断到 Postgres `timestamptz` 的微秒精度：客户端带纳秒的
「同一时间戳」不会被误判为严格更新，重复提交同内容补丁稳定幂等。

**触发条件**（`kind` 决定必填字段，出现无关字段即 400 `SCENE_TRIGGER_INVALID`）：

| kind | 必填 | 可选 / 说明 |
| --- | --- | --- |
| `device_offline` | `device` | `offlineSeconds` 60–86400，默认 300 |
| `rssi_below` | `device`、`rssiThreshold` | 阈值 -100–0 dBm |
| `metric_threshold` | `device`、`deviceType`、`metric` | `metric` 含 `metric`/`comparator`(lt,lte,gt,gte)/`threshold`/`consecutive`(1–30)；设备类型必须存在于目录且声明该指标 |
| `schedule` | `timeOfDay`（HH:MM） | `timezone`（IANA，默认 UTC）、`daysOfWeek`（1=周一，省略=每天） |

定时选择「每天固定时刻 + 时区 + 可选星期」而不是 cron：动作由客户端本地调度执行，
cron 解析与服务端/客户端时区、夏令时差异会让语义漂移。

**动作**（1–4 个、种类不得重复；与两端已有能力对应）：

- `notify`：`title` / `body` 文本模板，由客户端渲染本地通知；
- `log`：`message` + `level`(info|warn)，写入客户端本地日志；
- `mark_device`：`mark` 标记名，客户端给触发设备打标。

**引用完整性**：写入时校验设备属于本人且未删除（`SCENE_DEVICE_NOT_FOUND`），
`metric_threshold` 还要校验目录类型与指标（`SCENE_METRIC_NOT_SUPPORTED`）。
设备之后被删除时**不级联删场景**，读取时实时计算 `deviceMissing=true`，保留用户配置。

**错误码**（均为 400）：`SCENE_NAME_INVALID`、`SCENE_COOLDOWN_OUT_OF_RANGE`、
`SCENE_TRIGGER_INVALID`、`SCENE_ACTION_INVALID`、`SCENE_DEVICE_NOT_FOUND`、
`SCENE_METRIC_NOT_SUPPORTED`；补丁缺 `updatedAt` 或空补丁为 `BAD_REQUEST`。

**账号删除与导出**：`DELETE /me` 的逐表清单已加入 `scenes`；个人数据导出新增
`scenes`（含 tombstone，带 `deletedAt`，`deviceMissing` 为导出时实时值）。

**分页（v0.23.0）**：`GET /scenes` 支持可选 `?limit=1..200&cursor=...`。**不传参数时返回全量，
响应体只有 `items`**，与 v0.22.0 完全一致；传 `limit` 时响应追加 `hasMore`，还有下一页时
追加 `nextCursor`。游标是不透明字符串（`updated_at|id` 的 Base64 编码），按
`updated_at DESC, id ASC` 稳定遍历；`limit<=0`、非数字或非法游标返回 400 `BAD_REQUEST`。

### 服务端推送与告警投递（v0.23.0）

服务端现在可以主动发起推送，而不是只能由客户端本地通知：

- `POST /push/devices`（Bearer，本人）：注册设备令牌，字段 `platform`（`android`/`ios`）、
  `token`、可选 `appVersion`、`deviceModel`；同一用户同一 token **幂等更新**（唯一键
  `(user_id, token)`，重新注册会把 `inactive` 重置为 `active`）。
- `DELETE /push/devices/{deviceId}`：把令牌标记 `inactive`，**不硬删**；`cmd/cleanup` 在 30 天后
  才删除失效行。跨用户或不存在统一 404。
- `POST /admin/alerts`（`X-Admin-Token`）：写入一条告警，并在**同一事务**内为每个 active
  设备写一条投递记录（`alerts` + `push_deliveries`）；返回 `{alert, deliveryCount}`。

投递链路：`push_deliveries` 行由后台 worker 以 `FOR UPDATE SKIP LOCKED` 抢占（多实例安全），
调用 `internal/push` 适配层；成功或 dry-run → `sent`，可重试错误 → 指数退避
（`PUSH_BACKOFF_BASE_SECONDS` × 2^(attempt-1)，上限 30m），终态错误 → `failed`；provider 明确
判定令牌无效/unregistered 时直接把对应 `push_devices` 置 `inactive`。`sending` 超过
`PUSH_SENDING_STALE_SECONDS` 未完成的行会被下一轮回收为 `pending`。

**dry-run**：未配置凭据时不会静默吞掉，也不会卡队列——适配层把「本应发送」的完整载荷
（provider/platform/userId/alertId/pushDeviceId/token/payload）写入日志，worker 把投递标记
`sent` + `dry_run=true`。凭据（`FCM_SERVER_KEY`、`APNS_KEY_FILE`/`APNS_KEY_PEM`、`APNS_KEY_ID`、
`APNS_TEAM_ID`、`APNS_TOPIC`）通过环境变量注入；`FCM_ENDPOINT` / `APNS_ENDPOINT` 可指向测试服务。
**真实 FCM/APNs 投递与真机收到通知尚未验证。**

**告警链路**：Prometheus 规则 → Alertmanager（[`deploy/alertmanager/alertmanager.yml`](deploy/alertmanager/alertmanager.yml)，
`group_by` 去重、runtime silence）→ [`cmd/alert-webhook`](cmd/alert-webhook/main.go) 逐条写 JSON 行。
本地复现：`./scripts/verify-alerting.sh`（已验证到 webhook 收到，含去重与静默；未接入真实电话/短信/邮件）。

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

### 设备标记（v0.25.0）

「标记设备」不再是纯本地状态：`UserDevice` 增加可空字段 `markedAt`（数据库 `user_devices.marked_at timestamptz NULL`），
标记与取消标记都通过 `POST /sync/push` 写入并进入 `change_log`（`entity=user_device`），
因此增量同步、多端与跨设备都能收敛到同一状态。

| 上报形式 | 服务端行为 |
| --- | --- |
| 字段缺失（absent） | 视为「未上报」，保持原值（不会清除已有标记） |
| `"markedAt": null` 或 `""` | **取消标记**，`marked_at` 置为 NULL |
| `"markedAt": "2026-11-07T08:00:00.123456+08:00"` | 标记；UTC 存储并**截断到微秒** |

语义要点：

- 与 `alias`/`group`/`deviceType` 一样受设备级 `updatedAt` 的字段级 LWW 约束：
  `updatedAt` 更旧的写入返回 `conflict`（`server_newer`），不会覆盖更新的标记；
- LWW 与内容比较前统一按微秒截断，重复上报**同一时间戳**且内容未变化时返回 `unchanged`，
  不会产生新的 `change_log` 条目；
- `GET /my/devices`、`GET /my/devices/{platform}/{externalId}`、账号导出与 `change_log` payload
  都返回同一字段；未标记时为 JSON `null`。

### 设备在线状态（v0.26.0）

`UserDevice` 新增两个**服务端只读**字段（迁移 `000020_device_presence`，`user_devices.last_seen_at timestamptz NULL`）：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `lastSeenAt` | `date-time \| null` | 服务端最后一次**接受**该设备读数上报的接收时间；UTC、截断到微秒；`null` = 从未上报 |
| `presence` | `unknown \| online \| offline` | 服务端按当前时间实时推导的三态 |

- **三态**：`lastSeenAt=null` → `unknown`（从未知道，区别于明确离线）；`now - lastSeenAt <= 15m` → `online`；
  超过 15 分钟 → `offline`（长时间无活动自然离线，没有单独的「下线」事件）。
- **判定来源**：只来自 `/sync/push` 中被服务端接受的 `readings`；使用服务端**接收时间**，
  不是 `recordedAt`，所以补录旧读数不会让设备重新上线。设备元数据 push、`/sync/pull` 都不算活动。
- **更新时机**：与读数写入同一数据库事务，单调推进；提交后 `/my/devices` 立即可见。
- **同步**：推进时写入一条 `user_device` 的 `change_log`，payload 为完整设备快照，
  因此多端、换机后的 `/sync/changes` 能拿到在线状态；`presence` 在响应生成时重算。
- **写入语义**：`lastSeenAt`/`presence` 不参与 LWW，客户端在 `POST /sync/push` 中上报会被忽略，
  不引入第二套写入路径；设备记录的其他字段仍按既有字段级 LWW 处理。
- **账号隔离**：读数只更新推送者本人名下的设备行；共享家庭设备显示 owner 记录上的状态，
  不会跨账号改写。

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

#### 个人数据导出：`POST /me/export` → `GET /me/export/{id}` → 下载

统一采用**异步**导出（不做「小体量同步 / 大体量异步」双分支，客户端只维护一套轮询语义）：

1. `POST /me/export`（Bearer）→ `202`，返回 `{id,status:"pending",createdAt,expiresAt}`；
2. 轮询 `GET /me/export/{id}`；`status` 为 `pending` / `ready` / `failed`；
   `failed` 时响应带 `failureReason`（例如进程重启后启动对账把超过 5 分钟仍未完成的任务标记失败），
   客户端应提示用户重新发起导出；
3. `ready` 时响应带 `downloadUrl`（含一次性令牌，10 分钟有效）与 `downloadExpiresAt`；
   每次查询都会签发**新**链接并让旧链接立即失效；
4. `GET /me/export/{id}/download?token=…` 不使用 Bearer；首次下载即失效，重放/过期 `410`，
   令牌错误 `403`；响应头 `Content-Disposition: attachment; filename="pjgoiot-export-<taskId>.json"`。

产物结构（仅本人数据）：

```json
{"user":{"id":"...","kind":"anonymous","createdAt":"..."},
 "devices":[...全部设备，含 tombstone，按 updatedAt 倒序...],
 "readings":[{"platform":"android","externalId":"...","metric":"...","unit":"celsius",
              "count":2,"min":21,"max":23,"avg":22,"firstAt":"...","lastAt":"..."}],
 "households":[{"id":"...","name":"...","role":"owner","personal":true,"createdAt":"...",
               "rooms":[{"id":"...","name":"...","sortOrder":0,"createdAt":"..."}]}],
 "feedback":[{"feedbackId":"...","kind":"bug","body":"...","contact":"...","diagnostics":"...",
              "status":"received","createdAt":"...","updatedAt":"..."}],
 "generatedAt":"..."}
```

- 读数按 `(platform, externalId, metric, unit)` **聚合**，产物大小与读数条数无关；逐条明细在删除前仍可通过读数接口获取；
- 家庭只含本人角色与房间，**不含其他成员身份或联系方式**；
- 反馈只含本人联系方式与诊断文本，**不含运营内部备注**；
- 产物保留 24 小时，到期由 `cmd/cleanup` 清理。

#### 注销：`POST /me/deletion-request` + `DELETE /me`

- 第一步（Bearer）`POST /me/deletion-request` → `201`，返回一次性令牌 `{token,expiresAt}`；
  服务端只存 SHA-256 哈希，10 分钟过期，同一账号再次申请让旧令牌立即失效；
- 第二步 `DELETE /me`，请求头 `X-Deletion-Token`（**不用 Bearer**）→ `204`；
- 单事务硬删除：设备（含 tombstone）、读数、房间、家庭关系、邀请码、本人反馈及其状态审计、
  token、推送幂等记录、变更日志、目录访问记录、导出任务；
- 本人是 owner 的家庭还有其他成员时返回 `409 ACCOUNT_DELETION_BLOCKED`，
  必须先转让所有权或移除成员（避免孤儿家庭）；被拦截时账号与数据完整保留；
- 删除后全部 token 立即 `401`；同一删除令牌重放返回 `404`（账号已不存在），不会部分删除；
- 令牌缺失 `400 DELETION_TOKEN_REQUIRED`、错误 `403 DELETION_TOKEN_INVALID`、过期 `410 DELETION_TOKEN_EXPIRED`；
- 不可恢复。

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

### 容量与压测基线（v0.21.0）

v0.8.0 只覆盖三条路径且跑在 2 vCPU Docker VM 内；v0.21.0 用**本机真实 PostgreSQL 16.15**
（Apple M1 Max 10 核 / 32 GB，API 与 PG 同机回环、HTTP 直连不经 Caddy）把上线前主要路径全部跑了一遍。
原始结果、复现命令、CPU 采样与 SQL 微基准见 [`docs/perf-baseline.md`](docs/perf-baseline.md)。

数据规模：A 轮 10 用户 / ≈5,400 设备 / ≈303,000 读数 / ≈225,000 `change_log`（626 MB）；
B 轮（push 拐点与多用户）18 用户 / ≈21,000 设备 / ≈1,993,000 读数 / ≈1,071,000 `change_log`（≈1.7 GB）。

A 轮逐路径结果（30s + 3s 预热，`RATE_LIMIT_PER_MINUTE=0`）：

| 路径 | 并发 | RPS | p50 | p95 | p99 | 错误率 |
| --- | --- | --- | --- | --- | --- | --- |
| `GET /my/devices` | 50 | 5,773.9 | 8 ms | 16 ms | 23 ms | 0.00% |
| `GET /readings/series`（1d 桶 / 90 天） | 50 | 4,766.0 | 8 ms | 20 ms | 77 ms | 0.00% |
| `POST /sync/push`（100 设备 + 200 读数，同一用户） | 50 | 42.4 | 1.053 s | 2.042 s | 2.752 s | 0.08% |
| `GET /sync/changes`（`limit=100`，带游标） | 50 | 136.7 | 345 ms | 597 ms | 761 ms | 0.00% |
| `GET /scenes`（50 个场景） | 50 | 7,174.5 | 6 ms | 13 ms | 18 ms | 0.00% |
| `PUT /scenes/{id}`（字段级 LWW 真实写入） | 50 | 2,097.7 | 22 ms | 43 ms | 57 ms | 0.00% |
| `POST /feedback` | 20 | 12,749.3 | 1 ms | 3 ms | 10 ms | 0.00% |
| `GET /feedback/mine?limit=20` | 20 | 11,443.6 | 1 ms | 5 ms | 13 ms | 0.00% |
| `GET /catalog/device-types`（`If-None-Match` 全命中） | 50 | 12,388.2 | 4 ms | 7 ms | 11 ms | 0.00%（371,679 全 304） |

**拐点（如实记录，不挑好看的数字）**：

- **单用户 push 被串行化**：10 并发 48.9 RPS（p99 227 ms）→ 50 并发 42.4 RPS（p99 2.752 s）→
  100 并发 41.5 RPS（p99 5.8 s，54 次 503 `TIMEOUT`）。根因是 `"push:"+userID` advisory lock，
  单用户吞吐天花板 ≈42–49 RPS（≈4,200–4,900 设备行/s + ≈8,400–9,800 读数行/s），加并发只增排队。
- **多用户可线性放大**：5 个进程 × 各自用户 × 10 并发，合计 **≈158.9 RPS**，API CPU ≈2.38 核，
  证明单用户上限不是数据库全局上限。API 单进程 CPU ≈2.4 核是当前同机压测的再上一个拐点。
- **`/sync/changes` 下限查询已修（v0.22.0）**：原 `MIN(seq)` 用 `user_id OR household_id IN (...)`
  导致主键前缀扫描（≈1.11M 行时 warm **347.7 ms / 233k buffers**）；改为「本人 MIN + 每家庭 MIN」后，
  同库同用户实测 p95 **66.28 ms → 0.56 ms**、buffers **87,928 → 10**（`Index Only Scan ... LIMIT 1`）。
  多家庭用户与生产级数据量仍待补测。
- 反馈写入 30s 造 38 万行，库增长快；保留期清理（`cmd/cleanup`）必须定时化。

多实例限流单独一轮（`RATE_LIMIT_PER_MINUTE=20`、`BURST=5`，同一 IP 在两个实例间交替 40 次）：

| 后端 | 实例数 | 放行 / 429 | 结论 |
| --- | --- | --- | --- |
| `postgres` | 2 | 5 / 35 | 额度共享，**不随实例数放大** |
| `memory`（对照） | 2 | 9 / 31 | 每进程独立桶，接近 2× 额度 |

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

### 故障与恢复演练（v0.21.0）

三项演练都在本机真实 PostgreSQL（`pjgoiot_load`，迁移 000001–000017）上执行，
完整命令与原始输出见 [`docs/preflight.md`](docs/preflight.md) §一。

**1. 备份 → 恢复到新库 → 行数与校验和**

| 指标 | 本次执行结果 |
| --- | --- |
| `pg_dump -Fc` 耗时 / 产物 | **9.958 s** / **138 MB**（源库约 1.7 GB） |
| sha256 | `f344d19c105b939ec6c6e6f6f13fc9a8fc50175cc0be046c140c54f98f681c72` |
| `pg_restore -j 4` 到新库 | **15.403 s** |
| 15 张表 `count(*)` + `md5(sum(hashtext(row::text)))` | **完全一致**（`diff` 无输出，含 1,993,200 条读数、1,071,255 条 `change_log`） |

**2. 迁移 `up → down → up`（有数据的库，不是空库）**

| 指标 | 本次执行结果 |
| --- | --- |
| down（17→16） | 1.99 s；`scenes` 表被 DROP；`users` 18 / `user_devices` ≈21,000 / `readings` 1,993,200 / `change_log` 1,071,255 **均不变** |
| up（16→17） | 0.125 s；`version=17 dirty=false`；核心表计数不变；`scenes` **60 行 → 0** |

结论：000017 的 `down` 会删场景配置，**生产回滚前必须先导出 `scenes`，或只做前滚修复**；用户、设备、读数、变更日志不受影响。

**3. 进程 `SIGKILL` 后重启**

| 检查点 | 本次执行结果 |
| --- | --- |
| advisory lock | 杀前 1 个 granted → 杀后 +0.4 s / +3.4 s 均为 **0**（无残留锁） |
| 重启后服务 | `/api/v1/health` 200；2 并发 push 0.00% 错误；`change_log` 继续增长 |
| 游标续传 | 从游标 `1,086,355` 继续返回 100 条，seq **1,086,356–1,086,455** 连续无缺 |
| 导出任务 | 注入的 `pending` 任务重启后**仍为 `pending`**，直到 24 h 过期——进程内 goroutine，无启动对账 |

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

# 12. 异步导出（先创建任务并轮询状态，ready 后用返回的 downloadUrl 下载）
TASK=$(curl -s -X POST "$BASE/me/export" -H "Authorization: Bearer $TOKEN" | python3 -c 'import json,sys;print(json.load(sys.stdin)["id"])')
curl -s "$BASE/me/export/$TASK" -H "Authorization: Bearer $TOKEN"
# 拿到 downloadUrl 后：curl -s "$BASE${downloadUrl}" -o pjgoiot-export.json

# 13. 注销账号（二次确认：先申请 10 分钟一次性令牌，再带 X-Deletion-Token 删除）
DEL=$(curl -s -X POST "$BASE/me/deletion-request" -H "Authorization: Bearer $TOKEN" \
  | python3 -c 'import json,sys;print(json.load(sys.stdin)["token"])')
curl -i -X DELETE $BASE/me -H "X-Deletion-Token: $DEL"

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
- 数据库只存 SHA-256 哈希；`tokens.revoked_at` 支持吊销（`/auth/logout`、`DELETE /sessions/{id}`）。
- 私有接口用 `Authorization: Bearer <token>`，服务端按 token 哈希查出 `userId` 后再执行查询。
- **刷新与多设备会话（v0.22.0）**：`POST /auth/refresh` 用当前有效 token 换新 token，**旧 token 同一事务内立即吊销**
  （滚动 180 天 TTL），刷新不能复活已吊销 token；`GET /sessions` 列出账号有效会话并用 `current` 标记本次会话，
  `DELETE /sessions/{id}` 可单独吊销丢失设备。服务端不会自动轮换；只有显式调用 `/auth/refresh` 才会轮换。
  刷新本身要求当前 token 有效，因此客户端收到 401 时仍应走「用户显式重新登录」，不能靠刷新自救。

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
| `FEEDBACK_USER_RATE_PER_MINUTE` | `10` | 反馈提交按账号的每分钟令牌数 |
| `FEEDBACK_USER_BURST` | `5` | 反馈提交按账号的令牌桶容量（允许的瞬时连发数） |
| `FEEDBACK_IP_RATE_PER_MINUTE` | `30` | 反馈提交按来源 IP 的每分钟令牌数 |
| `FEEDBACK_IP_BURST` | `10` | 反馈提交按来源 IP 的令牌桶容量 |
| `ADMIN_TOKEN` | 空 | 运营侧 `/admin/feedback`、`/admin/catalog/*` 的独立凭据（请求头 `X-Admin-Token`）；为空表示入口关闭，整组返回 404 |
| `ADMIN_TOKEN_PREVIOUS` | 空 | 轮换期间保留的上一把运营密钥；设置时必须同时设置 `ADMIN_TOKEN_ROTATED_AT` |
| `ADMIN_TOKEN_ROTATED_AT` | 空 | 新密钥生效时间（RFC3339，建议 UTC，如 `2026-11-07T08:00:00Z`）；上一把密钥从该时刻开始计时 |
| `ADMIN_TOKEN_ROTATION_WINDOW_SECONDS` | `86400` | 上一把密钥的保留窗口（秒）；窗口结束后自动失效，未知密钥一律 401 |
| `METRICS_ENABLED` | `true` | 是否注册根路径 `/metrics`；生产可关闭或仅内网暴露 |
| `RUN_MIGRATIONS_ON_BOOT` | `true` | postgres 模式下启动时执行迁移；失败进程非零退出（fixture 模式忽略） |
| `CORS_ALLOWED_ORIGINS` | `*` | 逗号分隔的允许来源，`*` 表示全部 |
| `LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error` |
| `TOKEN_TTL_DAYS` | `180` | 签发 token 的有效期（天）；过期后鉴权返回 401，`cmd/cleanup` 删除过期行 |
| `APP_VERSION` | `0.26.0` | 服务版本（健康接口与日志展示）；与 OpenAPI 契约版本 0.21.0 不是同一个号 |
| `DB_MAX_OPEN_CONNS` | `20` | postgres 连接池最大连接数；过小会在高并发下排队并在超时预算后返回 503 TIMEOUT |
| `DB_MAX_IDLE_CONNS` | `10` | 连接池最大空闲连接数，超过 `DB_MAX_OPEN_CONNS` 时自动收敛 |
| `HEALTHCHECK_URL` | 自动推导 | `-healthcheck` 模式请求的完整 URL |
| `TEST_DATABASE_URL` | 空 | 设置后运行 Postgres 集成测试 |
| `READING_RETENTION_DAYS` | `90` | **仅 cleanup 容器**：读数保留天数，映射到 `-retention-days` |
| `CHANGE_LOG_RETENTION_DAYS` | `90` | **仅 cleanup 容器/命令**：变更日志保留天数 |
| `IDEMPOTENCY_RETENTION_DAYS` | `7` | **仅 cleanup 容器/命令**：幂等键保留天数 |
| `PUSH_WORKER_ENABLED` | `true` | postgres 模式下是否启动推送投递 worker；关闭时告警只入队不消费 |
| `PUSH_WORKER_INTERVAL_SECONDS` | `5` | worker 轮询间隔（>0） |
| `PUSH_WORKER_BATCH` | `50` | 每轮最多抢占的投递条数（1–500） |
| `PUSH_MAX_ATTEMPTS` | `5` | 可重试错误的最大尝试次数（1–20），超过后置 `failed` |
| `PUSH_BACKOFF_BASE_SECONDS` | `30` | 指数退避基数：`base × 2^(attempt-1)`，上限 30 分钟 |
| `PUSH_SENDING_STALE_SECONDS` | `300` | `sending` 超过该秒数未完成的投递会被回收为 `pending`（≥60） |
| `FCM_SERVER_KEY` | 空 | FCM legacy server key；为空时 Android 走 dry-run |
| `FCM_ENDPOINT` | 空 | 覆盖 FCM 发送地址（测试/代理用） |
| `APNS_KEY_FILE` / `APNS_KEY_PEM` | 空 | APNs `.p8` 私钥路径或内联 PEM（二选一）；缺失时 iOS 走 dry-run |
| `APNS_KEY_ID` / `APNS_TEAM_ID` / `APNS_TOPIC` | 空 | APNs Key ID、Team ID、Bundle ID（topic）；任一项缺失即 dry-run |
| `APNS_ENDPOINT` | 空 | 覆盖 APNs 地址（默认 `https://api.push.apple.com`，测试可用 sandbox） |

> 清理工具不在 API 进程中运行，API 侧无需配置保留期变量。v0.23.0 新增的推送投递/失效设备保留期
> 只有命令行参数（`-push-delivery-retention-days` 默认 7、`-push-device-retention-days` 默认 30）。

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
| v0.12.0 家庭模型与迁移回填 | 通过（真实 PostgreSQL）：迁移 `000011` 在临时库上 `up → down → up`；旧账号自动获得「我的家」个人家庭与 owner 成员，设备 `household_id` 回填、`room_id` 为 NULL；down 后 4 张家庭表与 2 个新列全部移除，再 up 回填结果一致 |
| v0.12.0 家庭共享与权限 | 通过（真实 PostgreSQL）：`TestHouseholdSharingPostgres` 覆盖邀请码一次性、跨家庭 404、member 改名 403、member 改他人设备 404、成员可读共享设备与读数、共享变更进入 `/sync/changes`、删房间清空 `roomId`、删家庭后设备回个人家庭；HTTP 层 `TestHouseholdLifecycleAndPermissions` 通过 |
| v0.12.0 OpenAPI 契约 | 通过：`npx @redocly/cli lint api/openapi.yaml` → **0 error**（13 条非阻断 warning：info/tags/示例类，本轮新增 `household` tag 触发 1 条描述缺失）；契约版本 0.8.0 |
| v0.12.0 CI | 通过：GitHub Actions [run 42037369419](https://github.com/platojobs/PJGoIoT/actions/runs/42037369419)，`构建与测试`、`配置与脚本校验`、`压测门槛（smoke）`、`构建镜像` 4 个 job 全绿（commit `c36db9a`） |
| v0.13.0 成员与邀请码管理 | 通过（真实 PostgreSQL）：`TestHouseholdManagementPostgres` 覆盖成员移除后设备回个人家庭、owner 移除自己 400、member 越权 403、member 退出 204、owner 有其他成员退出 409、owner 独退删家庭、个人家庭退出 400；邀请码列表仅 8 位哈希前缀 + `active/expired/locked`、撤销后领取 404。HTTP 层 `TestHouseholdMemberAndInvitationManagement` 同步通过 |
| v0.13.0 房间整段重排 | 通过：`PUT /households/{id}/rooms/order` 写 `0..n-1`；缺项/多项/重复 id → 409 `CONFLICT`；事务内 `FOR UPDATE` 行锁串行化并发重排；`GET /households` 的 `roomCount` 随之更新 |
| v0.13.0 迁移 000012 | 通过（真实 PostgreSQL，临时库）：`up → down → up`；`household_invitations.id` 列与 `household_invitations_id_idx` / `household_invitations_active_idx` 可正确增删 |
| v0.13.0 OpenAPI 契约 | 通过：`npx @redocly/cli lint api/openapi.yaml` → **0 error**（13 条非阻断 warning）；契约版本 **0.9.0** |
| v0.13.0 CI | 通过：GitHub Actions [run 42042030047](https://github.com/platojobs/PJGoIoT/actions/runs/42042030047)，`构建与测试`、`配置与脚本校验`、`构建镜像`、`压测门槛（smoke）` 全绿；`构建与测试` 中 `OpenAPI 契约校验` 步骤实际执行并通过，`Postgres 集成测试` 正则已包含 `TestHouseholdManagementPostgres`（commit `3f5380f`） |
| v0.14.0 所有权转让 | 通过（真实 PostgreSQL）：`TestHouseholdOwnershipPostgres` 覆盖成功转让后单 owner 不变量、目标非成员 `ErrHouseholdTargetNotMember`、个人家庭拒绝、转让给自己拒绝、member 调用 403、旧 owner 读 200/写 403/可 `leave`、新 owner 有其他成员拒绝退出；HTTP 层 `TestHouseholdOwnershipTransfer` 同步覆盖 403/404/400 边界与 `isSelf` |
| v0.14.0 并发转让 | 通过（真实 PostgreSQL）：`TestHouseholdOwnershipTransferConcurrentPostgres` 两个 goroutine 并发转让给不同目标，恰好 1 笔成功、1 笔 `ErrForbidden`，库内 `role='owner'` 行数为 1 且 `households.owner_user_id` 指向获胜目标 |
| v0.14.0 可见性修正 | 通过：被移除成员的 token 读取家庭/成员/房间/邀请码一律 404（`TestHouseholdManagementPostgres` 断言）；`TestHouseholdRoomSetMismatchCode` 断言缺项重排返回 409 且 `error.code == ROOM_SET_MISMATCH` |
| v0.14.0 迁移 | 本轮无 schema 变更，未新增迁移；迁移版本保持 `000012`（已在 v0.13.0 验证 `up → down → up`） |
| v0.14.0 OpenAPI 契约 | 通过：`npx @redocly/cli lint api/openapi.yaml` → **0 error**（13 条非阻断 warning）；契约版本 **0.10.0** |
| v0.14.0 CI | 通过：GitHub Actions [run 42051601765](https://github.com/platojobs/PJGoIoT/actions/runs/42051601765)，四个 job 全绿；`Postgres 集成测试` 正则已放宽为 `TestHousehold`，日志可见 `TestHouseholdOwnershipPostgres`、`TestHouseholdOwnershipTransferConcurrentPostgres` 实际执行并通过；`OpenAPI 契约校验` 步骤实际执行（0 error） |
| v0.11.0 CI 补跑 | 通过：`v0.11-fix` 分支 [run 42037314989](https://github.com/platojobs/PJGoIoT/actions/runs/42037314989) 全绿；修复 shellcheck 对 trap 清理函数的 SC2317/SC2329 误报与未使用变量，v0.11.0 tag 已指向该修复提交 |
| v0.15.0 结构变更同步 | 通过（真实 PostgreSQL）：`TestHouseholdStructureChangesPostgres` 覆盖创建家庭/改名/建房/重排/转让/移除各阶段的 `household` 与 `room` 变更，断言两账号各自 `role` 视角正确（owner→member、member→owner）、重排后 `sortOrder` 直接可用、无关账号看不到、被移除者不再收到后续结构变更但仍能看到离开前的历史、新成员不回放历史房间变更 |
| v0.15.0 结构变更 410 | 通过（真实 PostgreSQL）：`TestHouseholdStructureChangesCursorExpiredPostgres` 模拟保留期清理后，仅由结构变更造成的缺口同样触发 `store.ErrCursorExpired`（HTTP 层 `410 RESYNC_REQUIRED`） |
| v0.15.0 未知 entity 兼容 | 通过：`TestSyncChangesUnknownEntityIgnoredByOldClient` 用固定 JSON 断言老客户端可解析含 `household`/`room`/`future_widget` 的响应，未知 entity 计数忽略且 `nextCursor` 照常推进 |
| v0.15.0 HTTP 层结构同步 | 通过：`TestSyncChangesHouseholdStructure` 经 fixture 走完整 HTTP 链路（建房→邀请→建房→改名→移除→改名），验证两个账号 `GET /sync/changes` 的条目与可见性 |
| v0.15.0 迁移 | 本轮无 schema 变更（`change_log` 已有 `user_id`/`entity`/`entity_id` 列），未新增迁移；版本保持 `000012` |
| v0.15.0 OpenAPI 契约 | 通过：`npx @redocly/cli lint api/openapi.yaml` → **0 error**（13 条非阻断 warning）；契约版本 **0.11.0** |
| v0.15.0 CI | 通过：GitHub Actions [run 42058675285](https://github.com/platojobs/PJGoIoT/actions/runs/42058675285)，四个 job 全绿；日志确认 `TestHouseholdStructureChangesPostgres` 与 `TestHouseholdStructureChangesCursorExpiredPostgres` 在 `Postgres 集成测试` 中实际 PASS，`OpenAPI 契约校验` 步骤实际执行且读到 `version: 0.11.0` |
| v0.16.0 反馈提交与幂等 | 通过（真实 PostgreSQL）：`TestPostgresFeedbackIntegration` 覆盖提交成功、空白/超长描述被约束拒绝、幂等重复提交返回同一 `feedbackId` 且不新增记录、同幂等键不同内容 `ErrIdempotencyConflict`、`submitted` 初始状态 |
| v0.16.0 本人列表与隔离 | 通过（真实 PostgreSQL）：同一测试断言 A/B 列表互不可见、游标分页不重不漏、非法游标 `ErrBadCursor`；`TestFeedbackMineIsolationAndPagination`（fixture HTTP 层）断言列表响应不含 `contact` |
| v0.16.0 校验与限流 | 通过：`TestFeedbackValidation` 覆盖空白/超长/未知类型/非法平台/缺上下文/非法 JSON；`TestFeedbackRateLimitByUserAndIP` 与真实 PG 中的 HTTP 子测试验证账号与 IP 双维度 429 `FEEDBACK_RATE_LIMITED`，未登录 401 |
| v0.16.0 契约一致性 | 通过：`TestOpenAPIFeedbackContract` 解析 `api/openapi.yaml`，比对 `FeedbackItem` 的 required/properties 与 `model.Feedback` 的 JSON 字段、kind/platform/status 枚举、错误码枚举与实现常量一致 |
| v0.16.0 迁移 | 通过：`000013_feedback` 在 `TestMigrationBackfillsLegacyDevices` 中验证 `up → down（回到 000012）→ up`，并断言 `feedback_user_created_idx` 与 `feedback_user_idem_idx` 存在、回滚不影响用户数据 |
| v0.16.0 OpenAPI 契约 | 通过：`npx @redocly/cli lint api/openapi.yaml` → **0 error**；契约版本 **0.12.0** |
| v0.16.0 CI | 通过：GitHub Actions [run 42077337348](https://github.com/platojobs/PJGoIoT/actions/runs/42077337348) 四 job 全绿；日志确认 `TestPostgresFeedbackIntegration` 全部 PASS，`OpenAPI 契约校验` 读到 `version: 0.12.0`；`单元测试` 中 `internal/httpapi`（含契约一致性测试）为 ok |
| v0.17.0 状态机与审计 | 通过（真实 PostgreSQL）：`TestPostgresFeedbackLifecycleIntegration` 覆盖用户关闭本人、已关闭再改 409、跨用户读写 404、`submitted→received→submitted→closed`、`closed→received/submitted` 409、跨用户列表状态过滤、审计表逐条比对（actor/from/to/note） |
| v0.17.0 幂等与 admin | 通过（真实 PostgreSQL）：同状态重复 PATCH 返回 200、`updatedAt` 不变、审计只增 1 条且备注不被覆盖；HTTP 层 admin 未配置 404、错误凭据 401、客户端 token 不能替代 admin 凭据、正确凭据 200 |
| v0.17.0 HTTP 鉴权矩阵 | 通过：`TestFeedbackDetailAndClose` 与 `TestAdminFeedbackAuthMatrixAndTransitions`（fixture）覆盖用户 token / admin token / 无 token 三态、错误码与状态码一致、用户详情与列表不回显 `contact`/`diagnostics`/`internalNote` |
| v0.17.0 契约一致性 | 通过：`TestOpenAPIFeedbackContract` 比对 `FeedbackDetail` / `AdminFeedbackItem` 的 properties/required 与模型 JSON 字段、`FeedbackCloseRequest` 与 admin 目标状态枚举、admin 路径 `x-audience: internal` / `x-client-sdk: false`、错误码枚举含 `FEEDBACK_INVALID_TRANSITION` |
| v0.17.0 迁移 | 通过：`000014_feedback_status_workflow` 在 `TestMigrationBackfillsLegacyDevices` 中验证 `up → down（000013）→ up`，断言 `internal_note`、审计表、`feedback_status_created_idx` 存在且回滚不破坏反馈表与用户数据 |
| v0.17.0 OpenAPI 契约 | 通过：`npx @redocly/cli lint api/openapi.yaml` → **0 error**（14 条非阻断 warning）；契约版本 **0.13.0** |
| v0.17.0 CI | 通过：GitHub Actions [run 42163927951](https://github.com/platojobs/PJGoIoT/actions/runs/42163927951) 四 job 全绿；日志确认 `TestPostgresFeedbackLifecycleIntegration` 各子测试执行且 job 成功，`OpenAPI 契约校验` 0 error |
| v0.17.0 运营工具联调 | 未验证：只提供 HTTP 契约与 curl 级验证，未接入真实运营后台；`ADMIN_TOKEN` 的下发、轮换与吊销流程未落地 |
| v0.17.0 admin 并发压测 | 未验证：`SELECT ... FOR UPDATE` 保证同一反馈的审计顺序，但未做多实例并发 PATCH 压测；共享 token 下审计 actor 固定为 `admin`，无法区分具体操作人 |
| v0.17.0 审计留存 | 未验证：审计表随反馈级联删除，未设计独立归档 / 保留期策略；长期增长与清理未评估 |
| v0.18.0 目录读取与一致性 | 通过（真实 PostgreSQL）：`TestPostgresCatalogDeviceTypesIntegration` 读取 5 个目录项；`metrics` / 来源特征 UUID / `gattServices` / `limitations` / `compatibility` 与 `internal/devicecatalog` 真值表逐项比对一致 |
| v0.18.0 ETag / 304 | 通过（真实 PostgreSQL + HTTP）：匿名读取 200 带 `ETag`/`Last-Modified`；`If-None-Match` 命中 304；管理端 PUT 后版本自增、ETag 变化，旧 ETag 不再命中 304，新 ETag 命中；fixture 层同步覆盖弱 ETag 与 `If-Modified-Since` |
| v0.18.0 管理与软删 | 通过（真实 PostgreSQL + HTTP）：PUT 整项更新、DELETE 软删后列表不再返回但行仍在库中；admin 未配置 404、错误凭据 401、正确凭据 200/204；不存在或已删除类型 PUT/DELETE 均 404；审计 `upsert`/`delete` 与版本号逐条核对 |
| v0.18.0 访问记录 | 通过（真实 PostgreSQL）：同一用户同一目录版本重复记录只保留 1 行（`ON CONFLICT DO NOTHING`）；匿名读取不写访问表；无效 token 仍按匿名返回 200 |
| v0.18.0 迁移 | 通过：临时库 `up → down（000014）→ up`，`device_type_catalog` 及其索引/审计/访问/版本表可正确增删；重新执行 up 后 seed 行数为 5 |
| v0.18.0 契约一致性 | 通过：`TestOpenAPICatalogContract` 比对 `CatalogMetric` / `CatalogDeviceType` / `DeviceTypeCatalog` / `CatalogDeviceTypeInput` 的 properties/required 与模型 JSON 字段；公开读取 `security: []`、304 响应、admin 路径 `x-audience: internal` / `x-client-sdk: false` |
| v0.18.0 OpenAPI 契约 | 通过：`npx @redocly/cli lint api/openapi.yaml` → **0 error**（14 条非阻断 warning，均为 operation-4xx-response 提示）；契约版本 **0.14.0** |
| v0.18.0 CI | 通过：GitHub Actions [run 42166636796](https://github.com/platojobs/PJGoIoT/actions/runs/42166636796) 四 job 全绿；`Postgres 集成测试` 执行 `TestPostgresCatalogDeviceTypesIntegration` 全部子测试，`OpenAPI 契约校验` 0 error |
| v0.19.0 迁移 | 通过：临时库 `up → down（000015）→ up`，`account_deletion_tokens` / `personal_data_export_tasks` 与四个索引可正确增删；重新执行 up 后迁移版本 16 |
| v0.19.0 OpenAPI 契约 | 通过：`npx @redocly/cli lint api/openapi.yaml` → **0 error**（14 条非阻断 warning）；契约版本 **0.15.0** |
| v0.19.0 CI | 通过：GitHub Actions [run 42170187766](https://github.com/platojobs/PJGoIoT/actions/runs/42170187766) 四 job 全绿；`Postgres 集成测试` 执行 `TestPostgresAccountDeletionExportIntegration` 三个子测试，`OpenAPI 契约校验` 0 error |
| v0.18.0 客户端接入 | 未验证：两端 App 仍使用本地常量，尚未切到 `GET /catalog/device-types`；缓存失效、离线回退与灰度开关未在真实客户端验证 |
| v0.18.0 admin 并发 | 未验证：目录写入单事务 + 版本自增，但未做多实例并发 PUT 压测；共享 `ADMIN_TOKEN` 下审计 actor 固定为 `admin` |
| v0.19.0 删除令牌与逐表清空 | 通过（真实 PostgreSQL）：错误令牌/过期令牌分别返回明确错误；删除前 14 张关联表全部非空，删除后逐表计数为 0，`users` 为 0；新申请作废旧令牌 |
| v0.19.0 旧 token 与重放 | 通过（真实 PostgreSQL）：删除后旧 token 返回 `ErrUnauthorized`；同一令牌重放返回 `ErrNotFound`（404 语义）；清理墓碑后重放返回 `ErrDeletionTokenInvalid` |
| v0.19.0 owner 拦截与转让 | 通过（真实 PostgreSQL + HTTP）：owner 家庭有成员时 409 `ACCOUNT_DELETION_BLOCKED` 且数据保留；转让后同一令牌可删除，家庭归新 owner |
| v0.19.0 异步导出与隔离 | 通过（真实 PostgreSQL）：任务 pending→ready；读数聚合字段正确；产物不含他人设备/读数/用户 id 与运营 `internal_note`；家庭仅含本人角色与房间 |
| v0.19.0 下载链接一次性 | 通过（真实 PostgreSQL + HTTP）：pending 不可签发（409）；首次下载成功，重放/过期 410，错误令牌 403；任务过期清理后查询 404 |
| v0.19.0 HTTP 鉴权与错误码 | 通过（fixture）：删除申请/导出须 Bearer；`DELETE /me` 缺/错令牌 400/403，owner 有成员 409，成功 204，重放 404；下载与任务查询隔离 |
| v0.19.0 契约一致性 | 通过：`TestOpenAPIDeletionExportContract` 校验 7 个新 schema 的 properties/required、`security: []`、错误码枚举与 `ExportTask` 状态枚举；契约版本 **0.15.0** |
| v0.20.0 场景 CRUD 与本人限定 | 通过（真实 PostgreSQL）：创建/列表/详情/更新/删除；跨用户读改删均 404，跨用户列表为空 |
| v0.20.0 字段级 LWW | 通过（真实 PostgreSQL）：并发改 `name` 与 `enabled` 两个字段均保留；过期 `name` + 较新 `cooldown` 只应用后者；同时间同内容幂等不增 `version`、不写 `change_log` |
| v0.20.0 时间戳精度 | 通过：`TestDecideField` 固定纳秒时间戳与微秒落库值的比较语义——不产生伪严格更新、生效时间截断到微秒、超前时钟兜底 |
| v0.20.0 非法组合 | 通过（真实 PostgreSQL + HTTP）：未知设备 `SCENE_DEVICE_NOT_FOUND`；目录类型不存在或类型不支持指标 `SCENE_METRIC_NOT_SUPPORTED`；RSSI/连续条数/比较符/多余字段/动作组合均 400 |
| v0.20.0 deviceMissing | 通过（真实 PostgreSQL）：设备删除后场景保留、`deviceMissing=true` 且场景 `version` 不变 |
| v0.20.0 软删与 tombstone | 通过（真实 PostgreSQL）：`deleted_at` 落库、`version+1`；GET/列表 404/空；`change_log` 的 delete payload 含 `deletedAt`；他人 `changes` 不可见、本人可见 upsert+delete |
| v0.20.0 删除与导出补齐 | 通过（真实 PostgreSQL）：账号删除后 `scenes` 计数为 0；导出含本人场景、不含他人场景/设备 |
| v0.20.0 契约一致性 | 通过：`TestOpenAPISceneContract` 比对 7 个 schema 的 properties/required 与模型 JSON 字段、kind 枚举、`Change.payload` oneOf 含 `Scene`、6 个错误码；契约版本 **0.16.0** |
| v0.20.0 CI | 通过：GitHub Actions [run 42176171594](https://github.com/platojobs/PJGoIoT/actions/runs/42176171594) 四 job 全绿（含 `Postgres 集成测试` 的 `TestPostgresSceneIntegration` 五个子测试与 `OpenAPI 契约校验`）；首次 run 42175459061 在 Linux 上暴露纳秒/微秒精度导致的「幂等补丁重复应用」，修复后复跑通过 |
| v0.21.0 容量基线 | 通过（本机真实 PostgreSQL 16.15，M1 Max 10 核 / 32 GB）：9 条路径 30s + 3s 预热，逐项 p50/p95/p99 与 RPS 见「容量与压测基线」；push 单用户 10/50/100 并发拐点与 5 用户合计 ≈158.9 RPS 均实跑。原始数据 [perf-baseline.md](docs/perf-baseline.md) |
| v0.21.0 多实例限流 | 通过：`RATE_LIMIT_BACKEND=postgres` 两实例交替 40 请求只放行 5 次（额度共享）；`memory` 对照放行 9 次（≈2×） |
| v0.21.0 备份与恢复 | 通过：`pg_dump -Fc` 9.958s / 138MB，`pg_restore -j 4` 15.403s，15 张表行数与 `hashtext` 校验和完全一致（含 199 万读数） |
| v0.21.0 迁移往返（有数据） | 通过：17→16→17 在 1.7 GB 真实库上执行；核心表计数不变，`scenes` 被 down 删除（60→0），已在 [preflight.md](docs/preflight.md) 记录为回滚风险 |
| v0.21.0 硬杀重启 | 通过：SIGKILL 后 advisory lock 0.4s 内释放、游标 seq 连续、push 恢复 0 错误；导出任务崩溃后停留 `pending`（未修复，列入上线前清单） |
| v0.21.0 CI | 通过：GitHub Actions [run 42181697527](https://github.com/platojobs/PJGoIoT/actions/runs/42181697527) 四 job 全绿（`构建与测试` 含真实 PG 集成与 OpenAPI 契约校验、`配置与脚本校验`、`构建镜像`、`压测门槛（smoke）` 1m9s） |
| v0.22.0 可见性下限性能 | 通过（本机真实 PG，1,112,155 行 `change_log` / 21,600 设备）：p50 62.77ms → 0.35ms、p95 66.28ms → 0.56ms、buffers 87,928 → 10；计划由 `change_log_pkey` 前缀扫描变为两次 `Index Only Scan ... LIMIT 1`。见 [perf-baseline.md §3.6](docs/perf-baseline.md) |
| v0.22.0 导出对账 | 通过（真实 PostgreSQL）：超时 pending → failed + 可读原因；未超时 pending 与 ready 不受影响；两个 Store 实例并发对账总条数=1、重复对账=0；HTTP 层 `GET /me/export/{id}` 返回 `failureReason` |
| v0.22.0 token 刷新与吊销 | 通过（真实 PostgreSQL + HTTP）：刷新后旧 token 401、新 token 可用、旧 token 二次刷新 401、新 token 可再次轮换；`GET /sessions` 标记 current；单会话吊销后 token 401 且刷新仍 401；跨用户 404、重复吊销幂等；删除账号后 tokens=0 |
| v0.22.0 契约一致性 | 通过：`openapi info.version=0.17.0`，新增 `/auth/refresh`、`/sessions`、`/sessions/{id}` 与 `failureReason`；`redocly lint` 0 error |
| v0.22.0 CI | 通过：GitHub Actions [run 42186201622](https://github.com/platojobs/PJGoIoT/actions/runs/42186201622) 四 job 全绿 |
| v0.23.0 推送令牌与队列 | 通过（真实 PostgreSQL）：`TestPostgresPushIntegration` 覆盖同用户同 token 注册幂等（唯一行、`active`）、跨用户隔离、非法参数 `PUSH_DEVICE_INVALID`、告警同事务入队、抢占 `attempts=1`/`sending`、dry-run `sent`+`dry_run=true`、退避 `next_attempt_at`、终态错误令牌 `inactive`、`sending` 超时回收、投递/失效设备清理 |
| v0.23.0 推送适配层与 worker | 通过（单元测试 / httptest）：FCM 分类（NotRegistered/InvalidRegistration→停用、503/429→可重试、401/403→终态）；APNs 分类（200/410/500/403，自生成 P-256 密钥）；网络错误可重试；worker 退避 30s→60s、最大次数失败、未知平台终态 |
| v0.23.0 真实 FCM/APNs | 未验证：无账号与凭据、未在真机收到通知；dry-run 仅记录「本应发送」的完整载荷 |
| v0.23.0 `/scenes` 分页 | 通过（真实 PostgreSQL + HTTP）：不传参数返回全量且响应无 `nextCursor`/`hasMore`；`limit=1` 游标遍历顺序与条数正确；`limit=0`/`abc`/非法 cursor 均 400 |
| v0.23.0 告警链路本地验证 | 通过（本机 Docker）：`scripts/verify-alerting.sh` ALL PASS——规则触发后 webhook 收到 1 条；重复告警去重为 1 次；silence 命中后 0 次。**只验证到 webhook 收到** |
| v0.23.0 真实告警通道 | 未验证：未接入电话/短信/邮件/Slack，未在生产部署 Alertmanager |
| v0.23.0 定时任务文件 | 已提供但未在生产安装：`deploy/systemd/pjgoiot-backup.{service,timer}`、`pjgoiot-cleanup.{service,timer}`、`deploy/cron/crontab.example`；安装与验证命令见 [runbook.md §6.2–§6.3](docs/runbook.md) |
| v0.23.0 契约一致性 | 通过：`openapi info.version=0.18.0`，新增 `/push/devices`、`/push/devices/{deviceId}`、`/admin/alerts` 与 `PushRegistration*`/`Alert*` schema；`/scenes` 增加 `limit`/`cursor`；`redocly lint` 0 error |
| v0.23.0 CI | 通过：GitHub Actions [run 42261314233](https://github.com/platojobs/PJGoIoT/actions/runs/42261314233) 四 job 全绿（`构建与测试` 含真实 PG 集成与 OpenAPI 契约校验、`配置与脚本校验` 含 `verify-alerting.sh` shellcheck、`构建镜像`、`压测门槛（smoke）` 1m8s） |
| v0.24.0 多家庭 sync 下限 | 通过（本机真实 PostgreSQL）：16 家庭、`change_log` 108 万行，floor p95 0.632 → **1.035ms**；主查询 cursor=0 p95 **104.9 → 2.4ms**、buffers 16,282 → 121；见 [perf-baseline.md §5](docs/perf-baseline.md) |
| v0.24.0 多家庭语义 | 通过（真实 PostgreSQL）：`TestChangesMultiHouseholdPaginationPostgres` 覆盖本人/家庭重复行去重、跨页严格递增、与旧 OR/IN 可见集合完全一致、`nextCursor` 正确 |
| v0.24.0 场景长稳 | 通过（本机 20 分钟）：GET 2,170,840 请求 p95 **20ms**/p99 39ms；PUT 1,106,619 请求 p95 **37ms**/p99 48ms；0 错误、全 200；连接池 20/20 饱和但未出现 503/超时；见 [perf-baseline.md §6](docs/perf-baseline.md) |
| v0.24.0 admin 双密钥轮换 | 通过（单元 + HTTP）：窗口内新旧密钥并存 200、未知/空 401、窗口过期后旧密钥 401 而当前仍 200、未配置 404 |
| v0.24.0 单测与 e2e | 通过：`go test ./...`（含 `TEST_DATABASE_URL` 真实 PG）**107 个顶层测试 + 194 子测试全 PASS，0 FAIL**；`scripts/stack-for-e2e.sh` + `seed-for-e2e.sh` 在 Docker PostgreSQL 上 `applied=1 skipped=0 conflicts=0`、readings=24 |
| v0.24.0 契约一致性 | 通过：`openapi info.version=0.19.0`；`/metrics` 新增连接池指标、admin 双密钥轮换语义写入契约；`redocly lint` **0 error**（14 条非阻断 warning） |
| v0.24.0 CI | 通过：GitHub Actions [run 42292106877](https://github.com/platojobs/PJGoIoT/actions/runs/42292106877) 四 job 全绿（`构建与测试` 含真实 PG 集成与 OpenAPI 契约校验、`配置与脚本校验` 含新脚本 shellcheck、`压测门槛（smoke）`、`构建镜像`） |
| v0.24.0 未验证 | 未验证：>40 并发 / 单段 >20 分钟 / 多实例 / 跨主机 TLS；把池排队推到请求预算以上观察 503；场景数上千；真实 FCM/APNs 状态同 v0.23.0 |
| `docker build` | 通过（GitHub Actions `构建镜像` job，v0.6.0 提交 8a5e4bd，run 41951827338） |
| GitHub Actions | 通过（v0.6.0：gofmt、vet、build、单元测试、迁移到 version=7、Postgres 集成测试、distroless 镜像构建；run 41951827338） |
| OpenAPI 静态校验 | 通过（v0.8.0）：`npx @redocly/cli lint api/openapi.yaml` → valid，0 errors（12 条非阻断 warning，多为 operation-4xx-response 提示）；`expiresAt`、`410 CursorExpired`、`503 Timeout`、`RESYNC_REQUIRED`/`TIMEOUT` 均进入契约且 `$ref` 可解析 |

集成测试运行方式：

```bash
TEST_DATABASE_URL='postgres://platojobs@127.0.0.1:5432/pjgoiot_test?sslmode=disable' \
  go test ./internal/store/postgres -run 'TestPostgresStoreIntegration|TestPostgresCatalogDeviceTypesIntegration|TestMigrationBackfillsLegacyDevices|TestHousehold|TestPostgresSceneIntegration|TestPostgresPushIntegration|TestPostgresScenePaginationIntegration|TestChangesMultiHousehold' -v
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

## 发布流程与 tag 纪律（v0.13.0 起）

### tag 纪律（硬约束）

**禁止移动、删除或强制重指任何已发布的 tag。** 已推送的 tag 一旦被 `git tag -f` / `git push --force` 改写，
其他人的本地 tag 与远端就会指向不同提交，CI 记录、发布物、回滚锚点也会对不上，且无法追溯到“当时到底发的是什么”。

- 发布后发现问题：**不要动旧 tag**，修复后打一个**递增版本**（如 `v0.13.0` 之后的修复是 `v0.13.1`，再往后 `v0.14.0`），
  在 README / runbook 里写清“修复版”，让旧 tag 保持原样。
- 只在 tag **尚未 push 到远端**时允许本地改正（`git tag -d` 后重打），一旦 `git push origin <tag>` 成功即视为发布。
- 不允许用 `git push --force origin refs/tags/<tag>`；仓库侧未配置 tag 保护时，这条只能靠流程自律，不得省略。
- 发布提交必须是 CI 全绿的提交；tag 打在 `main` 上，**annotated tag**（`git tag -a`），message 写版本与主题。

### 标准发布步骤

```bash
# 1. 全量验证（真实 PostgreSQL）
gofmt -l . | grep -v '^internal/store/postgres/sqlcgen/'   # 应无输出
go build ./... && go vet ./...
TEST_DATABASE_URL='postgres://platojobs@127.0.0.1:5432/pjgoiot_test?sslmode=disable' go test ./...
go run ./cmd/migrate -direction up                          # 迁移到最新
npx --yes @redocly/cli@latest lint api/openapi.yaml          # 0 error

# 2. 同步版本号：APP_VERSION / Dockerfile / Makefile / compose / runbook / README
#    契约版本在 api/openapi.yaml info.version，必须递增

# 3. 提交并推送，等 CI 全绿（lint job 必须实际执行）
git push origin main
gh run watch --exit-status

# 4. 打 annotated tag 并推送（不要动历史 tag）
git tag -a v0.17.0 -m 'v0.17.0: 反馈状态流转（用户关闭 + 运营受理 + 审计）'
git push origin v0.17.0
```

- CI 的 `OpenAPI 契约校验` 步骤（`npx @redocly/cli lint api/openapi.yaml`）是发布门槛，失败即不发 tag。
- workflow 文件变更需要 `workflow` scope 的凭据；HTTPS token 缺该 scope 时改用 SSH over 443 推送，
  不要绕过或删除 CI 步骤。

## 已知限制与下一步

- 场景已上云，但两端 App **尚未切换到 `/scenes`**：本地 SQLite/Room 仍是实际执行源，首次同步、冲突合并、`deviceMissing` 在 UI 的呈现均未在真实客户端验证；`schedule` 的本地调度也未跨时区/夏令时实测。
- 场景触发条件中的 `deviceType` 与 `metric` 校验依赖 `device_type_catalog` 当前内容；目录项被删除后，新写入会被拒绝，但已存在场景不会被回改（读取时只计算 `deviceMissing`，不校验指标是否仍受支持）。
- 场景列表 v0.23.0 起支持可选 `limit`/`cursor`；不传参数仍返回全量，客户端未接入分页也不受影响。
- **服务端推送链路已具备（v0.23.0）**：`push_devices` 表与注册/停用接口、告警同事务入队、后台 worker、FCM/APNs 适配层、无凭据 dry-run、指数退避与失效停用、cleanup 保留期。**真实 FCM/APNs 投递与真机收到通知未验证**（无账号/凭据/设备）；场景的 `notify` 仍由客户端本地通知执行。若产品承诺「设备离线也能收到提醒」，需补齐凭据后再验（见 [preflight.md](docs/preflight.md)）。
- `/sync/changes` 的 `MIN(seq)` 可见性下限已在 v0.22.0 改为索引探测（≈1.11M 行时 p95 0.56 ms / 10 buffers）；多家庭用户与生产级数据量仍待补测。详见 [perf-baseline.md §3.6](docs/perf-baseline.md)。
- 个人数据导出任务在 API 进程内以 goroutine 执行：进程崩溃后由**启动对账**把超过 5 分钟仍 `pending` 的任务标记为 `failed` 并写 `failureReason`；真实 SIGKILL 重启联调未重跑。见 [preflight.md](docs/preflight.md) §1.4.2。
- `cmd/loadgen` 新增的 changes / scenes / feedback / catalog 场景**未纳入 CI `-smoke`**（短跑造数与阈值缺失）；CI 仍只回归 devices / push / series 三场景。
- 本轮压测为**单机回环、30s/项**，未覆盖跨主机网络/TLS、长稳压、多实例混合负载与默认限流下的端到端吞吐；这些已列入 [preflight.md](docs/preflight.md) 的分级清单。
- `/auth/login` 仍为 501，只有匿名账号；后续可加账号密码 / OAuth 与匿名账号绑定。
- token 已有有效期（默认 180 天）与 `POST /auth/refresh` 滚动轮换、`GET /sessions` / `DELETE /sessions/{id}` 会话管理；两端 App 尚未接入刷新流程。客户端不得在 401 后静默换新匿名账号（会造成 userId 漂移）。`ADMIN_TOKEN` 支持当前 + 上一把密钥的窗口化轮换（v0.24.0），但运营后台的密钥下发与吊销仍依赖部署侧环境变量。
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
- 家庭结构变更（建房/改名/删房/删家庭/加入成员）不写 `change_log`，客户端需主动拉取 `/households*`；删房间导致设备 `roomId` 清空时不追加设备变更，成员端可能短暂不一致。
- 两位成员各自扫描同一台物理设备会产生两条独立设备记录；当前不做跨账号设备指纹合并。
- 家庭邀请码为 8 位、一次性、TTL 10 分钟、错 5 次作废；没有邮箱/短信二次验证，码的传递安全由客户端负责。
- `/metrics` 的业务计数为 `pg_class.reltuples` 近似值（依赖 autovacuum/ANALYZE），非精确行数；不应作为计费或审计依据。
- 完整栈已在本地 `docker compose up` 实测（Caddy 自签 HTTPS、非 root、只读根文件系统、启动迁移、卷持久化、docker 模式备份恢复均通过）；**真实域名 / ACME 证书 / 云主机仍未验证**，仓库未开通任何云账号。
- distroless 镜像没有 shell、`id`、`curl`，容器内排查请用 `docker top` / `docker inspect` / `docker compose exec api /api -healthcheck`；健康检查已使用二进制自带的 `-healthcheck`，无需额外工具。
- 备份脚本默认 `host` 模式，需要本机有 `pg_dump`/`pg_restore`；容器内执行用 `BACKUP_MODE=docker`/`RESTORE_MODE=docker`，本轮已实测。
- 告警规则已用容器内 `promtool check rules` + `check config` 做过语义校验；v0.23.0 提供 Alertmanager 配置与 `cmd/alert-webhook`，`scripts/verify-alerting.sh` 本机验证到 webhook 收到（含去重与静默）。**未接入真实电话/短信/邮件/Slack 通道，未在生产部署 Alertmanager**；阈值仍需按真实流量基线调整。
- `deploy/systemd/pjgoiot-{backup,cleanup}.{service,timer}` 与 `deploy/cron/crontab.example` 已提供，但**未在生产主机安装**；安装与验证命令见 [runbook.md §6.2–§6.3](docs/runbook.md)。

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
