package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// rateLimitSweepInterval 是惰性清理 rate_limit_buckets 的最小间隔。
const rateLimitSweepInterval = 5 * time.Minute

// rateLimitBucketTTL 是限制计数行的保留时间：超过该时间未访问的 IP 桶会被删除。
// 时间常数远大于令牌桶完全回满所需时间（burst/rate 最多几分钟），删除不会放宽限流。
const rateLimitBucketTTL = 10 * time.Minute

// AllowRateLimit 实现进程间共享的令牌桶限流：
// 状态存放在 rate_limit_buckets，多个 API 实例对同一 key 使用同一行计数。
//
// 语义与进程内 ipLimiter 一致：容量 burst、每秒回填 perMinute/60 个令牌。
// 返回 false 表示本次请求超限；返回错误表示计数查询失败（调用方回退进程内计数）。
//
// 代价：每个受限请求一次单行 UPSERT。同一 IP 的高并发会在该行上串行化，
// 因此共享模式适合限流额度不高、需要跨实例一致性的场景（见 README「限流边界」）。
func (s *Store) AllowRateLimit(ctx context.Context, key string, perMinute, burst int) (bool, error) {
	if perMinute <= 0 {
		return true, nil
	}
	if burst <= 0 {
		burst = perMinute
	}
	rate := float64(perMinute) / 60.0
	capacity := float64(burst)

	// 单次查询预算；数据库慢或不可达时快速失败，由调用方回退本地计数。
	qctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	// 原子地完成「回填 + 扣减 + 判断」：
	//   - 插入时桶是满的（capacity），本次请求扣 1；
	//   - 冲突时按上次访问至今的时间回填，但不超过 capacity；
	//   - WHERE 保证余额 >= 1 才更新，否则不返回行（sql.ErrNoRows = 超限）。
	// 未超限时不更新 updated_at，使被拒绝的请求不会推迟后续回填。
	const query = `
INSERT INTO rate_limit_buckets (bucket, tokens, updated_at)
VALUES ($1, $2::float8 - 1, now())
ON CONFLICT (bucket) DO UPDATE
SET tokens = LEAST(
        $2::float8,
        rate_limit_buckets.tokens
          + GREATEST(EXTRACT(EPOCH FROM (now() - rate_limit_buckets.updated_at)), 0) * $3::float8
    ) - 1,
    updated_at = now()
WHERE LEAST(
        $2::float8,
        rate_limit_buckets.tokens
          + GREATEST(EXTRACT(EPOCH FROM (now() - rate_limit_buckets.updated_at)), 0) * $3::float8
    ) >= 1
RETURNING tokens`

	var tokens float64
	err := s.db.QueryRowContext(qctx, query, key, capacity, rate).Scan(&tokens)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("共享限流计数失败: %w", err)
	}
	s.maybeSweepRateLimitBuckets(ctx)
	return true, nil
}

// maybeSweepRateLimitBuckets 每 5 分钟清理一次长期未访问的桶。
// 多实例同时清理是安全的（幂等 DELETE）；失败不影响限流判定。
func (s *Store) maybeSweepRateLimitBuckets(ctx context.Context) {
	now := time.Now().Unix()
	prev := s.rateLimitSweep.Load()
	if now-prev < int64(rateLimitSweepInterval.Seconds()) {
		return
	}
	if !s.rateLimitSweep.CompareAndSwap(prev, now) {
		return
	}
	sweepCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if _, err := s.db.ExecContext(sweepCtx,
		`DELETE FROM rate_limit_buckets WHERE updated_at < now() - $1::interval`,
		rateLimitBucketTTL.String()); err != nil {
		// 清理是尽力而为；限流本身已完成。
		return
	}
}
