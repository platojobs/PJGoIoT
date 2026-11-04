package httpapi

import (
	"testing"
	"time"
)

func TestIPLimiter(t *testing.T) {
	now := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)
	l := newIPLimiter(2, 2)
	l.now = func() time.Time { return now }

	if !l.allow("1.2.3.4") || !l.allow("1.2.3.4") {
		t.Fatalf("burst 内应允许两次")
	}
	if l.allow("1.2.3.4") {
		t.Fatalf("超过 burst 应拒绝")
	}
	if !l.allow("5.6.7.8") {
		t.Fatalf("不同 IP 应独立计数")
	}

	now = now.Add(31 * time.Second)
	if !l.allow("1.2.3.4") {
		t.Fatalf("补充令牌后应允许")
	}
}
