// Command cleanup 按保留期删除测量读数、变更日志、幂等键与过期 token。
//
// 用法：
//
//	DATABASE_URL=... go run ./cmd/cleanup
//	DATABASE_URL=... go run ./cmd/cleanup -retention-days 90 -dry-run
//	DATABASE_URL=... CHANGE_LOG_RETENTION_DAYS=90 IDEMPOTENCY_RETENTION_DAYS=7 go run ./cmd/cleanup
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/platojobs/PJGoIoT/internal/store/postgres"
)

// envInt 读取整型环境变量，未设置或非法时回退默认值。
func envInt(key string, fallback int) int {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

func main() {
	dsn := flag.String("database-url", os.Getenv("DATABASE_URL"), "PostgreSQL 连接串，默认读取 DATABASE_URL")
	retentionDays := flag.Int("retention-days", 90, "读数保留天数；早于 now-retention 的 readings 会被删除")
	changeLogDays := flag.Int("change-log-retention-days", envInt("CHANGE_LOG_RETENTION_DAYS", 90),
		"变更日志保留天数（默认 90，或环境变量 CHANGE_LOG_RETENTION_DAYS）")
	idempotencyDays := flag.Int("idempotency-retention-days", envInt("IDEMPOTENCY_RETENTION_DAYS", 7),
		"幂等键保留天数（默认 7，或环境变量 IDEMPOTENCY_RETENTION_DAYS）")
	opTimeout := flag.Duration("operation-timeout", 30*time.Second, "每类清理操作的超时时间")
	dryRun := flag.Bool("dry-run", false, "只打印将要删除的时间边界，不实际删除")
	flag.Parse()

	if *dsn == "" {
		fmt.Fprintln(os.Stderr, "缺少 DATABASE_URL 或 -database-url")
		os.Exit(1)
	}
	if *retentionDays <= 0 || *changeLogDays <= 0 || *idempotencyDays <= 0 {
		fmt.Fprintln(os.Stderr, "所有保留天数必须为正整数")
		os.Exit(1)
	}

	now := time.Now().UTC()
	readingsCutoff := now.Add(-time.Duration(*retentionDays) * 24 * time.Hour)
	changeLogCutoff := now.Add(-time.Duration(*changeLogDays) * 24 * time.Hour)
	idempotencyCutoff := now.Add(-time.Duration(*idempotencyDays) * 24 * time.Hour)

	if *dryRun {
		fmt.Printf("dry-run:\n  readings: recorded_at < %s\n  change_log: server_ts < %s\n  idempotency_keys: created_at < %s\n  tokens: expires_at < %s\n  pairing_codes: expires_at < %s\n",
			readingsCutoff.Format(time.RFC3339), changeLogCutoff.Format(time.RFC3339),
			idempotencyCutoff.Format(time.RFC3339), now.Format(time.RFC3339), now.Format(time.RFC3339))
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	st, err := postgres.New(ctx, *dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "连接数据库失败:", err)
		os.Exit(1)
	}
	defer st.Close()

	run := func(name string, fn func(context.Context) (int64, error)) int64 {
		opCtx, opCancel := context.WithTimeout(ctx, *opTimeout)
		defer opCancel()
		n, err := fn(opCtx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "清理 %s 失败: %v\n", name, err)
			os.Exit(1)
		}
		return n
	}

	readings := run("readings", func(c context.Context) (int64, error) { return st.CleanupReadings(c, readingsCutoff) })
	changes := run("change_log", func(c context.Context) (int64, error) { return st.CleanupChangeLog(c, changeLogCutoff) })
	idempotency := run("idempotency_keys", func(c context.Context) (int64, error) { return st.CleanupIdempotencyKeys(c, idempotencyCutoff) })
	tokens := run("tokens", func(c context.Context) (int64, error) { return st.CleanupTokens(c, now) })
	codes := run("pairing_codes", func(c context.Context) (int64, error) { return st.CleanupPairingCodes(c, now) })

	fmt.Printf("清理完成 readings_deleted=%d readings_cutoff=%s retention_days=%d\n"+
		"change_log_deleted=%d change_log_cutoff=%s change_log_retention_days=%d\n"+
		"idempotency_keys_deleted=%d idempotency_cutoff=%s idempotency_retention_days=%d\n"+
		"tokens_deleted=%d pairing_codes_deleted=%d\n",
		readings, readingsCutoff.Format(time.RFC3339), *retentionDays,
		changes, changeLogCutoff.Format(time.RFC3339), *changeLogDays,
		idempotency, idempotencyCutoff.Format(time.RFC3339), *idempotencyDays,
		tokens, codes)
}
