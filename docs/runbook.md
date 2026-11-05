# PJGoIoT 运行手册（v0.7.0）

> 适用范围：`docker-compose.yml` + `deploy/Caddyfile` 的单机部署形态（Caddy → API → PostgreSQL）。
> 本手册区分「已验证」与「未验证」，未实际执行的步骤不得当作已验证结论。

## 0. 本机验证状态（2026-11-05）

| 项目 | 状态 | 证据 |
| --- | --- | --- |
| `docker compose config` 静态解析 | **已验证** | 本机 Docker Compose v5.7.0，退出码 0 |
| `deploy/Caddyfile` 语法 | **已验证** | `caddy:2.8-alpine caddy validate` → `Valid configuration`；`caddy fmt --diff` 无差异 |
| `docker compose build` / 完整 `up` | **未验证** | 未实际构建镜像、未启动 api/postgres/caddy 全栈；仅做静态解析 |
| 启动自动迁移（宿主机二进制） | **已验证** | `cmd/api` + 真实 PostgreSQL，日志 `version=7 dirty=false`；错误 DSN 非零退出 |
| `/healthz` liveness / `/api/v1/health` readiness | **已验证**（宿主机） | 两者均 200，版本 `0.7.0`；容器内未验证 |
| `backup.sh` 真实备份 | **已验证** | 对本机 PostgreSQL 执行，产出 `.dump.gz` + `.sha256`，校验通过 |
| `restore.sh` 真实恢复 + 迁移/行数检查 | **已验证** | 恢复到临时库 `pjgoiot_restoretest`，校验和与行数摘要一致 |
| 备份保留策略删除 | **已验证** | 构造 10 天前假备份，`RETENTION_DAYS=1` 后被删除 |
| Prometheus `promtool check rules` | **未执行** | 本机无 `promtool`，仅做 YAML 结构 + 字段校验 |
| shellcheck / shfmt | **未执行** | 本机未安装，仅 `bash -n` |
| 生产域名 / ACME 证书 | **未验证** | 需要真实域名与 80/443 可达，本机未开通任何云账号 |
| systemd timer / 真实 cron | **未验证** | 仅提供示例文本，未在目标主机安装 |

## 1. 部署前检查清单

- [ ] 已确定域名 `DOMAIN`，DNS A/AAAA 指向本机，80/443 可从公网访问（仅本机自签可跳过）。
- [ ] 已生成强随机数据库密码，写入 `.env`（**不要提交到 git**）：
  `POSTGRES_PASSWORD=$(openssl rand -hex 24)`
- [ ] 已确认磁盘空间：`pgdata` 至少预留数据库大小 2 倍；`backups/` 独立磁盘或对象存储。
- [ ] 已确认备份目录权限：`umask 077`，仅运维账号可读。
- [ ] 已阅读 §5 回滚风险：**迁移 down 可能丢列/丢数据**，生产优先用镜像回滚 + 前滚兼容。
- [ ] 已配置监控抓取（内网 `api:8080/metrics`），并导入 `deploy/prometheus/alerts.yml`。
- [ ] 已配置每日清理任务（§7），否则读数不会按 90 天保留策略删除。
- [ ] 防火墙只开放 80/443；数据库端口绑定 `127.0.0.1:55432`，不得改为 `0.0.0.0`。

## 2. 首次部署

```bash
# 1) 准备环境变量（compose 自动读取同目录 .env）
cat > .env <<'EOF'
DOMAIN=localhost
POSTGRES_PASSWORD=change-me-strong-password
APP_VERSION=0.7.0
METRICS_ENABLED=true
RUN_MIGRATIONS_ON_BOOT=true
EOF

# 2) 静态校验编排
docker compose config --quiet

# 3) 构建并启动（postgres 健康后 api 才启动；api 启动时自动迁移）
docker compose up -d --build

# 4) 等待 api 健康（readiness 含数据库 Ping）
docker compose ps
docker compose exec api /api -healthcheck

# 5) 验证 HTTPS 终止与反向代理（自签证书用 -k）
curl -k https://localhost/api/v1/health
curl -k https://localhost/healthz
# /metrics 不应被公网读到（Caddy 返回 404）
curl -k -o /dev/null -w '%{http_code}\n' https://localhost/metrics
```

首次启动行为：

- `postgres` 先拉起并通过 `pg_isready` 健康检查；
- `api` 仅在 `postgres` healthy 后启动，并执行迁移（`RUN_MIGRATIONS_ON_BOOT=true`）；
- 迁移失败（含 dirty）→ 进程非零退出，`restart: unless-stopped` 会重试；**不会在旧 schema 上提供服务**；
- `caddy` 等 `api` 健康后才启动，避免 502。

健康探针区别：

| 端点 | 用途 | 是否访问数据库 | 失败含义 |
| --- | --- | --- | --- |
| `GET /healthz` | liveness（进程存活） | 否 | 进程卡死/未启动，应重启容器 |
| `GET /api/v1/health` | readiness（可服务） | 是（2s 超时 Ping） | 数据库不可用，摘流量但不要重启 |

