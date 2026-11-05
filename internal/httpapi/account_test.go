package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/platojobs/PJGoIoT/internal/auth"
	"github.com/platojobs/PJGoIoT/internal/config"
	"github.com/platojobs/PJGoIoT/internal/httpapi"
	"github.com/platojobs/PJGoIoT/internal/store/fixture"
)

type pairingCodeResp struct {
	Code      string    `json:"code"`
	ExpiresAt time.Time `json:"expiresAt"`
}

type claimResp struct {
	UserID    string    `json:"userId"`
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expiresAt"`
}

type exportReading struct {
	ID         string    `json:"id"`
	Platform   string    `json:"platform"`
	ExternalID string    `json:"externalId"`
	Metric     string    `json:"metric"`
	Value      float64   `json:"value"`
	Unit       string    `json:"unit"`
	RecordedAt time.Time `json:"recordedAt"`
}

type exportResp struct {
	User struct {
		ID        string    `json:"id"`
		Kind      string    `json:"kind"`
		CreatedAt time.Time `json:"createdAt"`
	} `json:"user"`
	Devices    []map[string]any `json:"devices"`
	Readings   []exportReading  `json:"readings"`
	ExportedAt time.Time        `json:"exportedAt"`
	NextCursor string           `json:"nextCursor"`
	HasMore    bool             `json:"hasMore"`
}

func newHandlerWithStore(t *testing.T, cfg config.Config, st *fixture.Store) http.Handler {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return httpapi.New(cfg, st, logger)
}

func createPairingCode(t *testing.T, h http.Handler, token string) pairingCodeResp {
	t.Helper()
	rec := do(t, h, http.MethodPost, "/api/v1/auth/pairing-code", "", token, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("生成配对码失败: status=%d body=%s", rec.Code, rec.Body.String())
	}
	return decode[pairingCodeResp](t, rec)
}

func claimPairingCode(t *testing.T, h http.Handler, code string) (int, []byte) {
	t.Helper()
	body := `{"code":"` + code + `"}`
	rec := do(t, h, http.MethodPost, "/api/v1/auth/pairing-code/claim", body, "", nil)
	return rec.Code, rec.Body.Bytes()
}

func TestPairingCodeFlow(t *testing.T) {
	h := newHandler(t, 0)
	userA := newUser(t, h)
	userB := newUser(t, h)

	first := createPairingCode(t, h, userA.Token)
	if len(first.Code) != auth.PairingCodeLength {
		t.Fatalf("配对码长度=%d", len(first.Code))
	}
	if !strings.Contains("ABCDEFGHJKLMNPQRSTUVWXYZ23456789", first.Code[:1]) {
		t.Fatalf("配对码字符集非法: %s", first.Code)
	}
	if d := time.Until(first.ExpiresAt); d < 9*time.Minute || d > 11*time.Minute {
		t.Fatalf("配对码 TTL 异常: %v", d)
	}

	// 同用户新码使旧码失效。
	second := createPairingCode(t, h, userA.Token)
	if second.Code == first.Code {
		t.Fatal("新码不应与旧码相同")
	}
	if code, body := claimPairingCode(t, h, first.Code); code != http.StatusNotFound {
		t.Fatalf("旧码应失效: status=%d body=%s", code, body)
	}

	// 新码换取同一 userId 的新 token。
	code, body := claimPairingCode(t, h, second.Code)
	if code != http.StatusOK {
		t.Fatalf("claim status=%d body=%s", code, body)
	}
	var claimed claimResp
	if err := json.Unmarshal(body, &claimed); err != nil {
		t.Fatalf("解析 claim 失败: %v body=%s", err, body)
	}
	if claimed.UserID != userA.UserID {
		t.Fatalf("claim userId=%s want=%s", claimed.UserID, userA.UserID)
	}
	if claimed.Token == "" || claimed.Token == userA.Token {
		t.Fatalf("应签发新 token: %q", claimed.Token)
	}
	if d := time.Until(claimed.ExpiresAt); d < 179*24*time.Hour || d > 181*24*time.Hour {
		t.Fatalf("claim expiresAt 异常: %v", d)
	}
	if rec := do(t, h, http.MethodGet, "/api/v1/my/devices", "", claimed.Token, nil); rec.Code != http.StatusOK {
		t.Fatalf("新 token 不可用: %d %s", rec.Code, rec.Body.String())
	}
	// 原 token 保持有效。
	if rec := do(t, h, http.MethodGet, "/api/v1/my/devices", "", userA.Token, nil); rec.Code != http.StatusOK {
		t.Fatalf("原 token 被吊销: %d %s", rec.Code, rec.Body.String())
	}

	// 一次性：重放 404。
	if code, body := claimPairingCode(t, h, second.Code); code != http.StatusNotFound {
		t.Fatalf("重放 status=%d body=%s", code, body)
	}

	// 跨用户：B 的码只能拿到 B。
	codeB := createPairingCode(t, h, userB.Token)
	code, body = claimPairingCode(t, h, codeB.Code)
	if code != http.StatusOK {
		t.Fatalf("B claim status=%d body=%s", code, body)
	}
	var claimedB claimResp
	if err := json.Unmarshal(body, &claimedB); err != nil {
		t.Fatal(err)
	}
	if claimedB.UserID != userB.UserID {
		t.Fatalf("B 的码拿到 %s，期望 %s", claimedB.UserID, userB.UserID)
	}

	// 不存在的码与格式错误的码都 404，且响应只有错误体，不含账号信息。
	for i := 0; i < 5; i++ {
		code, body := claimPairingCode(t, h, "ZZZZZZZZ")
		if code != http.StatusNotFound {
			t.Fatalf("不存在码 status=%d body=%s", code, body)
		}
		var errResp errResponse
		if err := json.Unmarshal(body, &errResp); err != nil {
			t.Fatalf("错误体解析失败: %v", err)
		}
		if errResp.Error.Code != "NOT_FOUND" || strings.Contains(string(body), userA.UserID) {
			t.Fatalf("错误体泄露信息: %s", body)
		}
	}
}

