# 上线前检查与故障演练（v0.23.0）

> 只写**事实状态**，不写「已完成」。演练数字均为本机真实执行，环境见
> [`docs/perf-baseline.md`](perf-baseline.md) 第 1 节（M1 Max 10 核 / 32 GB、PostgreSQL 16.15、
> 库 `pjgoiot_load`，迁移 000001–000018）。
>
> v0.22.0 修掉 v0.21.0 清单中「上线前应完成」的三项（`/sync/changes` 下限性能、导出任务崩溃对账、
> token 刷新与吊销）。逐项结论见第 1.4 节，原始性能对比见 `perf-baseline.md` §3.6。
>
> v0.23.0 补上服务端推送通道（设备令牌、投递队列、worker、FCM/APNs 适配层与 dry-run）、
> `/scenes` 可选游标分页、Alertmanager + webhook 最小闭环、备份/清理的 systemd timer 与
> 容器 cron 文件。逐项事实见第 1.5 节；**真实 FCM/APNs 投递、真实告警接收通道、定时器
> 在生产机安装均未执行**。

## 一、故障与恢复演练（三项）

### 1.1 备份 → 恢复到新库 → 行数与校验和比对

**目的**：证明 `pg_dump -Fc` 产物能在新库完整重建，且关键表内容一致。

**前置**：本地 PG 可连；库内已有真实数据（18 用户、≈21,000 设备、1,993,200 读数、1,071,255 条 `change_log`、≈1.7 GB）。

**执行步骤**：

```bash
# 备份（生产上 deploy/scripts/backup.sh 做的是同一件事 + gzip + sha256 + 保留期）
psql 'postgres://platojobs@127.0.0.1:5432/pjgoiot_load?sslmode=disable' -c "checkpoint"
time pg_dump -Fc -f /tmp/pjgoiot_load.dump \
  'postgres://platojobs@127.0.0.1:5432/pjgoiot_load?sslmode=disable'
ls -lh /tmp/pjgoiot_load.dump && shasum -a 256 /tmp/pjgoiot_load.dump

# 恢复到新库（生产上 deploy/scripts/restore.sh 会先校验 sha256，并要求 --confirm）
psql 'postgres://platojobs@127.0.0.1:5432/postgres?sslmode=disable' \
  -c "DROP DATABASE IF EXISTS pjgoiot_restore" -c "CREATE DATABASE pjgoiot_restore"
time pg_restore -d 'postgres://platojobs@127.0.0.1:5432/pjgoiot_restore?sslmode=disable' \
  --no-owner -j 4 /tmp/pjgoiot_load.dump

# 行数 + 内容校验和（每张表：count(*) 与 md5(sum(hashtext(row::text)))）
psql '<源库>' -tA -f /tmp/checksum.sql > /tmp/ck-src.txt
psql '<新库>' -tA -f /tmp/checksum.sql > /tmp/ck-restore.txt
diff /tmp/ck-src.txt /tmp/ck-restore.txt && echo IDENTICAL
```

`/tmp/checksum.sql` 覆盖 15 张表：`users`、`tokens`、`user_devices`、`readings`、`change_log`、
`scenes`、`feedback`、`feedback_status_events`、`personal_data_export_tasks`、`households`、
`household_members`、`household_rooms`、`device_type_catalog`、`pairing_codes`、`idempotency_keys`。
每张表算 `count(*)` 与 `md5(coalesce(sum(hashtext(t::text)),0)::text)`。

**本次执行结果**：

| 指标 | 结果 |
| --- | --- |
| 备份耗时 | **9.958s** |
| 产物大小 | **138 MB**（`-Fc` 压缩；源库约 1.7 GB） |
| sha256 | `f344d19c105b939ec6c6e6f6f13fc9a8fc50175cc0be046c140c54f98f681c72` |
| 恢复耗时（`-j 4`，新库） | **15.403s** |
| 15 张表行数 + 校验和 | **完全一致**（`diff` 无输出），含 1,071,255 条 `change_log`、1,993,200 条 `readings` |

**结论（事实）**：本机备份/恢复可用；生产环境未执行（无生产库、无云主机）。

### 1.2 迁移 `up → down → up`（在有数据的库上）

**目的**：证明回滚一步不会破坏用户核心数据，并明确回滚对 `scenes` 的影响。

**执行步骤**：