## 3. 升级流程

1. **备份**（必须先做）：`./deploy/scripts/backup.sh`
2. **预检迁移**：只跑迁移、不启动服务：
   `docker compose run --rm --no-deps --entrypoint /migrate api -direction up`（本机未验证，需 Docker 环境）。
3. **构建新镜像**：`APP_VERSION=0.7.x docker compose build api`
4. **滚动替换**：`docker compose up -d api caddy`
5. **验证**：
   - `docker compose exec api /api -healthcheck`
   - `curl -k https://$DOMAIN/api/v1/health`
   - `curl -k https://$DOMAIN/healthz`
   - 抽查 `GET /my/devices`、`POST /sync/push`（用测试账号）；
   - 查看 `/metrics` 的 5xx 与延迟是否正常。
6. **观察 15 分钟**：告警 `ApiHigh5xxRate`、`ApiDBErrorsIncreasing` 无触发后再结束变更窗口。

升级注意：

- 迁移是**前滚**设计；新版本必须先迁移再切流量。
- `RUN_MIGRATIONS_ON_BOOT=false` 仅用于「迁移由独立 Job 执行」的编排；此时必须保证迁移先完成。

## 4. 回滚流程

### 4.1 只回滚镜像（推荐）

```bash
# 将 compose 中 api 镜像/构建版本改回上一版本号后
docker compose up -d --no-deps api
docker compose exec api /api -healthcheck
```

前提：**新版本迁移没有破坏旧代码依赖的列/语义**。本项目 000003–000007 迁移均为加列/加表，
旧版本代码可继续读取；若未来出现删列/改类型迁移，必须评估后再回滚。

### 4.2 回滚迁移（高风险，谨慎）

```bash
# 只回退一步（示例：7 → 6）
docker compose run --rm --entrypoint /migrate api -direction down
```

风险：

- `000006` down 会 **DROP readings 表**，`000007` down 会 **DROP pairing_codes 表**——数据不可恢复；
- 回滚后旧镜像可能因缺少列而报错；
- 若迁移处于 `dirty` 状态，**不要盲目 down**：先 `SELECT * FROM schema_migrations` 确认版本，
  修复失败语句或用备份恢复，再清理 dirty 标记。
- 正确顺序：停 API → 备份 → 评估 down 影响 → 执行 down → 启动旧镜像 → 验证。

### 4.3 灾难恢复

数据库损坏/误删时走 §6 的恢复流程，而不是迁移回滚。

## 5. 备份与恢复演练

### 5.1 备份

```bash
# 宿主机安装 postgresql-client 后（或 BACKUP_MODE=docker）
DATABASE_URL='postgres://pjgoiot:<pass>@127.0.0.1:55432/pjgoiot?sslmode=disable' \
BACKUP_DIR=./backups RETENTION_DAYS=14 \
./deploy/scripts/backup.sh
```

产物：

- `backups/pjgoiot-<UTC时间戳>.dump.gz`（`pg_dump -Fc` + gzip）
- `backups/pjgoiot-<UTC时间戳>.dump.gz.sha256`

失败（`pg_dump` 非零、空文件、缺工具、校验和工具缺失）都会非零退出，可直接接入告警。

### 5.2 恢复（演练建议用临时库）

```bash
# 建临时库，避免覆盖生产
createdb -h 127.0.0.1 -p 55432 -U pjgoiot pjgoiot_restoretest

DATABASE_URL='postgres://pjgoiot:<pass>@127.0.0.1:55432/pjgoiot_restoretest?sslmode=disable' \
./deploy/scripts/restore.sh --confirm --file ./backups/pjgoiot-<ts>.dump.gz
```

脚本行为：

1. 无 `--confirm` 直接拒绝（退出码 2）；
2. 校验 `.sha256`，不一致立即中止；
3. `pg_restore --clean --if-exists` 恢复；
4. 查询 `schema_migrations` 输出 `version/dirty`；
5. 输出 `users/user_devices/readings/change_log/pairing_codes` 行数摘要。

演练验收：行数摘要与备份时点一致、`dirty=false`、API 指向临时库时 `/api/v1/health` 为 200。

## 6. 清理任务（读数 90 天）

读数保留依赖 `cmd/cleanup`，**不是** API 自动执行。

### 6.1 compose profile

```bash
docker compose --profile maintenance run --rm cleanup
# 等价于 /cleanup -retention-days 90；同时清理过期配对码
```

### 6.2 systemd timer 示例（未验证）

`/etc/systemd/system/pjgoiot-cleanup.service`：

```ini
[Unit]
Description=PJGoIoT readings cleanup
After=docker.service
Requires=docker.service

[Service]
Type=oneshot
WorkingDirectory=/opt/pjgoiot
ExecStart=/usr/bin/docker compose --profile maintenance run --rm cleanup
```

`/etc/systemd/system/pjgoiot-cleanup.timer`：

```ini
[Unit]
Description=Daily PJGoIoT cleanup

[Timer]
OnCalendar=*-*-* 03:15:00
Persistent=true

[Install]
WantedBy=timers.target
```

