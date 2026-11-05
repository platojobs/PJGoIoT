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