```bash
DB='postgres://platojobs@127.0.0.1:5432/pjgoiot_load?sslmode=disable'
for t in users user_devices readings change_log; do psql "$DB" -tAc "select '$t='||count(*) from $t"; done

time go run ./cmd/migrate -database-url "$DB" -direction down -steps 1   # 17 → 16
psql "$DB" -tAc "select to_regclass('scenes')"                            # 应为空
time go run ./cmd/migrate -database-url "$DB" -direction up               # 16 → 17
psql "$DB" -tAc "select version,dirty from schema_migrations"
for t in users user_devices readings change_log; do psql "$DB" -tAc "select '$t='||count(*) from $t"; done
psql "$DB" -tAc "select count(*) from scenes"                              # 应为 0
```

**本次执行结果**：

| 指标 | 结果 |
| --- | --- |
| down（17→16）耗时 | **1.99s**（含 `go run` 编译检查；纯迁移为秒级） |
| down 后 `scenes` 表 | `to_regclass('scenes')` 为空，表已 DROP |
| down 后核心数据 | `users` 18、`user_devices` ≈21,000、`readings` 1,993,200、`change_log` 1,071,255，**与 down 前一致** |
| up（16→17）耗时 | **0.125s** |
| up 后 `schema_migrations` | `version=17`、`dirty=false` |
| up 后核心数据 | 与 down 前一致 |
| up 后 `scenes` | **0 行** |

**结论（事实）**：000017 的 down 会 **DROP `scenes` 表**，回滚会丢失用户已上云的场景配置（60 行 → 0）。这是 golang-migrate `down` 的语义，不是数据损坏；**生产回滚前必须单独导出 `scenes`，或只做前滚修复（up）**。`users` / `user_devices` / `readings` / `change_log` 不受影响。

### 1.3 进程被硬杀后重启

**目的**：确认 `SIGKILL` 后 advisory lock 释放、`change_log` 游标连续、导出任务状态不出现假完成。

**执行步骤**：

```bash
PID=$(lsof -iTCP:18080 -sTCP:LISTEN -t)          # 单实例 API
# 运行中持续 push（5 并发）
./loadgen -base-url http://127.0.0.1:18080 -token "$TOKEN" -scenario push \
  -duration 8s -warmup 1s -push-concurrency 5 &
sleep 2.5
psql "$DB" -tAc "select count(*) from pg_locks where locktype='advisory' and granted"
kill -9 "$PID"
sleep 0.4; psql "$DB" -tAc "select count(*) from pg_locks where locktype='advisory' and granted"
sleep 3;   psql "$DB" -tAc "select count(*) from pg_locks where locktype='advisory' and granted"

# 重启后：健康、push、从杀前游标继续增量、pending 导出任务状态
curl -s http://127.0.0.1:18080/api/v1/health
curl -s -H "Authorization: Bearer $TOKEN" \
  "http://127.0.0.1:18080/api/v1/sync/changes?limit=100&cursor=$C"
curl -s -H "Authorization: Bearer $TOKEN" \
  "http://127.0.0.1:18080/api/v1/me/export/$TASK_ID"
```

为模拟「崩溃瞬间导出任务已建但未完成」，演练前用 SQL 注入一条 `personal_data_export_tasks(status='pending')` 记录（正常路径在毫秒级就会完成，难以精确命中）。

**本次执行结果**：

| 检查点 | 结果 |
| --- | --- |
| 杀前 advisory lock | **1** 个 granted（push 持有 `"push:"+userID`） |
| 杀后 +0.4s / +3.4s | **0 / 0**（连接断开后 PG 立即释放，无残留锁） |
| 重启后 `/api/v1/health` | 200 |
| 重启后 push | 3s、2 并发、0.00% 错误 |
| 从游标 `1,086,355` 继续 | 200，返回 100 条，seq 区间 **1,086,356–1,086,455**，连续无缺 |
| 杀前写入是否丢失 | 无：`change_log.max(seq)` 继续增长，seq 唯一约束未报冲突 |
| pending 导出任务 | **重启后仍为 `pending`**，`GET /me/export/{id}` 返回 200 + `status=pending`，直到 24h `expiresAt` 都没有任何重试/失败标记 |

**结论（事实）**：advisory lock 与游标在硬杀后自愈；v0.21.0 演练时**导出任务是进程内 goroutine，进程死亡后永久停在 `pending`**。v0.22.0 已加入启动对账（第 1.4.2 节），本节数字保留为修复前基线。

### 1.4 v0.22.0 修复三项的验证结果

#### 1.4.1 `/sync/changes` 可见性下限查询

