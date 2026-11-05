package httpapi_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/platojobs/PJGoIoT/internal/auth"
	"github.com/platojobs/PJGoIoT/internal/config"
	"github.com/platojobs/PJGoIoT/internal/httpapi"
	"github.com/platojobs/PJGoIoT/internal/model"
	"github.com/platojobs/PJGoIoT/internal/store"
	"github.com/platojobs/PJGoIoT/internal/store/fixture"
)

type anonymousWithExpiry struct {
	UserID    string    `json:"userId"`
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// stubStore 在 fixture 之上注入指定错误，用于验证 HTTP 错误映射。
type stubStore struct {
	*fixture.Store
	changesErr error
	seriesErr  error
}

func (s *stubStore) Changes(ctx context.Context, userID string, cursor int64, limit int) (model.ChangesPage, error) {
	if s.changesErr != nil {
		return model.ChangesPage{}, s.changesErr
	}
	return s.Store.Changes(ctx, userID, cursor, limit)
}

func (s *stubStore) ReadingsSeries(ctx context.Context, userID, platform, externalID, metric string, from, to time.Time, bucket string) (model.ReadingsSeries, error) {
	if s.seriesErr != nil {
		return model.ReadingsSeries{}, s.seriesErr
	}
	return s.Store.ReadingsSeries(ctx, userID, platform, externalID, metric, from, to, bucket)
}

func newStubHandler(t *testing.T, st store.Store) http.Handler {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return httpapi.New(config.Config{
		Version:            "test",
		CORSAllowedOrigins: []string{"*"},
		MetricsEnabled:     true,
		TokenTTLDays:       180,
	}, st, logger)
}

func newFixtureStore(t *testing.T) *fixture.Store {
	t.Helper()
	st, err := fixture.New("../../fixtures")
	if err != nil {
		t.Fatalf("加载 fixtures 失败: %v", err)
	}
	return st
}

// TestAnonymousReturnsExpiresAt 验证匿名登录返回 expiresAt 且为默认 180 天。
func TestAnonymousReturnsExpiresAt(t *testing.T) {
	h := newStubHandler(t, newFixtureStore(t))
	rec := do(t, h, http.MethodPost, "/api/v1/auth/anonymous", "", "", nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := decode[anonymousWithExpiry](t, rec)
	if body.ExpiresAt.IsZero() {
		t.Fatal("expiresAt 缺失")
	}
	if d := time.Until(body.ExpiresAt); d < 179*24*time.Hour || d > 181*24*time.Hour {
		t.Fatalf("expiresAt 距离现在 %v，期望约 180 天", d)
	}
}

// TestExpiredTokenReturns401 验证过期 token 被中间件拒绝。
func TestExpiredTokenReturns401(t *testing.T) {
	st := newFixtureStore(t)
	token := "expired-" + uuid.NewString()
	if _, err := st.CreateUserWithToken(context.Background(), auth.Hash(token), time.Now().UTC().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	h := newStubHandler(t, st)
	rec := do(t, h, http.MethodGet, "/api/v1/my/devices", "", token, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("过期 token status=%d body=%s", rec.Code, rec.Body.String())
	}
	if body := decode[errResponse](t, rec); body.Error.Code != httpapi.CodeUnauthorized {
		t.Fatalf("code=%s", body.Error.Code)
	}
}

// TestCursorExpiredReturns410 验证游标失效映射为 410 RESYNC_REQUIRED。
func TestCursorExpiredReturns410(t *testing.T) {
	st := &stubStore{Store: newFixtureStore(t), changesErr: store.ErrCursorExpired}
	h := newStubHandler(t, st)
	user := newUser(t, h)
	rec := do(t, h, http.MethodGet, "/api/v1/sync/changes?cursor=1", "", user.Token, nil)
	if rec.Code != http.StatusGone {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := decode[errResponse](t, rec)
	if body.Error.Code != httpapi.CodeResyncRequired || body.Error.Message != "游标已过期，请全量重新同步" {
		t.Fatalf("错误体=%+v", body.Error)
	}
}

// TestQueryTimeoutReturns503 验证数据库超时映射为 503 TIMEOUT。
func TestQueryTimeoutReturns503(t *testing.T) {
	st := &stubStore{Store: newFixtureStore(t), seriesErr: store.ErrTimeout}
	h := newStubHandler(t, st)
	user := newUser(t, h)
	path := "/api/v1/my/devices/ios/" + uuid.NewString() + "/readings/series?metric=heart_rate&bucket=1h&from=2026-01-01T00:00:00Z&to=2026-01-02T00:00:00Z"
	rec := do(t, h, http.MethodGet, path, "", user.Token, nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if body := decode[errResponse](t, rec); body.Error.Code != httpapi.CodeTimeout {
		t.Fatalf("code=%s", body.Error.Code)
	}
}

// TestListLimitOutOfRange 验证列表接口的 limit 上限返回 400。
func TestListLimitOutOfRange(t *testing.T) {
	h := newHandler(t, 0)
	user := newUser(t, h)
	cases := []string{
		"/api/v1/my/devices/ios/" + uuid.NewString() + "/readings?limit=501",
		"/api/v1/sync/changes?limit=501",
		"/api/v1/me/export?limit=5001",
	}
	for _, path := range cases {
		rec := do(t, h, http.MethodGet, path, "", user.Token, nil)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s status=%d body=%s", path, rec.Code, rec.Body.String())
		}
	}
}
