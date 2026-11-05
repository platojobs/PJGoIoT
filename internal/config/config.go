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
	RateLimitPerMin    int
	RateLimitBurst     int
	CORSAllowedOrigins []string
	LogLevel           slog.Level
	ShutdownTimeout    time.Duration
	Version            string
}

// Load 读取环境变量并返回配置。默认不连接数据库，直接使用 fixtures，便于快速启动。
func Load() (Config, error) {
	cfg := Config{
		Addr:            getEnv("ADDR", ":8080"),
		StoreBackend:    strings.ToLower(getEnv("STORE", "")),
		DatabaseURL:     os.Getenv("DATABASE_URL"),
		FixturesDir:     getEnv("FIXTURES_DIR", "./fixtures"),
		RateLimitPerMin: getEnvInt("RATE_LIMIT_PER_MINUTE", 120),
		RateLimitBurst:  getEnvInt("RATE_LIMIT_BURST", 40),
		LogLevel:        parseLogLevel(getEnv("LOG_LEVEL", "info")),
		ShutdownTimeout: 10 * time.Second,
		Version:         getEnv("APP_VERSION", "0.4.0"),
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
	if cfg.RateLimitPerMin < 0 || cfg.RateLimitBurst < 0 {
		return Config{}, fmt.Errorf("限流参数不能为负数")
	}
	return cfg, nil
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