// TestPairingCodeExpired 覆盖过期码统一 404。
func TestPairingCodeExpired(t *testing.T) {
	st, err := fixture.New("../../fixtures")
	if err != nil {
		t.Fatal(err)
	}
	h := newHandlerWithStore(t, config.Config{Version: "test"}, st)

	uid, err := st.CreateUserWithToken(context.Background(), "hash-expired", time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	code, err := auth.NewPairingCode()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreatePairingCode(context.Background(), uid, auth.PairingCodeHash(code), time.Now().UTC().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if status, body := claimPairingCode(t, h, code); status != http.StatusNotFound {
		t.Fatalf("过期码 status=%d body=%s", status, body)
	}
}

func TestExportContentAndPagination(t *testing.T) {
	h := newHandler(t, 0)
	user := newUser(t, h)
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)

	readings := []string{
		readingJSON("33333333-3333-4333-8333-333333333333", "android", "AA:BB:CC:00:00:01", "temperature_celsius", "23", "celsius", base.Add(20*time.Minute)),
		readingJSON("22222222-2222-4222-8222-222222222222", "android", "AA:BB:CC:00:00:01", "relative_humidity", "48", "percent", base.Add(10*time.Minute)),
		readingJSON("44444444-4444-4444-8444-444444444444", "android", "AA:BB:CC:00:00:01", "temperature_celsius", "21", "celsius", base),
	}
	body := pushBody(t, "["+deviceJSON("11111111-1111-4111-8111-111111111111", "AA:BB:CC:00:00:01", base)+"]", readings)
	rec := do(t, h, http.MethodPost, "/api/v1/sync/push", body, user.Token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("push status=%d body=%s", rec.Code, rec.Body.String())
	}

	rec = do(t, h, http.MethodGet, "/api/v1/me/export", "", user.Token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("export status=%d body=%s", rec.Code, rec.Body.String())
	}
	wantCD := `attachment; filename="pjkotlinapp-export-` + user.UserID + `.json"`
	if got := rec.Header().Get("Content-Disposition"); got != wantCD {
		t.Fatalf("Content-Disposition=%q want=%q", got, wantCD)
	}
	first := decode[exportResp](t, rec)
	if first.User.ID != user.UserID || first.User.Kind != "anonymous" || first.User.CreatedAt.IsZero() {
		t.Fatalf("user 信息不完整: %+v", first.User)
	}
	if len(first.Devices) != 1 {
		t.Fatalf("devices=%d", len(first.Devices))
	}
	if len(first.Readings) != 3 {
		t.Fatalf("readings=%d", len(first.Readings))
	}
	for i := 1; i < len(first.Readings); i++ {
		if first.Readings[i].RecordedAt.Before(first.Readings[i-1].RecordedAt) {
			t.Fatalf("readings 未按 recordedAt 升序: %+v", first.Readings)
		}
	}
	if first.Readings[0].ID != "44444444-4444-4444-8444-444444444444" ||
		first.Readings[2].ID != "33333333-3333-4333-8333-333333333333" {
		t.Fatalf("导出内容与写入不一致: %+v", first.Readings)
	}
	if first.HasMore || first.NextCursor != "" {
		t.Fatalf("默认 limit 下不应分页: hasMore=%v cursor=%q", first.HasMore, first.NextCursor)
	}

	// limit=2 分页。
	rec = do(t, h, http.MethodGet, "/api/v1/me/export?limit=2", "", user.Token, nil)
	page1 := decode[exportResp](t, rec)
	if len(page1.Readings) != 2 || !page1.HasMore || page1.NextCursor == "" {
		t.Fatalf("第一页异常: len=%d hasMore=%v cursor=%q", len(page1.Readings), page1.HasMore, page1.NextCursor)
	}
	rec = do(t, h, http.MethodGet, "/api/v1/me/export?limit=2&cursor="+page1.NextCursor, "", user.Token, nil)
	page2 := decode[exportResp](t, rec)
	if len(page2.Readings) != 1 || page2.HasMore || page2.NextCursor != "" {
		t.Fatalf("第二页异常: len=%d hasMore=%v cursor=%q", len(page2.Readings), page2.HasMore, page2.NextCursor)
	}
	combined := append(append([]exportReading{}, page1.Readings...), page2.Readings...)
	if len(combined) != 3 || combined[0].ID != first.Readings[0].ID || combined[2].ID != first.Readings[2].ID {
		t.Fatalf("分页内容不连续: %+v", combined)
	}

	// 非法分页参数。
	for _, path := range []string{
		"/api/v1/me/export?limit=0",
		"/api/v1/me/export?limit=5001",
		"/api/v1/me/export?limit=abc",
		"/api/v1/me/export?cursor=not-base64",
	} {
		rec := do(t, h, http.MethodGet, path, "", user.Token, nil)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s status=%d body=%s", path, rec.Code, rec.Body.String())
		}
	}
	if rec := do(t, h, http.MethodGet, "/api/v1/me/export", "", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("未认证 export status=%d", rec.Code)
	}
}

