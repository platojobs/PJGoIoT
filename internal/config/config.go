// Package config 从环境变量加载服务配置。
package config

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config 是 API 服务的全部运行时配置。
type Config struct {
	Addr               string
	StoreBackend       string // fixture 或 postgres
	DatabaseURL        string
	FixturesDir        string
	RateLimitBackend   string // memory（进程内）或 postgres（跨实例共享）
	RateLimitPerMin    int
	RateLimitBurst     int
	PairingClaimPerMin int
	PairingClaimBurst  int
	// FeedbackUserPerMin / FeedbackUserBurst 是反馈接口按账号的每分钟令牌数，
	// FeedbackIPPerMin / FeedbackIPBurst 是按来源 IP 的额度；两个维度同时生效。
	FeedbackUserPerMin int
	FeedbackUserBurst  int
	FeedbackIPPerMin   int
	FeedbackIPBurst    int
	// AdminToken 是运营侧受理入口的独立凭据（请求头 X-Admin-Token）。
	// 为空表示运维入口整体关闭：相关路由一律返回 404，不暴露接口是否存在。
	AdminToken          string
	MetricsEnabled      bool
	RunMigrationsOnBoot bool
	CORSAllowedOrigins  []string
	LogLevel            slog.Level
	ShutdownTimeout     time.Duration
	// ShutdownDrain 是收到 SIGTERM 后先标记 readiness 失败、继续服务的时间，
	// 给反向代理留出摘流量窗口，避免滚动重启时丢请求。
	ShutdownDrain time.Duration
	// InstanceID 仅用于响应头 X-Instance 与日志，便于确认请求落到哪个实例。
	InstanceID   string
	TokenTTLDays int
	Version      string
}

// Load 读取环境变量并返回配置。默认不连接数据库，直接使用 fixtures，便于快速启动。
func Load() (Config, error) {
	cfg := Config{
		Addr:                getEnv("ADDR", ":8080"),
		StoreBackend:        strings.ToLower(getEnv("STORE", "")),
		DatabaseURL:         os.Getenv("DATABASE_URL"),
		FixturesDir:         getEnv("FIXTURES_DIR", "./fixtures"),
		RateLimitBackend:    strings.ToLower(getEnv("RATE_LIMIT_BACKEND", "memory")),
		RateLimitPerMin:     getEnvInt("RATE_LIMIT_PER_MINUTE", 120),
		RateLimitBurst:      getEnvInt("RATE_LIMIT_BURST", 40),
		PairingClaimPerMin:  getEnvInt("PAIRING_CLAIM_RATE_PER_MINUTE", 20),
		PairingClaimBurst:   getEnvInt("PAIRING_CLAIM_BURST", 10),
		FeedbackUserPerMin:  getEnvInt("FEEDBACK_USER_RATE_PER_MINUTE", 10),
		FeedbackUserBurst:   getEnvInt("FEEDBACK_USER_BURST", 5),
		FeedbackIPPerMin:    getEnvInt("FEEDBACK_IP_RATE_PER_MINUTE", 30),
		FeedbackIPBurst:     getEnvInt("FEEDBACK_IP_BURST", 10),
		AdminToken:          getEnv("ADMIN_TOKEN", ""),
		MetricsEnabled:      getEnvBool("METRICS_ENABLED", true),
		RunMigrationsOnBoot: getEnvBool("RUN_MIGRATIONS_ON_BOOT", true),
		LogLevel:            parseLogLevel(getEnv("LOG_LEVEL", "info")),
		ShutdownTimeout:     10 * time.Second,
		ShutdownDrain:       time.Duration(getEnvInt("SHUTDOWN_DRAIN_SECONDS", 5)) * time.Second,
		InstanceID:          getEnv("INSTANCE_ID", ""),
		TokenTTLDays:        getEnvInt("TOKEN_TTL_DAYS", 180),
		Version:             getEnv("APP_VERSION", "0.18.0"),
	}

	origins := getEnv("CORS_ALLOWED_ORIGINS", "*")
	for _, part := range strings.Split(origins, ",") {
		if p := strings.TrimSpace(part); p != "" {
			cfg.CORSAllowedOrigins = append(cfg.CORSAllowedOrigins, p)
		}
	}

	if cfg.StoreBackend == "" {
		if cfg.DatabaseURL != "" {
			cfg.StoreBackend = "postgres"
		} else {
			cfg.StoreBackend = "fixture"
		}
	}
	if cfg.StoreBackend != "fixture" && cfg.StoreBackend != "postgres" {
		return Config{}, fmt.Errorf("STORE 只能是 fixture 或 postgres，当前为 %q", cfg.StoreBackend)
	}
	if cfg.StoreBackend == "postgres" && cfg.DatabaseURL == "" {
		return Config{}, fmt.Errorf("STORE=postgres 时必须设置 DATABASE_URL")
	}
	if cfg.RateLimitBackend != "memory" && cfg.RateLimitBackend != "postgres" {
		return Config{}, fmt.Errorf("RATE_LIMIT_BACKEND 只能是 memory 或 postgres，当前为 %q", cfg.RateLimitBackend)
	}
	if cfg.RateLimitBackend == "postgres" && cfg.StoreBackend != "postgres" {
		// 共享计数依赖数据库；fixture 模式下没有可用的共享后端，直接拒绝而不是静默退化为进程内。
		return Config{}, fmt.Errorf("RATE_LIMIT_BACKEND=postgres 时必须使用 STORE=postgres")
	}
	if cfg.RateLimitPerMin < 0 || cfg.RateLimitBurst < 0 {
		return Config{}, fmt.Errorf("限流参数不能为负数")
	}
	if cfg.ShutdownDrain < 0 || cfg.ShutdownDrain > time.Minute {
		return Config{}, fmt.Errorf("SHUTDOWN_DRAIN_SECONDS 必须在 0~60 之间")
	}
	if cfg.PairingClaimPerMin < 0 || cfg.PairingClaimBurst < 0 {
		return Config{}, fmt.Errorf("配对码限流参数不能为负数")
	}
	if cfg.FeedbackUserPerMin < 0 || cfg.FeedbackUserBurst < 0 || cfg.FeedbackIPPerMin < 0 || cfg.FeedbackIPBurst < 0 {
		return Config{}, fmt.Errorf("反馈限流参数不能为负数")
	}
	if cfg.TokenTTLDays <= 0 {
		return Config{}, fmt.Errorf("TOKEN_TTL_DAYS 必须大于 0，当前为 %d", cfg.TokenTTLDays)
	}
	return cfg, nil
}

// TokenTTL 返回 token 有效期；直接构造 Config 且未设置天数时回退到 180 天默认值。
func (c Config) TokenTTL() time.Duration {
	days := c.TokenTTLDays
	if days <= 0 {
		days = 180
	}
	return time.Duration(days) * 24 * time.Hour
}

func getEnv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

func getEnvBool(key string, fallback bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	if v == "" {
		return fallback
	}
	switch v {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}

func parseLogLevel(v string) slog.Level {
	switch strings.ToLower(v) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
