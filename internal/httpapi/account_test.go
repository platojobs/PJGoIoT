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

type deletionRequestResp struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expiresAt"`
}

type exportTaskResp struct {
	ID                string     `json:"id"`
	Status            string     `json:"status"`
	CreatedAt         time.Time  `json:"createdAt"`
	CompletedAt       *time.Time `json:"completedAt,omitempty"`
	FailureReason     string     `json:"failureReason,omitempty"`
	DownloadURL       string     `json:"downloadUrl,omitempty"`
	DownloadExpiresAt *time.Time `json:"downloadExpiresAt,omitempty"`
	ExpiresAt         time.Time  `json:"expiresAt"`
}

type exportAggregate struct {
	Platform   string    `json:"platform"`
	ExternalID string    `json:"externalId"`
	Metric     string    `json:"metric"`
	Unit       string    `json:"unit"`
	Count      int64     `json:"count"`
	Min        float64   `json:"min"`
	Max        float64   `json:"max"`
	Avg        float64   `json:"avg"`
	FirstAt    time.Time `json:"firstAt"`
	LastAt     time.Time `json:"lastAt"`
}

type personalExportResp struct {
	User struct {
		ID        string    `json:"id"`
		Kind      string    `json:"kind"`
		CreatedAt time.Time `json:"createdAt"`
	} `json:"user"`
	Devices     []map[string]any  `json:"devices"`
	Readings    []exportAggregate `json:"readings"`
	Households  []map[string]any  `json:"households"`
	Feedback    []map[string]any  `json:"feedback"`
	GeneratedAt time.Time         `json:"generatedAt"`
}