func TestDeleteMeIdempotent(t *testing.T) {
	h := newHandler(t, 0)
	victim := newUser(t, h)
	other := newUser(t, h)

	base := time.Now().UTC().Add(-time.Minute)
	body := pushBody(t, "["+deviceJSON("11111111-1111-4111-8111-111111111111", "AA:BB:CC:00:00:01", base)+"]",
		[]string{readingJSON("22222222-2222-4222-8222-222222222222", "android", "AA:BB:CC:00:00:01", "temperature_celsius", "20", "celsius", base)})
	if rec := do(t, h, http.MethodPost, "/api/v1/sync/push", body, victim.Token, nil); rec.Code != http.StatusOK {
		t.Fatalf("push status=%d body=%s", rec.Code, rec.Body.String())
	}

	rec := do(t, h, http.MethodDelete, "/api/v1/me", "", victim.Token, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("注销 status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := do(t, h, http.MethodGet, "/api/v1/me/export", "", victim.Token, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("注销后 export status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := do(t, h, http.MethodDelete, "/api/v1/me", "", victim.Token, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("重复注销 status=%d body=%s", rec.Code, rec.Body.String())
	}
	// 其他用户不受影响。
	if rec := do(t, h, http.MethodGet, "/api/v1/my/devices", "", other.Token, nil); rec.Code != http.StatusOK {
		t.Fatalf("其他用户受注销影响: %d", rec.Code)
	}
}

func TestMetricsEndpoint(t *testing.T) {
	h := newHandler(t, 0)
	do(t, h, http.MethodGet, "/api/v1/health", "", "", nil)

	rec := do(t, h, http.MethodGet, "/metrics", "", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics status=%d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("Content-Type=%q", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"pjgoiot_http_requests_total{method=\"GET\",route=\"/api/v1/health\",status=\"200\"}",
		"pjgoiot_http_request_duration_seconds_bucket",
		"pjgoiot_http_request_duration_seconds_sum",
		"pjgoiot_http_request_duration_seconds_count",
		"pjgoiot_db_errors_total",
		"pjgoiot_users_approx",
		"pjgoiot_devices_approx",
		"pjgoiot_readings_approx",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics 缺少 %q:\n%s", want, body)
		}
	}

	// 简易 Prometheus 文本解析：非注释行必须是 `name{labels} value` 且数值合法。
	metricLine := regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*(\{[^}]*\})? -?[0-9.e+]+$`)
	for _, line := range strings.Split(strings.TrimSpace(body), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !metricLine.MatchString(line) {
			t.Fatalf("无法解析的指标行: %q", line)
		}
	}
}

func TestHealthzLiveness(t *testing.T) {
	h := newHandler(t, 0)
	rec := do(t, h, http.MethodGet, "/healthz", "", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("/healthz status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Status  string `json:"status"`
		Service string `json:"service"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析 /healthz 响应失败: %v", err)
	}
	if body.Status != "ok" || body.Service != "pjgoiot-api" {
		t.Fatalf("liveness 响应不符合预期: %+v", body)
	}
	// 与 readiness 区分：/healthz 是根路径，/api/v1/health 才检查数据库。
	if rec := do(t, h, http.MethodGet, "/healthz", "", "", nil); rec.Header().Get("X-Request-Id") == "" {
		t.Fatal("liveness 响应缺少 X-Request-Id")
	}
}