- 改法：把 `user_id OR household_id IN (...)` 拆成「本人 MIN + 每个家庭 MIN」的 `UNION ALL`，
  每个分支走 `(user_id, seq)` / `(household_id, seq)` 的 `Index Only Scan LIMIT 1`。
  语义（410 判定、返回顺序、可见性并集）不变；**无需迁移**（两个组合索引在 000001/000011 已存在）。
- 同库同用户实测（1,112,155 行 `change_log`、21,600 台设备）：p50 **62.77ms → 0.35ms**、
  p95 **66.28ms → 0.56ms**、buffers **87,928 → 10**。完整计划与复现命令见 `perf-baseline.md` §3.6。
- 未验证：多家庭（>10 个）用户的 p95、生产级数据量（>1 亿行）、跨主机网络。

#### 1.4.2 导出任务崩溃对账

- 选择「标记 failed」而非重新入队：任务执行体没有持久队列/领取协议，重新入队需要额外实现
  防重复与产物写回；导出可随时重新申请且产物有 24h TTL，直接收敛并给出可读原因更简单可靠。
- 启动时以单条 `UPDATE ... WHERE status='pending' AND created_at < now()-5min` 对账；
  多实例并发启动时只有一次会推进状态，幂等；不触碰 `ready`/`failed`。
- `GET /me/export/{id}` 对 `failed` 返回 `failureReason`（「导出任务因服务重启中断，请重新发起导出」）。
- 测试（真实 PG）：超时 pending → failed 且带原因；未超时 pending 与 ready 不受影响；
  两个 Store 实例并发对账总条数 = 1；重复对账 = 0。
- 未验证：对账在**真实 SIGKILL 后重启**的完整客户端联调未重跑（用回拨 `created_at` 等价模拟）。

#### 1.4.3 token 刷新与吊销

- `POST /auth/refresh`：同一事务内 `SELECT ... FOR UPDATE` 校验旧 token 仍有效 → 吊销旧 token →
  写入新 token（滚动 180 天 TTL）。旧 token 立即 401，且刷新无法复活已吊销 token；
  并发刷新同一 token 只有一个成功。
- `GET /sessions`：列出未吊销、未过期的会话（不返回 token 本身），`current` 标记本次会话。
- `DELETE /sessions/{id}`：按账号范围吊销；跨用户/不存在统一 404，重复吊销幂等 204。
- 删除账号沿用既有单事务硬删除，`tokens` 随账号级联清理。
- 测试（真实 PG）：刷新后旧 token 401 / 新 token 可用 / 旧 token 二次刷新 401 / 二次刷新再轮换；
  吊销指定会话后该 token 401 且刷新仍 401；跨用户吊销 404；重复吊销幂等；删除账号后 tokens=0。
- 未验证：客户端在刷新失败时的重新登录体验；`ADMIN_TOKEN` 仍是单一静态密钥（不在本项范围）。

### 1.5 v0.23.0 新增能力的验证结果

#### 1.5.1 设备推送令牌与投递队列

- 迁移 `000018_push_and_alerts` 新增三张表：`push_devices`（唯一键 `(user_id, token)`，
  注册即幂等更新并重置为 `active`）、`alerts`、`push_deliveries`（投递队列）。
- 接口：`POST /push/devices`（Bearer，本人）、`DELETE /push/devices/{id}`（软停用为 `inactive`，
  不硬删）、`POST /admin/alerts`（`X-Admin-Token`，运营/测试入口，与告警同事务写队列）。
- 真实 PG 测试（`TestPostgresPushIntegration`）：同一用户同一 token 重复注册只保留一行且
  `status=active`；跨用户同 token 各自一行互不影响；注册缺 platform/token 返回
  `ErrPushDeviceInvalid`；告警创建时同事务写入每个 active 设备一条 `pending` 投递；
  `ClaimPushDeliveries` 抢占后 `attempts=1`、`status=sending`；dry-run 完成后
  `status=sent, dry_run=true`；可重试错误写入 `next_attempt_at` 退避；终态错误把令牌置
  `inactive` 且投递 `failed`；`sending` 超时未完成的记录可被下一轮回收为 `pending`。
- `cmd/cleanup` 新增两个保留期：投递记录 7 天（`-push-delivery-retention-days`）、
  失效设备 30 天（`-push-device-retention-days`）。
- **未验证**：真实 FCM/APNs 投递（无账号与凭据）、真机收到通知、厂商错误码在真实环境下的分类。

