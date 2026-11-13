# PJGoIoT 运行手册（v0.26.0）

只保留会用到的步骤。上线阻塞项见 [`preflight.md`](preflight.md)，性能见
[`perf-results/summary.md`](perf-results/summary.md)。

## 1. 部署前检查

- [ ] `DOMAIN` 已解析、80/443 可达；防火墙只开 80/443，数据库只绑 `127.0.0.1:55432`。
- [ ] `.env` 密码强随机（`openssl rand -hex 24`），不入 git；`pgdata` 预留库大小 2 倍。
- [ ] `backups/` 在独立磁盘或对象存储（`umask 077`）；已读 §4 的回滚风险。
- [ ] 已抓取 `api:8080/metrics` 并导入 `deploy/prometheus/alerts.yml`。
- [ ] 已安装每日清理（§6），否则表无限增长。

## 2. 首次部署

```bash
cat > .env <<'EOF'
DOMAIN=localhost
POSTGRES_PASSWORD=change-me-strong-password
APP_VERSION=0.26.0
EOF
docker compose config --quiet && docker compose up -d --build
docker compose exec api1 /api -healthcheck && docker compose exec api2 /api -healthcheck
curl -k https://localhost/api/v1/health
curl -k -o /dev/null -w '%{http_code}\n' https://localhost/metrics   # 404 是预期
```

顺序：`postgres` healthy → `api1`/`api2` 各自迁移（PG advisory lock 串行）→ `caddy`。
迁移失败（含 dirty）进程非零退出，不会在旧 schema 上服务。

| 端点 | 用途 | 访问 DB | 失败时 |
| --- | --- | --- | --- |
| `GET /healthz` | liveness | 否 | 重启容器 |
| `GET /api/v1/health` | readiness | 是（2s Ping） | 摘流量，不重启 |

## 3. 升级（滚动发布）

```bash
./deploy/scripts/backup.sh                                # 1) 备份
docker compose run --rm --no-deps --entrypoint /migrate api1 -direction up  # 2) 预检迁移
APP_VERSION=<新版本> docker compose build                  # 3) 构建
bash scripts/rolling-restart.sh                           # 4) 逐实例摘流/重启/等健康
```

`rolling-restart.sh` 依次对 `api1`、`api2` 执行 stop（SIGTERM → readiness 503 → Caddy 摘流 →
继续服务 `SHUTDOWN_DRAIN_SECONDS`）→ start → healthy → 恢复流量；`NO_BUILD=1` 可复用现有镜像，
v0.11.0 实测停机请求 0。随后验证两台 `-healthcheck`、`X-Instance` 都能命中、抽查 `/my/devices`
与 `/sync/push`，观察 15 分钟无 `ApiHigh5xxRate`/`ApiDBErrorsIncreasing` 再结束变更窗口。

迁移前滚，必须先迁移后切流量；滚动窗口内新旧代码必须兼容同一 schema；`RATE_LIMIT_BACKEND=memory`
时额度会短暂叠加。

**tag 纪律**：已推送的 tag 禁止移动、删除或强制重指；发布后发现问题发递增版本（如 `v0.13.1`），
只有未推送时才可本地重打。tag 必须是 CI 全绿的 `main` 提交，用 annotated tag。

## 4. 回滚

- **只回滚镜像（推荐）**：把 compose 的 `APP_VERSION` 改回上一版，执行 `rolling-restart.sh`，
  再逐实例 `-healthcheck`。前提是新迁移未破坏旧代码依赖的列（本项目以加列/加表为主）。
- **回滚迁移（高风险）**：`docker compose run --rm --entrypoint /migrate api1 -direction down`。
  `000006` down **DROP readings**、`000007` down **DROP pairing_codes**、`000017` down
  **DROP scenes**，数据不可恢复；`000008` down 删除 `tokens.expires_at`，token 变长期有效。
  正确顺序：停 API → 备份 → 评估影响 → down → 起旧镜像 → 验证。
