package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/platojobs/PJGoIoT/internal/config"
	"github.com/platojobs/PJGoIoT/internal/httpapi"
	"github.com/platojobs/PJGoIoT/internal/model"
	"github.com/platojobs/PJGoIoT/internal/store"
	"github.com/platojobs/PJGoIoT/internal/store/postgres"
)

// TestPostgresFeedbackIntegration 用真实 PostgreSQL 覆盖反馈的写入、校验、幂等、隔离、
// 分页以及 HTTP 层的 401/限流行为。
func TestPostgresFeedbackIntegration(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("未设置 TEST_DATABASE_URL，跳过真实数据库集成测试")
	}
	runMigrations(t, dsn)

	ctx := context.Background()
	st, err := postgres.New(ctx, dsn)
	if err != nil {
		t.Fatalf("连接数据库失败: %v", err)
	}
	defer st.Close()

	cleanupDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("打开清理连接失败: %v", err)
	}
	defer cleanupDB.Close()

	tokenA := "feedback-token-a-" + uuid.NewString()
	tokenB := "feedback-token-b-" + uuid.NewString()
	userA, err := st.CreateUserWithToken(ctx, hashOf(tokenA), time.Now().UTC().Add(180*24*time.Hour))
	if err != nil {
		t.Fatalf("创建用户 A 失败: %v", err)
	}
	userB, err := st.CreateUserWithToken(ctx, hashOf(tokenB), time.Now().UTC().Add(180*24*time.Hour))
	if err != nil {
		t.Fatalf("创建用户 B 失败: %v", err)
	}
	t.Cleanup(func() {
		cleanupDB.Exec("DELETE FROM users WHERE id = $1 OR id = $2", userA, userB)
	})

	sub := model.FeedbackSubmission{
		Kind:        "bug",
		Body:        "家庭邀请码页面在深色模式下看不清",
		Contact:     strPtr("feedback@example.com"),
		Platform:    "android",
		AppVersion:  "1.2.3",
		BuildType:   "release",
		OSVersion:   "Android 14",
		DeviceModel: "Pixel 7",
		Diagnostics: strPtr("E/AndroidRuntime: NullPointerException"),
	}

	var first model.Feedback

	t.Run("提交成功且本人可见", func(t *testing.T) {
		item, replayed, err := st.CreateFeedback(ctx, userA, sub, store.FeedbackOptions{
			IdempotencyKey: "k:it-feedback-1", RequestHash: "h-1",
		})
		if err != nil || replayed {
			t.Fatalf("err=%v replayed=%v", err, replayed)
		}
		if item.FeedbackID == "" || item.CreatedAt.IsZero() || item.Status != "submitted" {
			t.Fatalf("返回值异常: %+v", item)
		}
		first = item

		page, err := st.ListUserFeedback(ctx, userA, "", 20)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Items) != 1 || page.Items[0].FeedbackID != item.FeedbackID {
			t.Fatalf("本人列表异常: %+v", page.Items)
		}
	})

	t.Run("空白描述被数据库约束拒绝", func(t *testing.T) {
		_, _, err := st.CreateFeedback(ctx, userA, model.FeedbackSubmission{
			Kind: "bug", Body: "   ", Platform: "android", AppVersion: "1.0",
			BuildType: "release", OSVersion: "x", DeviceModel: "y",
		}, store.FeedbackOptions{IdempotencyKey: "k:blank", RequestHash: "h-blank"})
		if err == nil {
			t.Fatal("空白描述应被拒绝")
		}
	})

	t.Run("超长描述被数据库约束拒绝", func(t *testing.T) {
		long := strings.Repeat("a", 4001)
		_, _, err := st.CreateFeedback(ctx, userA, model.FeedbackSubmission{
			Kind: "bug", Body: long, Platform: "android", AppVersion: "1.0",
			BuildType: "release", OSVersion: "x", DeviceModel: "y",
		}, store.FeedbackOptions{IdempotencyKey: "k:long", RequestHash: "h-long"})
		if err == nil {
			t.Fatal("超长描述应被拒绝")
		}
	})

	t.Run("幂等重复提交返回同一 ID 且不新增记录", func(t *testing.T) {
		again, replayed, err := st.CreateFeedback(ctx, userA, sub, store.FeedbackOptions{
			IdempotencyKey: "k:it-feedback-1", RequestHash: "h-1",
		})
		if err != nil || !replayed {
			t.Fatalf("err=%v replayed=%v", err, replayed)
		}
		if again.FeedbackID != first.FeedbackID || !again.CreatedAt.Equal(first.CreatedAt) {
			t.Fatalf("幂等结果不一致: %+v vs %+v", again, first)
		}
		page, err := st.ListUserFeedback(ctx, userA, "", 20)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Items) != 1 {
			t.Fatalf("重复提交产生了多条记录: %d", len(page.Items))
		}
	})

	t.Run("同幂等键不同内容冲突", func(t *testing.T) {
		_, _, err := st.CreateFeedback(ctx, userA, sub, store.FeedbackOptions{
			IdempotencyKey: "k:it-feedback-1", RequestHash: "h-different",
		})
		if !errors.Is(err, store.ErrIdempotencyConflict) {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("跨用户隔离", func(t *testing.T) {
		// B 没有任何反馈；A 的记录不会出现在 B 的列表里。
		page, err := st.ListUserFeedback(ctx, userB, "", 20)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Items) != 0 {
			t.Fatalf("B 不应看到任何反馈: %+v", page.Items)
		}
		_, _, err = st.CreateFeedback(ctx, userB, model.FeedbackSubmission{
			Kind: "feature", Body: "希望支持导出 CSV", Platform: "ios", AppVersion: "2.0",
			BuildType: "beta", OSVersion: "iOS 17", DeviceModel: "iPhone 15",
		}, store.FeedbackOptions{IdempotencyKey: "k:it-feedback-b", RequestHash: "h-b"})
		if err != nil {
			t.Fatal(err)
		}
		pageA, err := st.ListUserFeedback(ctx, userA, "", 20)
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range pageA.Items {
			if item.FeedbackID == "" {
				t.Fatalf("A 列表出现空 id")
			}
			if item.Body == "希望支持导出 CSV" {
				t.Fatalf("A 列表泄露了 B 的反馈")
			}
		}
		pageB, err := st.ListUserFeedback(ctx, userB, "", 20)
		if err != nil {
			t.Fatal(err)
		}
		if len(pageB.Items) != 1 || pageB.Items[0].Body != "希望支持导出 CSV" {
			t.Fatalf("B 列表异常: %+v", pageB.Items)
		}
	})

	t.Run("游标分页与非法游标", func(t *testing.T) {
		// A 已有 1 条，再补 2 条共 3 条。
		for i, body := range []string{"分页一", "分页二"} {
			key := "k:page-" + strconv.Itoa(i)
			_, _, err := st.CreateFeedback(ctx, userA, model.FeedbackSubmission{
				Kind: "other", Body: body, Platform: "android", AppVersion: "1.0",
				BuildType: "debug", OSVersion: "Android 13", DeviceModel: "Mi 11",
			}, store.FeedbackOptions{
				IdempotencyKey: key, RequestHash: "h-" + key,
			})
			if err != nil {
				t.Fatal(err)
			}
		}
		seen := map[string]bool{}
		page, err := st.ListUserFeedback(ctx, userA, "", 2)
		if err != nil {
			t.Fatal(err)
		}
		if !page.HasMore || page.NextCursor == "" || len(page.Items) != 2 {
			t.Fatalf("首页异常: %+v", page)
		}
		for _, item := range page.Items {
			seen[item.FeedbackID] = true
		}
		page2, err := st.ListUserFeedback(ctx, userA, page.NextCursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		if page2.HasMore || len(page2.Items) != 1 {
			t.Fatalf("第二页异常: %+v", page2)
		}
		if seen[page2.Items[0].FeedbackID] {
			t.Fatalf("分页重复返回同一条")
		}

		if _, err := st.ListUserFeedback(ctx, userA, "not-a-cursor", 20); !errors.Is(err, store.ErrBadCursor) {
			t.Fatalf("非法游标 err=%v", err)
		}
	})

	t.Run("HTTP 未登录 401 与双维度限流", func(t *testing.T) {
		h := httpapi.New(config.Config{
			Version:            "test",
			CORSAllowedOrigins: []string{"*"},
			RateLimitBackend:   "postgres",
			FeedbackUserPerMin: 2,
			FeedbackUserBurst:  2,
			MetricsEnabled:     true,
		}, st, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)

		body := `{"kind":"bug","body":"HTTP 限流测试","context":{"platform":"android",` +
			`"appVersion":"1.0","buildType":"release","osVersion":"Android 14","deviceModel":"Pixel"}}`

		rec := doHTTP(t, h, http.MethodPost, "/api/v1/feedback", body, "")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("未登录 status=%d body=%s", rec.Code, rec.Body.String())
		}

		// 账号维度额度 2：前两次成功（第二次为同内容幂等重放），第三次 429。
		for i := 0; i < 2; i++ {
			rec := doHTTP(t, h, http.MethodPost, "/api/v1/feedback", body, tokenA)
			if rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
				t.Fatalf("第 %d 次提交 status=%d body=%s", i+1, rec.Code, rec.Body.String())
			}
		}
		rec = doHTTP(t, h, http.MethodPost, "/api/v1/feedback", body, tokenA)
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("超限应 429，实际 %d body=%s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), httpapi.CodeFeedbackRateLimited) {
			t.Fatalf("错误码缺失: %s", rec.Body.String())
		}
	})
}

func doHTTP(t *testing.T, h http.Handler, method, path, body, token string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}
