# 压测与验证摘要

按版本逐份的详细报告已合并到这里；原始数字与更细的 SQL/EXPLAIN 见
[`../perf-baseline.md`](../perf-baseline.md)。结论只对「本机单点 + 真实 PostgreSQL」成立。

## 结论一览

| 主题 | 结论 | 复现 |
| --- | --- | --- |
| `/sync/changes` 多家庭 | 108 万行、16 家庭：floor p95 0.73–0.87ms（buffers 9–54，随家庭数线性）；主查询 cursor=0 从旧 OR/IN 的 ≈105ms / 16,282 buffers 降到新分支结构的 ≈3–4ms / 121 buffers | `scripts/perf-sync-floor.sh` |
| 迁移 19/20 对同步的影响 | 同一库 A/B（60/30 次采样）：floor 16 家庭 p95 0.905→0.869ms，主查询 cursor=0 p95 5.708→4.179ms，buffers 54/121 完全不变；无回退 | 同上，`RUNS=60 LEGACY_RUNS=30` |
| 可见性下限改造 | 111 万行时 p95 66.28→0.56ms、buffers 87,928→10（`change_log_pkey` 前缀扫描 → 两次 `Index Only Scan ... LIMIT 1`） | `scripts/perf-sync-floor.sh` |
| `/sync/changes` 长稳 | 20 并发、60s：78,446 请求、1,307 RPS、p95 35ms、0 错误；游标推进后 RPS 自然下降、p50 从 4ms 升到 20ms | `cmd/loadgen -scenario changes` |
| 场景长稳 | 40 并发 × 各 10 分钟：GET 2,170,840 请求 p95 20ms/p99 39ms；PUT 1,106,619 请求 p95 37ms/p99 48ms；0 错误 | `scripts/scenes-soak.sh` |
| 双客户端并发标记 `markedAt` | 真实 HTTP + PG，10 轮微秒/毫秒交错 push：LWW 结果唯一、无重复 `change_log`、`null` 不覆盖缺省、`...123456Z` 幂等回放不新增条目、游标照常推进；37 条断言 0 FAIL | `scripts/verify-marked-concurrency.sh` |
| 设备在线状态（0.21.0） | 读数上报才推进 `lastSeenAt`；同事务写 `user_device` change_log；14m50s=online、15m10s=offline、重报回 online；从未上报 `null/unknown`；跨账号互不影响 | `go test ./internal/store/postgres -run TestDevicePresencePostgres` |
| 备份恢复 | 138MB 库 dump 9.96s、restore 15.40s，15 张表校验和一致 | `deploy/scripts/backup.sh` / `restore.sh` |

## 复现命令

```bash
# 数据集：先迁移到 000018，再灌 108 万行 / 16 家庭
psql "$DB" -v ON_ERROR_STOP=1 -f scripts/perf-multi-household-seed.sql

# /sync/changes 数据库层
DB_URL='postgres://platojobs@127.0.0.1:5432/pjgoiot_perf?sslmode=disable' \
  RUNS=60 LEGACY_RUNS=30 scripts/perf-sync-floor.sh

# HTTP 层长稳 / 标记并发（自带起栈）
scripts/scenes-soak.sh
scripts/verify-marked-concurrency.sh
```

## HTTP 层 vs 真机

- **已用真实 HTTP + 真实 PG 验证**：标记并发、幂等回放、`change_log` 计数、游标推进、在线状态读写、
  e2e 栈的 `/my/devices` 与 `/sync/changes` payload。
- **只能真机验证**：BLE 扫描/连接本身、iOS `CBPeripheral` 与 Android MAC 的真实取值、
  后台唤醒与推送到达、客户端本地 SQLite/Room 的合并表现、真机弱网下的同步体验。
