package httpapi

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/platojobs/PJGoIoT/internal/config"
	"github.com/platojobs/PJGoIoT/internal/store"
)

// rateLimiter 是限流中间件的抽象：进程内计数或跨实例共享计数。
type rateLimiter interface {
	allow(ctx context.Context, key string) bool
}

// ipLimiter 是按客户端 IP 划分的令牌桶限流器，进程内实现。
// 多实例部署且 RATE_LIMIT_BACKEND=memory 时各实例独立计数，实际额度会按实例数放大。
type ipLimiter struct {
	mu        sync.Mutex
	rate      float64 // 每秒补充的令牌数
	burst     float64
	buckets   map[string]*bucket
	now       func() time.Time
	lastSweep time.Time
}

type bucket struct {
	tokens   float64
	lastSeen time.Time
}

func newIPLimiter(perMinute, burst int) *ipLimiter {
	if perMinute <= 0 {
		return nil
	}
	if burst <= 0 {
		burst = perMinute
	}
	return &ipLimiter{
		rate:      float64(perMinute) / 60.0,
		burst:     float64(burst),
		buckets:   map[string]*bucket{},
		now:       time.Now,
		lastSweep: time.Now(),
	}
}

// allow 实现 rateLimiter；上下文不参与本地计算，保留参数是为了与共享实现统一签名。
func (l *ipLimiter) allow(_ context.Context, key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	l.sweep(now)

	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, lastSeen: now}
		l.buckets[key] = b
	}
	elapsed := now.Sub(b.lastSeen).Seconds()
	if elapsed > 0 {
		b.tokens = minFloat(l.burst, b.tokens+elapsed*l.rate)
	}
	b.lastSeen = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// sweep 清理 10 分钟未访问的桶，避免 IP 数量增长导致内存泄漏。
func (l *ipLimiter) sweep(now time.Time) {
	if now.Sub(l.lastSweep) < time.Minute {
		return
	}
	l.lastSweep = now
	for key, b := range l.buckets {
		if now.Sub(b.lastSeen) > 10*time.Minute {
			delete(l.buckets, key)
		}
	}
}

func minFloat(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

// sharedRateLimitStore 是存储层可选的共享限流能力。
// fixture 实现不满足该接口；RATE_LIMIT_BACKEND=postgres 时配置校验会要求 STORE=postgres。
type sharedRateLimitStore interface {
	AllowRateLimit(ctx context.Context, key string, perMinute, burst int) (bool, error)
}

// sharedLimiter 把令牌桶状态放在 PostgreSQL，使多个 API 实例共享同一份计数。
type sharedLimiter struct {
	store     sharedRateLimitStore
	perMinute int
	burst     int
	fallback  *ipLimiter
	log       *slog.Logger
	lastWarn  atomic.Int64
}

func newRateLimiter(cfg config.Config, st store.Store, logger *slog.Logger) rateLimiter {
	local := newIPLimiter(cfg.RateLimitPerMin, cfg.RateLimitBurst)
	if local == nil {
		return nil
	}
	if cfg.RateLimitBackend != "postgres" {
		return local
	}
	shared, ok := st.(sharedRateLimitStore)
	if !ok {
		// 配置层已拒绝 fixture + postgres 组合，这里只做兜底，避免因接口扩展遗漏而崩溃。
		logger.Warn("存储不支持共享限流，回退进程内计数", "store", st.Backend())
		return local
	}
	burst := cfg.RateLimitBurst
	if burst <= 0 {
		burst = cfg.RateLimitPerMin
	}
	return &sharedLimiter{
		store:     shared,
		perMinute: cfg.RateLimitPerMin,
		burst:     burst,
		fallback:  local,
		log:       logger,
	}
}

func (l *sharedLimiter) allow(ctx context.Context, key string) bool {
	ok, err := l.store.AllowRateLimit(ctx, key, l.perMinute, l.burst)
	if err != nil {
		// 数据库故障时退化为进程内计数（fail-open on limiter, not on business traffic）：
		// 限流是保护措施，不应让整个 API 因为限流查询失败而不可用。
		// 每 30s 最多记一条警告，避免数据库故障期间刷爆日志。
		now := time.Now().Unix()
		if prev := l.lastWarn.Load(); now-prev >= 30 && l.lastWarn.CompareAndSwap(prev, now) {
			l.log.Warn("共享限流查询失败，回退进程内计数", "error", err)
		}
		return l.fallback.allow(ctx, key)
	}
	return ok
}
