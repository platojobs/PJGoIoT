// Command cleanup 删除超过保留期（默认 90 天）的测量读数。
//
// 用法：
//
//	DATABASE_URL=... go run ./cmd/cleanup
//	DATABASE_URL=... go run ./cmd/cleanup -retention-days 90 -dry-run
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/platojobs/PJGoIoT/internal/store/postgres"
)

func main() {
	dsn := flag.String("database-url", os.Getenv("DATABASE_URL"), "PostgreSQL 连接串，默认读取 DATABASE_URL")
	retentionDays := flag.Int("retention-days", 90, "保留天数；早于 now-retention 的读数会被删除")
	dryRun := flag.Bool("dry-run", false, "只打印将要删除的时间边界，不实际删除")
	flag.Parse()

	if *dsn == "" {
		fmt.Fprintln(os.Stderr, "缺少 DATABASE_URL 或 -database-url")
		os.Exit(1)
	}
	if *retentionDays <= 0 {
		fmt.Fprintln(os.Stderr, "retention-days 必须为正整数")
		os.Exit(1)
	}

	cutoff := time.Now().UTC().Add(-time.Duration(*retentionDays) * 24 * time.Hour)
	if *dryRun {
		fmt.Printf("dry-run: 将删除 recorded_at < %s 的读数，并清理过期配对码\n", cutoff.Format(time.RFC3339))
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

	deleted, err := st.CleanupReadings(ctx, cutoff)
	if err != nil {
		fmt.Fprintln(os.Stderr, "清理读数失败:", err)
		os.Exit(1)
	}
	codes, err := st.CleanupPairingCodes(ctx, time.Now().UTC())
	if err != nil {
		fmt.Fprintln(os.Stderr, "清理配对码失败:", err)
		os.Exit(1)
	}
	fmt.Printf("清理完成 deleted=%d cutoff=%s retention_days=%d pairing_codes_deleted=%d\n",
		deleted, cutoff.Format(time.RFC3339), *retentionDays, codes)
}
