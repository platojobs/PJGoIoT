// Package main 启动 PJGoIoT API 服务。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/platojobs/PJGoIoT/internal/config"
	"github.com/platojobs/PJGoIoT/internal/httpapi"
	migrateboot "github.com/platojobs/PJGoIoT/internal/migrate"
	"github.com/platojobs/PJGoIoT/internal/store"
	"github.com/platojobs/PJGoIoT/internal/store/fixture"
	"github.com/platojobs/PJGoIoT/internal/store/postgres"
)

func main() {
	healthcheck := flag.Bool("healthcheck", false, "作为容器健康检查：请求本地健康接口后退出")
	flag.Parse()

	if *healthcheck {
		os.Exit(runHealthcheck())
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "配置错误:", err)
		os.Exit(1)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(logger)

	// 启动迁移失败必须直接退出，不能以“半可用”状态对外提供服务。
	if cfg.RunMigrationsOnBoot {
		if cfg.StoreBackend != "postgres" {
			logger.Warn("STORE 非 postgres，跳过启动迁移", "store", cfg.StoreBackend)
		} else {
			migrateCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			version, dirty, err := migrateboot.Up(migrateCtx, cfg.DatabaseURL)
			cancel()
			if err != nil {
				logger.Error("启动迁移失败", "error", err)
				os.Exit(1)
			}
			logger.Info("启动迁移完成", "version", version, "dirty", dirty)
		}
	} else {
		logger.Info("已禁用启动迁移，需由外部任务保证数据库版本", "version", cfg.Version)
	}

	st, err := buildStore(cfg)
	if err != nil {
		logger.Error("初始化存储失败", "error", err, "backend", cfg.StoreBackend)
		os.Exit(1)
	}
	defer st.Close()

	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
	if n, err := st.CleanupPairingCodes(cleanupCtx, time.Now().UTC()); err != nil {
		logger.Warn("启动清理过期配对码失败", "error", err)
	} else if n > 0 {
		logger.Info("启动清理过期配对码完成", "deleted", n)
	}
	cleanupCancel()

	handler := httpapi.New(cfg, st, logger)
	server := &http.Server{
		Addr:              cfg.Addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		logger.Info("服务启动", "addr", cfg.Addr, "store", cfg.StoreBackend, "version", cfg.Version)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("HTTP 服务异常退出", "error", err)
			os.Exit(1)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()

	logger.Info("收到退出信号，开始优雅关闭")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("优雅关闭失败", "error", err)
	}
}

func buildStore(cfg config.Config) (store.Store, error) {
	switch cfg.StoreBackend {
	case "postgres":
		return postgres.New(context.Background(), cfg.DatabaseURL)
	case "fixture":
		return fixture.New(cfg.FixturesDir)
	default:
		return nil, fmt.Errorf("未知的存储类型 %q", cfg.StoreBackend)
	}
}

// runHealthcheck 用纯 Go 发起请求，便于在 distroless 镜像中作为 HEALTHCHECK。
func runHealthcheck() int {
	addr := os.Getenv("HEALTHCHECK_URL")
	if addr == "" {
		base := os.Getenv("ADDR")
		if base == "" {
			base = ":8080"
		}
		host, port, err := net.SplitHostPort(base)
		if err != nil {
			host, port = "127.0.0.1", "8080"
		}
		if host == "" || strings.EqualFold(host, "0.0.0.0") {
			host = "127.0.0.1"
		}
		addr = fmt.Sprintf("http://%s:%s/healthz", host, port)
	}

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "健康检查请求失败:", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "健康检查状态码非 200:", resp.StatusCode)
		return 1
	}
	return 0
}