#### 1.5.2 dry-run 载荷

无凭据时不做网络请求，但把完整载荷写入结构化日志，并由 worker 把该条投递标记
`sent` + `dry_run=true`（队列不会卡住）。日志字段为 `provider`、`platform`、`userId`、
`alertId`、`pushDeviceId`、`token`、`payload`（JSON）。样例见
[`runbook.md`](runbook.md) §9.14。

#### 1.5.3 `/scenes` 游标分页

- `GET /scenes` 支持可选 `limit`（1–200）与 `cursor`；**不传参数时返回全量且响应体只有
  `items`**，与 v0.22.0 行为一致（响应中不出现 `nextCursor`/`hasMore`）。
- 边界测试（HTTP + 真实 PG）：`limit=1` 游标遍历顺序稳定、条数正确；非法 `limit`
  （`0`/`abc`/负数）与非法 `cursor` 均 400。
- 契约 `SceneList` 的 `nextCursor`/`hasMore` 为可选字段；契约版本升到 **0.18.0**。

#### 1.5.4 告警链路本地验证

`scripts/verify-alerting.sh` 用 Docker 起 Prometheus v2.55.0 + Alertmanager v0.27.0 +
本地 `cmd/alert-webhook`，实跑结果 **ALL PASS**：

1. 验证规则 `VerifyAlertingPipeline`（`vector(1)`）触发 → Alertmanager → webhook 收到 1 条；
2. 同一告警重复提交两次，webhook 只收到 **1** 次（`group_by` 去重）；
3. 创建 silence 后再提交 `SilenceProbe`，webhook 收到 **0** 次（静默生效）。

**验证边界**：只验证到 webhook 收到；**未验证**真实电话/短信/邮件/Slack 通道、生产
Alertmanager 部署与外部可达性。服务端自身的告警（`Post /admin/alerts`）走内部投递队列，
与 Prometheus → Alertmanager 是两条链路，不互相替代。

#### 1.5.5 定时任务文件

- 已提供 `deploy/systemd/pjgoiot-backup.{service,timer}`、`pjgoiot-cleanup.{service,timer}`
  （UTC 03:30 / 04:17，`Persistent=true`、随机延迟）与 `deploy/cron/crontab.example`
  （主机 cron + `docker compose run --rm --entrypoint /cleanup api`）。
- **已提供但未在生产环境安装**；本机没有 systemd/cron 生产宿主，安装与验证命令见
  [`runbook.md`](runbook.md) §6.2–§6.3。

## 二、上线前清单（按分级）

分级口径：**阻塞上线** = 不做就不能对外发布；**上线前应完成** = 首发可勉强不带，但会留下明确风险；**可延后** = 不阻塞首发。

### 2.1 阻塞上线

| 事项 | 为什么重要 | 当前状态（事实） | 补齐需要什么 |
| --- | --- | --- | --- |
| 真实域名 + ACME 证书 | 客户端 baseURL、App 审核与 HTTPS 可用性都依赖它 | `deploy/Caddyfile` 仍是 `tls internal` 自签；真实域名、80/443 可达、证书签发**从未执行**；仓库未开通任何云账号 | 一个域名、一台可 80/443 出网的主机、DNS 控制权；执行一次真实签发并验证续期 |
| 备份定时化 | 没有定时备份，任何误删/故障都不可回滚 | `deploy/scripts/backup.sh` 可用（本机演练通过）；v0.23.0 已提供 `deploy/systemd/pjgoiot-backup.{service,timer}` 与 `deploy/cron/crontab.example`，但**未在任何生产机安装**；生产库未跑过 | 目标主机 + 备份目录/对象存储 + 按 runbook §6.2/§6.3 安装定时器；恢复演练在**生产同规格库**复跑一次 |
| 告警真实投递 | 规则存在但没人收到，等于没有告警 | `deploy/prometheus/alerts.yml` 有 5 条规则；v0.23.0 新增 `deploy/alertmanager/alertmanager.yml`、`cmd/alert-webhook` 与 `scripts/verify-alerting.sh`，**本机实跑验证到 webhook 收到（含去重与静默）**；**未接入任何真实电话/短信/邮件/Slack 通道，未在生产线部署** | 一个真实接收通道与值班人；按 runbook §8.3 部署 Alertmanager 并人为触发一次确认送达 |
| 客户端真机验收 | 后端再稳，客户端没连过真实服务端就不算上线 | 两端 App **未在真机连真实后端跑完整流程**；仓库只有 `scripts/stack-for-e2e.sh` 绑 `127.0.0.1` 的模拟器路径 | Android/iOS 真机各一台、局域网或公网可达的 API、真机安装包 |
| BLE 真机流程 | 配网/连接/断连/重连是产品核心路径，模拟器无法覆盖 | 客户端侧**未执行**；服务端只验证了 push/series 的 HTTP 契约 | 真实 BLE 设备与两台真机；端到端脚本/验收清单 |
| 签名与 TestFlight / 上架 | 没有签名与商店包就无法分发 | 未执行：无开发者账号证书、无 TestFlight 构建、无商店素材与隐私清单 | Apple/Google 开发者账号、签名证书、商店后台权限 |
| 服务端推送通道（按产品承诺判定） | 场景的 `notify` 若要求「设备离线也能提醒」，必须有 APNs/FCM | **v0.23.0 服务端链路已具备**：`push_devices` 表与注册/停用接口、告警同事务入队、worker 消费、FCM/APNs 适配层（`FCM_SERVER_KEY` / `APNS_*` 环境变量）、无凭据 dry-run、重试与失效停用、cleanup 保留期；真实 PG 测试覆盖。**但真实 FCM/APNs 投递与真机收到通知未验证**（无账号/凭据/设备） | 若首发只接受「App 在前台/本地调度时通知」，可降级为不阻塞；否则需 FCM/APNs 凭据 + 真机验收 |