func newHandlerWithStore(t *testing.T, cfg config.Config, st *fixture.Store) http.Handler {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return httpapi.New(cfg, st, logger, nil)
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

func TestExportAsyncAndDownload(t *testing.T) {
	st, err := fixture.New("../../fixtures")
	if err != nil {
		t.Fatal(err)
	}
	h := newHandlerWithStore(t, config.Config{Version: "test"}, st)
	user := newUser(t, h)
	other := newUser(t, h)
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)

	readings := []string{
		readingJSON("33333333-3333-4333-8333-333333333333", "android", "AA:BB:CC:00:00:01", "temperature_celsius", "23", "celsius", base.Add(20*time.Minute)),
		readingJSON("22222222-2222-4222-8222-222222222222", "android", "AA:BB:CC:00:00:01", "relative_humidity", "48", "percent", base.Add(10*time.Minute)),
		readingJSON("44444444-4444-4444-8444-444444444444", "android", "AA:BB:CC:00:00:01", "temperature_celsius", "21", "celsius", base),
	}
	body := pushBody(t, "["+deviceJSON("11111111-1111-4111-8111-111111111111", "AA:BB:CC:00:00:01", base)+"]", readings)
	if rec := do(t, h, http.MethodPost, "/api/v1/sync/push", body, user.Token, nil); rec.Code != http.StatusOK {
		t.Fatalf("push status=%d body=%s", rec.Code, rec.Body.String())
	}
	// 另一个账号的数据绝不能出现在导出里。
	otherBody := pushBody(t, "["+deviceJSON("55555555-5555-4555-8555-555555555555", "AA:BB:CC:00:00:02", base)+"]",
		[]string{readingJSON("66666666-6666-4666-8666-666666666666", "android", "AA:BB:CC:00:00:02", "temperature_celsius", "99", "celsius", base)})
	if rec := do(t, h, http.MethodPost, "/api/v1/sync/push", otherBody, other.Token, nil); rec.Code != http.StatusOK {
		t.Fatalf("other push status=%d body=%s", rec.Code, rec.Body.String())
	}

	if rec := do(t, h, http.MethodPost, "/api/v1/me/export", "", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("未认证 export status=%d", rec.Code)
	}
	rec := do(t, h, http.MethodPost, "/api/v1/me/export", "", user.Token, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("创建导出任务 status=%d body=%s", rec.Code, rec.Body.String())
	}
	created := decode[exportTaskResp](t, rec)
	if created.ID == "" || created.Status != "pending" || created.ExpiresAt.IsZero() {
		t.Fatalf("创建任务响应异常: %+v", created)
	}

	ready := waitExportReady(t, h, user.Token, created.ID)
	if ready.Status != "ready" || ready.DownloadURL == "" || ready.DownloadExpiresAt == nil {
		t.Fatalf("任务未就绪: %+v", ready)
	}
	if d := time.Until(*ready.DownloadExpiresAt); d < 9*time.Minute || d > 11*time.Minute {
		t.Fatalf("下载链接 TTL 异常: %v", d)
	}

	// 错误令牌 403。
	rec = do(t, h, http.MethodGet, "/api/v1/me/export/"+created.ID+"/download?token=not-a-real-token", "", "", nil)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "EXPORT_DOWNLOAD_INVALID") {
		t.Fatalf("错误下载令牌 status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = do(t, h, http.MethodGet, ready.DownloadURL, "", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("下载 status=%d body=%s", rec.Code, rec.Body.String())
	}
	wantCD := `attachment; filename="pjgoiot-export-` + created.ID + `.json"`
	if got := rec.Header().Get("Content-Disposition"); got != wantCD {
		t.Fatalf("Content-Disposition=%q want=%q", got, wantCD)
	}
	exported := decode[personalExportResp](t, rec)
	if exported.User.ID != user.UserID || exported.User.Kind != "anonymous" || exported.User.CreatedAt.IsZero() {
		t.Fatalf("user 信息不完整: %+v", exported.User)
	}
	if len(exported.Devices) != 1 {
		t.Fatalf("devices=%d", len(exported.Devices))
	}
	// 3 条读数聚合为 temperature(2 条) 与 humidity(1 条)。
	if len(exported.Readings) != 2 {
		t.Fatalf("readings 聚合数=%d want=2: %+v", len(exported.Readings), exported.Readings)
	}
	var temp *exportAggregate
	for i := range exported.Readings {
		if exported.Readings[i].Metric == "temperature_celsius" {
			temp = &exported.Readings[i]
		}
	}
	if temp == nil || temp.Count != 2 || temp.Min != 21 || temp.Max != 23 || temp.Avg != 22 {
		t.Fatalf("temperature 聚合异常: %+v", temp)
	}
	if !temp.FirstAt.Equal(base) || !temp.LastAt.Equal(base.Add(20*time.Minute)) {
		t.Fatalf("聚合时间范围异常: first=%s last=%s", temp.FirstAt, temp.LastAt)
	}
	if strings.Contains(rec.Body.String(), "AA:BB:CC:00:00:02") || strings.Contains(rec.Body.String(), other.UserID) {
		t.Fatalf("导出泄露他人数据: %s", rec.Body.String())
	}

	// 一次性：同一链接第二次 410；重新查询任务可拿到新链接。
	if rec := do(t, h, http.MethodGet, ready.DownloadURL, "", "", nil); rec.Code != http.StatusGone {
		t.Fatalf("重放下载 status=%d body=%s", rec.Code, rec.Body.String())
	}
	again := waitExportReady(t, h, user.Token, created.ID)
	if again.DownloadURL == "" || again.DownloadURL == ready.DownloadURL {
		t.Fatalf("应签发新下载链接: %q", again.DownloadURL)
	}
	if rec := do(t, h, http.MethodGet, again.DownloadURL, "", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("新链接不可用: %d %s", rec.Code, rec.Body.String())
	}

	// 过期链接：把下载令牌有效期拨到过去后必须 410。
	expiredToken, expiredHash, err := auth.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.PrepareExportDownload(context.Background(), user.UserID, created.ID, expiredHash, time.Now().UTC().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	rec = do(t, h, http.MethodGet, "/api/v1/me/export/"+created.ID+"/download?token="+expiredToken, "", "", nil)
	if rec.Code != http.StatusGone || !strings.Contains(rec.Body.String(), "EXPORT_DOWNLOAD_EXPIRED") {
		t.Fatalf("过期链接 status=%d body=%s", rec.Code, rec.Body.String())
	}

	if rec := do(t, h, http.MethodGet, "/api/v1/me/export/"+created.ID, "", other.Token, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("他人任务 status=%d", rec.Code)
	}
}

func waitExportReady(t *testing.T, h http.Handler, token string, taskID string) exportTaskResp {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rec := do(t, h, http.MethodGet, "/api/v1/me/export/"+taskID, "", token, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("查询导出任务 status=%d body=%s", rec.Code, rec.Body.String())
		}
		task := decode[exportTaskResp](t, rec)
		if task.Status == "ready" {
			return task
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("等待导出任务就绪超时")
	return exportTaskResp{}
}

func TestDeleteMeRequiresDeletionToken(t *testing.T) {
	h := newHandler(t, 0)
	victim := newUser(t, h)
	other := newUser(t, h)

	base := time.Now().UTC().Add(-time.Minute)
	body := pushBody(t, "["+deviceJSON("11111111-1111-4111-8111-111111111111", "AA:BB:CC:00:00:01", base)+"]",
		[]string{readingJSON("22222222-2222-4222-8222-222222222222", "android", "AA:BB:CC:00:00:01", "temperature_celsius", "20", "celsius", base)})
	if rec := do(t, h, http.MethodPost, "/api/v1/sync/push", body, victim.Token, nil); rec.Code != http.StatusOK {
		t.Fatalf("push status=%d body=%s", rec.Code, rec.Body.String())
	}

	if rec := do(t, h, http.MethodPost, "/api/v1/me/deletion-request", "", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("未认证 deletion-request status=%d", rec.Code)
	}
	rec := do(t, h, http.MethodPost, "/api/v1/me/deletion-request", "", victim.Token, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("deletion-request status=%d body=%s", rec.Code, rec.Body.String())
	}
	request := decode[deletionRequestResp](t, rec)
	if len(request.Token) < 32 || request.ExpiresAt.IsZero() {
		t.Fatalf("删除令牌响应异常: %+v", request)
	}
	if d := time.Until(request.ExpiresAt); d < 9*time.Minute || d > 11*time.Minute {
		t.Fatalf("删除令牌 TTL 异常: %v", d)
	}

	// 少令牌与错令牌必须是明确错误码。
	rec = do(t, h, http.MethodDelete, "/api/v1/me", "", "", nil)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "DELETION_TOKEN_REQUIRED") {
		t.Fatalf("缺令牌 status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = do(t, h, http.MethodDelete, "/api/v1/me", "", "", map[string]string{"X-Deletion-Token": "wrong-token"})
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "DELETION_TOKEN_INVALID") {
		t.Fatalf("错令牌 status=%d body=%s", rec.Code, rec.Body.String())
	}

	rec = do(t, h, http.MethodDelete, "/api/v1/me", "", "", map[string]string{"X-Deletion-Token": request.Token})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("注销 status=%d body=%s", rec.Code, rec.Body.String())
	}
	// 旧 Bearer 立即失效。
	if rec := do(t, h, http.MethodGet, "/api/v1/my/devices", "", victim.Token, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("注销后旧 token status=%d body=%s", rec.Code, rec.Body.String())
	}
	// 同一删除请求重放：账号已不存在，返回 404 而不是未知错误。
	rec = do(t, h, http.MethodDelete, "/api/v1/me", "", "", map[string]string{"X-Deletion-Token": request.Token})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("重放删除 status=%d body=%s", rec.Code, rec.Body.String())
	}
	// 新的删除申请也无法再找到账号（Bearer 已失效）。
	if rec := do(t, h, http.MethodPost, "/api/v1/me/deletion-request", "", victim.Token, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("注销后申请删除 status=%d body=%s", rec.Code, rec.Body.String())
	}
	// 其他用户不受影响。
	if rec := do(t, h, http.MethodGet, "/api/v1/my/devices", "", other.Token, nil); rec.Code != http.StatusOK {
		t.Fatalf("其他用户受注销影响: %d", rec.Code)
	}
}

