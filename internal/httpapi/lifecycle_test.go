package httpapi_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

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
	pushErr    error
	tokenErr   error
}

func (s *stubStore) UserIDByTokenHash(ctx context.Context, hash string) (string, error) {
	if s.tokenErr != nil {
		return "", s.tokenErr
	}
	return s.Store.UserIDByTokenHash(ctx, hash)
}

func (s *stubStore) Push(ctx context.Context, userID string, req model.PushRequest, opts store.PushOptions) (model.PushResult, bool, error) {
	if s.pushErr != nil {
		return model.PushResult{}, false, s.pushErr
	}
	return s.Store.Push(ctx, userID, req, opts)
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
	}, st, logger, nil)
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

// TestDuplicateDeviceReturns400 验证同批重复设备在 HTTP 层映射为 400 而不是 500。
func TestDuplicateDeviceReturns400(t *testing.T) {
	st := &stubStore{Store: newFixtureStore(t), pushErr: store.ErrDuplicateDevice}
	h := newStubHandler(t, st)
	user := newUser(t, h)
	body := `{"devices":[{"id":"` + uuid.NewString() + `","mac":"AA:BB:CC:00:12:01",` +
		`"rules":{},"scanRecords":[],"updatedAt":"` + time.Now().UTC().Format(time.RFC3339) + `"}]}`
	rec := do(t, h, http.MethodPost, "/api/v1/sync/push", body, user.Token, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := decode[errResponse](t, rec); got.Error.Code != httpapi.CodeBadRequest {
		t.Fatalf("code=%s", got.Error.Code)
	}
}

// TestStatementTimeoutReturns503 验证数据库 statement_timeout（SQLSTATE 57014）映射为 503 TIMEOUT。
func TestStatementTimeoutReturns503(t *testing.T) {
	st := &stubStore{Store: newFixtureStore(t), seriesErr: &pgconn.PgError{
		Code: "57014", Message: "canceling statement due to statement timeout",
	}}
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

// TestDBUnavailableReturns503 验证连接被拒映射为 503 UNAVAILABLE（可重试），而不是 500。
func TestDBUnavailableReturns503(t *testing.T) {
	st := &stubStore{Store: newFixtureStore(t), seriesErr: errors.New(
		"dial tcp 127.0.0.1:5432: connect: connection refused")}
	h := newStubHandler(t, st)
	user := newUser(t, h)
	path := "/api/v1/my/devices/ios/" + uuid.NewString() + "/readings/series?metric=heart_rate&bucket=1h&from=2026-01-01T00:00:00Z&to=2026-01-02T00:00:00Z"
	rec := do(t, h, http.MethodGet, path, "", user.Token, nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if body := decode[errResponse](t, rec); body.Error.Code != httpapi.CodeUnavailable {
		t.Fatalf("code=%s", body.Error.Code)
	}
}

// TestAuthDBUnavailableReturns503 验证鉴权阶段的数据库不可达也返回 503 而不是 500。
func TestAuthDBUnavailableReturns503(t *testing.T) {
	st := &stubStore{Store: newFixtureStore(t), tokenErr: errors.New(
		"failed to connect to `user=pjgoiot`: connect: connection refused")}
	h := newStubHandler(t, st)
	rec := do(t, h, http.MethodGet, "/api/v1/my/devices", "", "any-token", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if body := decode[errResponse](t, rec); body.Error.Code != httpapi.CodeUnavailable {
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
	}
	for _, path := range cases {
		rec := do(t, h, http.MethodGet, path, "", user.Token, nil)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s status=%d body=%s", path, rec.Code, rec.Body.String())
		}
	}
}