### 2.2 上线前应完成

| 事项 | 为什么重要 | 当前状态（事实） | 补齐需要什么 |
| --- | --- | --- | --- |
| 多实例限流后端设为 postgres | `memory` 后端下额度随实例数放大，限流失效 | 本轮实测：`postgres` 2 实例交替 40 请求只放行 5 次（额度共享）；`memory` 2 实例放行 9 次（≈2 倍）。但 `docker-compose.yml` 默认仍是 `RATE_LIMIT_BACKEND=memory` | 部署时显式设置 `RATE_LIMIT_BACKEND=postgres`（要求 `STORE=postgres`）；写进部署清单与验收脚本 |
| `change_log` 可见性下限查询性能 | 新用户每次增量同步都要先付这笔成本，随全局行数线性变慢 | **v0.22.0 已修复（本机实测）**：同库 1,112,155 行 / 21,600 设备下 p95 66.28ms → **0.56ms**、buffers 87,928 → **10**；计划由 `change_log_pkey` 前缀扫描变为两个 `Index Only Scan ... LIMIT 1`。详见 `perf-baseline.md` §3.6 | 多家庭用户与生产级数据量（>1 亿行）补测 |
| 导出任务崩溃对账 | 崩溃后用户看到永久 `pending`，且无失败提示 | **v0.22.0 已修复**：启动时把创建超过 5 分钟仍 `pending` 的任务标记 `failed` 并写可读原因；多实例并发幂等；真实 PG 测试覆盖超时/新任务/ready/重复对账。真实 SIGKILL 重启联调未重跑 | 在生产同规格环境做一次真实杀进程 → 重启 → 客户端提示的完整演练 |
| 清理任务定时化 | 反馈/变更/幂等/限流桶持续增长，本机 30s 压测写入 38 万条反馈 | `cmd/cleanup` 已实现（默认保留 90 天、幂等 7 天，v0.23.0 新增推送投递 7 天/失效设备 30 天）；v0.23.0 已提供 systemd timer 与容器 cron 文件，但**未安装**；仅本机手工执行过 | 按 runbook §6.2/§6.3 安装并保留一次真实执行记录 |
| 限流其实会让压测数字更差 | 生产默认 120/min + burst 40，客户端批量推送会被限 | 压测全部在限流关闭下完成；未测「开启默认限流」时的端到端吞吐与 429 比例 | 用默认限流跑一轮 push/changes，记录 429 比例与 `Retry-After` |
| token 生命周期 | token 泄露后无法吊销，只能等过期；admin 单密钥无轮换 | **v0.22.0 已补齐用户侧**：`POST /auth/refresh` 滚动轮换（旧 token 立即失效）、`GET /sessions`、`DELETE /sessions/{id}`；删除账号级联清理。**未覆盖**：`ADMIN_TOKEN` 仍是单一静态密钥，无多密钥轮换/吊销 | 用户侧客户端接入刷新流程；admin 多密钥与审计 actor 方案 |
| `scenes` 列表分页 | 场景多时响应无上界 | **v0.23.0 已实现**：可选 `limit`（1–200）/`cursor`，不传参数返回全量且响应与旧版一致；HTTP + 真实 PG 边界测试覆盖。见第 1.5.3 节 | 客户端按需接入分页（不接入也不破） |
| 长稳压与多实例混合负载 | 30s 短跑看不出内存/连接泄漏 | 未执行：本轮最长 30s/项；多实例只在限流专项起过 2 个 postgres 实例 | 1–4 小时混合负载（push/changes/scenes/feedback 同时），观察 RSS、连接池、GC、PG 连接数 |
| 反馈审计留存 | 审计表随反馈级联删除，运营追溯会一起消失 | 未改：`feedback_status_events` 随反馈删除；未评估保留期 | 决定是否独立归档/软删反馈，或延长保留期 |
| `cmd/loadgen` 全场景与 CI smoke 一致 | CI 只跑原有三场景，新路径无回归 | 本版新增 changes/scenes/feedback/catalog 场景，但 `-smoke` 只注册原有三场景（避免短跑造数据与阈值缺失） | 若要 CI 覆盖新路径，需要独立的 seed/清理与阈值配置；当前是有意不纳入 |

