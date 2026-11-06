package httpapi_test

import (
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/platojobs/PJGoIoT/internal/config"
	"github.com/platojobs/PJGoIoT/internal/httpapi"
	"github.com/platojobs/PJGoIoT/internal/store/fixture"
)

type feedbackCreated struct {
	FeedbackID string `json:"feedbackId"`
	CreatedAt  string `json:"createdAt"`
}

type feedbackItem struct {
	FeedbackID  string `json:"feedbackId"`
	Kind        string `json:"kind"`
	Body        string `json:"body"`
	Status      string `json:"status"`
	Platform    string `json:"platform"`
	AppVersion  string `json:"appVersion"`
	BuildType   string `json:"buildType"`
	OSVersion   string `json:"osVersion"`
	DeviceModel string `json:"deviceModel"`
	CreatedAt   string `json:"createdAt"`
}

type feedbackPage struct {
	Items      []feedbackItem `json:"items"`
	NextCursor string         `json:"nextCursor"`
	HasMore    bool           `json:"hasMore"`
}

func newFeedbackHandler(t *testing.T, cfg config.Config) http.Handler {
	t.Helper()
	st, err := fixture.New("../../fixtures")
	if err != nil {
		t.Fatalf("加载 fixtures 失败: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg.Version = "test"
	cfg.CORSAllowedOrigins = []string{"*"}
	cfg.MetricsEnabled = true
	return httpapi.New(cfg, st, logger, nil)
}

func feedbackBody(kind, body string) string {
	return `{"kind":"` + kind + `","body":` + quoteJSON(body) + `,"contact":"user@example.com",` +
		`"context":{"platform":"android","appVersion":"1.2.3","buildType":"release",` +
		`"osVersion":"Android 14","deviceModel":"Pixel 7"},"diagnostics":"stacktrace"}`
}

func quoteJSON(s string) string {
	return `"` + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`) + `"`
}

func TestFeedbackRequiresAuth(t *testing.T) {
	h := newFeedbackHandler(t, config.Config{})
	rec := do(t, h, http.MethodPost, "/api/v1/feedback", feedbackBody("bug", "登录后闪退"), "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("未登录应 401，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	if got := decode[errResponse](t, rec).Error.Code; got != httpapi.CodeUnauthorized {
		t.Fatalf("错误码=%s", got)
	}

	rec = do(t, h, http.MethodGet, "/api/v1/feedback/mine", "", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("未登录列表应 401，实际 %d", rec.Code)
	}
}

func TestFeedbackValidation(t *testing.T) {
	h := newFeedbackHandler(t, config.Config{})
	user := newUser(t, h)

	cases := []struct {
		name string
		body string
		want int
	}{
		{"空白描述", feedbackBody("bug", "   \n\t "), http.StatusBadRequest},
		{"超长描述", feedbackBody("bug", strings.Repeat("a", 4001)), http.StatusBadRequest},
		{"未知类型", feedbackBody("unknown", "描述"), http.StatusBadRequest},
		{"非法平台", `{"kind":"bug","body":"描述","context":{"platform":"web","appVersion":"1.0",` +
			`"buildType":"release","osVersion":"x","deviceModel":"y"}}`, http.StatusBadRequest},
		{"缺少客户端上下文", `{"kind":"bug","body":"描述"}`, http.StatusBadRequest},
		{"非法 JSON", `{"kind":`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, h, http.MethodPost, "/api/v1/feedback", tc.body, user.Token, nil)
			if rec.Code != tc.want {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}

	// 合法枚举值全部应被接受。
	for _, kind := range []string{"bug", "feature", "experience", "other"} {
		rec := do(t, h, http.MethodPost, "/api/v1/feedback", feedbackBody(kind, "描述-"+kind), user.Token, nil)
		if rec.Code != http.StatusCreated {
			t.Fatalf("kind=%s status=%d body=%s", kind, rec.Code, rec.Body.String())
		}
	}
}

func TestFeedbackSubmitAndIdempotency(t *testing.T) {
	h := newFeedbackHandler(t, config.Config{})
	user := newUser(t, h)
	body := feedbackBody("experience", "家庭邀请码输入框在小屏上被键盘遮挡")

	rec := do(t, h, http.MethodPost, "/api/v1/feedback", body, user.Token, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("首次提交 status=%d body=%s", rec.Code, rec.Body.String())
	}
	first := decode[feedbackCreated](t, rec)
	if first.FeedbackID == "" || first.CreatedAt == "" {
		t.Fatalf("响应缺少 feedbackId/createdAt: %+v", first)
	}

	// 无 Idempotency-Key 时按规范化请求体去重。
	rec = do(t, h, http.MethodPost, "/api/v1/feedback", body, user.Token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("重复提交应 200 重放，实际 %d", rec.Code)
	}
	if rec.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatalf("缺少 Idempotent-Replayed 头")
	}
	if again := decode[feedbackCreated](t, rec); again.FeedbackID != first.FeedbackID {
		t.Fatalf("重复提交 feedbackId 不一致: %s vs %s", again.FeedbackID, first.FeedbackID)
	}

	// 显式 Idempotency-Key：相同 body 命中，不同 body 409。
	headers := map[string]string{"Idempotency-Key": "feedback-key-1"}
	rec = do(t, h, http.MethodPost, "/api/v1/feedback", body, user.Token, headers)
	firstKeyed := decode[feedbackCreated](t, rec)
	if rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
		t.Fatalf("带 key 首次提交 status=%d", rec.Code)
	}
	rec = do(t, h, http.MethodPost, "/api/v1/feedback", body, user.Token, headers)
	if rec.Code != http.StatusOK || decode[feedbackCreated](t, rec).FeedbackID != firstKeyed.FeedbackID {
		t.Fatalf("同 key 同 body 未去重: status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = do(t, h, http.MethodPost, "/api/v1/feedback",
		feedbackBody("bug", "完全不同的内容"), user.Token, headers)
	if rec.Code != http.StatusConflict || decode[errResponse](t, rec).Error.Code != httpapi.CodeIdempotencyConflict {
		t.Fatalf("同 key 不同 body 应 409，status=%d body=%s", rec.Code, rec.Body.String())
	}

	// 列表只返回一条且状态为 submitted。
	rec = do(t, h, http.MethodGet, "/api/v1/feedback/mine", "", user.Token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("列表 status=%d", rec.Code)
	}
	page := decode[feedbackPage](t, rec)
	if len(page.Items) != 2 { // body 派生的一条 + 带 key 的一条
		t.Fatalf("items=%d body=%s", len(page.Items), rec.Body.String())
	}
	for _, item := range page.Items {
		if item.Status != "submitted" {
			t.Fatalf("状态=%s", item.Status)
		}
	}
}

func TestFeedbackMineIsolationAndPagination(t *testing.T) {
	h := newFeedbackHandler(t, config.Config{})
	alice := newUser(t, h)
	bob := newUser(t, h)

	for i := 0; i < 3; i++ {
		rec := do(t, h, http.MethodPost, "/api/v1/feedback",
			feedbackBody("bug", "alice-"+strconv.Itoa(i)), alice.Token, nil)
		if rec.Code != http.StatusCreated {
			t.Fatalf("alice 提交失败: %d %s", rec.Code, rec.Body.String())
		}
	}
	rec := do(t, h, http.MethodPost, "/api/v1/feedback", feedbackBody("feature", "bob-1"), bob.Token, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("bob 提交失败: %d", rec.Code)
	}

	// 分页：limit=2，翻页后共 3 条且不重复。
	seen := map[string]bool{}
	cursor := ""
	for page := 0; page < 3; page++ {
		path := "/api/v1/feedback/mine?limit=2"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		rec := do(t, h, http.MethodGet, path, "", alice.Token, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("分页 status=%d body=%s", rec.Code, rec.Body.String())
		}
		p := decode[feedbackPage](t, rec)
		for _, item := range p.Items {
			if seen[item.FeedbackID] {
				t.Fatalf("分页出现重复 feedbackId=%s", item.FeedbackID)
			}
			seen[item.FeedbackID] = true
		}
		if !p.HasMore {
			break
		}
		cursor = p.NextCursor
	}
	if len(seen) != 3 {
		t.Fatalf("alice 应只看到自己的 3 条，实际 %d", len(seen))
	}

	// bob 的列表不包含 alice 的数据，也不含 alice 的联系方式。
	rec = do(t, h, http.MethodGet, "/api/v1/feedback/mine", "", bob.Token, nil)
	p := decode[feedbackPage](t, rec)
	if len(p.Items) != 1 || p.Items[0].Body != "bob-1" {
		t.Fatalf("bob 列表异常: %+v", p.Items)
	}
	if strings.Contains(rec.Body.String(), "user@example.com") {
		t.Fatalf("列表响应泄露联系方式: %s", rec.Body.String())
	}

	// 非法游标 400。
	rec = do(t, h, http.MethodGet, "/api/v1/feedback/mine?cursor=not-a-cursor", "", alice.Token, nil)
	if rec.Code != http.StatusBadRequest || decode[errResponse](t, rec).Error.Code != httpapi.CodeBadRequest {
		t.Fatalf("非法游标 status=%d body=%s", rec.Code, rec.Body.String())
	}
}

type feedbackDetail struct {
	FeedbackID    string `json:"feedbackId"`
	Kind          string `json:"kind"`
	Body          string `json:"body"`
	Status        string `json:"status"`
	StatusMessage string `json:"statusMessage"`
	CreatedAt     string `json:"createdAt"`
	UpdatedAt     string `json:"updatedAt"`
}

type adminFeedbackItem struct {
	FeedbackID   string  `json:"feedbackId"`
	UserID       string  `json:"userId"`
	Body         string  `json:"body"`
	Contact      *string `json:"contact"`
	Diagnostics  *string `json:"diagnostics"`
	Status       string  `json:"status"`
	InternalNote *string `json:"internalNote"`
	CreatedAt    string  `json:"createdAt"`
	UpdatedAt    string  `json:"updatedAt"`
}

type adminFeedbackPage struct {
	Items      []adminFeedbackItem `json:"items"`
	NextCursor string              `json:"nextCursor"`
	HasMore    bool                `json:"hasMore"`
}

func TestFeedbackDetailAndClose(t *testing.T) {
	h := newFeedbackHandler(t, config.Config{})
	alice := newUser(t, h)
	bob := newUser(t, h)

	rec := do(t, h, http.MethodPost, "/api/v1/feedback", feedbackBody("bug", "详情页闪退"), alice.Token, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("提交 status=%d body=%s", rec.Code, rec.Body.String())
	}
	created := decode[feedbackCreated](t, rec)
	detailPath := "/api/v1/feedback/" + created.FeedbackID

	// 本人详情：含状态说明，不含联系方式与诊断。
	rec = do(t, h, http.MethodGet, detailPath, "", alice.Token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("详情 status=%d body=%s", rec.Code, rec.Body.String())
	}
	d := decode[feedbackDetail](t, rec)
	if d.FeedbackID != created.FeedbackID || d.Status != "submitted" || d.StatusMessage == "" {
		t.Fatalf("详情字段异常: %+v", d)
	}
	if d.CreatedAt == "" || d.UpdatedAt == "" {
		t.Fatalf("缺少时间字段: %+v", d)
	}
	if strings.Contains(rec.Body.String(), "user@example.com") || strings.Contains(rec.Body.String(), "stacktrace") {
		t.Fatalf("详情泄露个人信息/诊断: %s", rec.Body.String())
	}

	// 他人不可见：GET/PATCH 一律 404。
	rec = do(t, h, http.MethodGet, detailPath, "", bob.Token, nil)
	if rec.Code != http.StatusNotFound || decode[errResponse](t, rec).Error.Code != httpapi.CodeNotFound {
		t.Fatalf("他人读取应 404，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	rec = do(t, h, http.MethodPatch, detailPath, `{"status":"closed"}`, bob.Token, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("他人关闭应 404，实际 %d body=%s", rec.Code, rec.Body.String())
	}

	// 非法 id 与非法目标状态。
	rec = do(t, h, http.MethodGet, "/api/v1/feedback/not-a-uuid", "", alice.Token, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("非法 id 应 404，实际 %d", rec.Code)
	}
	rec = do(t, h, http.MethodPatch, detailPath, `{"status":"received"}`, alice.Token, nil)
	if rec.Code != http.StatusBadRequest || decode[errResponse](t, rec).Error.Code != httpapi.CodeBadRequest {
		t.Fatalf("用户推进到 received 应 400，实际 %d body=%s", rec.Code, rec.Body.String())
	}

	// 本人关闭：submitted -> closed，返回详情结构。
	rec = do(t, h, http.MethodPatch, detailPath, `{"status":"closed"}`, alice.Token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("关闭 status=%d body=%s", rec.Code, rec.Body.String())
	}
	d = decode[feedbackDetail](t, rec)
	if d.Status != "closed" || d.StatusMessage == "" {
		t.Fatalf("关闭后详情异常: %+v", d)
	}

	// 已关闭再改：409 FEEDBACK_INVALID_TRANSITION，不允许回退。
	rec = do(t, h, http.MethodPatch, detailPath, `{"status":"closed"}`, alice.Token, nil)
	if rec.Code != http.StatusConflict || decode[errResponse](t, rec).Error.Code != httpapi.CodeFeedbackInvalidTransition {
		t.Fatalf("重复关闭应 409，实际 %d body=%s", rec.Code, rec.Body.String())
	}

	// 列表按状态过滤，游标语义不变。
	rec = do(t, h, http.MethodGet, "/api/v1/feedback/mine?status=closed", "", alice.Token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("closed 过滤 status=%d", rec.Code)
	}
	if page := decode[feedbackPage](t, rec); len(page.Items) != 1 || page.Items[0].Status != "closed" {
		t.Fatalf("closed 过滤异常: %+v", page.Items)
	}
	rec = do(t, h, http.MethodGet, "/api/v1/feedback/mine?status=submitted", "", alice.Token, nil)
	if page := decode[feedbackPage](t, rec); len(page.Items) != 0 {
		t.Fatalf("submitted 过滤应为空: %+v", page.Items)
	}
	rec = do(t, h, http.MethodGet, "/api/v1/feedback/mine?status=bogus", "", alice.Token, nil)
	if rec.Code != http.StatusBadRequest || decode[errResponse](t, rec).Error.Code != httpapi.CodeBadRequest {
		t.Fatalf("非法 status 应 400，实际 %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAdminFeedbackAuthMatrixAndTransitions(t *testing.T) {
	const adminToken = "admin-secret-for-test"

	// 未配置 ADMIN_TOKEN：整组入口与不存在路由一致（404），不暴露存在性。
	disabled := newFeedbackHandler(t, config.Config{})
	aliceDisabled := newUser(t, disabled)
	cases := []struct {
		name  string
		token string
		hdr   map[string]string
	}{
		{"无凭据", "", nil},
		{"客户端 token", aliceDisabled.Token, nil},
		{"错误 admin 凭据", "", map[string]string{"X-Admin-Token": "whatever"}},
	}
	for _, tc := range cases {
		t.Run("未配置-"+tc.name, func(t *testing.T) {
			rec := do(t, disabled, http.MethodGet, "/api/v1/admin/feedback", "", tc.token, tc.hdr)
			if rec.Code != http.StatusNotFound || decode[errResponse](t, rec).Error.Code != httpapi.CodeNotFound {
				t.Fatalf("未配置 admin 应 404，实际 %d body=%s", rec.Code, rec.Body.String())
			}
		})
	}

	// 已配置：鉴权矩阵（admin 凭据 / 客户端 token / 无 token 三态）。
	h := newFeedbackHandler(t, config.Config{AdminToken: adminToken})
	alice := newUser(t, h)
	bob := newUser(t, h)
	adminHdr := map[string]string{"X-Admin-Token": adminToken}

	rec := do(t, h, http.MethodGet, "/api/v1/admin/feedback", "", "", nil)
	if rec.Code != http.StatusUnauthorized || decode[errResponse](t, rec).Error.Code != httpapi.CodeUnauthorized {
		t.Fatalf("无凭据应 401，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	rec = do(t, h, http.MethodGet, "/api/v1/admin/feedback", "", "", map[string]string{"X-Admin-Token": "wrong"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("错误凭据应 401，实际 %d", rec.Code)
	}
	// 只有客户端 Bearer token、没有 X-Admin-Token：仍然是 401，两种凭据互不替代。
	rec = do(t, h, http.MethodGet, "/api/v1/admin/feedback", "", alice.Token, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("客户端 token 不应通过 admin 鉴权，实际 %d", rec.Code)
	}
	rec = do(t, h, http.MethodGet, "/api/v1/admin/feedback", "", "", adminHdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin 凭据应 200，实际 %d body=%s", rec.Code, rec.Body.String())
	}

	// 准备跨用户数据：alice 两条、bob 一条。
	for i := 0; i < 2; i++ {
		body := feedbackBody("bug", "alice-admin-"+strconv.Itoa(i))
		if r := do(t, h, http.MethodPost, "/api/v1/feedback", body, alice.Token, nil); r.Code != http.StatusCreated {
			t.Fatalf("alice 提交失败: %d", r.Code)
		}
	}
	if r := do(t, h, http.MethodPost, "/api/v1/feedback", feedbackBody("feature", "bob-admin"), bob.Token, nil); r.Code != http.StatusCreated {
		t.Fatalf("bob 提交失败: %d", r.Code)
	}

	rec = do(t, h, http.MethodGet, "/api/v1/admin/feedback", "", "", adminHdr)
	page := decode[adminFeedbackPage](t, rec)
	if len(page.Items) != 3 {
		t.Fatalf("admin 应跨用户看到 3 条，实际 %d: %s", len(page.Items), rec.Body.String())
	}
	userIDs := map[string]bool{}
	for _, item := range page.Items {
		userIDs[item.UserID] = true
	}
	if !userIDs[alice.UserID] || !userIDs[bob.UserID] {
		t.Fatalf("admin 列表未覆盖两个用户: %v", userIDs)
	}
	if page.Items[0].Contact == nil || page.Items[0].Diagnostics == nil {
		t.Fatalf("admin 视图应包含联系方式与诊断")
	}

	rec = do(t, h, http.MethodGet, "/api/v1/admin/feedback?status=submitted&limit=2", "", "", adminHdr)
	if p := decode[adminFeedbackPage](t, rec); len(p.Items) != 2 || !p.HasMore || p.NextCursor == "" {
		t.Fatalf("admin 状态过滤/分页异常: %+v", p)
	}
	rec = do(t, h, http.MethodGet, "/api/v1/admin/feedback?status=bogus", "", "", adminHdr)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法 status 应 400，实际 %d", rec.Code)
	}

	// 取 alice 的一条做状态流转。
	aliceID := ""
	for _, item := range page.Items {
		if item.UserID == alice.UserID {
			aliceID = item.FeedbackID
			break
		}
	}
	adminPath := "/api/v1/admin/feedback/" + aliceID

	rec = do(t, h, http.MethodPatch, adminPath, `{"status":"received","note":"已复现"}`, "", adminHdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin 受理 status=%d body=%s", rec.Code, rec.Body.String())
	}
	first := decode[adminFeedbackItem](t, rec)
	if first.Status != "received" || first.InternalNote == nil || *first.InternalNote != "已复现" {
		t.Fatalf("受理结果异常: %+v", first)
	}

	// 同状态重复 PATCH：200 幂等，不改 updatedAt，也不覆盖备注。
	rec = do(t, h, http.MethodPatch, adminPath, `{"status":"received","note":"不该写入"}`, "", adminHdr)
	again := decode[adminFeedbackItem](t, rec)
	if rec.Code != http.StatusOK || again.Status != "received" {
		t.Fatalf("同状态 PATCH 应 200，实际 %d", rec.Code)
	}
	if again.UpdatedAt == "" || again.UpdatedAt != first.UpdatedAt {
		// 时间相等表示未改写。
		t.Fatalf("幂等 no-op 不应修改 updatedAt: %s vs %s", first.UpdatedAt, again.UpdatedAt)
	}
	if again.InternalNote == nil || *again.InternalNote != "已复现" {
		t.Fatalf("幂等 no-op 不应覆盖备注: %+v", again.InternalNote)
	}

	// received -> submitted 允许（误受理回退），submitted -> closed 允许。
	rec = do(t, h, http.MethodPatch, adminPath, `{"status":"submitted"}`, "", adminHdr)
	if rec.Code != http.StatusOK || decode[adminFeedbackItem](t, rec).Status != "submitted" {
		t.Fatalf("回退 submitted status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = do(t, h, http.MethodPatch, adminPath, `{"status":"closed"}`, "", adminHdr)
	if rec.Code != http.StatusOK || decode[adminFeedbackItem](t, rec).Status != "closed" {
		t.Fatalf("直接关闭 status=%d body=%s", rec.Code, rec.Body.String())
	}

	// closed 为终态：任何跳转 409；同状态仍为幂等 200。
	rec = do(t, h, http.MethodPatch, adminPath, `{"status":"received"}`, "", adminHdr)
	if rec.Code != http.StatusConflict || decode[errResponse](t, rec).Error.Code != httpapi.CodeFeedbackInvalidTransition {
		t.Fatalf("closed -> received 应 409，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	rec = do(t, h, http.MethodPatch, adminPath, `{"status":"closed"}`, "", adminHdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("closed -> closed 应幂等 200，实际 %d", rec.Code)
	}

	// 入参校验与不存在 id。
	rec = do(t, h, http.MethodPatch, adminPath, `{"status":"unknown"}`, "", adminHdr)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法目标状态应 400，实际 %d", rec.Code)
	}
	longNote := strings.Repeat("n", 2001)
	rec = do(t, h, http.MethodPatch, adminPath, `{"status":"closed","note":"`+longNote+`"}`, "", adminHdr)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("超长备注应 400，实际 %d", rec.Code)
	}
	rec = do(t, h, http.MethodPatch, "/api/v1/admin/feedback/00000000-0000-4000-8000-000000000000", `{"status":"received"}`, "", adminHdr)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("不存在反馈应 404，实际 %d body=%s", rec.Code, rec.Body.String())
	}

	// 用户侧详情不会回显内部备注。
	rec = do(t, h, http.MethodGet, "/api/v1/feedback/"+aliceID, "", alice.Token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("用户详情 status=%d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "已复现") || strings.Contains(rec.Body.String(), "internalNote") {
		t.Fatalf("用户详情泄露内部备注: %s", rec.Body.String())
	}
}

func TestFeedbackRateLimitByUserAndIP(t *testing.T) {
	t.Run("按账号", func(t *testing.T) {
		h := newFeedbackHandler(t, config.Config{FeedbackUserPerMin: 1, FeedbackUserBurst: 1})
		user := newUser(t, h)

		rec := do(t, h, http.MethodPost, "/api/v1/feedback", feedbackBody("bug", "第一条"), user.Token, nil)
		if rec.Code != http.StatusCreated {
			t.Fatalf("首条 status=%d", rec.Code)
		}
		rec = do(t, h, http.MethodPost, "/api/v1/feedback", feedbackBody("bug", "第二条"), user.Token, nil)
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("超限应 429，实际 %d body=%s", rec.Code, rec.Body.String())
		}
		if got := decode[errResponse](t, rec).Error.Code; got != httpapi.CodeFeedbackRateLimited {
			t.Fatalf("错误码=%s", got)
		}
		if rec.Header().Get("Retry-After") == "" {
			t.Fatalf("缺少 Retry-After")
		}
	})

	t.Run("按 IP", func(t *testing.T) {
		h := newFeedbackHandler(t, config.Config{FeedbackIPPerMin: 1, FeedbackIPBurst: 1})
		userA := newUser(t, h)
		userB := newUser(t, h)

		if rec := do(t, h, http.MethodPost, "/api/v1/feedback", feedbackBody("bug", "A"), userA.Token, nil); rec.Code != http.StatusCreated {
			t.Fatalf("A 首条 status=%d", rec.Code)
		}
		// 同一来源 IP 的第二个账号也触发 IP 维度限流。
		rec := do(t, h, http.MethodPost, "/api/v1/feedback", feedbackBody("bug", "B"), userB.Token, nil)
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("IP 超限应 429，实际 %d body=%s", rec.Code, rec.Body.String())
		}
		if got := decode[errResponse](t, rec).Error.Code; got != httpapi.CodeFeedbackRateLimited {
			t.Fatalf("错误码=%s", got)
		}
	})
}