- **dirty**：先 `SELECT * FROM schema_migrations;` 确认真实 schema，修复失败语句或恢复备份；
  `cmd/migrate` 没有 `-force`，schema 修好后手工
  `UPDATE schema_migrations SET version=<已完成版本>, dirty=false;` 再启动。
- **数据库损坏/误删**：走 §5 恢复，不要用迁移回滚。

## 5. 备份、恢复与 Admin 轮换

```bash
# 备份（host 模式需 postgresql-client；BACKUP_MODE=docker 则走容器）
DATABASE_URL='postgres://pjgoiot:<pass>@127.0.0.1:55432/pjgoiot?sslmode=disable' \
BACKUP_DIR=./backups RETENTION_DAYS=14 ./deploy/scripts/backup.sh

# 恢复：先建临时库演练，确认后再对生产执行；容器模式先停 API
createdb -h 127.0.0.1 -p 55432 -U pjgoiot pjgoiot_restoretest
DATABASE_URL='postgres://pjgoiot:<pass>@127.0.0.1:55432/pjgoiot_restoretest?sslmode=disable' \
./deploy/scripts/restore.sh --confirm --file ./backups/pjgoiot-<ts>.dump.gz
```

产物 `pjgoiot-<UTC>.dump.gz` + `.sha256`；`restore.sh` 无 `--confirm` 拒绝，校验 sha256，
`pg_restore --clean --if-exists`，末尾打印 `schema_migrations` 与
`users/user_devices/readings/change_log/pairing_codes` 行数。验收：行数一致、`dirty=false`、
`/api/v1/health` 200。

**Admin 轮换**：新密钥 → `ADMIN_TOKEN`，旧密钥 → `ADMIN_TOKEN_PREVIOUS`，两实例写同一个
`ADMIN_TOKEN_ROTATED_AT`（RFC3339），窗口 `ADMIN_TOKEN_ROTATION_WINDOW_SECONDS`（默认 86400）后
旧密钥自动失效；配置不完整进程拒绝启动。回退：`ADMIN_TOKEN` 改回旧值并清空
`ADMIN_TOKEN_PREVIOUS`/`ADMIN_TOKEN_ROTATED_AT` 后滚动重启。未配置时运营路由 404。

## 6. 清理任务

`cmd/cleanup` 执行，API 不自动跑：

| 数据 | 默认保留 | 参数 | compose 变量 |
| --- | --- | --- | --- |
| `readings` | 90 天（`recorded_at`） | `-retention-days` | `READING_RETENTION_DAYS` |
| `change_log` | 90 天（`server_ts`） | `-change-log-retention-days` | `CHANGE_LOG_RETENTION_DAYS` |
| `idempotency_keys` | 7 天 | `-idempotency-retention-days` | `IDEMPOTENCY_RETENTION_DAYS` |
| `tokens` / `pairing_codes` | 过期即删 | 无 | 无 |
| `push_deliveries` / `push_devices` | 7 / 30 天 | `-push-*-retention-days` | `PUSH_DELIVERY_RETENTION_DAYS` / `PUSH_DEVICE_RETENTION_DAYS` |

```bash
docker compose --profile maintenance run --rm cleanup
docker compose --profile maintenance run --rm cleanup -dry-run   # 只看会删什么
```

`change_log` 保留期不得短于客户端最长离线时间，否则会触发 `410 RESYNC_REQUIRED`。
定时安装（文件已入库、未在生产安装）：systemd `deploy/systemd/pjgoiot-{backup,cleanup}.{service,timer}`
（UTC 03:30 / 04:17，`Persistent=true`，`EnvironmentFile=/etc/pjgoiot/*.env` 0600），或
`deploy/cron/crontab.example`（busybox cron 不支持 `CRON_TZ`，需换算本地时区）。
核对：`SELECT min(seq), max(seq) FROM change_log;`，正常输出形如
`readings_deleted=... change_log_deleted=... tokens_deleted=...`。

