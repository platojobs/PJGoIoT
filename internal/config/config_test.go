package config_test

import (
	"testing"
	"time"

	"github.com/platojobs/PJGoIoT/internal/config"
)

func TestLoadAdminRotationConfig(t *testing.T) {
	t.Run("完整轮换配置", func(t *testing.T) {
		t.Setenv("ADMIN_TOKEN", "new")
		t.Setenv("ADMIN_TOKEN_PREVIOUS", "old")
		t.Setenv("ADMIN_TOKEN_ROTATED_AT", "2026-11-07T08:00:00Z")
		t.Setenv("ADMIN_TOKEN_ROTATION_WINDOW_SECONDS", "3600")
		cfg, err := config.Load()
		if err != nil {
			t.Fatalf("合法配置加载失败: %v", err)
		}
		if cfg.AdminToken != "new" || cfg.AdminPreviousToken != "old" {
			t.Fatalf("密钥未正确加载: %+v", cfg)
		}
		want := time.Date(2026, 11, 7, 8, 0, 0, 0, time.UTC)
		if !cfg.AdminRotatedAt.Equal(want) || cfg.AdminRotationWindow != time.Hour {
			t.Fatalf("轮换时间/窗口未正确解析: %v / %v", cfg.AdminRotatedAt, cfg.AdminRotationWindow)
		}
	})

	tests := []struct {
		name string
		env  map[string]string
	}{
		{"上一把密钥缺少生效时间", map[string]string{"ADMIN_TOKEN": "new", "ADMIN_TOKEN_PREVIOUS": "old"}},
		{"上一把密钥与当前相同", map[string]string{"ADMIN_TOKEN": "same", "ADMIN_TOKEN_PREVIOUS": "same", "ADMIN_TOKEN_ROTATED_AT": "2026-11-07T08:00:00Z"}},
		{"只有上一把密钥", map[string]string{"ADMIN_TOKEN_PREVIOUS": "old", "ADMIN_TOKEN_ROTATED_AT": "2026-11-07T08:00:00Z"}},
		{"生效时间格式错误", map[string]string{"ADMIN_TOKEN": "new", "ADMIN_TOKEN_PREVIOUS": "old", "ADMIN_TOKEN_ROTATED_AT": "2026-11-07 08:00:00"}},
		{"窗口非法", map[string]string{"ADMIN_TOKEN": "new", "ADMIN_TOKEN_PREVIOUS": "old", "ADMIN_TOKEN_ROTATED_AT": "2026-11-07T08:00:00Z", "ADMIN_TOKEN_ROTATION_WINDOW_SECONDS": "0"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clearAdminEnv(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			if _, err := config.Load(); err == nil {
				t.Fatal("非法轮换配置应返回错误")
			}
		})
	}
}

func clearAdminEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"ADMIN_TOKEN", "ADMIN_TOKEN_PREVIOUS", "ADMIN_TOKEN_ROTATED_AT",
		"ADMIN_TOKEN_ROTATION_WINDOW_SECONDS",
	} {
		t.Setenv(k, "")
	}
}