func TestDeleteMeBlockedByOwnedHousehold(t *testing.T) {
	st, err := fixture.New("../../fixtures")
	if err != nil {
		t.Fatal(err)
	}
	h := newHandlerWithStore(t, config.Config{Version: "test"}, st)
	ctx := context.Background()

	owner := newUser(t, h)
	member := newUser(t, h)
	household, err := st.CreateHousehold(ctx, owner.UserID, "共享家庭")
	if err != nil {
		t.Fatal(err)
	}
	code, err := auth.NewPairingCode()
	if err != nil {
		t.Fatal(err)
	}
	hash := auth.PairingCodeHash(code)
	if _, err := st.CreateHouseholdInvitation(ctx, owner.UserID, household.ID, hash, time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimHouseholdInvitation(ctx, member.UserID, hash, time.Now().UTC(), time.Minute); err != nil {
		t.Fatal(err)
	}

	rec := do(t, h, http.MethodPost, "/api/v1/me/deletion-request", "", owner.Token, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("deletion-request status=%d body=%s", rec.Code, rec.Body.String())
	}
	request := decode[deletionRequestResp](t, rec)

	rec = do(t, h, http.MethodDelete, "/api/v1/me", "", "", map[string]string{"X-Deletion-Token": request.Token})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "ACCOUNT_DELETION_BLOCKED") {
		t.Fatalf("owner 有成员时应 409: status=%d body=%s", rec.Code, rec.Body.String())
	}
	// 被拦截后账号仍完整可用。
	if rec := do(t, h, http.MethodGet, "/api/v1/my/devices", "", owner.Token, nil); rec.Code != http.StatusOK {
		t.Fatalf("拦截后账号不可用: %d", rec.Code)
	}

	// 转让所有权后可以删除。
	if err := st.TransferHouseholdOwnership(ctx, owner.UserID, household.ID, member.UserID); err != nil {
		t.Fatal(err)
	}
	rec = do(t, h, http.MethodDelete, "/api/v1/me", "", "", map[string]string{"X-Deletion-Token": request.Token})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("转让后注销 status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := do(t, h, http.MethodGet, "/api/v1/my/devices", "", member.Token, nil); rec.Code != http.StatusOK {
		t.Fatalf("新 owner 受影响: %d", rec.Code)
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
	h2 := httpapi.New(config.Config{Version: "test", MetricsEnabled: true}, st, logger, nil)
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
