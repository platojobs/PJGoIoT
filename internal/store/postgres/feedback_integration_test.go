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

// TestPostgresFeedbackLifecycleIntegration 覆盖 v0.17.0 的状态机：用户关闭、运营受理/回退/关闭、
// 非法跳转、跨用户隔离、幂等 no-op、审计写入，以及 admin 入口的 404/401。
func TestPostgresFeedbackLifecycleIntegration(t *testing.T) {
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

	tokenA := "feedback-lifecycle-a-" + uuid.NewString()
	tokenB := "feedback-lifecycle-b-" + uuid.NewString()
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
		Kind: "bug", Body: "状态流转集成测试", Contact: strPtr("lifecycle@example.com"),
		Platform: "ios", AppVersion: "1.0.0", BuildType: "debug", OSVersion: "iOS 18",
		DeviceModel: "iPhone 16", Diagnostics: strPtr("lifecycle diagnostics"),
	}
	mk := func(userID, key string) model.Feedback {
		item, _, err := st.CreateFeedback(ctx, userID, sub, store.FeedbackOptions{IdempotencyKey: key, RequestHash: key})
		if err != nil {
			t.Fatalf("提交反馈失败: %v", err)
		}
		return item
	}
	fbA1 := mk(userA, "k:lifecycle-a1")
	fbA2 := mk(userA, "k:lifecycle-a2")
	fbB := mk(userB, "k:lifecycle-b")

	t.Run("用户关闭自己的反馈", func(t *testing.T) {
		detail, err := st.CloseUserFeedback(ctx, userA, fbA1.FeedbackID)
		if err != nil {
			t.Fatalf("关闭失败: %v", err)
		}
		if detail.Status != "closed" || detail.StatusMessage == "" {
			t.Fatalf("关闭结果异常: %+v", detail)
		}
		if detail.UpdatedAt.Before(detail.CreatedAt) {
			t.Fatalf("updatedAt 不应早于 createdAt: %+v", detail)
		}
	})

	t.Run("已关闭不可再次修改", func(t *testing.T) {
		if _, err := st.CloseUserFeedback(ctx, userA, fbA1.FeedbackID); !errors.Is(err, store.ErrFeedbackInvalidTransition) {
			t.Fatalf("期望 ErrFeedbackInvalidTransition，实际 %v", err)
		}
	})

	t.Run("用户不能读取或关闭他人反馈", func(t *testing.T) {
		if _, err := st.GetUserFeedback(ctx, userA, fbB.FeedbackID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("跨用户读取期望 ErrNotFound，实际 %v", err)
		}
		if _, err := st.CloseUserFeedback(ctx, userA, fbB.FeedbackID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("跨用户关闭期望 ErrNotFound，实际 %v", err)
		}
	})

	t.Run("运营合法流转与非法跳转", func(t *testing.T) {
		received, err := st.UpdateFeedbackStatus(ctx, fbA2.FeedbackID, "received", strPtr("已复现，转客户端组"))
		if err != nil {
			t.Fatalf("受理失败: %v", err)
		}
		if received.Status != "received" || received.InternalNote == nil || *received.InternalNote != "已复现，转客户端组" {
			t.Fatalf("受理结果异常: %+v", received)
		}

		// 误受理回退：received -> submitted。
		if _, err := st.UpdateFeedbackStatus(ctx, fbA2.FeedbackID, "submitted", nil); err != nil {
			t.Fatalf("回退失败: %v", err)
		}
		// submitted -> closed 允许。
		closed, err := st.UpdateFeedbackStatus(ctx, fbA2.FeedbackID, "closed", strPtr("用户已确认"))
		if err != nil {
			t.Fatalf("关闭失败: %v", err)
		}
		if closed.Status != "closed" {
			t.Fatalf("关闭结果异常: %+v", closed)
		}
		// closed 为终态。
		if _, err := st.UpdateFeedbackStatus(ctx, fbA2.FeedbackID, "received", nil); !errors.Is(err, store.ErrFeedbackInvalidTransition) {
			t.Fatalf("closed -> received 期望 ErrFeedbackInvalidTransition，实际 %v", err)
		}
		if _, err := st.UpdateFeedbackStatus(ctx, fbA2.FeedbackID, "submitted", nil); !errors.Is(err, store.ErrFeedbackInvalidTransition) {
			t.Fatalf("closed -> submitted 期望 ErrFeedbackInvalidTransition，实际 %v", err)
		}
	})

	t.Run("同状态 PATCH 幂等且不重复审计", func(t *testing.T) {
		var before struct {
			updatedAt time.Time
			events    int
		}
		if err := cleanupDB.QueryRow(`SELECT updated_at, (SELECT count(*) FROM feedback_status_events WHERE feedback_id = $1) FROM feedback WHERE id = $1`, fbB.FeedbackID).
			Scan(&before.updatedAt, &before.events); err != nil {
			t.Fatalf("读取前置状态失败: %v", err)
		}
		// 先真实受理一次，再同状态重复两次。
		if _, err := st.UpdateFeedbackStatus(ctx, fbB.FeedbackID, "received", strPtr("首次受理")); err != nil {
			t.Fatalf("受理失败: %v", err)
		}
		if _, err := st.UpdateFeedbackStatus(ctx, fbB.FeedbackID, "received", strPtr("不该写入")); err != nil {
			t.Fatalf("同状态应幂等，实际 %v", err)
		}
		item, err := st.UpdateFeedbackStatus(ctx, fbB.FeedbackID, "received", nil)
		if err != nil {
			t.Fatalf("同状态第二次应幂等，实际 %v", err)
		}
		if item.InternalNote == nil || *item.InternalNote != "首次受理" {
			t.Fatalf("幂等 no-op 不应覆盖备注: %+v", item.InternalNote)
		}
		var after struct {
			updatedAt time.Time
			events    int
		}
		if err := cleanupDB.QueryRow(`SELECT updated_at, (SELECT count(*) FROM feedback_status_events WHERE feedback_id = $1) FROM feedback WHERE id = $1`, fbB.FeedbackID).
			Scan(&after.updatedAt, &after.events); err != nil {
			t.Fatalf("读取后置状态失败: %v", err)
		}
		if after.events != before.events+1 {
			t.Fatalf("审计应只增加 1 条，before=%d after=%d", before.events, after.events)
		}
		if after.updatedAt.Equal(before.updatedAt) {
			t.Fatalf("真实受理应更新 updatedAt")
		}
	})

	t.Run("审计记录可查", func(t *testing.T) {
		rows, err := cleanupDB.Query(`SELECT actor_type, actor_id, from_status, to_status, COALESCE(note, '')
			FROM feedback_status_events WHERE feedback_id = $1 ORDER BY created_at, id`, fbA2.FeedbackID)
		if err != nil {
			t.Fatalf("查询审计失败: %v", err)
		}
		defer rows.Close()
		type event struct{ actorType, actorID, from, to, note string }
		var events []event
		for rows.Next() {
			var e event
			if err := rows.Scan(&e.actorType, &e.actorID, &e.from, &e.to, &e.note); err != nil {
				t.Fatal(err)
			}
			events = append(events, e)
		}
		want := []event{
			{"admin", "admin", "submitted", "received", "已复现，转客户端组"},
			{"admin", "admin", "received", "submitted", ""},
			{"admin", "admin", "submitted", "closed", "用户已确认"},
		}
		if len(events) != len(want) {
			t.Fatalf("审计条数=%d，期望 %d: %+v", len(events), len(want), events)
		}
		for i := range want {
			if events[i] != want[i] {
				t.Fatalf("审计[%d]=%+v，期望 %+v", i, events[i], want[i])
			}
		}

		// 用户关闭的反馈也要留痕，actor 为本人 userID。
		var actorType, actorID, from, to string
		if err := cleanupDB.QueryRow(`SELECT actor_type, actor_id, from_status, to_status FROM feedback_status_events
			WHERE feedback_id = $1 ORDER BY created_at DESC LIMIT 1`, fbA1.FeedbackID).
			Scan(&actorType, &actorID, &from, &to); err != nil {
			t.Fatalf("查询用户审计失败: %v", err)
		}
		if actorType != "user" || actorID != userA || from != "submitted" || to != "closed" {
			t.Fatalf("用户审计异常: %s %s %s->%s", actorType, actorID, from, to)
		}
	})

	t.Run("运营列表跨用户并按状态过滤", func(t *testing.T) {
		page, err := st.ListAdminFeedback(ctx, "submitted", "", 20)
		if err != nil {
			t.Fatal(err)
		}
		seen := map[string]string{}
		for _, item := range page.Items {
			seen[item.FeedbackID] = item.UserID
			if item.Status != "submitted" {
				t.Fatalf("状态过滤失效: %+v", item)
			}
		}
		if _, ok := seen[fbA1.FeedbackID]; ok {
			t.Fatalf("已关闭的反馈不应出现在 submitted 过滤中")
		}
		if _, ok := seen[fbB.FeedbackID]; ok {
			t.Fatalf("已受理的反馈不应出现在 submitted 过滤中")
		}

		closedPage, err := st.ListAdminFeedback(ctx, "closed", "", 20)
		if err != nil {
			t.Fatal(err)
		}
		users := map[string]bool{}
		for _, item := range closedPage.Items {
			users[item.UserID] = true
		}
		if !users[userA] {
			t.Fatalf("closed 过滤应包含用户 A: %+v", closedPage.Items)
		}

		all, err := st.ListAdminFeedback(ctx, "", "", 50)
		if err != nil {
			t.Fatal(err)
		}
		if len(all.Items) < 3 {
			t.Fatalf("跨用户列表条数不足: %d", len(all.Items))
		}
		// 只针对本测试创建的三条断言个人信息可见，避免受同库其他测试数据干扰。
		own := map[string]bool{fbA1.FeedbackID: true, fbA2.FeedbackID: true, fbB.FeedbackID: true}
		for _, item := range all.Items {
			if !own[item.FeedbackID] {
				continue
			}
			if item.Contact == nil || item.Diagnostics == nil {
				t.Fatalf("运营视图缺少个人信息/诊断: %+v", item)
			}
		}
	})

	t.Run("HTTP admin 入口未配置 404、凭据错误 401", func(t *testing.T) {
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		base := config.Config{Version: "test", CORSAllowedOrigins: []string{"*"}}

		disabled := httpapi.New(base, st, logger, nil)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/feedback", nil)
		disabled.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("未配置 ADMIN_TOKEN 应 404，实际 %d", rec.Code)
		}

		base.AdminToken = "pg-admin-" + uuid.NewString()
		enabled := httpapi.New(base, st, logger, nil)
		rec = httptest.NewRecorder()
		req = httptest.NewRequest(http.MethodGet, "/api/v1/admin/feedback", nil)
		req.Header.Set("X-Admin-Token", "wrong")
		enabled.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("错误 admin 凭据应 401，实际 %d", rec.Code)
		}
		rec = httptest.NewRecorder()
		req = httptest.NewRequest(http.MethodGet, "/api/v1/admin/feedback", nil)
		req.Header.Set("X-Admin-Token", base.AdminToken)
		enabled.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("正确 admin 凭据应 200，实际 %d body=%s", rec.Code, rec.Body.String())
		}
	})
}

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

		page, err := st.ListUserFeedback(ctx, userA, "", "", 20)
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
		page, err := st.ListUserFeedback(ctx, userA, "", "", 20)
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
		page, err := st.ListUserFeedback(ctx, userB, "", "", 20)
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
		pageA, err := st.ListUserFeedback(ctx, userA, "", "", 20)
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
		pageB, err := st.ListUserFeedback(ctx, userB, "", "", 20)
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
		page, err := st.ListUserFeedback(ctx, userA, "", "", 2)
		if err != nil {
			t.Fatal(err)
		}
		if !page.HasMore || page.NextCursor == "" || len(page.Items) != 2 {
			t.Fatalf("首页异常: %+v", page)
		}
		for _, item := range page.Items {
			seen[item.FeedbackID] = true
		}
		page2, err := st.ListUserFeedback(ctx, userA, "", page.NextCursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		if page2.HasMore || len(page2.Items) != 1 {
			t.Fatalf("第二页异常: %+v", page2)
		}
		if seen[page2.Items[0].FeedbackID] {
			t.Fatalf("分页重复返回同一条")
		}

		if _, err := st.ListUserFeedback(ctx, userA, "", "not-a-cursor", 20); !errors.Is(err, store.ErrBadCursor) {
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
