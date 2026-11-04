package httpapi

import (
	"sync"
	"time"
)

// ipLimiter 是按客户端 IP 划分的令牌桶限流器，进程内实现，多副本时不共享。
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

func (l *ipLimiter) allow(key string) bool {
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
