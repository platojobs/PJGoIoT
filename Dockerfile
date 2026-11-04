# 多阶段构建：编译阶段使用官方 Go 镜像，运行阶段使用 distroless 静态镜像。
# 产物包含 api 与 migrate 两个二进制，便于 docker-compose 中先迁移后启动。
FROM golang:1.22-alpine AS build

ARG APP_VERSION=0.3.2
WORKDIR /src

# 先复制依赖清单，充分利用 Docker 层缓存。
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO_ENABLED=0 生成静态二进制，才能直接放进 distroless/static。
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/api ./cmd/api && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/migrate ./cmd/migrate

FROM gcr.io/distroless/static-debian12:nonroot

ARG APP_VERSION=0.3.2

COPY --from=build /out/api /api
COPY --from=build /out/migrate /migrate
# fixture 模式需要读取 JSON 文件；postgres 模式不会使用。
COPY --from=build /src/fixtures /fixtures

ENV ADDR=:8080 \
    FIXTURES_DIR=/fixtures \
    APP_VERSION=${APP_VERSION}

EXPOSE 8080

# 使用二进制自带的 -healthcheck，无需在 distroless 中安装 curl/wget。
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 \
    CMD ["/api", "-healthcheck"]

USER nonroot:nonroot
ENTRYPOINT ["/api"]
