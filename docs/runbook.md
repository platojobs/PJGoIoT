# PJGoIoT 运行手册（v0.23.0）

> 适用范围：`docker-compose.yml` + `deploy/Caddyfile` 的单机多实例部署形态
> （Caddy → `api1`/`api2` → PostgreSQL）。
> 本手册区分「已验证」与「未验证」，未实际执行的步骤不得当作已验证结论。

## 0. 本机验证状态（2026-11-06）

| 项目 | 状态 | 证据 |
| --- | --- | --- |
| `docker compose config` 静态解析 | **已验证** | Docker Compose v5.7.0，退出码 0 |
| `deploy/Caddyfile` 语法 | **已验证** | `caddy:2.8-alpine caddy validate` → `Valid configuration`；`caddy fmt --diff` 无差异 |
| `docker compose build` | **已验证** | 成功构建 `pjgoiot-api:0.7.1`（distroless，43.9MB） |
| 完整 `docker compose up -d` | **已验证** | postgres/api `healthy`、caddy `Up`；60s 内每 5s `docker compose ps` 均正常 |
| Caddy 自签 HTTPS → readiness | **已验证** | `curl -k https://localhost/api/v1/health` → 200，`store=postgres`，`version=0.7.1` |
| Caddy 拦截 `/metrics` | **已验证** | `HTTP/2 404`，body `not found` |
| 容器内 `/metrics` | **已验证** | `pjgoiot_edge` 网络内请求 `api:8080/metrics` → 200，含 `pjgoiot_http_requests_total` |
| 非 root / 只读根文件系统 | **已验证** | `User=nonroot:nonroot`、UID 65532、`ReadonlyRootfs=true`；`docker cp` 写入被拒绝 |
| `X-Request-Id` | **已验证** | 自定义值回显；未带时生成 UUID |
| 重启与卷持久化 | **已验证** | `restart api` 后设备仍在；`down`（无 `-v`）后卷保留 |
| 容器启动自动迁移 | **已验证** | 两次启动日志均 `version=7 dirty=false` |
| `backup.sh`（host + docker 模式） | **已验证** | 分别产出 `.dump.gz` + `.sha256`，校验通过 |
| `restore.sh`（host + docker 模式） | **已验证** | host 恢复到临时库、docker 模式恢复到运行中 compose 库；均 `version=7 dirty=f` 并输出行数 |
| 备份保留策略删除 | **已验证** | 构造 10 天前假备份，`RETENTION_DAYS=1` 后被删除 |
| Prometheus `promtool check rules` / `check config` | **已验证** | 容器内 `SUCCESS: 6 rules found`；config `valid prometheus config file syntax` |
| shellcheck | **CI 通过** | 本机未安装；GitHub `lint-configs` 中 shellcheck 执行通过 |
| shfmt | **未执行** | 本机与 CI runner 均无 `shfmt`；仅 `bash -n` |
| v0.8.0 保留期清理（readings / change_log / 幂等键 / token / 配对码） | **已验证** | 真实 compose 栈执行 `cleanup`：`readings=1628 / change_log=1 / idempotency=0 / tokens=0`；集成测试 `TestCleanupAndRetentionLifecycle` |
| v0.8.0 游标失效 410 | **已验证** | 旧游标 `seq0-1` → `410 RESYNC_REQUIRED`；新游标 `seq0` → 200 且剩 2 条；`/my/devices` 仍 3 台 |
| v0.8.0 token 过期 | **已验证** | 过期 token → 401；`/auth/anonymous` 返回 `expiresAt` 且与库内一致；`CleanupTokens` 后行数 0 |
| v0.8.0 压测（3 场景，30s） | **已验证** | 见 §9.7 与 README「容量与压测结果」；devices p95 215 ms、push p95 2 005 ms、series p95 127 ms，错误率均 0% |
| v0.9.0 e2e 栈脚本（起停/自动换端口/迁移/BASE_URL） | **已验证** | `scripts/stack-for-e2e.sh` 自动选端口 65500/65504，迁移 `version=8 dirty=false`，`/health` readiness 通过；`--stop` 清理容器与 `.e2e/` |
| v0.9.0 seed 幂等 | **已验证** | 连续执行两次，第二次 `skippedReadings=24`、`user_devices=1`、`readings=24`；真实型号与固件/配件来自迁移 seed |
| v0.9.0 curl 全链路（匿名→push→changes→series→metrics→export→注销） | **已验证** | 见 §9.8；注销 `DELETE /me` 204，旧 token 再访问 401 |
| v0.9.0 smoke 门槛与真实数字 | **已验证** | devices p95 28 ms、series p95 15 ms、push p95 417 ms，错误率 0%，`SMOKE PASS`（阈值 400/400/4 000 ms） |
| v0.9.0 dashboard / SLO | **本地已验证** | `scripts/validate-dashboard.sh` 通过（9 面板，指标名均在 Go 代码中）；SLO PromQL 未接入真实 Prometheus 长期抓取 |
| v0.10.0 push 整批单事务与计数守恒 | **已验证** | 真实 PG `TestPushTransactionBoundaries`：重复 id/键 400、读数校验失败整批回滚、5 个崩溃注入点零残留、`applied+skipped==len(readings)`、并发 push 游标无缺口 |
| v0.10.0 故障注入（停机/重启/慢查询/连接池） | **已验证** | 见 §9.9：PG 停 36s 全部 503 `UNAVAILABLE`；重启 0s 自愈；100ms `statement_timeout` → 503 `TIMEOUT`；池=2 高并发排队后 503 `TIMEOUT` |
| v0.10.0 迁移演练脚本 | **已验证** | `bash scripts/migrate-drill.sh` 三场景可重复执行：空库→9、000001 数据→9 回填、down 8 步→1 再 up→9；见 §9.9 |
| v0.11.0 双实例 + Caddy 负载均衡 | **已验证** | 20 次探测 `api1`/`api2` 各 10 次；响应头 `X-Instance` 正确；见 §9.10 |
| v0.11.0 跨实例 seq 与游标 | **已验证** | 30 个并发 push 打到两个实例并同时轮询 changes；31 条 seq 与库内集合一致、严格递增、无缺口 |
| v0.11.0 跨实例幂等 | **已验证** | 同 key 实例 A 首次 200，实例 B 重放 200 + `Idempotent-Replayed: true`；不同 body 409 `IDEMPOTENCY_CONFLICT` |
| v0.11.0 停止实例摘流 | **已验证** | `api1` 停止期间 160 个请求全部 200、0 个 502；重启后恢复接流量 |
| v0.11.0 滚动发布 | **已验证** | `scripts/rolling-restart.sh` 532 请求全 200，停机请求数 **0**（api1=239 / api2=293） |
| v0.11.0 并发迁移 | **已验证** | advisory lock 外部持有 5s，两实例同时启动均 5 261ms 后就绪，`version=10 dirty=false`、迁移表仅 1 行 |
| v0.11.0 共享限流 | **已验证** | memory 双实例 3/分钟额度实际放行 6；postgres 后端全局只放行 3，两实例均 429 |
| v0.12.0 家庭模型与回填 | **已验证** | 迁移 `000011` 在真实 PG 上 `up → down → up`：旧用户设备回填「我的家」、`room_id=NULL`；down 后家庭表与 2 个新列全部移除，再 up 结果一致 |
| v0.12.0 家庭权限与共享 | **已验证** | 真实 PG `TestHouseholdSharingPostgres`：邀请码一次性/跨家庭 404、member 改名 403、member 改他人设备 404、成员可读设备与读数、删房间清空 `room_id`、删家庭设备回个人家庭；HTTP fixture 测试 `TestHouseholdLifecycleAndPermissions` 同样通过 |
| v0.12.0 OpenAPI 契约 | **已验证** | `npx @redocly/cli lint api/openapi.yaml` → 0 error（13 条既有 warning） |
| v0.12.0 CI | **已验证** | GitHub Actions [run 42037369419](https://github.com/platojobs/PJGoIoT/actions/runs/42037369419)：构建与测试、配置与脚本校验、smoke、镜像构建 4 job 全绿 |
| v0.11.0 CI 补跑 | **已验证** | `v0.11-fix` [run 42037314989](https://github.com/platojobs/PJGoIoT/actions/runs/42037314989) 全绿；修复 shellcheck SC2317/SC2329 误报与未使用变量，v0.11.0 tag 已指向修复提交 |
| v0.13.0 成员与邀请码管理 | **已验证** | 真实 PG `TestHouseholdManagementPostgres` + HTTP `TestHouseholdMemberAndInvitationManagement`：移除成员后设备回个人家庭、owner 移除自己 400、member 越权 403、member 退出 204、owner 有其他成员退出 409、owner 独退删家庭、个人家庭退出 400；邀请码列表只返回 8 位哈希前缀与 `active/expired/locked` 状态、撤销后领取 404 |
| v0.13.0 房间整段重排 | **已验证** | `PUT /households/{id}/rooms/order` 成功写 0..n-1；缺项/多项/重复 id → 409 `CONFLICT`；事务内 `FOR UPDATE` 行锁；`GET /households` 的 `roomCount` 同步 |
| v0.13.0 迁移 000012 | **已验证** | 临时库 `up → down → up`：`household_invitations.id` 列与两个索引可正确增删，回填结果一致 |
| v0.13.0 OpenAPI 契约 | **已验证** | `npx @redocly/cli lint api/openapi.yaml` → **0 error**（13 条非阻断 warning）；契约版本 0.9.0 |
| v0.13.0 CI | **已验证** | GitHub Actions [run 42042030047](https://github.com/platojobs/PJGoIoT/actions/runs/42042030047) 全绿；`构建与测试` 中 `OpenAPI 契约校验` 步骤实际执行并通过，`Postgres 集成测试` 正则含 `TestHouseholdManagementPostgres` |
| v0.14.0 所有权转让 | **已验证** | 真实 PG `TestHouseholdOwnershipPostgres`：转让后单 owner、`owner_user_id` 同步；目标非成员 404 语义、个人家庭 400、转让给自己 400、member 调用 403；旧 owner 读 200 / 结构性写 403 / 可 `leave`；HTTP `TestHouseholdOwnershipTransfer` 同步通过 |
| v0.14.0 并发转让 | **已验证** | `TestHouseholdOwnershipTransferConcurrentPostgres`：两笔并发转让恰好 1 成功 1 `ErrForbidden`，`role='owner'` 行数恒为 1 |
| v0.14.0 可见性与错误码 | **已验证** | 被移除成员 token 读家庭/成员/房间/邀请码均 404；缺项重排 409 且 `error.code=ROOM_SET_MISMATCH`；`HouseholdMember.isSelf` 生效 |
| v0.14.0 OpenAPI 契约 | **已验证** | `npx @redocly/cli lint api/openapi.yaml` → **0 error**（13 条非阻断 warning）；契约版本 0.10.0 |
| v0.14.0 迁移 | **不涉及** | 本轮无 schema 变更，迁移版本仍为 `000012` |
| v0.14.0 CI | **已验证** | GitHub Actions [run 42051601765](https://github.com/platojobs/PJGoIoT/actions/runs/42051601765) 四 job 全绿；`Postgres 集成测试` 正则改为 `TestHousehold`，日志确认 `TestHouseholdOwnershipPostgres` 与 `TestHouseholdOwnershipTransferConcurrentPostgres` 实际执行；`OpenAPI 契约校验` 步骤实际执行 |
| v0.15.0 结构变更同步 | **已验证** | 真实 PG `TestHouseholdStructureChangesPostgres`：创建/改名/建房/重排/转让/移除各阶段写入 `household`/`room` 变更，两账号 `role` 视角正确，无关账号不可见，被移除者不再收到后续结构变更但仍可读离开前历史，新成员不回放历史房间结构 |
| v0.15.0 结构变更 410 | **已验证** | `TestHouseholdStructureChangesCursorExpiredPostgres`：仅由结构变更造成的缺口同样返回 `ErrCursorExpired`（HTTP 410 `RESYNC_REQUIRED`） |
| v0.15.0 未知 entity | **已验证** | `TestSyncChangesUnknownEntityIgnoredByOldClient`：含 `household`/`room`/未来 entity 的响应可被老客户端解析并忽略，`nextCursor` 正常推进 |
| v0.15.0 OpenAPI 契约 | **已验证** | `npx @redocly/cli lint api/openapi.yaml` → **0 error**（13 条非阻断 warning）；契约版本 0.11.0 |
| v0.15.0 迁移 | **不涉及** | 本轮无 schema 变更，迁移版本仍为 `000012` |
| v0.15.0 CI | **已验证** | GitHub Actions [run 42058675285](https://github.com/platojobs/PJGoIoT/actions/runs/42058675285) 四 job 全绿；日志确认 `TestHouseholdStructureChangesPostgres` / `...CursorExpiredPostgres` 实际 PASS，`OpenAPI 契约校验` 读到 `version: 0.11.0` |
| v0.16.0 反馈提交与幂等 | **已验证** | 真实 PG `TestPostgresFeedbackIntegration`：提交成功、空白/超长描述被约束拒绝、幂等重复提交同 `feedbackId` 且不新增、同幂等键不同内容冲突、初始状态 `submitted` |
| v0.16.0 本人列表与隔离 | **已验证** | 同一测试验证 A/B 列表互不可见、游标分页不重不漏、非法游标 `ErrBadCursor`；fixture HTTP 测试断言列表不含 `contact` |
| v0.16.0 校验与限流 | **已验证** | `TestFeedbackValidation` + `TestFeedbackRateLimitByUserAndIP` + 真实 PG HTTP 子测试：空白/超长/未知类型/非法平台 400，账号与 IP 双维度 429 `FEEDBACK_RATE_LIMITED`（带 `Retry-After`），未登录 401 |
| v0.16.0 契约一致性 | **已验证** | `TestOpenAPIFeedbackContract`：`FeedbackItem` 字段与 `model.Feedback` JSON 标签一致，kind/platform/status 枚举与实现一致，错误码枚举含 `FEEDBACK_RATE_LIMITED` |
| v0.16.0 迁移 | **已验证** | `000013_feedback` 在 `TestMigrationBackfillsLegacyDevices` 中验证 `up → down（000012）→ up`，索引 `feedback_user_created_idx` / `feedback_user_idem_idx` 存在 |
| v0.16.0 OpenAPI 契约 | **已验证** | `npx @redocly/cli lint api/openapi.yaml` → **0 error**；契约版本 0.12.0 |
| v0.16.0 CI | **已验证** | GitHub Actions [run 42077337348](https://github.com/platojobs/PJGoIoT/actions/runs/42077337348) 四 job 全绿；`TestPostgresFeedbackIntegration` 全部 PASS，OpenAPI lint 读到 `version: 0.12.0` |
| v0.17.0 状态机与审计 | **已验证** | 真实 PG `TestPostgresFeedbackLifecycleIntegration`：用户关闭本人、已关闭再改 409、跨用户读写 404、`submitted→received→submitted→closed`、`closed→received/submitted` 409；审计表逐条比对 actor/from/to/note |
| v0.17.0 幂等与 admin | **已验证** | 同状态重复 PATCH 返回 200、`updatedAt` 不变、审计只增 1 条、备注不被覆盖；HTTP 层 admin 未配置 404、错误凭据 401、客户端 token 不可替代 admin 凭据、正确凭据 200 |
| v0.17.0 HTTP 鉴权矩阵 | **已验证** | `TestFeedbackDetailAndClose` + `TestAdminFeedbackAuthMatrixAndTransitions`：三态鉴权、错误码与状态码一致、用户侧不回显 `contact`/`diagnostics`/`internalNote` |
| v0.17.0 契约一致性 | **已验证** | `TestOpenAPIFeedbackContract`：新 schema 字段与模型 JSON 标签一致、枚举对齐、admin 路径 `x-audience: internal` / `x-client-sdk: false`、错误码含 `FEEDBACK_INVALID_TRANSITION` |
| v0.17.0 迁移 | **已验证** | `000014_feedback_status_workflow` 在 `TestMigrationBackfillsLegacyDevices` 中验证 `up → down（000013）→ up`，`internal_note`、审计表、`feedback_status_created_idx` 均存在且回滚不破坏反馈表与用户数据 |
| v0.17.0 OpenAPI 契约 | **已验证** | `npx @redocly/cli lint api/openapi.yaml` → **0 error**（14 条非阻断 warning）；契约版本 0.13.0 |
| v0.17.0 CI | **已验证** | GitHub Actions [run 42163927951](https://github.com/platojobs/PJGoIoT/actions/runs/42163927951) 四 job 全绿；`TestPostgresFeedbackLifecycleIntegration` 全部子测试执行且 job 成功 |
| v0.18.0 目录一致性 | **已验证** | 真实 PG `TestPostgresCatalogDeviceTypesIntegration`：5 个目录项的 `metrics` / 特征 UUID / `gattServices` / `limitations` / `compatibility` 与 `internal/devicecatalog` 真值表逐项一致 |
| v0.18.0 ETag / 304 | **已验证** | 真实 PG + HTTP：匿名 200 带 ETag/Last-Modified；`If-None-Match` 命中 304；PUT 后版本自增、ETag 变化，旧 ETag 不再命中 304；fixture 层覆盖弱 ETag 与 `If-Modified-Since` |
| v0.18.0 管理与软删 | **已验证** | 真实 PG：PUT 后目录反映更新、版本 +1、审计 `upsert`；DELETE 后列表不再返回但 `deleted_at` 已写、审计 `delete`；重复 PUT/DELETE 与非法类型均 `ErrNotFound`；HTTP 层 admin 未配置 404、错误凭据 401、正确凭据 200/204 |
| v0.18.0 访问记录 | **已验证** | 真实 PG：同一用户同一版本重复记录只保留 1 行；匿名不写；无效 token 仍匿名 200 |
| v0.18.0 迁移 | **已验证** | 临时库 `up → down（000014）→ up`：`device_type_catalog` / 索引 / 审计 / 访问 / 版本表增删正确，重新 up 后 seed 5 行 |
| v0.18.0 契约一致性 | **已验证** | `TestOpenAPICatalogContract`：4 个 schema 的 properties/required 与模型一致、公开读取 `security: []`、304 响应存在、admin 路径 `x-audience: internal` / `x-client-sdk: false` |
| v0.18.0 OpenAPI 契约 | **已验证** | `npx @redocly/cli lint api/openapi.yaml` → **0 error**（14 条非阻断 warning）；契约版本 0.14.0 |
| v0.18.0 CI | **已验证** | GitHub Actions [run 42166636796](https://github.com/platojobs/PJGoIoT/actions/runs/42166636796) 四 job 全绿；`TestPostgresCatalogDeviceTypesIntegration` 各子测试执行且 job 成功 |
| v0.19.0 迁移 | **已验证** | 临时库 `up → down（000015）→ up`：两表与四个索引增删正确，重新 up 后迁移版本 16 |
| v0.19.0 OpenAPI 契约 | **已验证** | `npx @redocly/cli lint api/openapi.yaml` → **0 error**（14 条非阻断 warning）；契约版本 0.15.0 |
| v0.19.0 CI | **已验证** | GitHub Actions [run 42170187766](https://github.com/platojobs/PJGoIoT/actions/runs/42170187766) 四 job 全绿；`Postgres 集成测试` 执行 `TestPostgresAccountDeletionExportIntegration` 三个子测试，`OpenAPI 契约校验` 0 error |
| 生产域名 / ACME 证书 | **未验证** | 需要真实域名与 80/443 可达，本机未开通任何云账号 |
| systemd timer / 真实 cron | **未验证** | 仅提供示例文本，未在目标主机安装 |
| 数小时长稳压 / 多实例长稳 | **未验证** | 多实例已做并发/滚动/故障短时验证；未做数小时长稳与跨主机多实例 |
| Android/iOS 真机局域网访问 e2e 栈 | **未验证** | 栈绑定 127.0.0.1，仅验证模拟器可用路径；两个客户端仓库本轮未实际执行 |
| v0.17.0 运营工具联调 | **未验证** | 只提供 HTTP 契约与 curl 级验证，未接入真实运营后台；`ADMIN_TOKEN` 的下发、轮换与吊销流程未落地 |
| v0.17.0 admin 并发压测 | **未验证** | `SELECT ... FOR UPDATE` 保证同一反馈的审计顺序，但未做多实例并发 PATCH 压测；共享 token 下审计 actor 固定为 `admin`，无法区分具体操作人 |
| v0.17.0 审计留存 | **未验证** | 审计表随反馈级联删除，未设计独立归档 / 保留期策略；长期增长与清理未评估 |
| v0.18.0 客户端接入 | **未验证** | 两端 App 仍使用本地常量，尚未切到 `GET /catalog/device-types`；缓存失效、离线回退与灰度开关未在真实客户端验证 |
| v0.18.0 admin 并发 | **未验证** | 目录写入单事务 + 版本自增，但未做多实例并发 PUT 压测；共享 `ADMIN_TOKEN` 下审计 actor 固定为 `admin` |
| v0.18.0 访问日志分析 | **未验证** | 访问表只做 `(user, 版本)` 去重，未接入灰度分流逻辑；未评估长期行数增长与清理策略 |
| v0.19.0 删除令牌 | **已验证** | 真实 PG：错误令牌→`ErrDeletionTokenInvalid`、过期令牌→`ErrDeletionTokenExpired`、有效令牌删除成功；缺令牌由 HTTP 层 400 拦截；令牌只存哈希，新申请作废旧令牌 |
| v0.19.0 逐表清空 | **已验证** | 真实 PG：删除前 14 张关联表全部非空，删除后 `tokens`/`user_devices`/`readings`/`change_log`/`idempotency_keys`/`pairing_codes`/`feedback`/`feedback_status_events`/目录访问/导出任务/家庭/成员/房间/邀请逐表计数为 0，`users` 为 0 |
| v0.19.0 旧 token 与重放 | **已验证** | 真实 PG：删除后 `UserIDByTokenHash` 返回 `ErrUnauthorized`；同一令牌重放返回 `ErrNotFound`（404 语义），无部分删除；清理墓碑后重放返回 `ErrDeletionTokenInvalid` |
| v0.19.0 owner 拦截与转让 | **已验证** | 真实 PG + HTTP：owner 家庭有成员时 `ErrAccountDeletionBlocked`/409，账号与家庭完整保留；转让后同一令牌可删除，家庭归新 owner 且成员数 1 |
| v0.19.0 异步导出与隔离 | **已验证** | 真实 PG：`CreateExportTask`→`BuildPersonalDataExport`→`FinishExportTask`；读数聚合 `count/min/max/avg/firstAt/lastAt`；产物不含他人设备/读数/用户 id、不含运营 `internal_note`；家庭仅含本人角色与房间 |
| v0.19.0 下载链接一次性 | **已验证** | 真实 PG + HTTP：pending 任务签发下载令牌返回 `ErrExportTaskNotReady`/409；首次消费成功，重放 410，错误令牌 403，过期 410；任务过期清理后查询 404 |
| v0.19.0 HTTP 错误码矩阵 | **已验证** | `TestExportAsyncAndDownload` / `TestDeleteMeRequiresDeletionToken` / `TestDeleteMeBlockedByOwnedHousehold`：未认证 401、缺/错/过期令牌 400/403/410、冲突 409、重放 404、他人任务 404、下载 403/410 |
| v0.19.0 迁移与清理 | **已验证** | 真实 PG 执行 `000016_account_deletion_export` up；`cmd/cleanup` 新增 `account_deletion_tokens` 与 `personal_data_export_tasks` 清理；dry-run 输出两个新计数 |
| v0.19.0 契约一致性 | **已验证** | `TestOpenAPIDeletionExportContract`：7 个新 schema 的 properties/required 与模型 JSON 标签逐项一致；`DELETE /me` 与下载接口 `security: []`；错误码枚举含 7 个新码；契约版本 0.15.0 |
| v0.20.0 迁移 | **已验证** | 真实 PG `TestMigrationBackfillsLegacyDevices`：`up → down（000016）→ up`；`scenes` 表与 `scenes_user_updated_idx` / `scenes_user_deleted_idx` 增删正确，回滚不破坏用户数据，重新 up 后迁移版本 17 |
| v0.20.0 场景 CRUD 与本人限定 | **已验证** | 真实 PG `TestPostgresSceneIntegration`：创建/列表/详情/更新/删除；跨用户读改删 404、列表为空；`household_id` 为 NULL 的 `scene` 变更不进入他人 `changes` |
| v0.20.0 字段级 LWW | **已验证** | 真实 PG：并发修改 `name` 与 `enabled` 两个字段都保留（`version=3`）；过期 `name` + 较新 `cooldown` 只应用后者；同时间同内容 no-op 不增版本、不写 `change_log`（场景变更计数 4） |
| v0.20.0 时间戳精度 | **已验证** | `TestDecideField`：纳秒时间戳与已落库微秒值比较不产生「伪严格更新」；生效时间截断到微秒；超前时钟兜底 |
| v0.20.0 非法组合 | **已验证** | 真实 PG + fixture HTTP：未知设备 `SCENE_DEVICE_NOT_FOUND`；目录类型不存在/类型不支持指标 `SCENE_METRIC_NOT_SUPPORTED`；RSSI、比较符、连续条数、多余字段、动作数量/重复/字段组合均 400 且错误码明确 |
| v0.20.0 deviceMissing | **已验证** | 真实 PG：设备删除后场景仍可读、`deviceMissing=true`、场景 `version` 不变；列表仍返回该场景 |
| v0.20.0 软删 tombstone | **已验证** | 真实 PG：`deleted_at` 落库且 `version+1`；GET/列表 404/空；`change_log` delete payload 含 `deletedAt`；本人 `changes` 可见 upsert+delete，他人不可见 |
| v0.20.0 删除与导出补齐 | **已验证** | 真实 PG：账号删除后 `scenes` 计数 0；导出 `scenes` 只含本人场景（含 tombstone），不含他人场景名/设备/用户 id |
| v0.20.0 契约一致性 | **已验证** | `TestOpenAPISceneContract`：7 个 schema properties/required 与模型 JSON 标签逐项一致；trigger/action kind 枚举一致；`Change.payload` oneOf 含 `Scene`；错误码枚举含 6 个新码；契约版本 0.16.0 |
| v0.20.0 CI | **已验证** | GitHub Actions [run 42176171594](https://github.com/platojobs/PJGoIoT/actions/runs/42176171594) 四 job 全绿（`Postgres 集成测试` 含 `TestPostgresSceneIntegration` 五个子测试，`OpenAPI 契约校验` 0 error）；首次 run 42175459061 因纳秒/微秒精度判定失败，修复后通过 |
| v0.20.0 客户端接入 | **未验证** | 两端 App 仍以本地 SQLite/Room 为执行源，尚未切到 `/scenes`；首次同步、冲突合并与 `deviceMissing` UI 呈现未在真机验证 |
| v0.20.0 定时调度 | **未验证** | `schedule` 的时区/夏令时在真实客户端本地调度未实测；服务端只做存储与结构校验 |
| v0.20.0 admin 并发 | **未涉及** | 本轮无 admin 接口；场景写入按用户串行化（与 push 共用 advisory lock），未做跨用户高并发压测 |
| v0.21.0 容量基线 | **已验证（本机）** | 真实 PG 16.15：9 条路径 p50/p95/p99 + RPS 全部实跑；push 单用户 10/50/100 并发拐点与 5 用户合计 ≈158.9 RPS；数据规模、机器规格与原始表见 [`perf-baseline.md`](perf-baseline.md) |
| v0.21.0 多实例限流 | **已验证** | `RATE_LIMIT_BACKEND=postgres` 两实例交替 40 请求放行 5 次（额度共享）；`memory` 对照放行 9 次（≈2×）。注意 `docker-compose.yml` 默认仍是 `memory` |
| v0.21.0 备份与恢复 | **已验证（本机）** | `pg_dump -Fc` 9.958s / 138MB；`pg_restore -j 4` 15.403s；15 张表行数与 `hashtext` 校验和完全一致。生产库未执行 |
| v0.21.0 迁移往返（有数据） | **已验证（本机）** | 1.7GB 真实库 17→16→17：核心表计数不变；`scenes` 被 down 删除（60→0）。生产回滚前必须先导出场景或只前滚 |
| v0.21.0 硬杀重启 | **已验证（本机）** | SIGKILL 后 advisory lock 0.4s 内释放、重启后 push 0 错误、游标 seq 连续；导出任务崩溃后仍 `pending`（v0.21.0 无对账，v0.22.0 已修复） |
| v0.21.0 生产环境 | **未验证** | 真实域名/ACME、告警真实投递、备份与清理定时器、客户端真机、BLE 真机、签名与上架、推送通道，均未执行。分级清单见 [`preflight.md`](preflight.md) |
| v0.21.0 CI | **已验证** | GitHub Actions [run 42181697527](https://github.com/platojobs/PJGoIoT/actions/runs/42181697527) 四 job 全绿（`构建与测试` 含真实 PG 集成与 OpenAPI 契约校验、`配置与脚本校验`、`构建镜像`、`压测门槛（smoke）`） |
| v0.22.0 可见性下限 | **已验证（本机）** | 1,112,155 行 `change_log` / 21,600 设备：p95 66.28ms → **0.56ms**、buffers 87,928 → **10**；计划从 `change_log_pkey` 前缀扫描改为两次 `Index Only Scan ... LIMIT 1`。见 [`perf-baseline.md`](perf-baseline.md) §3.6 |
| v0.22.0 导出对账 | **已验证（真实 PG 测试）** | 启动把创建 >5 分钟仍 `pending` 的任务标记 `failed` 并写原因；ready/新任务不受影响；两实例并发对账总条数=1、重复对账=0；`GET /me/export/{id}` 返回 `failureReason`。真实 SIGKILL 联调未重跑 |
| v0.22.0 token 生命周期 | **已验证（真实 PG 测试）** | 刷新轮换后旧 token 401、新 token 可用、旧 token 二次刷新仍 401；`GET /sessions` 列出有效会话并标记 current；单会话吊销后 token 401、跨用户 404、重复吊销幂等；删除账号 tokens 清空 |
| v0.22.0 客户端接入 token | **未验证** | 两端 App 尚未接入 `/auth/refresh` 与 `/sessions`；刷新失败后的重新登录体验未测 |
| v0.22.0 CI | **已验证** | GitHub Actions [run 42186201622](https://github.com/platojobs/PJGoIoT/actions/runs/42186201622) 四 job 全绿（`构建与测试` 含真实 PG 集成与 OpenAPI 契约校验、`配置与脚本校验`、`构建镜像`、`压测门槛（smoke）`） |
| v0.22.0 生产环境 | **未验证** | 与 v0.21.0 相同：域名/ACME、告警真实投递、定时器、真机、商店、推送通道均未执行 |
| v0.23.0 推送令牌与队列 | **已验证（真实 PG 测试）** | `TestPostgresPushIntegration`：同用户同 token 注册幂等（单行、`active`）、跨用户隔离、非法参数 `PUSH_DEVICE_INVALID`；告警同事务入队；抢占 `attempts=1`/`sending`；dry-run `sent`+`dry_run=true`；退避写入 `next_attempt_at`；终态错误令牌 `inactive`；`sending` 超时回收；两个 cleanup 计数 |
| v0.23.0 推送适配层与 worker | **已验证（单元测试 / httptest）** | FCM 分类（NotRegistered/InvalidRegistration → 停用，503/429 → 可重试，401/403 → 终态）；APNs 分类（200/410/500/403，自生成 P-256 密钥）；网络错误可重试；worker 退避 30s→60s、达到最大次数后失败、未知平台终态 |
| v0.23.0 真实 FCM/APNs | **未验证** | 无账号与凭据；未在真机收到任何通知；dry-run 仅证明「本应发送」载荷已记录 |
| v0.23.0 `/scenes` 分页 | **已验证（真实 PG + HTTP）** | 不传参数返回全量且响应无 `nextCursor`/`hasMore`；`limit=1` 游标遍历顺序与条数正确；`limit=0`/`abc`/非法 cursor 400 |
| v0.23.0 告警链路（本地） | **已验证（本机 Docker）** | `scripts/verify-alerting.sh` ALL PASS：规则触发→Alertmanager→webhook 收到 1 条；重复告警去重为 1 次；silence 命中后 0 次。**只验证到 webhook 收到** |
| v0.23.0 真实告警通道 | **未验证** | 未接入电话/短信/邮件/Slack；未在生产部署 Alertmanager |
| v0.23.0 定时任务文件 | **已提供但未安装** | `deploy/systemd/*.{service,timer}` 与 `deploy/cron/crontab.example` 已入库；本机无生产宿主，未安装、未由 timer 触发过 |
| v0.23.0 契约与 CI | **已验证** | `openapi info.version=0.18.0`、redocly lint 0 error；GitHub Actions [run 42261314233](https://github.com/platojobs/PJGoIoT/actions/runs/42261314233) 四 job 全绿（含 `Postgres 集成测试`、新增脚本 shellcheck、`构建镜像`、`压测门槛（smoke）` 1m8s） |

## 1. 部署前检查清单

- [ ] 已确定域名 `DOMAIN`，DNS A/AAAA 指向本机，80/443 可从公网访问（仅本机自签可跳过）。
- [ ] 已生成强随机数据库密码，写入 `.env`（**不要提交到 git**）：
  `POSTGRES_PASSWORD=$(openssl rand -hex 24)`
- [ ] 已确认磁盘空间：`pgdata` 至少预留数据库大小 2 倍；`backups/` 独立磁盘或对象存储。
- [ ] 已确认备份目录权限：`umask 077`，仅运维账号可读。
- [ ] 已阅读 §5 回滚风险：**迁移 down 可能丢列/丢数据**，生产优先用镜像回滚 + 前滚兼容。
- [ ] 已配置监控抓取（内网 `api:8080/metrics`），并导入 `deploy/prometheus/alerts.yml`。
- [ ] 已配置每日清理任务（§6）：`readings`/`change_log` 90 天、幂等键 7 天、过期 token 与配对码；未配置则所有表无限增长。
- [ ] 防火墙只开放 80/443；数据库端口绑定 `127.0.0.1:55432`，不得改为 `0.0.0.0`。

## 2. 首次部署

```bash
# 1) 准备环境变量（compose 自动读取同目录 .env）
cat > .env <<'EOF'
DOMAIN=localhost
POSTGRES_PASSWORD=change-me-strong-password
APP_VERSION=0.14.0
TOKEN_TTL_DAYS=180
METRICS_ENABLED=true
RUN_MIGRATIONS_ON_BOOT=true
EOF

# 2) 静态校验编排
docker compose config --quiet

# 3) 构建并启动（postgres 健康后 api1/api2 才启动；两个实例启动时都会尝试迁移）
docker compose up -d --build

# 4) 等待两个实例健康（readiness 含数据库 Ping）
docker compose ps
docker compose exec api1 /api -healthcheck
docker compose exec api2 /api -healthcheck

# 5) 验证 HTTPS 终止与反向代理（自签证书用 -k；X-Instance 显示实际实例）
curl -k https://localhost/api/v1/health
curl -k https://localhost/healthz
curl -k -o /dev/null -w '%{http_code}\n' https://localhost/metrics
```

首次启动行为：

- `postgres` 先拉起并通过 `pg_isready` 健康检查；
- `api1`/`api2` 仅在 `postgres` healthy 后启动，并各执行迁移（`RUN_MIGRATIONS_ON_BOOT=true`）；
  两个实例同时启动时由 golang-migrate 的 PG advisory lock 串行化，后到者等待，不会重复建表；
- 迁移失败（含 dirty）→ 进程非零退出，`restart: unless-stopped` 会重试；**不会在旧 schema 上提供服务**；
- `caddy` 等两个实例都健康后才启动，避免 502。

健康探针区别：

| 端点 | 用途 | 是否访问数据库 | 失败含义 |
| --- | --- | --- | --- |
| `GET /healthz` | liveness（进程存活） | 否 | 进程卡死/未启动，应重启容器 |
| `GET /api/v1/health` | readiness（可服务） | 是（2s 超时 Ping） | 数据库不可用，摘流量但不要重启 |

## 3. 升级流程（滚动发布）

1. **备份**（必须先做）：`./deploy/scripts/backup.sh`
2. **预检迁移**：只跑迁移、不启动服务：
   `docker compose run --rm --no-deps --entrypoint /migrate api1 -direction up`。
3. **构建新镜像**：`APP_VERSION=0.14.0 docker compose build`
4. **逐实例滚动替换**：
   ```bash
   bash scripts/rolling-restart.sh
   ```
   脚本对 `api1`、`api2` 依次执行：`stop`（SIGTERM → readiness 503 → Caddy 摘流 →
   继续服务 `SHUTDOWN_DRAIN_SECONDS`）→ `start` → 等 healthy → 观察 3s 重新接流量。
   全程持续打请求并统计非 200 数量；v0.11.0 实测停机请求数为 0。
   也可手工执行同样顺序：
   ```bash
   docker compose stop api1 && docker compose start api1
   docker compose exec api1 /api -healthcheck
   docker compose stop api2 && docker compose start api2
   docker compose exec api2 /api -healthcheck
   ```
5. **验证**：
   - `docker compose exec api1 /api -healthcheck`、`docker compose exec api2 /api -healthcheck`
   - `curl -k https://$DOMAIN/api/v1/health`（多打几次，看 `X-Instance` 是否两个实例都能命中）
   - `curl -k https://$DOMAIN/healthz`
   - 抽查 `GET /my/devices`、`POST /sync/push`（用测试账号）；
   - 查看 `/metrics` 的 5xx 与延迟是否正常。
6. **观察 15 分钟**：告警 `ApiHigh5xxRate`、`ApiDBErrorsIncreasing` 无触发后再结束变更窗口。

升级注意：

- 迁移是**前滚**设计；新版本必须先迁移再切流量。
- `RUN_MIGRATIONS_ON_BOOT=false` 仅用于「迁移由独立 Job 执行」的编排；此时必须保证迁移先完成。
- 两个实例版本不一致期间（滚动窗口内），新旧代码必须兼容同一 schema；本项目迁移只加列/加表，
  但新增业务字段时仍需按“先迁移、后切流量”的顺序。
- `RATE_LIMIT_BACKEND=postgres` 时，滚动期间限流额度不会翻倍（计数在数据库）；`memory` 时
  滚动窗口内额度会短暂按新旧实例组合叠加。

### 3.1 发布 tag 纪律（v0.13.0 起执行）

**已发布的 tag 一律不得移动、删除或强制重指。** 曾经出现过用 `git tag -f` 把已推送的 `v0.11.0`
改指到修复提交的情况：远端与本地 tag 随即不一致，CI 记录、发布物与回滚锚点都对不上。

- tag 一旦 `git push origin <tag>` 成功即视为发布，此后只增不改；
- 发布后发现问题，**修复版使用递增版本号**（`v0.13.0` 的修复版是 `v0.13.1`，再往后 `v0.14.0`），
  旧 tag 保持原样；
- 禁止 `git push --force origin refs/tags/<tag>`；只在 tag 尚未推送时允许本地 `git tag -d` 重打；
- tag 必须打在 CI 全绿的 `main` 提交上，使用 annotated tag（`git tag -a`）；
- 发布前确认：`gofmt`/`go build`/`go vet`/真实 PG `go test ./...` 全绿、迁移 `up → down → up`、
  `npx @redocly/cli lint api/openapi.yaml` 0 error、CI 的 `OpenAPI 契约校验` 步骤实际执行。

## 4. 回滚流程

### 4.1 只回滚镜像（推荐）

```bash
# 将 compose 中 api1/api2 镜像/构建版本改回上一版本号后，逐实例滚动
bash scripts/rolling-restart.sh
docker compose exec api1 /api -healthcheck
docker compose exec api2 /api -healthcheck
```

前提：**新版本迁移没有破坏旧代码依赖的列/语义**。本项目 000003–000008 迁移均为加列/加表/加索引，
旧版本代码可继续读取；若未来出现删列/改类型迁移，必须评估后再回滚。

### 4.2 回滚迁移（高风险，谨慎）

```bash
# 只回退一步（示例：9 → 8）
docker compose run --rm --entrypoint /migrate api1 -direction down
```

风险：

- `000006` down 会 **DROP readings 表**，`000007` down 会 **DROP pairing_codes 表**——数据不可恢复；
- `000008` down 会**删除 `tokens.expires_at` 列**，旧 token 从此不再有有效期校验；只回滚镜像时该列可以保留（旧代码忽略它）；
- 回滚后旧镜像可能因缺少列而报错；
- 若迁移处于 `dirty` 状态，**不要盲目 down**：先 `SELECT * FROM schema_migrations` 确认版本，
  修复失败语句或用备份恢复，再清理 dirty 标记。
- 正确顺序：停 API → 备份 → 评估 down 影响 → 执行 down → 启动旧镜像 → 验证。

### 4.3 灾难恢复

数据库损坏/误删时走 §6 的恢复流程，而不是迁移回滚。

## 5. 备份与恢复演练

### 5.1 备份

```bash
# 方式 A：宿主机执行（需 postgresql-client）
DATABASE_URL='postgres://pjgoiot:<pass>@127.0.0.1:55432/pjgoiot?sslmode=disable' \
BACKUP_DIR=./backups RETENTION_DAYS=14 \
./deploy/scripts/backup.sh

# 方式 B：容器内执行（无需宿主机 pg_dump，要求 compose 栈已启动）
BACKUP_MODE=docker BACKUP_DIR=./backups RETENTION_DAYS=14 \
  ./deploy/scripts/backup.sh
```

产物：

- `backups/pjgoiot-<UTC时间戳>.dump.gz`（`pg_dump -Fc` + gzip）
- `backups/pjgoiot-<UTC时间戳>.dump.gz.sha256`

失败（`pg_dump` 非零、空文件、缺工具、校验和工具缺失）都会非零退出，可直接接入告警。

### 5.2 恢复（演练建议用临时库）

```bash
# 方式 A：host 模式，建临时库避免覆盖生产
createdb -h 127.0.0.1 -p 55432 -U pjgoiot pjgoiot_restoretest
DATABASE_URL='postgres://pjgoiot:<pass>@127.0.0.1:55432/pjgoiot_restoretest?sslmode=disable' \
./deploy/scripts/restore.sh --confirm --file ./backups/pjgoiot-<ts>.dump.gz

# 方式 B：docker 模式，直接恢复到 compose 的 postgres 库
# 建议先 `docker compose stop api1 api2`，恢复完成后再 start，避免恢复期间业务写入
RESTORE_MODE=docker ./deploy/scripts/restore.sh --confirm --file ./backups/pjgoiot-<ts>.dump.gz
```

脚本行为：

1. 无 `--confirm` 直接拒绝（退出码 2）；
2. 校验 `.sha256`，不一致立即中止；
3. `pg_restore --clean --if-exists` 恢复；
4. 查询 `schema_migrations` 输出 `version/dirty`；
5. 输出 `users/user_devices/readings/change_log/pairing_codes` 行数摘要。

演练验收：行数摘要与备份时点一致、`dirty=false`、API 指向临时库时 `/api/v1/health` 为 200。
本轮已在真实 compose 栈上实测 docker 模式：备份 8 193 bytes，恢复后 `version=7 dirty=f`、行数 `1/1/0/1/0`，API 重新 `healthy` 且原设备仍可查。

## 6. 清理任务（保留期与游标失效）

所有保留期删除都依赖 `cmd/cleanup`，**不是** API 自动执行。一次运行处理七类数据：

| 数据 | 默认保留 | 参数 | 环境变量（compose） |
| --- | --- | --- | --- |
| `readings` | 90 天（按 `recorded_at`） | `-retention-days` | `READING_RETENTION_DAYS` |
| `change_log` | 90 天（按 `server_ts`） | `-change-log-retention-days` | `CHANGE_LOG_RETENTION_DAYS` |
| `idempotency_keys` | 7 天（按 `created_at`） | `-idempotency-retention-days` | `IDEMPOTENCY_RETENTION_DAYS` |
| `tokens` | 过期即删（`expires_at < now()`） | 无 | 无 |
| `pairing_codes` | 过期即删（`expires_at < now()`） | 无 | 无 |
| `push_deliveries`（v0.23.0） | 7 天（按 `created_at`，仅 `sent`/`failed`） | `-push-delivery-retention-days` | 无（仅命令行参数） |
| `push_devices` 的 `inactive` 行（v0.23.0） | 30 天（按 `updated_at`） | `-push-device-retention-days` | 无（仅命令行参数） |

`-operation-timeout`（默认 30s）为每类操作单独的超时；`-dry-run` 只打印时间边界与计数。

### 6.1 compose profile

```bash
# 默认 90/90/7 天 + 过期 token/配对码
docker compose --profile maintenance run --rm cleanup

# 自定义保留期（也可写入 .env）
CHANGE_LOG_RETENTION_DAYS=90 IDEMPOTENCY_RETENTION_DAYS=7 \
  docker compose --profile maintenance run --rm cleanup

# 只看会删什么（不删数据）
docker compose --profile maintenance run --rm cleanup -dry-run
```

> 生产上限建议：`change_log` 保留期**不要短于客户端最长离线时间**。客户端离线超过保留期后
> 再同步会收到 `410 RESYNC_REQUIRED`（§7.6），必须全量重拉。

### 6.2 systemd timer（文件已提供，未在生产安装）

仓库内单元文件：

| 文件 | 作用 |
| --- | --- |
| [`deploy/systemd/pjgoiot-cleanup.service`](../deploy/systemd/pjgoiot-cleanup.service) | `ExecStart=/opt/pjgoiot/bin/cleanup`，`EnvironmentFile=/etc/pjgoiot/cleanup.env` |
| [`deploy/systemd/pjgoiot-cleanup.timer`](../deploy/systemd/pjgoiot-cleanup.timer) | 每日 UTC 04:17，`Persistent=true`，随机延迟最多 10 分钟 |
| [`deploy/systemd/pjgoiot-backup.service`](../deploy/systemd/pjgoiot-backup.service) | `ExecStart=/opt/pjgoiot/deploy/scripts/backup.sh`，`EnvironmentFile=/etc/pjgoiot/backup.env` |
| [`deploy/systemd/pjgoiot-backup.timer`](../deploy/systemd/pjgoiot-backup.timer) | 每日 UTC 03:30（早于清理），`Persistent=true`，随机延迟最多 15 分钟 |

安装步骤（在目标主机执行，**需 root**）：

```bash
# 1) 目录与账号
sudo useradd --system --home /opt/pjgoiot --shell /usr/sbin/nologin pjgoiot
sudo mkdir -p /etc/pjgoiot /var/backups/pjgoiot /var/log/pjgoiot
sudo chown -R pjgoiot:pjgoiot /opt/pjgoiot /var/backups/pjgoiot /var/log/pjgoiot

# 2) 环境变量（权限 0600；至少 DATABASE_URL / BACKUP_DIR）
sudo install -m 600 -o pjgoiot -g pjgoiot /dev/null /etc/pjgoiot/cleanup.env
sudo install -m 600 -o pjgoiot -g pjgoiot /dev/null /etc/pjgoiot/backup.env
# 编辑两个文件后写入：DATABASE_URL='postgres://...?sslmode=disable' / BACKUP_DIR=/var/backups/pjgoiot

# 3) 安装单元并校验
sudo cp deploy/systemd/pjgoiot-*.service deploy/systemd/pjgoiot-*.timer /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now pjgoiot-backup.timer pjgoiot-cleanup.timer

# 4) 验证
systemctl list-timers --all | grep pjgoiot          # NEXT/LAST 时间应出现
sudo systemctl start pjgoiot-cleanup.service        # 立即跑一次
journalctl -u pjgoiot-cleanup.service -n 30 --no-pager   # 应输出六/七类计数
sudo systemctl start pjgoiot-backup.service
journalctl -u pjgoiot-backup.service -n 20 --no-pager
ls -lh /var/backups/pjgoiot/                        # 应出现 .dump.gz 与 .sha256
```

> **状态**：上述文件已入库，但**未在任何生产机安装**，也没有由 timer 触发过的真实记录。

### 6.3 容器环境 cron（文件已提供，未在生产安装）

[`deploy/cron/crontab.example`](../deploy/cron/crontab.example) 提供主机 cron +
`docker compose` 方案（无需在主机装 `pg_dump`）：

```cron
CRON_TZ=UTC
30 3 * * * cd /opt/pjgoiot && docker compose exec -T postgres pg_dump -U pjgoiot -d pjgoiot --format=custom --no-owner --no-privileges | gzip -9 > /var/backups/pjgoiot/pjgoiot-$(date -u +\%Y\%m\%dT\%H\%M\%SZ).dump.gz 2>>/var/log/pjgoiot/backup.log
17 4 * * * cd /opt/pjgoiot && docker compose run --rm --entrypoint /cleanup api >> /var/log/pjgoiot/cleanup.log 2>&1
```

安装与验证：

```bash
sudo mkdir -p /var/log/pjgoiot /var/backups/pjgoiot
sudo chown -R pjgoiot:pjgoiot /var/log/pjgoiot /var/backups/pjgoiot
crontab -e        # 粘贴上面两行（注意 CRON_TZ 支持情况）
crontab -l | grep pjgoiot                       # 应看到两行
# 立即验证命令本体可用（不等到点）：
cd /opt/pjgoiot && docker compose run --rm --entrypoint /cleanup api -dry-run
```

> busybox cron 不支持 `CRON_TZ`，需按本地时区换算；两种方案都要求 cron 用户能执行
> `docker`（加入 `docker` 组）。**状态**：已提供，未在生产安装。


### 6.4 执行结果与核对

正常输出（真实执行）：

```text
清理完成 readings_deleted=1628 readings_cutoff=... retention_days=90
change_log_deleted=1 change_log_cutoff=... change_log_retention_days=90
idempotency_keys_deleted=0 idempotency_cutoff=... idempotency_retention_days=7
tokens_deleted=0 pairing_codes_deleted=0
```

核对 SQL：

```sql
-- 最老/最新 seq：判断客户端游标是否会过期
SELECT min(seq), max(seq) FROM change_log;
-- 清理后各类行数
SELECT count(*) FROM change_log;
SELECT count(*) FROM idempotency_keys;
SELECT count(*) FROM tokens WHERE expires_at < now();
-- 设备与读数不应被 cleanup 删除（读数仅按保留期删）
SELECT count(*) FROM user_devices;
```

## 7. 常见故障

### 7.1 API 启动即退出 / 数据库连接失败

- 看 `docker compose logs api1 api2`，确认 `DATABASE_URL` 主机名是 `postgres`（compose 服务名），不是 `localhost`；
- `pg_isready` 是否健康：`docker compose ps postgres`；
- 密码含特殊字符需 URL 编码；`sslmode=disable` 仅用于内网；
- 迁移失败会打印 `启动迁移失败`，进程退出是预期行为。

### 7.2 迁移 dirty

- 现象：启动日志 `Dirty database version N. Fix and force version.`；
- 处理：`SELECT * FROM schema_migrations;` 确认实际 schema；修复失败 SQL 或用备份恢复；
- 清理 dirty（确认 schema 已正确后）：`docker compose run --rm --entrypoint /migrate api1 -direction force -version <N>`；
- **不要**在 dirty 状态下反复重启。

### 7.3 大量 429

- `RATE_LIMIT_PER_MINUTE`/`BURST` 过小，或客户端重试风暴；
- 配对码领取有独立限流：`PAIRING_CLAIM_RATE_PER_MINUTE`/`PAIRING_CLAIM_BURST`；
- 通过访问日志按 `ip`、`path` 聚合定位来源；确认没有共享出口 IP 误伤；
- **多实例**：`RATE_LIMIT_BACKEND=memory` 时每个实例独立计数，双实例额度约为配置值的 2 倍；
  若 429 数量与配置明显不符，先确认后端与实例数（`docker compose exec api1 printenv | grep RATE_LIMIT`）；

### 7.4 `/metrics` 为空或 404

- 公网访问 404 是**预期**（Caddy 显式拒绝），应改为内网直连 `api:8080/metrics`；
- 容器内 404：检查 `METRICS_ENABLED` 是否为 false；
- 容器内 200 但序列缺失：Prometheus `targets` 是否健康、`scrape.yml` 的 job 是否加载。

### 7.5 HTTPS 证书告警

- 本地 `localhost` 使用 Caddy 内部 CA，自签告警正常；
- 生产需把 `deploy/Caddyfile` 的 `tls internal` 改为 `tls <邮箱>`，并确保 80/443 可达；本机未验证真实 ACME 签发。

### 7.6 同步持续返回 `410 RESYNC_REQUIRED`（游标失效）

现象：`GET /api/v1/sync/changes?cursor=N` 返回：

```http
HTTP/1.1 410 Gone
{"error":{"code":"RESYNC_REQUIRED","message":"游标已过期，请全量重新同步"}}
```

含义：`change_log` 中该用户 `N` 之后、当前最老 `seq` 之前的行已被清理，继续增量同步会永久丢数据。

排查顺序：

1. 查该用户游标与全局最老 seq：
   ```sql
   SELECT min(seq), max(seq) FROM change_log;
   -- 该用户最老保留行（user_id 已知时）
   SELECT min(seq) FROM change_log WHERE user_id = '<uid>';
   ```
2. 若 `cursor < min(seq) - 1` 且 `cursor > 0`，410 是**预期**的；
3. 对照 `cleanup` 日志的 `change_log_cutoff` 与 `-change-log-retention-days`，确认是否保留期太短；
4. 确认客户端已按契约实现：收到 410 → **清空本地游标/缓存** → 重新 `cursor=0` 全量拉取 →
   以响应的 `nextCursor` 继续增量。
5. 若客户端发送的是 `cursor=0` 却仍收到 410，属于服务端缺陷（`cursor=0` 不应触发 410），
   请保留请求 `X-Request-Id` 与服务端日志排查。

处理建议：

- **不要**通过调大保留期“修复”已经丢失的行——行已被物理删除，只能全量重拉；
- 若大量客户端同时 410，评估把 `CHANGE_LOG_RETENTION_DAYS` 调到大于最长离线时间，并观察
  `change_log` 表体积与清理耗时；
- 410 不计入 5xx 告警，但客户端应上报“重新同步”事件，便于评估保留期参数。

### 7.7 `503 TIMEOUT`（查询超时）

现象：`{"error":{"code":"TIMEOUT","message":"数据库查询超时，请稍后重试"}}`。

- 读数、变更与清理相关查询统一带 **3s** 查询超时；超时返回 503 而非挂住连接；
- 排查：`pg_stat_activity` 是否有长事务/锁等待，`pg_stat_statements` 找最慢 SQL，
  检查 `readings` / `change_log` 是否因未配置清理而异常膨胀；
- 索引核对：`readings_user_device_metric_time_idx`、`change_log_server_ts_idx`、
  `change_log_user_seq_idx`、`idempotency_keys_created_at_idx`、`tokens_expires_at_idx`；
- 先执行一次 `cleanup`（可 `-dry-run` 确认边界），再观察 `/metrics` 的 DB 错误计数；
- 客户端应退避重试，不要并发重试放大压力。

### 7.8 401 `UNAUTHORIZED`（token 过期/吊销）

现象：原本可用的 token 开始返回 401，响应体：

```json
{"error":{"code":"UNAUTHORIZED","message":"token 无效、已过期或已吊销"}}
```

- 查库：`SELECT user_id, created_at, expires_at, revoked_at FROM tokens WHERE token_hash = encode(digest('<token>','sha256'),'hex');`
- `expires_at <= now()` → 已过期（默认 180 天，`TOKEN_TTL_DAYS` 可配）；`revoked_at` 非空 → 已注销；
- 客户端应重新走 `/auth/anonymous` 或配对码领取，**不得**在 401 后静默换号导致 userId 漂移；
- `cmd/cleanup` 删除过期 token 行后，旧 token 仍返回 401（不存在等价于无效）。

### 7.9 列表接口 400（limit 越界）

- `GET /my/devices/.../readings`：`limit` 上限 **500**；
- `GET /export`：`limit` 上限 **5000**；
- `GET /sync/changes`：已有限制（默认/上限见 openapi）；
- 超出上限统一返回 `400 BAD_REQUEST`，不会静默截断；客户端应改为分页拉取。

### 7.10 CI 压测 job（perf-smoke）失败

`perf-smoke` 会起 postgres service + API，然后跑 `cmd/loadgen -smoke`。loadgen 在任一场景
**错误率 > 0 或 p95 超阈值**时打印 `SMOKE ... FAIL reasons=...` 并以非 0 退出，
job 因此变红（工作流里没有 `|| true`，不要为了方便改成吞错）。

排查顺序：

1. 看 job 日志里 `SMOKE <场景> FAIL` 的 `reasons=`：
   - `没有完成任何请求`：API 未就绪、限流未关、base URL/端口错误；
   - `错误率 x% 超过 0%`：先看紧邻的 `statuses=` 分布，4xx 多为契约/种子问题，5xx 看 API 日志；
   - `p95 361ms 超过 400ms`：真实性能回归，进入第 3 步。
2. 看失败的 `失败时输出 API 日志` 步骤（`/tmp/api.log` 最后 120 行）：找 `request_id`、
   `db_errors`、迁移 dirty、连接池耗尽、`statement_timeout`。
3. 本地复现同一门槛：
   ```bash
   bash scripts/stack-for-e2e.sh
   go run ./cmd/loadgen -base-url "$(grep BASE_URL .e2e/stack.env | cut -d= -f2-)" -smoke
   bash scripts/stack-for-e2e.sh --stop
   ```
   - 本地也超阈值 → 代码/索引/查询回归，按 §7.7 用 `pg_stat_statements` + `EXPLAIN (ANALYZE, BUFFERS)`
     定位；不要先调大阈值。
   - 仅 CI 超阈值 → 对比 README 实测表与本次 `SMOKE` 行；runner 与本地 Docker VM 的 CPU 差异、
     同 runner 上其他 job 抢占都可能造成 2–3x 抖动，可重跑一次确认，但**连续两次超阈值按回归处理**。
4. 常见原因清单：
   - `RATE_LIMIT_PER_MINUTE` 没设为 `0`，压测撞 429（错误率非 0）；
   - smoke 前没有种子数据，series/push 直接 409/404；
   - 新加索引/迁移未生效（`RUN_MIGRATIONS_ON_BOOT` 被关掉）；
   - 直方图 `route` 标签缺失不影响 loadgen，但会让 SLO 面板查不到数据（见 §7.4）；
   - CI 机器降配或 Docker layer cache 失效导致首次请求慢（看 p50 是否也整体抬升）。
5. **改阈值必须附实测证据**：在 PR 中贴出连续 3 次本机与 CI 的数字，并同步更新
   README「压测门槛与阈值」的取值依据；没有证据不得放宽。

### 7.11 事务边界与数据库故障（v0.10.0）

`POST /sync/push` 是**整批单事务**（设备 + 读数 + `change_log` + 幂等记录）。下列错误都意味着
**整批没有提交**，不存在“前几台已写入”的半成功状态；可直接用同一 `Idempotency-Key` 重试。

#### 7.11.1 push 返回 400 `BAD_REQUEST`（批内重复）

现象：`{"error":{"code":"BAD_REQUEST","message":"同一批设备存在重复 id 或唯一键"}}`。

- 同一批里出现重复设备 `id`，或重复 `(platform, externalId)`；服务端在事务内第一遍规范化时拒绝，
  整批不写、不产生 `change_log`；
- 客户端应在发送前按 `(platform, externalId)` 去重，而不是靠服务端静默去重；
- 若确认客户端没有重复，检查是否把“重试”与“新批次”拼进了同一个请求体。

```bash
# 验证整批未写入（把 <uid> 换成日志里的 user_id）
docker compose exec -T postgres psql -U pjgoiot -d pjgoiot \
  -c "SELECT count(*) FROM change_log WHERE user_id='<uid>';"
```

#### 7.11.2 503 `UNAVAILABLE`（数据库不可达）

现象：`{"error":{"code":"UNAVAILABLE","message":"数据库暂时不可用，请稍后重试"}}`，且
`pjgoiot_db_errors_total{...}` 增长。

- 映射覆盖：连接被拒绝/重置、`unexpected EOF`、`driver: bad connection`、SQLSTATE `08xxx` /
  `57P01` / `57P02` / `57P03` / `53300`；
- 这不是 500：进程和 HTTP 栈正常，只是下游不可达，**可重试**；客户端应指数退避（1s→2s→4s，上限 30s）；
- 排查顺序：
  ```bash
  docker compose ps postgres
  docker compose logs --tail=50 postgres
  docker compose exec -T postgres pg_isready -U pjgoiot
  # API 日志里应能看到 "数据库暂时不可用" + request_id
  docker compose logs --tail=100 api1 api2 | grep -E "UNAVAILABLE|数据库暂时不可用|request_id"
  ```
- 重启 API 不是必需的：`database/sql` 会丢弃坏连接，Postgres 恢复后首个请求即成功
  （v0.10.0 实测停机 36s 后恢复用时 0s，无需重启 API）。

#### 7.11.3 503 `TIMEOUT`（语句超时或连接池排队）

- 与 §7.7 同一错误码，但 v0.10.0 起覆盖两种来源：
  1. 查询/事务预算耗尽：普通查询 3s、鉴权查询 3s、push 总预算 10s；
  2. 连接池排队超过预算：`DB_MAX_OPEN_CONNS` 太小时，请求在 `database/sql` 里排队直到超时。
- 判定方法：
  ```bash
  # 看是否有长期占用的连接（池小 + 慢查询时常见）
  docker compose exec -T postgres psql -U pjgoiot -d pjgoiot \
    -c "SELECT state, count(*) FROM pg_stat_activity WHERE datname='pjgoiot' GROUP BY state;"
  # 连接池配置（默认 20/10）
  docker compose exec -T api1 printenv | grep DB_MAX
  ```
- 处理：先按 §7.7 找慢 SQL；确认数据库本身不慢后，再把 `DB_MAX_OPEN_CONNS` 调到
  覆盖预期并发。**不要**通过调大 HTTP `ReadTimeout` 掩盖排队（v0.10.0 已给鉴权查询加 3s 预算，
  避免请求挂到 15s 后被判成 400）。

#### 7.11.4 `/healthz` 与 `/api/v1/health` 的差异

| 端点 | 是否访问数据库 | 数据库故障时 | 用途 |
| --- | --- | --- | --- |
| `GET /healthz` | 否 | **200** | 容器 liveness：证明进程存活，不要因 DB 故障重启容器 |
| `GET /api/v1/health` | 是（`Ping`） | **503 `UNAVAILABLE`** | readiness：失败即从负载均衡摘流 |

排障时不要用 `/healthz` 200 来判断“服务可用”；判断数据库链路必须看 `/api/v1/health`。

### 7.12 多实例 / 滚动发布排障（v0.11.0）

#### 7.12.1 只有 `api1` 接到流量

- Caddy 主动健康检查依赖 `/api/v1/health`；若某实例 readiness 失败，Caddy 会摘除它：
  `docker compose exec caddy wget -qO- http://api1:8080/api/v1/health`、
  `docker compose logs --tail=50 api1`；
- 确认 `deploy/Caddyfile` 的 upstream 同时包含 `api1:8080` 与 `api2:8080`；
- 连续请求看实例分布：
  ```bash
  for i in $(seq 1 10); do
    curl -sk -D - -o /dev/null https://localhost/api/v1/health | grep -i x-instance
  done
  ```

#### 7.12.2 滚动期间出现 502

- 正常滚动**不该**有 502：SIGTERM 后实例先把 readiness 置 503，Caddy 2s 健康检查摘除，
  进程再继续服务 `SHUTDOWN_DRAIN_SECONDS`（compose 默认 8s）；
- 若出现 502，依次检查：
  1. `SHUTDOWN_DRAIN_SECONDS` 是否小于 Caddy 摘除所需时间（至少 2 个健康检查周期）；
  2. `docker compose ps` 的 `stop_grace_period` 是否被改小；
  3. Caddy 日志 `docker compose logs caddy` 是否有 `no healthy upstreams`；
- 回滚方式：`docker compose up -d api1 api2` 重新拉起两个实例，再重跑滚动脚本。

#### 7.12.3 迁移锁等待

- 两个实例同时启动时，后到者会等待 advisory lock；日志出现
  `try lock failed` / 迁移耗时变长是**预期**，不是脏数据；
- 确认锁与迁移状态：
  ```bash
  docker compose exec -T postgres psql -U pjgoiot -d pjgoiot \
    -c "SELECT locktype, classid, objid, pid, granted FROM pg_locks WHERE locktype='advisory';"
  docker compose exec -T postgres psql -U pjgoiot -d pjgoiot \
    -c "SELECT * FROM schema_migrations;"
  ```
- 若长时间不结束，先确认是否有实例在做 DDL；不要在两个实例上同时手工执行迁移。
- 迁移完成后两个实例都会继续启动；`schema_migrations` 只应有 1 行。

#### 7.12.4 共享限流后端

- `RATE_LIMIT_BACKEND=postgres` 时，限流依赖 `rate_limit_buckets` 表（迁移 000010）；
  表不存在会启动失败（配置校验要求先完成迁移）；
- 数据库故障时限流**降级为进程内**并打 warn 日志：
  `docker compose logs api1 api2 | grep -i '限流'`；这是可用性优先的取舍，
  故障期间全局限流退化为单实例限流；
- 热点 IP 会在单行上排队；若 `pg_stat_activity` 出现大量相同 `bucket` 的等待，
  考虑前置网关限流或调大 `RATE_LIMIT_PER_MINUTE`。

### 7.13 家庭 / 房间 / 成员排障（v0.12.0 引入，v0.13.0 扩展，v0.14.0 补转让）

#### 7.13.1 看不到家庭或家庭内设备

- 每个账号在创建时自动生成个人家庭；迁移 `000011` 也会为既有用户回填。检查：
  ```bash
  docker compose exec -T postgres psql -U pjgoiot -d pjgoiot -c \
    "SELECT h.id,h.name,h.is_personal,m.role FROM households h
       JOIN household_members m ON m.household_id=h.id WHERE m.user_id='<userId>';"
  docker compose exec -T postgres psql -U pjgoiot -d pjgoiot -c \
    "SELECT platform,external_id,household_id,room_id FROM user_devices WHERE user_id='<userId>';"
  ```
- 设备 `household_id` 为 NULL 表示未归属家庭（历史异常数据），可调用
  `PATCH /my/devices/{platform}/{externalId}` 传 `householdId` 修正；
- 跨家庭访问按设计返回 **404**，不是 403；不要把 404 当成“服务未部署”。

#### 7.13.2 邀请码领取失败

- 无效/过期/已用/家庭已删统一 404，属于预期；不要向客户端区分原因。
- 邀请码 TTL 10 分钟；同一码累计 5 次失败即作废。查库：
  ```bash
  docker compose exec -T postgres psql -U pjgoiot -d pjgoiot -c \
    "SELECT household_id,attempts,expires_at FROM household_invitations ORDER BY created_at DESC LIMIT 5;"
  ```
  只存哈希，无法从库中还原明文；需要重发时由 owner 重新 `POST /households/{id}/invitations`。
- 大量 429：领取接口走每 IP 限流中间件，按「限流边界」排查；不要通过清空
  `household_invitations` 绕过限流。

#### 7.13.3 member 收到 403

- 家庭结构写操作（改名、删家庭、签发邀请码、房间增删改）仅 owner 可做；
  member 收到 `403 FORBIDDEN` 是设计行为；
- member 修改**他人设备**返回 404（不暴露设备存在性）；member 可以修改自己 push 的设备，
  包括把它移动到共享家庭或指定房间。

#### 7.13.4 所有权转让与 owner 退出（v0.14.0）

- `POST /households/{id}/transfer-ownership`，body `{"userId":"<家庭成员>"}`。仅 owner 可调用；
  目标必须是该家庭现有成员，否则 **404**（与跨家庭 404 一致）；个人家庭 **400**；转给自己 **400**；
  member 调用 **403**。
- 事务内对 `households` 行 `FOR UPDATE` 后同时改两处：`household_members.role`（原 owner→member、
  目标→owner）与 `households.owner_user_id`，因此任意时刻恰好一个 owner。
- 并发转让被行锁串行化：只有一笔成功，其余返回 **403**（调用者已不是 owner）；若运维在告警里看到
  同一家庭短时间内多笔 403，属于客户端并发重试，不是数据不一致。
- **owner 退出被 409 拒绝时**：`error.code=CONFLICT`，message 为
  「家庭还有其他成员，请先转让所有权或移除其他成员后再退出」。产品流程固定为：
  1）`GET /households/{id}/members` 选成员；2）转让；3）以 member 身份 `leave`。
  若只有 owner 一人，直接 `leave` 会删除该家庭。
- 核对单 owner 不变量：
  ```bash
  docker compose exec -T postgres psql -U pjgoiot -d pjgoiot -c \
    "SELECT h.id,h.owner_user_id,count(*) FILTER (WHERE m.role='owner') AS owner_rows
       FROM households h JOIN household_members m ON m.household_id=h.id
      WHERE h.id='<householdId>' GROUP BY h.id,h.owner_user_id;"
  ```
  `owner_rows` 必须为 1，且 `owner_user_id` 对应同一行。

#### 7.13.5 删除家庭后的数据去向
- 成员、房间、邀请码级联删除；设备回到各自 owner 的个人家庭且 `room_id` 清空；
- 设备与读数不删除，`change_log` 行保留但 `household_id` 置 NULL；
- 个人家庭不可删除（403）；若客户端需要“解散家庭”，只能删除非个人家庭。
- 核对：
  ```bash
  docker compose exec -T postgres psql -U pjgoiot -d pjgoiot -c \
    "SELECT id,name,owner_user_id FROM households WHERE id='<householdId>';"
  docker compose exec -T postgres psql -U pjgoiot -d pjgoiot -c \
    "SELECT user_id,household_id,room_id FROM user_devices WHERE household_id='<householdId>';"
  ```

#### 7.13.6 成员移除 / 退出出错

| 现象 | 原因 | 处理 |
| --- | --- | --- |
| owner 移除自己 → **400** | 移除接口不用于退群 | 改用 `POST /households/{id}/leave` |
| member 调用移除 → **403** | 只有 owner 能移除成员 | 由 owner 操作；member 只能自己 leave |
| owner 退出 → **409 CONFLICT** | 家庭还有其他成员 | v0.14.0 起用 `POST /households/{id}/transfer-ownership` 转让给成员后再 leave；或先 `DELETE /members/{userId}` 移除其他成员（见 §7.13.4） |
| 退出个人家庭 → **400** | 个人家庭不可退出 | 个人家庭随账号存在，不能退 |
| 非成员退出 → **404** | 跨家庭一律 404 | 确认 userId 与 householdId 对应关系 |

- 移除/退出后，该成员设备会回到**他本人的个人家庭**且 `room_id=NULL`；核对：
  ```bash
  docker compose exec -T postgres psql -U pjgoiot -d pjgoiot -c \
    "SELECT h.name,h.is_personal,ud.external_id,ud.room_id FROM user_devices ud
       JOIN households h ON h.id=ud.household_id WHERE ud.user_id='<userId>';"
  ```
- owner 作为唯一成员退出会**删除整个非个人家庭**（成员/房间/邀请码级联），操作前建议备份。

#### 7.13.7 邀请码管理

- `GET /households/{id}/invitations` 只返回 `codeHashPrefix`（8 位）与 `status`，**不返回明文**；
  这是设计约束，不可为排障临时改接口。
- `status` 判定：`expired`（已过 `expiresAt`）、`locked`（`attempts >= 5`）、`active`；已领取的码
  领取时即删除，不会出现在列表。
- 撤销用 `DELETE /households/{id}/invitations/{invitationId}`，只删未领取的码；对已领取/不存在返回 404。
- 查库（只看元数据，不要打印完整 `code_hash`）：
  ```bash
  docker compose exec -T postgres psql -U pjgoiot -d pjgoiot -c \
    "SELECT id,left(code_hash,8) AS prefix,attempts,expires_at,created_at
       FROM household_invitations WHERE household_id='<householdId>'
       ORDER BY created_at DESC;"
  ```
- `household_id=NULL` 的行只是猜码失败计数，不属于任何家庭，也不会出现在管理列表。

#### 7.13.8 房间排序 / 重排 409

- 单房间排序：`PATCH /households/{id}/rooms/{roomId}` 传 `sortOrder`（非负整数）。
- 整段重排：`PUT /households/{id}/rooms/order`，body `{"roomIds":[...]}`，必须**恰好覆盖全部房间**；
  缺项、多项、重复 id → **409**，且 `error.code` 为可机读的 **`ROOM_SET_MISMATCH`**（v0.14.0 起）。
  客户端按 code 判断，不要解析 message 文本。
- 客户端收到 `ROOM_SET_MISMATCH` 后应重新 `GET /households/{id}/rooms`，按最新列表重算顺序再提交整段。
- 服务端在事务内 `SELECT ... FOR UPDATE` 锁住该家庭房间行，再按数组下标写 `0..n-1`；
  并发重排按行锁串行，后提交者覆盖先提交者（last write wins），不会出现重复序号。
- 核对序号连续性：
  ```bash
  docker compose exec -T postgres psql -U pjgoiot -d pjgoiot -c \
    "SELECT id,name,sort_order FROM household_rooms WHERE household_id='<householdId>'
       ORDER BY sort_order, id;"
  ```

#### 7.13.9 结构变更的增量同步（v0.15.0）

- `GET /sync/changes` 现在除 `user_device` 外还返回 `entity=household` / `entity=room`。
  `household` payload 含 `id/name/ownerUserId/role(接收者本人)/memberCount/deleted`；
  `room` payload 含 `id/householdId/name/sortOrder/deleted`。
- **扇出写入**：每条结构变更按「写入时刻的成员集合」逐人各写一行（`user_id=接收者`、`household_id` 留空）。
  因此查某条结构变更的接收者：
  ```bash
  docker compose exec -T postgres psql -U pjgoiot -d pjgoiot -c \
    "SELECT seq,user_id,entity,entity_id,op,payload->>'name' AS name,server_ts
       FROM change_log WHERE entity IN ('household','room') ORDER BY seq DESC LIMIT 20;"
  ```
  `household_id` 为空是**预期**的（不代表缺失归属，归属在 payload / `entity_id` 里）。
- **可见性排查**「成员被移除后还收到家庭变更？」→ 不应发生：移除之后的结构行里没有该用户的 `user_id`。
  若出现，检查该行 `user_id` 是否等于被移除者以及写入时间是否在移除之前。
- **新成员看不到历史**是设计取舍：首次基线走 `GET /households`；不要期待 `/sync/changes` 回放旧结构。
- **未知 entity**：客户端必须忽略并照常推进 `nextCursor`。若客户端因 `future_entity` 卡住游标，
  属于客户端 bug，不是服务端拒绝；服务端对 `entity` 不做白名单校验。
- **410**：结构变更同样适用 `change_log` 90 天保留。`cursor` 早于仍保留的最老 `seq`
  时返回 `410 RESYNC_REQUIRED`，包括缺口**仅由结构变更**造成的情况。
  核对：
  ```bash
  docker compose exec -T postgres psql -U pjgoiot -d pjgoiot -c \
    "SELECT min(seq), max(seq) FROM change_log;"
  ```
- 设备归属变更仍走已有的 `user_device` 变更，不会重复写结构变更；查家庭内设备变更归属看
  `change_log.household_id`，查结构变更看 `user_id` 扇出。

### 7.14 意见反馈接口排障（v0.16.0 提交/列表，v0.17.0 状态流转）

- **429 `FEEDBACK_RATE_LIMITED`**：账号与 IP 双维度独立计数，默认账号 10/min（桶 5）、IP 30/min（桶 10）。
  先看 `FEEDBACK_USER_*` / `FEEDBACK_IP_*`，再按访问日志的 `ip` 与 `user_id` 判断是单个账号刷量还是共享出口 IP；
  `RATE_LIMIT_BACKEND=memory` 时多实例额度按实例数放大，与全局限流一致。
- **409 `IDEMPOTENCY_CONFLICT`**：同一个 `Idempotency-Key` 被用于不同内容。客户端应为每条草稿固定一个 key，
  不要在内容变化后复用旧 key；未携带 key 时服务端按规范化请求体哈希去重，不会出现该冲突。
- **重复提交仍返回同一条**：响应码 `200` 且带 `Idempotent-Replayed: true` 属于正常幂等重放，不是新建。
- **列表看不到刚提交的反馈**：确认请求 token 与提交时是同一账号；`GET /feedback/mine` 不存在跨账号读取路径。
  若带了 `?status=`，确认状态拼写为 `submitted` / `received` / `closed`，未知值会返回 400。
- **用户关闭后仍显示旧状态**：客户端应刷新详情或列表；`PATCH /feedback/{id}` 成功返回 `200` 与最新
  `status`/`statusMessage`/`updatedAt`。已关闭再次 PATCH 返回 `409 FEEDBACK_INVALID_TRANSITION`，
  这是终态语义，不是幂等重放。
- **运营入口返回 404**：先确认目标环境配置了 `ADMIN_TOKEN`（compose 需把该变量传进容器）。
  未配置时整组 `/admin/feedback` 返回 404，与不存在的路由一致，属预期行为而非路由丢失。
- **运营入口返回 401**：请求头 `X-Admin-Token` 缺失或与 `ADMIN_TOKEN` 不一致；客户端 Bearer token
  不能替代它。排查时不要把凭据写进日志。
- **运营 PATCH 返回 409**：状态机只允许 `submitted → received → closed`、`submitted → closed`
  与 `received → submitted`；`closed` 是终态。同状态重复 PATCH 是 200 幂等 no-op。
- **审计核查**：每次真实状态变更一条记录，直接查库：
  ```sql
  SELECT feedback_id, actor_type, actor_id, from_status, to_status, note, created_at
  FROM feedback_status_events WHERE feedback_id = '<id>' ORDER BY created_at, id;
  ```
  `feedback.internal_note` 只保留最新备注，历史以审计表为准。
- **状态序号不连续**：`submitted → closed` 是允许的捷径，`received → submitted` 是误受理回退，
  审计里出现这类跳转都正常；不要用「必须经过 received」做监控断言。
- **个人信息核查**：`contact` 只落 `feedback.contact` 列，用户侧详情与列表都不回显；运营视图才返回。
  排查时不要把它拼进日志或告警文本。需要核查可直接查库：
  `SELECT id, user_id, created_at, status FROM feedback ORDER BY created_at DESC LIMIT 20;`
- **账号注销**：`feedback.user_id` 与 `feedback_status_events.feedback_id` 均为 `ON DELETE CASCADE`，
  注销后自动清理，无需额外清理任务。

### 7.15 设备类型目录排障（v0.18.0）

- **目录 404 / 路由不存在**：公开读取是 `GET /api/v1/catalog/device-types`；运营管理是
  `/api/v1/admin/catalog/device-types/{type}`。确认没有把旧 `/device-types/{type}/catalog`
  （固件/配件）当成目录接口。
- **运营入口 404**：先确认配置了 `ADMIN_TOKEN`；未配置时整组 admin 路由返回 404，与不存在一致。
- **运营入口 401**：`X-Admin-Token` 缺失或与 `ADMIN_TOKEN` 不一致；客户端 Bearer token 不能替代。
- **PUT 返回 404**：管理端只更新已存在的目录项（新增型号走真值表 + 迁移）。软删除后再次 PUT
  也是 404；这是软删语义，不是数据丢失。
- **PUT 返回 400**：`displayName` 为空、UUID 非 128 位、metric key/unit 非法、数组超限或
  `compatibility` 不是 JSON 对象。错误信息会指出具体下标字段。
- **客户端一直拿 304 / 看不到更新**：先看响应头 `ETag`（`"catalog-v<version>"`）。管理端每次
  PUT/DELETE 都在同一事务内自增 `device_type_catalog_meta.version`，即便内容相同也会变。
  若 ETag 未变，检查写入是否真正提交（审计表是否有对应行）。
- **`If-Modified-Since` 不生效**：该头只在没有 `If-None-Match` 时参与比较，且按秒级比较；
  ETag 优先。客户端应优先使用 ETag。
- **目录与解析能力不一致**：不要手改迁移 seed。改
  `internal/devicecatalog/catalog.go` 后执行 `go run ./cmd/cataloggen`，再跑
  `TestPostgresCatalogDeviceTypesIntegration`；不一致会直接失败。
- **审计核查**：
  ```sql
  SELECT device_type, action, actor, catalog_version, created_at
  FROM device_type_catalog_audits ORDER BY created_at DESC, id DESC LIMIT 20;
  ```
- **灰度访问记录核查**：同一用户同一目录版本只有一行：
  ```sql
  SELECT user_id, catalog_version, first_accessed_at
  FROM device_type_catalog_accesses ORDER BY first_accessed_at DESC LIMIT 20;
  ```
  该表只做去重记录，不参与鉴权；匿名读取不写入。
- **软删恢复**：当前管理端不提供恢复接口；需要恢复时由 DBA 将 `deleted_at` 置空、
  执行 `UPDATE device_type_catalog_meta SET version = version + 1, updated_at = now() WHERE id = 1`
  并补一条审计，或按真值表重新生成 seed 后走发布流程。操作前先备份该行。

### 7.16 账号删除与个人数据导出排障（v0.19.0）

- **删除被 409 拦截**：说明调用者是某个家庭的 owner 且该家庭还有其他成员。
  先让 owner 用 `POST /households/{id}/transfer-ownership` 转让，或先移除其他成员，再重新申请删除令牌。
  被拦截不会删除任何数据。
- **删除令牌报错**：400 = 请求头缺 `X-Deletion-Token`；403 = 令牌不存在/不属于该账号；
  410 = 超过 10 分钟；404 = 令牌已用于一次成功删除（账号已不存在，重放语义）。重新申请即可。
- **旧 token 仍能访问**：正常删除是单事务并删除 `tokens`，不应出现；若出现，检查是否删除事务失败
  （看 API 日志 `删除 xxx 失败`），事务失败会整体回滚、账号保持可用。
- **导出一直 pending**：后台构建有 60 秒超时，失败会置为 `failed`；查看 API 日志
  `生成个人数据导出失败` / `写入导出产物失败`。`failed` 任务需要重新 `POST /me/export`。
- **下载 410**：链接一次性且 10 分钟有效，重复下载或超时都会 410；重新 `GET /me/export/{id}`
  会签发新链接并让旧链接失效。403 表示令牌不正确（不要手动拼接 `token`）。
- **产物保留**：导出产物保留 24 小时；`cmd/cleanup` 同时清理过期的 `account_deletion_tokens`
  与 `personal_data_export_tasks`，dry-run 会打印两类计数。

## 8. 监控与告警

- 抓取目标：`api:8080/metrics`（内网），样例见 `deploy/prometheus/scrape.yml`；
- 告警规则见 `deploy/prometheus/alerts.yml`（共 9 条：可用性错误预算燃烧、5xx 比例、devices/series
  分接口 p95、全局 p95、DB 错误、readiness 失败、请求量为 0、指标缺失）；
- 3 条 SLO 与告警的对应关系、PromQL 与错误预算算法见 `docs/slo.md`；面板见
  `deploy/grafana/dashboard.json`（指标名引用由 `scripts/validate-dashboard.sh` 校验）；
- `/metrics` 的业务计数为 `pg_class.reltuples` 近似值，不应作为计费/审计依据；
- v0.9.0 起 HTTP 指标带 `route` 标签（路由模板），分接口查询用
  `pjgoiot_http_request_duration_seconds_bucket{route="..."}`；
- 访问日志字段：`request_id`、`method`、`path`、`status`、`duration_ms`、`ip`、`user_id`（不打印 token/配对码）。

### 8.1 Alertmanager 接入（v0.23.0，未在生产部署）

- 配置：[`deploy/alertmanager/alertmanager.yml`](../deploy/alertmanager/alertmanager.yml)。
  `route.group_by: [alertname, service, env]`（去重粒度）、`group_wait: 10s`、
  `repeat_interval: 4h`（critical 5s/1h）、receiver 指向 `http://alert-webhook:8099/alerts`。
- 接收端：[`cmd/alert-webhook/main.go`](../cmd/alert-webhook/main.go)，把每批告警逐条写成
  JSON 行（`receivedAt`/`alertname`/`severity`/`service`/`env`/`fingerprint`/`summary`/`startsAt`/`endsAt`）。
  默认 `:8099`，`ALERT_WEBHOOK_ADDR` / `ALERT_WEBHOOK_LOG` 可覆盖。
- `deploy/prometheus/scrape.yml` 已加 `alerting.alertmanagers` 指向 `alertmanager:9093`。
- 静默（silence）按 matchers 在运行时创建，命令模板见配置文件末尾；
  列表/过期：`GET /api/v2/silences`、`DELETE /api/v2/silence/{id}`。
- **未验证**：真实电话/短信/邮件/Slack 接收通道；生产 Alertmanager 部署与外部可达性。

### 8.2 本地验证告警链路（可复现）

```bash
./scripts/verify-alerting.sh
# 输出结尾应为：
# PASS: 规则触发后 webhook 收到 VerifyAlertingPipeline
# PASS: 重复告警去重生效（webhook 收到 1 次）
# PASS: 静默生效（webhook 收到 0 次）
# ALL PASS: ...；未验证真实电话/短信/邮件通道。
```

依赖 Docker（`host.docker.internal` 可解析）与 Go；脚本会构建 `cmd/alert-webhook`、
起 `prom/alertmanager:v0.27.0` 与 `prom/prometheus:v2.55.0`，结束时清理容器与临时目录。
**验证边界：只到 webhook 收到。**


## 9. 本机实际验证记录（v0.7.1 … v0.23.0）

### 9.1 构建与启动

```text
docker compose build
 → Successfully tagged pjgoiot-api:0.7.1（43.9MB，distroless nonroot）
docker compose up -d
 → pjgoiot-postgres-1 Started → Healthy
 → pjgoiot-api-1 Started → Healthy
 → pjgoiot-caddy-1 Started
之后 60s 内每 5s `docker compose ps`（12 次）：
 → api/postgres 始终 healthy，caddy Up
```

### 9.2 逐项探活（真实输出）

```text
a) curl -k https://localhost/api/v1/health
   → HTTP 200 {"service":"pjgoiot-api","status":"ok","store":"postgres","version":"0.7.1"}
   → 含 strict-transport-security: max-age=31536000 等安全头

b) curl -k https://localhost/metrics
   → HTTP/2 404，content-length: 9，body: not found

c) 容器网络内（pjgoiot_edge），node fetch http://api:8080/metrics
   → HTTP 200；pjgoiot_http_requests_total{method="GET",status="200"} 12

d) docker inspect pjgoiot-api-1 → User=nonroot:nonroot, ReadonlyRootfs=true
   docker top pjgoiot-api-1 → UID 65532, COMMAND api
   docker cp /tmp/write-test.txt pjgoiot-api-1:/write-test.txt
   → Error response from daemon: container rootfs is marked read-only
   docker compose exec api id
   → OCI runtime exec failed: exec: "id": executable file not found in $PATH
     （distroless 无 id；改用 docker top / inspect 取证，非缺陷）

e) curl -k -H 'X-Request-Id: local-verify-123' https://localhost/healthz
   → x-request-id: local-verify-123
   curl -k https://localhost/healthz
   → x-request-id: 93317b48-1b74-4318-9d7b-70efedb6d862（自动生成 UUID）

f) 写入设备 AA:BB:CC:00:00:71 → docker compose restart api
   → api health: starting → healthy；GET /my/devices 仍返回该设备，HTTP 200

g) docker compose logs api
   → 启动迁移完成 version=7 dirty=false（首次启动与重启后各一条）
```

### 9.3 PromQL 语义校验

```text
docker run --rm --entrypoint /bin/promtool -v "$PWD/deploy/prometheus":/cfg \
  prom/prometheus:latest check rules /cfg/alerts.yml
 → Checking /cfg/alerts.yml
 → SUCCESS: 6 rules found

docker run --rm --entrypoint /bin/promtool -v "$PWD/deploy/prometheus":/cfg \
  prom/prometheus:latest check config /cfg/scrape.yml
 → SUCCESS: 1 rule files found
 → SUCCESS: /cfg/scrape.yml is valid prometheus config file syntax
```

### 9.4 docker 模式备份 / 恢复

```text
BACKUP_MODE=docker bash deploy/scripts/backup.sh
 → pjgoiot-20261105T031905Z.dump.gz（8 193 bytes）
 → SHA-256: 4b0718ec119e833d958396c1f279f697c7cc3dfed0c9ce0ecd753be8908a8994

docker compose stop api
RESTORE_MODE=docker bash deploy/scripts/restore.sh --confirm \
  --file ./backups/pjgoiot-20261105T031905Z.dump.gz
 → pjgoiot-...dump.gz: OK
 → 迁移版本检查: version=7 dirty=f
 → 行数摘要: users=1 user_devices=1 readings=0 change_log=1 pairing_codes=0
docker compose start api → healthy；GET /my/devices 仍返回恢复前的设备
```

### 9.5 收尾

```text
docker compose down（不加 -v）
 → 容器与网络删除；卷保留：
   pjgoiot_caddy_config / pjgoiot_caddy_data / pjgoiot_pgdata
```

GitHub Actions（run 41962617787，commit ef0c7af）：`构建与测试`、`构建镜像`、`配置与脚本校验` 均 success。

### 9.6 仍未验证

- 真实域名 / ACME 证书签发（未开通任何云账号）；
- systemd timer / 真实 cron 安装；
- 数小时长稳压、故障注入（`RATE_LIMIT_BACKEND=postgres` 的多实例共享额度已在 v0.21.0 实测，见 §9.11）；
- `shfmt`（本机与 CI runner 均无）。

### 9.7 v0.8.0 生命周期与压测（真实输出）

```text
# 匿名登录（响应含 expiresAt）
POST /api/v1/auth/anonymous → 201
  → expiresAt=2027-05-04T...Z（约 180 天后），库内 tokens.expires_at 一致

# push 3 台设备
POST /api/v1/sync/push → 200，serverSeq=[246402,246403,246404]
GET /sync/changes?cursor=0 → 3 条

# 把最早一条 change_log 老化 100 天，再按 90 天清理
docker compose exec -T postgres psql -U pjgoiot -d pjgoiot \
  -c "UPDATE change_log SET server_ts = now() - interval '100 days' WHERE seq = 246402;"
docker compose --profile maintenance run --rm cleanup
  → readings_deleted=1628 / change_log_deleted=1 / idempotency_keys_deleted=0
  → tokens_deleted=0 / pairing_codes_deleted=0

# 旧游标失效 / 新游标正常
GET /sync/changes?cursor=246401 → 410 {"error":{"code":"RESYNC_REQUIRED",...}}
GET /sync/changes?cursor=246402 → 200，剩 2 条
GET /my/devices → 仍 3 台（cleanup 不影响 user_devices）

# token 过期与清理
把 tokens.expires_at 改为过去时间 → 任意鉴权接口 401
docker compose --profile maintenance run --rm cleanup → tokens_deleted=1
SELECT count(*) FROM tokens → 0
```

压测（`cmd/loadgen`，Docker Desktop VM 2 vCPU / 4 GB，30s/场景，`RATE_LIMIT_PER_MINUTE=0`）：

| 场景 | 并发 | RPS | p50 | p95 | p99 | 错误率 |
| --- | --- | --- | --- | --- | --- | --- |
| `GET /my/devices` | 100 | 1 065.6 | 75 ms | 215 ms | 395 ms | 0% |
| `POST /sync/push`（100 设备+200 读数） | 50 | 51.5 | 801 ms | 2 005 ms | 2 818 ms | 0% |
| `GET /readings/series`（1d，90 天） | 50 | 802.2 | 55 ms | 127 ms | 183 ms | 0% |

p95 归因与修正见 README「容量与压测结果」：`/my/devices` 的 N+1 已合并为单条 JOIN（EXPLAIN 0.25 ms），
push 从约 900 条 SQL/批降到约 7 条；剩余延迟是 2 vCPU 本地 VM 的 CPU/写吞吐上限，不是索引问题。

### 9.8 v0.9.0 真实存储联调栈与 CI 压测门槛

```text
bash scripts/stack-for-e2e.sh
 → 启动 Postgres（第 1 次尝试，端口 65500）
 → Postgres 已就绪: 127.0.0.1:65500
 → 迁移完成 version=8 dirty=false
 → 启动 API（第 1 次尝试，端口 65504）
 → API readiness 通过: http://127.0.0.1:65504
 → stdout: BASE_URL=http://127.0.0.1:65504

bash scripts/seed-for-e2e.sh
 → push 结果: applied=1 skippedReadings=0 conflicts=0
 → 校验通过: user_devices=1 readings=24 metrics=pressure_hpa,relative_humidity,temperature_celsius
             device_types=5 firmware=ESP-IDF v5.2.2 accessory=USB-C 数据线 public_model=ESP32-C3-DevKitM-1

# 再执行一次：复用 token，skippedReadings=24，user_devices/readings 数量不变

curl 全链路：
 匿名登录 201 (expiresAt=2027-05-04T05:05:08Z)
 push 200 (applied=1, serverSeq=[2])
 changes?cursor=0 200 (1 条, nextCursor=2)
 series?metric=temperature_celsius&bucket=1d 200 (2 个桶)
 metrics 200 (1 个指标, count=8)
 export 200 (devices=1, readings=8, hasMore=false)
 DELETE /me 204 → 同一 token 再访问 /my/devices 401 UNAUTHORIZED

bash scripts/stack-for-e2e.sh --stop
 → 停止 API / 删除 Postgres 容器 / 清理 .e2e/
```

smoke（真实 Postgres 栈，10s/场景，2s 预热）：

| 场景 | 并发 | 请求数 | RPS | p95 | 阈值 | 结果 |
| --- | --- | --- | --- | --- | --- | --- |
| `GET /my/devices` | 20 | 20 421 | 2 040.7 | 28 ms | 400 ms | PASS |
| `POST /sync/push` | 5 | 165 | 16.2 | 417 ms | 4 000 ms | PASS |
| `GET /readings/series` | 10 | 14 207 | 1 420.2 | 15 ms | 400 ms | PASS |
| 错误率 | — | — | — | 0% | 0% | PASS |

`bash scripts/validate-dashboard.sh` → 9 个面板结构有效，`pjgoiot_*` 指标均存在于 Go 代码，
`probe_success` 由 blackbox_exporter 提供（唯一豁免）。

### 9.9 v0.10.0 事务边界、故障注入与迁移演练（真实输出）

#### 9.9.1 事务边界与崩溃窗口（真实 PG）

```text
TEST_DATABASE_URL='postgres://pjgoiot:***@127.0.0.1:55901/pjgoiot_fault?sslmode=disable' \
  go test ./internal/store/postgres -run TestPushTransactionBoundaries -v -count=1

--- PASS: TestPushTransactionBoundaries (0.23s)
    --- PASS: 同批重复设备 id 明确拒绝且无残留
    --- PASS: 同批重复唯一键明确拒绝且无残留
    --- PASS: 读数校验失败时设备与变更日志一并回滚
    --- PASS: 崩溃注入 devices_written
    --- PASS: 崩溃注入 change_log_written
    --- PASS: 崩溃注入 readings_written
    --- PASS: 崩溃注入 idempotency_written
    --- PASS: 崩溃注入 before_commit
    --- PASS: 计数与实际写入严格对应
    --- PASS: 并发 push 不丢变更且游标单调
```

崩溃注入点覆盖 `devices_written → change_log_written → readings_written →
idempotency_written → before_commit`：每个阶段返回错误后断言 `user_devices` /
`readings` / `change_log` / `idempotency_keys` **零新增**，随后用同一 `Idempotency-Key`
重试可正常提交。

#### 9.9.2 Postgres 停机 36s 与重启自愈

```bash
# API 指向独立 PG 容器（非 compose），30 次采样
docker stop pjgoiot-fault-pg          # 停机窗口 36s
# 每 2s 请求一次：GET /my/devices、POST /sync/push、/healthz、/api/v1/health
docker start pjgoiot-fault-pg
```

```text
== 停机窗口（30 次采样）==
GET  /my/devices     {'503': 30}
POST /sync/push      {'503': 30}
GET  /healthz        {'200': 30}
GET  /api/v1/health  {'503': 30}   # body: {"error":{"code":"UNAVAILABLE",...}}

== 重启后 ==
/api/v1/health 200，恢复用时 0s
GET /my/devices 200（12.7ms），设备数据完好，无需重启 API
```

结论：数据库故障不会伪装成 500，也不会让 liveness 探针误杀容器；readiness 会真实摘流。

#### 9.9.3 慢查询 → 503 `TIMEOUT`

```bash
docker exec pjgoiot-fault-pg psql -U pjgoiot -d pjgoiot_fault \
  -c "ALTER ROLE pjgoiot SET statement_timeout='100ms';"
# 请求一个约 2M 行的 series 查询
curl -s -o /tmp/out -w '%{http_code} %{time_total}\n' \
  -H "Authorization: Bearer $TOKEN" "$BASE/readings/series?metric=temperature_celsius&bucket=1d"
```

```text
baseline（statement_timeout=0）：200，1.34s / 1.32s / 1.54s
100ms 超时：503 TIMEOUT，0.10–0.17s（连续 3 次）
pjgoiot_db_errors_total 0 -> 3
恢复：ALTER ROLE pjgoiot RESET statement_timeout; 再请求 200
```

#### 9.9.4 连接池耗尽（`DB_MAX_OPEN_CONNS=2`）

```bash
DB_MAX_OPEN_CONNS=2 DB_MAX_IDLE_CONNS=1 ./api   # 独立进程，端口 55902
# loadgen：GET /my/devices 并发 200，8s；POST /sync/push 并发 100，8s
```

| 场景 | 并发 | 请求数 | RPS | p95 | 状态分布 | 错误率 |
| --- | --- | --- | --- | --- | --- | --- |
| `GET /my/devices` | 200 | 7 330 | 896.7 | 540 ms | 全部 200（排队后成功） | 0% |
| `POST /sync/push` | 100 | 250 | — | 10.2 s | 113×200 + 137×503(TIMEOUT) | 54.8% |

60 并发突刺时抓到 503 响应体 `{"error":{"code":"TIMEOUT",...}}`，确认是**排队超预算后快速失败**，
不是无限挂起。首轮曾出现 15 个 400 / 部分请求 20s：根因是鉴权查询没有超时，等待超过 HTTP
`ReadTimeout=15s` 后 body 读取失败被 Gin 判 400；给鉴权查询加 3s 预算后消失。

#### 9.9.5 迁移演练（`scripts/migrate-drill.sh`，可重复执行）

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

已知边界：`000006` 的 down 会删除 `readings` 表，跨 version 1 的往返会丢读数（迁移语义）；
`users` / `tokens` / `user_devices` / `change_log` 行数不丢。该演练发现并修复了 `000004`
对大写 MAC 的漏回填（新增 `000009_mac_case_backfill`）。

GitHub Actions（run 42007845297，commit c61e0c6，tag `v0.10.0`）：`构建与测试`、`构建镜像`、
`配置与脚本校验`（含 `scripts/migrate-drill.sh` 的 `bash -n`/shellcheck）、`压测门槛（smoke）`
（devices p95 11ms / push 392ms / series 6ms，错误率 0%）均 success。

### 9.10 v0.11.0 多实例、滚动发布与限流（真实输出）

四个脚本均可重复执行，前置条件：Docker daemon、80/443/55432 可用，且本地已有
`pjgoiot-api:0.11.0` 镜像（`NO_BUILD=1` 时不重新构建）。

#### 9.10.1 双实例与 Caddy 负载均衡（`scripts/multi-instance-check.sh`）

```text
[ok] api1 ready（X-Instance: api1）
[ok] api2 ready（X-Instance: api2）
[ok] Caddy 负载均衡：20 次探测 api1=10 api2=10
[ok] 跨实例鉴权：实例 A 登录、实例 B 复用 token 读 /my/devices
[ok] 跨实例幂等：A 首次 200；B 重放 200 + Idempotent-Replayed: true，body 一致
[ok] 同 key 不同 body：409 IDEMPOTENCY_CONFLICT
[ok] 并发 push：30 批打到两个实例，change_log 共 31 条
[ok] 游标：轮询 /sync/changes 拉到的 seq 集合 == 库内集合，严格递增、无缺口
[ok] 停止 api1：160 个请求经 Caddy 全部 200（0 个 502），尾部只由 api2 服务
[ok] 重启 api1：重新 healthy 并恢复接流量
```

`seq` 严格有序的原因不是“单实例串行”，而是 push 事务内对用户行持有
`pg_advisory_xact_lock`，`change_log` 的 `seq` 在该事务提交前不会对其他事务可见；
两个实例并发 push 同一用户时按事务串行，不同用户互不阻塞。游标只有在 `seq` 已提交后才可见，
因此不会越过未提交变更。

#### 9.10.2 滚动发布停机请求数（`scripts/rolling-restart.sh`）

```text
负载：2 个 worker 经 Caddy，约 20 req/s
逐实例：stop（SIGTERM drain）→ start → wait healthy → 观察 3s
结果：总请求=532 非200=0（502=0 连接失败=0）
      api1=239 api2=293
[PASS] 停机请求数=0
```

#### 9.10.3 并发迁移（`scripts/migration-concurrency-check.sh`）

```text
空库 pjgoiot_migconc；迁移 advisory lock id = 1942488908
外部 psql 会话持有锁 5s
api1/api2 同时以 RUN_MIGRATIONS_ON_BOOT=true 启动
两者就绪耗时：5261ms（均 > 5s 锁持有时间）
version=10 dirty=false
schema_migrations 行数：1
表存在：users/user_devices/change_log/readings/rate_limit_buckets = 5/5
[PASS]
```

结论：golang-migrate 的 PG advisory lock 跨实例有效，只有先拿到锁的实例执行迁移，
另一个等待后直接看到已是最新版本；不会并发建表或出现 dirty。

#### 9.10.4 限流边界（`scripts/shared-rate-limit-check.sh`）

```text
场景 A：RATE_LIMIT_BACKEND=memory，额度 3/min，burst 3
  经 Caddy 串行请求：前 6 个 200，第 7 个起 429
  => 双实例各 3 个 = 2 倍额度（已知限制）

场景 B：RATE_LIMIT_BACKEND=postgres，额度 3/min，burst 3
  经 Caddy 串行请求：前 3 个 200，之后两个实例均返回 429
  => 全局限额生效
```

运维成本：`postgres` 后端每个请求多一次 UPSERT；`rate_limit_buckets` 只保存活跃 IP 的桶，
5 分钟惰性清理 10 分钟未更新的行；数据库故障时降级为进程内限流（可用性优先）。
未引入 Redis：避免新增高可用、持久化与网络运维面。

### 9.11 v0.12.0 家庭模型迁移与共享（真实输出）

#### 9.11.1 迁移 up → down → up（临时库 `hh_roundtrip`）

先迁移到 version 10，写入一个 account 用户与一台旧设备，再执行 `000011`：

```text
迁移完成 version=11 dirty=false
-- 回填结果 --
我的家|t|owner|t|t        # name|is_personal|role|household_id 已回填|room_id 为 NULL
-- down 到 10 --
迁移完成 version=10 dirty=false
0 0                      # 家庭表数量=0；user_devices 新列数量=0
-- 再 up --
迁移完成 version=11 dirty=false
我的家|owner|1            # 家庭与成员行恢复，且每账号仅 1 个家庭
```

#### 9.11.2 家庭共享集成测试（真实 PostgreSQL）

```bash
TEST_DATABASE_URL='postgres://pjgoiot:...@127.0.0.1:55432/pjgoiot_v11test?sslmode=disable' \
  go test ./internal/store/postgres -run TestHouseholdSharingPostgres -count=1 -v
```

```text
=== RUN   TestHouseholdSharingPostgres
--- PASS: TestHouseholdSharingPostgres (0.15s)
PASS
ok  	github.com/platojobs/PJGoIoT/internal/store/postgres	0.615s
```

覆盖点：默认个人家庭、个人家庭不可删、创建共享家庭、邀请码一次性、跨家庭读取 404、
member 改名 403、设备 push 后移入家庭/房间、member 可见设备与读数、member 改他人设备 404、
共享变更出现在 member 的 `/sync/changes`、删房间清空 `room_id`、删家庭后设备回个人家庭且成员失去可见性。

#### 9.11.3 HTTP 层家庭生命周期（fixture 存储）

```text
=== RUN   TestHouseholdLifecycleAndPermissions
--- PASS: TestHouseholdLifecycleAndPermissions (0.00s)
PASS
ok  	github.com/platojobs/PJGoIoT/internal/httpapi	0.463s
```

覆盖点：默认家庭、创建家庭、邀请码领取、member 写权限 403、跨家庭 404、房间创建、
设备移动、三态字段清除、成员共享可见性、删除级联与个人家庭 403。

#### 9.11.4 OpenAPI 契约

```text
api/openapi.yaml: validated in 62ms
Woohoo! Your API description is valid. 🎉
You have 13 warnings.
```

0 error；warning 均为 info/tags/501/login 示例等非阻断类型（本轮新增 `household` tag 也触发 1 条 tag 描述缺失 warning），非本轮新代码错误。

#### 9.11.5 CI（GitHub Actions）

```text
run 42037369419（main, c36db9a）
构建与测试 success / 配置与脚本校验 success / 压测门槛（smoke） success / 构建镜像 success

run 42037314989（v0.11-fix, 33839cf）
全部 job success（v0.11.0 的 shellcheck 修复验证）
```

首次 v0.11.0 CI（run 42029098771）仅 `配置与脚本校验` 失败：
shellcheck 对 `trap` 清理函数报 SC2317（不可达）与 SC2329（未调用），
并对 `multi-instance-check.sh` 中未使用的 `APPLIED_IDS` 报 SC2034。
修复方式：删除无用命令替换、在脚本头部声明
`# shellcheck disable=SC2317,SC2329`（仅屏蔽 trap 回调误报）。

### 9.12 v0.21.0 容量基线与故障演练（真实输出）

> 环境：Apple M1 Max 10 核 / 32 GB、PostgreSQL 16.15、库 `pjgoiot_load`（迁移 17）；
> 完整命令与原始表见 [`perf-baseline.md`](perf-baseline.md) 与 [`preflight.md`](preflight.md)。

#### 9.12.1 压测（7 条路径 + push 拐点 + 多实例限流）

```text
GET /my/devices            50c  5,773.9 RPS   p50 8ms   p95 16ms   p99 23ms   0.00%
GET /readings/series       50c  4,766.0 RPS   p50 8ms   p95 20ms   p99 77ms   0.00%
POST /sync/push（单用户）  50c     42.4 RPS   p50 1.053s p95 2.042s p99 2.752s 0.08%
GET /sync/changes          50c    136.7 RPS   p50 345ms  p95 597ms  p99 761ms  0.00%
GET /scenes                50c  7,174.5 RPS   p50 6ms   p95 13ms   p99 18ms   0.00%
PUT /scenes/{id}           50c  2,097.7 RPS   p50 22ms  p95 43ms   p99 57ms   0.00%
POST /feedback             20c 12,749.3 RPS   p50 1ms   p95 3ms    p99 10ms   0.00%
GET /feedback/mine         20c 11,443.6 RPS   p50 1ms   p95 5ms    p99 13ms   0.00%
GET /catalog/device-types  50c 12,388.2 RPS   p50 4ms   p95 7ms    p99 11ms   0.00%

push 同一用户拐点：10c 48.9 RPS(p99 227ms) → 50c 42.4(p99 2.752s) → 100c 41.5(p99 5.8s，54×503)
push 5 用户×10c：合计 ≈158.9 RPS，API CPU ≈2.38 核
限流 postgres 2 实例：5 放行 / 35×429；memory 对照：9 放行 / 31×429
```

#### 9.12.2 备份恢复 / 迁移往返 / 硬杀重启

```text
pg_dump -Fc 9.958s → 138MB，sha256 f344d19c…81c72
pg_restore -j4 15.403s → 15 张表 count + hashtext 校验和 diff 为空

17→16 down 1.99s：scenes 表 DROP，users/user_devices/readings/change_log 计数不变
16→17 up 0.125s：version=17 dirty=false，scenes 60→0

SIGKILL：advisory lock 1 → +0.4s 0 → +3.4s 0；重启 health 200；游标 1,086,355 后续 100 条 seq 连续
导出任务：重启后仍 pending，直到 24h 过期（v0.21.0 无启动对账；v0.22.0 已修复）
```

#### 9.12.3 上线前清单

阻塞项、上线前应完成项与可延后项的完整表格见 [`preflight.md`](preflight.md) §二；
本机可复现不等于生产已验证（域名、证书、告警投递、定时器、真机、商店、推送通道均未执行）。

### 9.13 v0.22.0 三项修复（真实输出）

```text
# 1. /sync/changes 可见性下限（pjgoiot_load，1,112,155 行 change_log / 21,600 设备）
改造前  Index Scan using change_log_pkey，Rows Removed by Filter: 225,355
        p50 62.77ms  p95 66.28ms  buffers 87,928
改造后  Index Only Scan ... LIMIT 1（本人分支 + 家庭分支），Heap Fetches: 0
        p50 0.35ms   p95 0.56ms   buffers 10

# 2. 导出对账（真实 PG 集成测试）
        pending 且 created_at < now()-5min → failed + failureReason
        两实例并发：对账总条数 1（无重复）；重复对账 0；ready 与新 pending 不受影响

# 3. token 刷新与吊销（真实 PG 集成测试）
        refresh 后旧 token 401、新 token 可用；旧 token 二次 refresh 仍 401
        sessions 列出 2 条（current 1 条）；吊销 1 条后对应 token 401
        跨用户吊销 404；重复吊销幂等；删除账号后 tokens=0

# 迁移：本次不需要（下限查询改用 000001/000011 已建的组合索引）
```

### 9.14 v0.23.0 推送通道、分页与告警链路（真实输出）

```text
# 1. 真实 PG 集成测试（pjgoiot_test）
$ TEST_DATABASE_URL='postgres://.../pjgoiot_test?sslmode=disable' \
    go test ./internal/store/postgres -run 'TestPostgresPushIntegration|TestPostgresScenePaginationIntegration' -v
=== RUN   TestPostgresPushIntegration
=== RUN   TestPostgresPushIntegration/注册幂等与跨用户隔离
=== RUN   TestPostgresPushIntegration/告警入队与投递状态机
--- PASS: TestPostgresPushIntegration (0.07s)
    --- PASS: TestPostgresPushIntegration/注册幂等与跨用户隔离 (0.03s)
    --- PASS: TestPostgresPushIntegration/告警入队与投递状态机 (0.02s)
=== RUN   TestPostgresScenePaginationIntegration
--- PASS: TestPostgresScenePaginationIntegration (0.03s)
ok  	github.com/platojobs/PJGoIoT/internal/store/postgres	0.759s

# 2. 告警链路（./scripts/verify-alerting.sh，本机 Docker）
PASS: 规则触发后 webhook 收到 VerifyAlertingPipeline
PASS: 重复告警去重生效（webhook 收到 1 次）
PASS: 静默生效（webhook 收到 0 次）
{"receivedAt":"2026-11-06T17:20:43.038263Z","status":"firing","alertname":"VerifyAlertingPipeline",
 "severity":"info","service":"pjgoiot-alert-verify","env":"local","fingerprint":"e39275674f5b2004",
 "summary":"本地验证：Prometheus 规则触发后应到达 alert-webhook",
 "startsAt":"2026-11-06T17:19:34.442Z","endsAt":"0001-01-01T00:00:00Z"}
{"receivedAt":"2026-11-06T17:20:44.55296Z","status":"firing","alertname":"DedupProbe","severity":"info",
 "service":"verify","env":"local","fingerprint":"49e822a70cc66101","summary":"dedup probe",
 "startsAt":"2026-11-06T17:19:32Z","endsAt":"0001-01-01T00:00:00Z"}
ALL PASS: 规则触发 -> Alertmanager -> webhook 链路在本地可用；未验证真实电话/短信/邮件通道。

# 3. dry-run 载荷（无 FCM/APNs 凭据时的日志，字段为结构化日志键）
push dry-run：未配置凭据，本应发送
  provider=fcm-dry-run platform=android userId=<uuid> alertId=<uuid> pushDeviceId=<uuid>
  token=<device-token>
  payload={"to":"<device-token>","notification":{"title":"...","body":"..."},
           "data":{"alertId":"<uuid>","kind":"..."},"priority":"high"}
  → 对应 push_deliveries 行：status=sent, dry_run=true, provider=fcm-dry-run
```

## 10. 本地栈自检步骤（可复现）

```bash
# 0) 前置：Docker daemon 可用，80/443/55432 未被占用
docker info --format '{{.ServerVersion}}'
lsof -nP -iTCP:80 -sTCP:LISTEN

# 1) 构建镜像（首次会拉取 golang/distroless/caddy/postgres）
docker compose build

# 2) 启动并观察 60s
docker compose up -d
for i in $(seq 1 12); do
  docker compose ps --format 'table {{.Name}}\t{{.Status}}'
  sleep 5
done

# 3) HTTPS / 健康 / 指标边界
curl -k -sS -w '\nHTTP %{http_code}\n' https://localhost/api/v1/health
curl -k -sS -w '\nHTTP %{http_code}\n' https://localhost/healthz
curl -k -sS -o /dev/null -w '%{http_code}\n' https://localhost/metrics   # 期望 404

# 4) 容器内指标（distroless 无 curl，用带 node 的容器进 edge 网络）
docker run --rm --network pjgoiot_edge node:20-alpine node -e \
  "fetch('http://api1:8080/metrics').then(r=>console.log(r.status))"    # 期望 200

# 5) 非 root / 只读（distroless 无 id，用 inspect/top）
docker inspect pjgoiot-api1-1 --format 'User={{.Config.User}} Readonly={{.HostConfig.ReadonlyRootfs}}'
docker top pjgoiot-api1-1 -o uid,pid,comm
echo x > /tmp/x && docker cp /tmp/x pjgoiot-api1-1:/x                     # 期望失败

# 6) 请求 ID
curl -k -sS -D - -o /dev/null -H 'X-Request-Id: local-verify-123' https://localhost/healthz | grep -i x-request-id
curl -k -sS -D - -o /dev/null https://localhost/healthz | grep -i x-request-id

# 7) 迁移日志（两个实例都应就绪；后到者等待锁）
docker compose logs api1 api2 | grep '启动迁移完成'

# 8) PromQL（无需宿主机 promtool）
docker run --rm --entrypoint /bin/promtool -v "$PWD/deploy/prometheus":/cfg \
  prom/prometheus:latest check rules /cfg/alerts.yml
docker run --rm --entrypoint /bin/promtool -v "$PWD/deploy/prometheus":/cfg \
  prom/prometheus:latest check config /cfg/scrape.yml

# 9) docker 模式备份 / 恢复
BACKUP_MODE=docker ./deploy/scripts/backup.sh
docker compose stop api1 api2
RESTORE_MODE=docker ./deploy/scripts/restore.sh --confirm --file ./backups/pjgoiot-<ts>.dump.gz
docker compose start api1 api2

# 10) v0.8.0 生命周期：清理与 410（先准备 3 台设备与 3 条 change_log）
TOKEN=$(curl -k -sS -X POST https://localhost/api/v1/auth/anonymous | jq -r .token)
docker compose --profile maintenance run --rm cleanup -dry-run          # 只看边界
docker compose --profile maintenance run --rm cleanup                   # 实际清理
docker compose exec -T postgres psql -U pjgoiot -d pjgoiot -c \
  "UPDATE change_log SET server_ts = now() - interval '100 days' WHERE seq = <最老 seq>;"
docker compose --profile maintenance run --rm cleanup
curl -k -sS -w '\nHTTP %{http_code}\n' \
  "https://localhost/api/v1/sync/changes?cursor=$((<最老 seq>-1))" -H "Authorization: Bearer $TOKEN"  # 期望 410

# 11) v0.8.0 压测（不进 CI；先关闭限流）
RATE_LIMIT_PER_MINUTE=0 docker compose up -d api1 api2
go run ./cmd/loadgen -base-url https://localhost -duration 30s -warmup 3s
go run ./cmd/loadgen -base-url https://localhost -scenario devices -concurrency 100 -duration 30s
go run ./cmd/loadgen -base-url https://localhost -scenario push    -concurrency 50  -duration 30s
go run ./cmd/loadgen -base-url https://localhost -scenario series  -concurrency 50  -duration 30s
RATE_LIMIT_PER_MINUTE=120 docker compose up -d api1 api2               # 恢复默认限流

# 12) v0.23.0 推送注册 / 告警入队 / dry-run（无凭据时）
AUTH=$(curl -k -sS -X POST https://localhost/api/v1/auth/anonymous)
TOKEN=$(echo "$AUTH" | jq -r .token)
USER_ID=$(echo "$AUTH" | jq -r .userId)
curl -k -sS -X POST https://localhost/api/v1/push/devices \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"platform":"android","token":"local-verify-token","appVersion":"1.0.0","deviceModel":"verify"}' | jq .
curl -k -sS -X POST https://localhost/api/v1/admin/alerts \
  -H "X-Admin-Token: ${ADMIN_TOKEN}" -H 'Content-Type: application/json' \
  -d "{\"userId\":\"$USER_ID\",\"kind\":\"verify\",\"title\":\"本地验证\",\"severity\":\"info\"}" | jq '{deliveryCount,alert}'
# worker 5s 内消费；日志应出现 push dry-run 完整载荷，投递行 status=sent / dry_run=true
docker compose logs api1 api2 | grep 'push dry-run' | tail -2
docker compose exec -T postgres psql -U pjgoiot -d pjgoiot -c \
  "SELECT status, dry_run, attempts, provider FROM push_deliveries ORDER BY created_at DESC LIMIT 2;"

# 13) v0.23.0 场景分页（不传参数仍全量）
curl -k -sS "https://localhost/api/v1/scenes" -H "Authorization: Bearer $TOKEN" | jq 'keys'
curl -k -sS "https://localhost/api/v1/scenes?limit=1" -H "Authorization: Bearer $TOKEN" | jq '{n:(.items|length),hasMore,nextCursor}'

# 14) v0.23.0 告警链路（Prometheus → Alertmanager → webhook）
./scripts/verify-alerting.sh

# 15) 收尾（保留卷）
docker compose down
```

注意：`curl -k` 仅用于本地自签证书；生产使用 ACME 后必须去掉 `-k` 并验证证书链。