### 2.3 可延后

| 事项 | 为什么重要 | 当前状态（事实） | 补齐需要什么 |
| --- | --- | --- | --- |
| 客户端切 `/catalog/device-types` | 目录已上架，但客户端仍用本地常量 | 两端 App 未切换；缓存失效、离线回退、灰度开关未验证 | 客户端版本迭代 + 灰度开关 |
| 客户端切 `/scenes` | 场景已上云，但执行仍在本地 | 两端 App 仍以 SQLite/Room 为执行源；首次同步、冲突合并、`deviceMissing` UI 未验证 | 客户端同步实现 + 真机验收 |
| `schedule` 的 DST / 跨时区 | 定时场景在夏令时切换日可能偏移/重复 | 服务端只做结构与时区名校验；客户端本地调度未实测 | 客户端在 DST 切换日实测，或限定常用时区 |
| 家庭 / 房间多设备协同 | 共享设备的并发编辑与冲突提示 | 服务端逻辑已有测试；**未在真机多设备**验收 | 两台真机 + 同一家庭 |
| 运营后台 admin 并发 | 共享静态 token 下多实例并发 PATCH 未压测 | 未测：审计 actor 固定 `admin`，无法区分操作人 | 运营后台接入方案 + 多实例压测 |
| 真实量产设备型号 / 固件 / 配件 SKU | seed 只有公开开发板数据 | 未编造：目录 seed 仅覆盖开发板与标准 GATT | 厂商提供可核实来源 |
| 全链路 Tracing | 目前只有 `/metrics` 与 `X-Request-Id` | 未接入 OpenTelemetry / 收集端 | 选型 + 采样策略 |
| `/auth/login` 账号密码 | 目前返回 501，只支持匿名账号 | **未实现**（`handlers_auth.go:36`）；若产品只做匿名账号，可不阻塞 | 账号体系设计 + 邮箱/密码或 OAuth |
| 跨主机/真实网络的压测 | 本机回环数字不能代表公网 | 未执行；见 `perf-baseline.md` §4 | 独立压测机 + 与生产同规格的 API/PG |

## 三、本轮事实状态（供评审引用，不代表生产已验证）

- 本机真实 PostgreSQL 上的备份/恢复、迁移往返、硬杀重启、双实例共享限流、7 条路径压测全部可复现（命令见上、`docs/perf-baseline.md`）。
- v0.22.0 三项修复的代码与测试均在真实 PG `pjgoiot_test` 上执行通过；性能对比在 111 万行 `change_log` / 21,600 设备库上实测。
- v0.23.0 推送链路（注册幂等、跨用户隔离、告警同事务入队、抢占/完成/退避/停用/回收、cleanup）在真实 PG 上通过；`/scenes` 分页在真实 PG + HTTP 上通过；`scripts/verify-alerting.sh` 在本机实跑 ALL PASS（规则→webhook、去重、静默）。
- 代码与文档在提交前通过了 `gofmt`、`go vet`、全量测试（含真实 PG）与 OpenAPI lint；CI 结果待填。
- **生产环境未验证**：真实域名/ACME、真实 FCM/APNs 投递与真机收通知、真实告警接收通道（仅验证到 webhook）、systemd/cron 在生产机安装、真机客户端、商店上架。