func TestMetricsBypassesRateLimit(t *testing.T) {
	h := newHandler(t, 1)
	if rec := do(t, h, http.MethodGet, "/api/v1/device-types", "", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("首次请求 status=%d", rec.Code)
	}
	if rec := do(t, h, http.MethodGet, "/api/v1/device-types", "", "", nil); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("限流未生效: %d", rec.Code)
	}
	if rec := do(t, h, http.MethodGet, "/metrics", "", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("/metrics 被限流: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, h, http.MethodGet, "/healthz", "", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("/healthz 被限流: %d %s", rec.Code, rec.Body.String())
	}
}

func TestRequestIDAndLogs(t *testing.T) {
	h := newHandler(t, 0)

	rec := do(t, h, http.MethodGet, "/api/v1/health", "", "", map[string]string{"X-Request-Id": "client-req-123"})
	if got := rec.Header().Get("X-Request-Id"); got != "client-req-123" {
		t.Fatalf("request id 未透传: %q", got)
	}

	rec = do(t, h, http.MethodGet, "/api/v1/health", "", "", nil)
	if _, err := uuid.Parse(rec.Header().Get("X-Request-Id")); err != nil {
		t.Fatalf("生成的 request id 不是 UUID: %q", rec.Header().Get("X-Request-Id"))
	}

	// 结构化日志包含 requestId/method/path/status/耗时，且不打印 token。
	st, err := fixture.New("../../fixtures")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h2 := httpapi.New(config.Config{Version: "test", MetricsEnabled: true}, st, logger)
	user := newUser(t, h2)
	do(t, h2, http.MethodGet, "/api/v1/my/devices", "", user.Token, map[string]string{"X-Request-Id": "log-req-1"})

	line := buf.String()
	for _, want := range []string{`"request_id":"log-req-1"`, `"method":"GET"`, `"path":"/api/v1/my/devices"`, `"status":200`, `"duration_ms":`} {
		if !strings.Contains(line, want) {
			t.Fatalf("日志缺少 %q:\n%s", want, line)
		}
	}
	if strings.Contains(line, user.Token) {
		t.Fatalf("日志打印了 token:\n%s", line)
	}
}
