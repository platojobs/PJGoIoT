# 已知问题与未验证项

本文件记录当前无法在本机完成验证、或明确尚未实现的事项。标记为“未验证”的条目不要当作已验证能力对外承诺。

## 未在本机验证

| 项目 | 状态 | 原因 / 处理 |
| --- | --- | --- |
| `docker compose up` | 未验证 | 本机未安装 Docker。Dockerfile 已通过 GitHub Actions `docker-build` job 验证；compose 依赖的镜像、迁移与健康检查配置尚未在真实 Docker 中启动验证。 |
| 云环境部署 | 未进行 | 需用户提供服务器、域名、HTTPS 与托管数据库等决策；仓库不擅自开通账号。 |

## 已在本机验证

- `go build ./...`、`go vet ./...`、`go test ./...` 通过。
- 真实 PostgreSQL 上执行迁移（version=5）与 `TestPostgresStoreIntegration`、`TestMigrationBackfillsLegacyDevices` 通过；000004 的 `device_type`/厂商/兼容性回填与 000005 的 `advertised_name` 默认值已验证。
- 分组清除、字段缺失保持、别名/广播名隔离在 fixture 与 PostgreSQL 两种存储下均已通过 HTTP 与集成测试。
- Postgres 模式下 HTTP 全接口 curl 验证通过，包括幂等重放、409、LWW 冲突、tombstone 404、跨用户 404、注销后 401、Android/iOS 同一用户双记录互不覆盖。
- GitHub Actions v0.3.2 运行通过（run 41897825528，提交 10fb27b）：gofmt、vet、build、单元测试、迁移、Postgres 集成测试、distroless 镜像构建。
- GitHub Actions v0.3.1 运行通过（run 41891902083，提交 1e3f451）：gofmt、vet、build、单元测试、迁移、Postgres 集成测试、distroless 镜像构建。
- GitHub Actions v0.2.0 运行通过（run 41881913964，提交 5524481）：gofmt、vet、build、单元测试、迁移、Postgres 集成测试、distroless 镜像构建。

## 功能限制

- `POST /api/v1/auth/login` 返回 501，仅提供匿名账号。
- token 不设过期时间，只支持通过 `/auth/logout` 吊销；无刷新机制。
- 限流为单进程内存令牌桶，多副本部署时各自计数。
- fixture 模式数据仅存内存，重启丢失。
- 公共元数据列表未分页。
- `/my/devices/{mac}` 仍按 mac 元数据查询；iOS 应使用 `/my/devices` 列表并以 `platform + externalId` 定位，暂未提供按复合键查询单台的接口。
- seed 仅覆盖公开开发板与标准 GATT 服务；真实量产型号、固件发布记录、官方配件 SKU 需可核实来源后再补，未编造。