启用：`systemctl enable --now pjgoiot-cleanup.timer`（本机未验证）。

## 7. 常见故障

### 7.1 API 启动即退出 / 数据库连接失败

- 看 `docker compose logs api`，确认 `DATABASE_URL` 主机名是 `postgres`（compose 服务名），不是 `localhost`；
- `pg_isready` 是否健康：`docker compose ps postgres`；
- 密码含特殊字符需 URL 编码；`sslmode=disable` 仅用于内网；
- 迁移失败会打印 `启动迁移失败`，进程退出是预期行为。

### 7.2 迁移 dirty

- 现象：启动日志 `Dirty database version N. Fix and force version.`；
- 处理：`SELECT * FROM schema_migrations;` 确认实际 schema；修复失败 SQL 或用备份恢复；
- 清理 dirty（确认 schema 已正确后）：`docker compose run --rm --entrypoint /migrate api -direction force -version <N>`；
- **不要**在 dirty 状态下反复重启。

### 7.3 大量 429

- `RATE_LIMIT_PER_MINUTE`/`BURST` 过小，或客户端重试风暴；
- 配对码领取有独立限流：`PAIRING_CLAIM_RATE_PER_MINUTE`/`PAIRING_CLAIM_BURST`；
- 通过访问日志按 `ip`、`path` 聚合定位来源；确认没有共享出口 IP 误伤。

### 7.4 `/metrics` 为空或 404

- 公网访问 404 是**预期**（Caddy 显式拒绝），应改为内网直连 `api:8080/metrics`；
- 容器内 404：检查 `METRICS_ENABLED` 是否为 false；
- 容器内 200 但序列缺失：Prometheus `targets` 是否健康、`scrape.yml` 的 job 是否加载。

### 7.5 HTTPS 证书告警

- 本地 `localhost` 使用 Caddy 内部 CA，自签告警正常；
- 生产需把 `deploy/Caddyfile` 的 `tls internal` 改为 `tls <邮箱>`，并确保 80/443 可达；本机未验证真实 ACME 签发。

## 8. 监控与告警

- 抓取目标：`api:8080/metrics`（内网），样例见 `deploy/prometheus/scrape.yml`；
- 告警规则见 `deploy/prometheus/alerts.yml`（5xx 比例、P95 延迟、DB 错误、readiness 失败、请求量为 0、指标缺失）；
- `/metrics` 的业务计数为 `pg_class.reltuples` 近似值，不应作为计费/审计依据；
- 访问日志字段：`request_id`、`method`、`path`、`status`、`duration_ms`、`ip`、`user_id`（不打印 token/配对码）。

## 9. 本机实际验证记录（v0.7.0 发布时）

发布前实际执行（宿主机，无云账号；Docker CLI 存在但**未启动全栈**）：

```text
gofmt -l .                               # 除 sqlcgen 外无输出
go build ./... && go vet ./...           # 通过
TEST_DATABASE_URL=... go test ./...      # 通过（含真实 PostgreSQL 集成测试）
docker compose config --quiet            # 退出码 0（Docker Compose v5.7.0）
caddy validate / caddy fmt --diff        # 官方 caddy:2.8-alpine 容器，Valid configuration / 无差异
ruby -ryaml -e 'YAML.load_file(...)'     # compose + alerts + scrape + ci.yml 均通过
bash -n deploy/scripts/backup.sh         # 通过
bash -n deploy/scripts/restore.sh        # 通过
```

启动迁移 / 探针（真实 PostgreSQL）：

```text
RUN_MIGRATIONS_ON_BOOT=true STORE=postgres DATABASE_URL=... go run ./cmd/api
# 日志：启动迁移完成 version=7 dirty=false / 服务启动 version=0.7.0
curl /healthz                → 200 {"service":"pjgoiot-api","status":"ok","version":"0.7.0"}
curl /api/v1/health          → 200 {"status":"ok","store":"postgres","version":"0.7.0"}
错误 DSN 启动                 → 日志「启动迁移失败」，进程非零退出
```

备份 / 恢复演练（真实 PostgreSQL）：

```text
backup.sh                    → pjgoiot-20261105T021017Z.dump.gz（67 999 bytes）+ .sha256
restore.sh（无 --confirm）    → 退出码 2，拒绝执行
restore.sh（校验和损坏）      → 退出码 1，sha256 FAILED
restore.sh --confirm         → 恢复到 pjgoiot_restoretest：
                                schema_migrations version=7 dirty=f
                                users=104 user_devices=267 readings=248 change_log=495 pairing_codes=2
保留策略                      → RETENTION_DAYS=1 删除 10 天前的假备份
```

未执行/未验证：

- 未实际执行 `docker compose build` / `docker compose up`，因此 **Caddy 自签 HTTPS、容器内健康检查、
  非 root/只读文件系统、caddy→api 反代均未验证**；
- 未在真实域名上验证 ACME 证书签发；
- 未安装 systemd timer；
- 本机无 `promtool`/`shellcheck`/`shfmt`，告警规则未做 PromQL 语义校验；
- 未做长时间压测与故障注入。
