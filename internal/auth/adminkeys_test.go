package auth_test

import (
	"testing"
	"time"

	"github.com/platojobs/PJGoIoT/internal/auth"
)

func TestAdminKeySetCurrentAndPrevious(t *testing.T) {
	rotatedAt := time.Date(2026, 11, 7, 8, 0, 0, 0, time.UTC)
	window := 24 * time.Hour
	keys := auth.NewAdminKeySet("new-secret", "old-secret", rotatedAt, window)

	if !keys.Enabled() {
		t.Fatal("配置了当前密钥时应启用")
	}

	t.Run("轮换窗口内新旧密钥同时有效", func(t *testing.T) {
		mid := rotatedAt.Add(12 * time.Hour)
		if !keys.Accept("new-secret", mid) {
			t.Fatal("当前密钥在窗口内应被接受")
		}
		if !keys.Accept("old-secret", mid) {
			t.Fatal("上一把密钥在窗口内应被接受（轮换期间不中断）")
		}
	})

	t.Run("窗口边界", func(t *testing.T) {
		if !keys.Accept("old-secret", rotatedAt.Add(window-time.Second)) {
			t.Fatal("过期前 1 秒上一把密钥仍应有效")
		}
		if keys.Accept("old-secret", rotatedAt.Add(window)) {
			t.Fatal("到达过期时间后上一把密钥应立即失效")
		}
	})

	t.Run("过期后旧密钥失效但新密钥不受影响", func(t *testing.T) {
		after := rotatedAt.Add(window + time.Hour)
		if keys.Accept("old-secret", after) {
			t.Fatal("窗口结束后上一把密钥必须失效")
		}
		if !keys.Accept("new-secret", after) {
			t.Fatal("当前密钥不应受上一把过期影响")
		}
	})

	t.Run("未知密钥拒绝", func(t *testing.T) {
		if keys.Accept("unknown", rotatedAt) {
			t.Fatal("未知密钥必须拒绝")
		}
		if keys.Accept("", rotatedAt) {
			t.Fatal("空凭据必须拒绝")
		}
	})
}

func TestAdminKeySetWithoutPrevious(t *testing.T) {
	keys := auth.NewAdminKeySet("only-current", "", time.Time{}, 0)
	if !keys.Enabled() {
		t.Fatal("应启用")
	}
	if !keys.Accept("only-current", time.Now()) {
		t.Fatal("当前密钥应有效")
	}
	if keys.Accept("some-old", time.Now()) {
		t.Fatal("未配置上一把密钥时不能接受其他值")
	}
}

func TestAdminKeySetDisabled(t *testing.T) {
	keys := auth.NewAdminKeySet("", "old", time.Now(), time.Hour)
	if keys.Enabled() {
		t.Fatal("未配置当前密钥时不应启用")
	}
	if keys.Accept("", time.Now()) || keys.Accept("old", time.Now()) {
		t.Fatal("未启用时应拒绝任何凭据")
	}
}
