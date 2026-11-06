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
	"sync/atomic"
	"syscall"
	"time"

	"github.com/platojobs/PJGoIoT/internal/config"
	"github.com/platojobs/PJGoIoT/internal/httpapi"
	migrateboot "github.com/platojobs/PJGoIoT/internal/migrate"
	"github.com/platojobs/PJGoIoT/internal/push"
	"github.com/platojobs/PJGoIoT/internal/store"
	"github.com/platojobs/PJGoIoT/internal/store/fixture"
	"github.com/platojobs/PJGoIoT/internal/store/postgres"
)

const (
	// exportReconcileThreshold 是启动对账的判定阈值。导出任务自身有 60 秒构建预算，
	// 5 分钟仍未完成说明执行它的进程已经退出，任务不可能再被推进。
	exportReconcileThreshold = 5 * time.Minute
	// exportInterruptedReason 是回给客户端的可读失败原因，客户端据此提示重新申请导出。
	exportInterruptedReason = "导出任务因服务重启中断，请重新发起导出"
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

	// 启动对账：导出任务在进程内 goroutine 执行，进程被 SIGKILL 后状态会永久停在 pending，
	// 直到 24 小时 TTL 被清理。启动时把超过阈值仍未完成的任务收敛为 failed 并写原因，
	// 客户端轮询时能拿到可读提示并重新申请。多实例并发启动时该 UPDATE 幂等，
	// 只有实际推进状态的一次会计入条数。对账失败不阻塞启动（服务本身仍可用）。
	reconcileCtx, reconcileCancel := context.WithTimeout(context.Background(), 10*time.Second)
	if n, err := st.ReconcileExportTasks(reconcileCtx, time.Now().UTC().Add(-exportReconcileThreshold), exportInterruptedReason); err != nil {
		logger.Warn("启动对账导出任务失败", "error", err)
	} else if n > 0 {
		logger.Info("启动对账导出任务完成", "failed", n)
	}
	reconcileCancel()

	// draining 为 true 后 readiness 立即返回 503；滚动发布脚本依赖该信号让反代先摘流量。
	var draining atomic.Bool
	handler := httpapi.New(cfg, st, logger, &draining)
	server := &http.Server{
		Addr:              cfg.Addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		logger.Info("服务启动", "addr", cfg.Addr, "store", cfg.StoreBackend,
			"version", cfg.Version, "instance", cfg.InstanceID)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("HTTP 服务异常退出", "error", err)
			os.Exit(1)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 推送投递 worker：消费告警产生时写入的投递队列。
	// 没有 FCM/APNs 凭据时适配层自动 dry-run（记录完整载荷并置 sent），队列不会被卡住。
	// 单实例内只有一个循环；多实例通过 FOR UPDATE SKIP LOCKED 互斥，不会重复消费。
	if cfg.PushWorkerEnabled {
		worker := push.NewWorker(st, push.NewService(logger), logger, push.WorkerConfig{
			Interval:          cfg.PushWorkerInterval,
			Batch:             cfg.PushWorkerBatch,
			MaxAttempts:       cfg.PushMaxAttempts,
			BackoffBase:       cfg.PushBackoffBase,
			SendingStaleAfter: cfg.PushSendingStaleAfter,
		})
		go worker.Run(ctx)
		logger.Info("推送投递 worker 已启动", "interval", cfg.PushWorkerInterval.String(),
			"batch", cfg.PushWorkerBatch, "maxAttempts", cfg.PushMaxAttempts)
	} else {
		logger.Warn("推送投递 worker 已禁用，告警只入队不投递", "PUSH_WORKER_ENABLED", false)
	}
	<-ctx.Done()

	// 优雅下线分两步：
	//  1. 先把 readiness 置为失败，让 Caddy 在 health_interval 内把本实例摘掉；
	//  2. 继续服务 drain 窗口，等待已经路由到本实例的请求完成，再真正关闭。
	// 若跳过第 2 步，反代在“发现不健康”之前仍可能把新请求打到正在退出的进程上，产生 502。
	logger.Info("收到退出信号，开始优雅关闭", "drain", cfg.ShutdownDrain.String(), "instance", cfg.InstanceID)
	draining.Store(true)
	if cfg.ShutdownDrain > 0 {
		time.Sleep(cfg.ShutdownDrain)
	}

	logger.Info("drain 结束，关闭 HTTP 服务")
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
