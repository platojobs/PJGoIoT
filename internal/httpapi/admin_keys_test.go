package httpapi_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/platojobs/PJGoIoT/internal/config"
	"github.com/platojobs/PJGoIoT/internal/httpapi"
)

// TestAdminKeyRotationHTTP 覆盖运营入口在 HTTP 层的轮换行为：
// 当前/上一把密钥在窗口内都可用，窗口结束后旧密钥 401，未知密钥 401。
func TestAdminKeyRotationHTTP(t *testing.T) {
	const current = "admin-current-2026"
	const previous = "admin-previous-2025"
	window := 2 * time.Hour

	inWindow := newFeedbackHandler(t, config.Config{
		AdminToken:          current,
		AdminPreviousToken:  previous,
		AdminRotatedAt:      time.Now().UTC().Add(-30 * time.Minute),
		AdminRotationWindow: window,
	})
	newUser(t, inWindow)

	cases := []struct {
		name string
		key  string
		want int
	}{
		{"当前密钥", current, http.StatusOK},
		{"上一把密钥（窗口内）", previous, http.StatusOK},
		{"未知密钥", "admin-unknown", http.StatusUnauthorized},
		{"空凭据", "", http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hdr := map[string]string{}
			if tc.key != "" {
				hdr["X-Admin-Token"] = tc.key
			}
			rec := do(t, inWindow, http.MethodGet, "/api/v1/admin/feedback", "", "", hdr)
			if rec.Code != tc.want {
				t.Fatalf("凭据=%q 期望 %d，实际 %d body=%s", tc.key, tc.want, rec.Code, rec.Body.String())
			}
		})
	}

	// 窗口已结束：上一把密钥必须失效，当前密钥保持可用。
	expired := newFeedbackHandler(t, config.Config{
		AdminToken:          current,
		AdminPreviousToken:  previous,
		AdminRotatedAt:      time.Now().UTC().Add(-window - time.Minute),
		AdminRotationWindow: window,
	})
	newUser(t, expired)

	rec := do(t, expired, http.MethodGet, "/api/v1/admin/feedback", "", "", map[string]string{"X-Admin-Token": previous})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("过期的上一把密钥应 401，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	rec = do(t, expired, http.MethodGet, "/api/v1/admin/feedback", "", "", map[string]string{"X-Admin-Token": current})
	if rec.Code != http.StatusOK {
		t.Fatalf("当前密钥不应受旧密钥过期影响，实际 %d body=%s", rec.Code, rec.Body.String())
	}

	// 未配置 ADMIN_TOKEN 时整组入口仍应按 404 隐藏。
	disabled := newFeedbackHandler(t, config.Config{})
	rec = do(t, disabled, http.MethodGet, "/api/v1/admin/feedback", "", "", map[string]string{"X-Admin-Token": previous})
	if rec.Code != http.StatusNotFound || decode[errResponse](t, rec).Error.Code != httpapi.CodeNotFound {
		t.Fatalf("未配置 admin 应 404，实际 %d body=%s", rec.Code, rec.Body.String())
	}
}
