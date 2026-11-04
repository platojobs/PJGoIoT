# 已知问题与未验证项

本文件记录当前无法在本机完成验证、或明确尚未实现的事项。标记为“未验证”的条目不要当作已验证能力对外承诺。

## 未在本机验证

| 项目 | 状态 | 原因 / 处理 |
| --- | --- | --- |
| `docker build` 与 `docker compose up` | 未验证 | 本机未安装 Docker（`docker: command not found`）。Dockerfile、docker-compose.yml 已按 distroless 多阶段方案编写，CI 已包含 `docker build`，首次推送后由 GitHub Actions 实际验证。 |
| GitHub Actions CI | 未运行 | 仓库首次推送后才会触发。工作流包含 gofmt 检查、`go vet`、`go build`、`go test`、Postgres service、迁移与集成测试、镜像构建。 |
| 云环境部署 | 未进行 | 需用户提供服务器、域名、HTTPS 与托管数据库等决策；仓库不擅自开通账号。 |

## 已在本机验证

- `go build ./...`、`go vet ./...`、`go test ./...` 通过。
- 真实 PostgreSQL 上执行迁移（version=2）与 `TestPostgresStoreIntegration` 通过。
- Postgres 模式下 HTTP 全接口 curl 验证通过，包括幂等重放、409、LWW 冲突、tombstone 404、跨用户 404、注销后 401。

## 功能限制

- `POST /api/v1/auth/login` 返回 501，仅提供匿名账号。
- token 不设过期时间，只支持通过 `/auth/logout` 吊销；无刷新机制。
- 限流为单进程内存令牌桶，多副本部署时各自计数。
- fixture 模式数据仅存内存，重启丢失。
- 公共元数据列表未分页。
- seed 仅覆盖公开开发板与标准 GATT 服务；真实量产型号、固件发布记录、官方配件 SKU 需可核实来源后再补，未编造。