## 7. 常见故障

| 现象 | 原因 / 处理 |
| --- | --- |
| 启动即退出 | `DATABASE_URL` 主机名用 compose 服务名 `postgres`，密码特殊字符需 URL 编码；看 `docker compose logs api1` |
| `Dirty database version N` | 查 `schema_migrations` 与备份，修好后手工清 dirty（见 §4）；不要反复重启 |
| 大量 429 | 调 `RATE_LIMIT_*`/`PAIRING_CLAIM_*`/`FEEDBACK_*`；`memory` 后端额度按实例数放大 |
| `/metrics` 404 | 公网 404 是 Caddy 预期；内网查 `METRICS_ENABLED` 与 scrape target |
| HTTPS 告警 | 本地自签正常；生产把 `tls internal` 换成 ACME |
| `410 RESYNC_REQUIRED` | 游标早于最老 seq；客户端清游标后 `cursor=0` 全量重拉，不要修补已删行 |
| `503 TIMEOUT` | 查询 3s / push 10s 预算或连接池排队；查 `pg_stat_activity`、`pg_stat_statements`、`DB_MAX_OPEN_CONNS` |
| `503 UNAVAILABLE` | DB 不可达，可重试；`database/sql` 自愈，Postgres 恢复后无需重启 API |
| `401 UNAUTHORIZED` | token 过期/吊销；重新登录，**不得**在 401 后静默换匿名号 |
| 400（limit 越界） | readings/changes 的 `limit` 上限 500（changes 默认 100），超限直接 400 |
| 滚动期间 502 | `SHUTDOWN_DRAIN_SECONDS` 至少覆盖 2 个健康检查周期；看 Caddy `no healthy upstreams` |
| 迁移锁等待 | 两实例同时启动属预期；查 `pg_locks` 的 advisory lock，勿手工并发迁移 |
| 运营入口 404 / 401 | 未配 `ADMIN_TOKEN` 为 404；`X-Admin-Token` 错误或缺失为 401 |

## 8. 监控与告警

抓取 `api:8080/metrics`；规则 `deploy/prometheus/alerts.yml`（9 条），面板
`deploy/grafana/dashboard.json`（`scripts/validate-dashboard.sh` 校验指标名）。

- SLO：可用性 `1 - 5xx/全部 ≥ 99.5%`（自然月，预算 0.5%）；`GET /my/devices` p95 < 300ms；
  `GET .../readings/series` p95 < 200ms。对应告警见 `alerts.yml` 的燃烧率与 p95 规则。
- 指标：`pjgoiot_http_requests_total{method,route,status}`、
  `pjgoiot_http_request_duration_seconds_bucket`、`pjgoiot_db_errors_total`、
  `pjgoiot_{users,devices,readings}_approx`（`reltuples` 近似值，不能计费/审计）。
- 日志：`request_id`/`method`/`path`/`status`/`duration_ms`/`ip`/`user_id`，不打 token 与配对码。
- Alertmanager 见 `deploy/alertmanager/alertmanager.yml`，receiver 指向 `cmd/alert-webhook`（`:8099`）；
  自检 `./scripts/verify-alerting.sh` 只验证到 webhook 收到，真实通知通道未验证。

## 9. 本地联调栈

```bash
bash scripts/stack-for-e2e.sh     # postgres + API（127.0.0.1）；配 scripts/seed-for-e2e.sh 造数
bash scripts/stack-for-e2e.sh --stop
bash scripts/migrate-drill.sh     # 空库、历史库、down→up 三场景演练
```

Android 模拟器用 `http://10.0.2.2:<port>`，iOS 模拟器用 `http://127.0.0.1:<port>`，base URL 以
`.e2e/stack.env` 为准。栈绑 `127.0.0.1`，真机经局域网访问未验证。
