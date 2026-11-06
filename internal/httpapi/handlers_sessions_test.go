package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/platojobs/PJGoIoT/internal/config"
	"github.com/platojobs/PJGoIoT/internal/model"
	"github.com/platojobs/PJGoIoT/internal/store/fixture"
)

type sessionResp struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"createdAt"`
	ExpiresAt time.Time `json:"expiresAt"`
	Current   bool      `json:"current"`
}

type sessionListResp struct {
	Sessions []sessionResp `json:"sessions"`
}

func listSessions(t *testing.T, h http.Handler, token string) sessionListResp {
	t.Helper()
	rec := do(t, h, http.MethodGet, "/api/v1/sessions", "", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("列出会话 status=%d body=%s", rec.Code, rec.Body.String())
	}
	return decode[sessionListResp](t, rec)
}

// TestTokenRefreshRotation 覆盖 v0.22.0 的 token 滚动刷新语义。
func TestTokenRefreshRotation(t *testing.T) {
	h := newHandler(t, 0)
	user := newUser(t, h)

	rec := do(t, h, http.MethodPost, "/api/v1/auth/refresh", "", user.Token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("刷新 status=%d body=%s", rec.Code, rec.Body.String())
	}
	var refreshed claimResp
	if err := json.Unmarshal(rec.Body.Bytes(), &refreshed); err != nil {
		t.Fatalf("解析刷新响应失败: %v body=%s", err, rec.Body.String())
	}
	if refreshed.UserID != user.UserID {
		t.Fatalf("刷新 userId=%s want=%s", refreshed.UserID, user.UserID)
	}
	if refreshed.Token == "" || refreshed.Token == user.Token {
		t.Fatalf("刷新应返回新 token: %q", refreshed.Token)
	}
	if d := time.Until(refreshed.ExpiresAt); d < 179*24*time.Hour || d > 181*24*time.Hour {
		t.Fatalf("刷新后 TTL 异常: %v", d)
	}

	// 旧 token 立即失效，且不能靠再次刷新复活。
	if rec := do(t, h, http.MethodGet, "/api/v1/my/devices", "", user.Token, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("旧 token 应 401: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := do(t, h, http.MethodPost, "/api/v1/auth/refresh", "", user.Token, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("旧 token 刷新应 401: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// 新 token 可用，并可继续滚动刷新。
	if rec := do(t, h, http.MethodGet, "/api/v1/my/devices", "", refreshed.Token, nil); rec.Code != http.StatusOK {
		t.Fatalf("新 token 不可用: %d %s", rec.Code, rec.Body.String())
	}
	rec = do(t, h, http.MethodPost, "/api/v1/auth/refresh", "", refreshed.Token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("二次刷新 status=%d body=%s", rec.Code, rec.Body.String())
	}
	var second claimResp
	if err := json.Unmarshal(rec.Body.Bytes(), &second); err != nil {
		t.Fatal(err)
	}
	if second.Token == refreshed.Token {
		t.Fatal("二次刷新应再次轮换 token")
	}
	if rec := do(t, h, http.MethodGet, "/api/v1/my/devices", "", refreshed.Token, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("被轮换的旧 token 应 401: %d", rec.Code)
	}
	if rec := do(t, h, http.MethodGet, "/api/v1/my/devices", "", second.Token, nil); rec.Code != http.StatusOK {
		t.Fatalf("最新 token 不可用: %d %s", rec.Code, rec.Body.String())
	}
}

// TestSessionListAndRevoke 覆盖会话列表、单会话吊销、跨用户 404 与幂等吊销。
func TestSessionListAndRevoke(t *testing.T) {
	h := newHandler(t, 0)
	user := newUser(t, h)
	other := newUser(t, h)

	// 通过配对码拿到同账号的第二个 token（原 token 保持有效）。
	code := createPairingCode(t, h, user.Token)
	status, body := claimPairingCode(t, h, code.Code)
	if status != http.StatusOK {
		t.Fatalf("配对 claim status=%d body=%s", status, body)
	}
	var claimed claimResp
	if err := json.Unmarshal(body, &claimed); err != nil {
		t.Fatal(err)
	}

	list := listSessions(t, h, user.Token)
	if len(list.Sessions) != 2 {
		t.Fatalf("会话数=%d want=2: %+v", len(list.Sessions), list.Sessions)
	}
	var current, second sessionResp
	for _, s := range list.Sessions {
		if s.ID == "" {
			t.Fatal("会话 id 为空")
		}
		if s.Current {
			current = s
		} else {
			second = s
		}
	}
	if current.ID == "" || second.ID == "" || current.ID == second.ID {
		t.Fatalf("current 标记异常: %+v", list.Sessions)
	}
	if current.ExpiresAt.IsZero() || current.CreatedAt.IsZero() {
		t.Fatalf("会话时间缺失: %+v", current)
	}

	// 跨用户吊销他人会话必须 404，且不泄露存在性。
	rec := do(t, h, http.MethodDelete, "/api/v1/sessions/"+second.ID, "", other.Token, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("跨用户吊销 status=%d body=%s", rec.Code, rec.Body.String())
	}

	// 吊销指定会话后，该 token 立即 401，且不能被刷新复活。
	rec = do(t, h, http.MethodDelete, "/api/v1/sessions/"+second.ID, "", user.Token, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("吊销会话 status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := do(t, h, http.MethodGet, "/api/v1/my/devices", "", claimed.Token, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("被吊销 token 应 401: %d", rec.Code)
	}
	if rec := do(t, h, http.MethodPost, "/api/v1/auth/refresh", "", claimed.Token, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("被吊销 token 不能刷新复活: %d", rec.Code)
	}
	if list := listSessions(t, h, user.Token); len(list.Sessions) != 1 || !list.Sessions[0].Current {
		t.Fatalf("吊销后会话列表异常: %+v", list.Sessions)
	}

	// 重复吊销幂等 204；不存在的会话 id 404。
	if rec := do(t, h, http.MethodDelete, "/api/v1/sessions/"+second.ID, "", user.Token, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("重复吊销 status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := do(t, h, http.MethodDelete, "/api/v1/sessions/not-a-uuid", "", user.Token, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("非法会话 id status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// TestExportFailureReason 覆盖对账把中断任务标记为 failed 后，GET 能拿到可读原因。
func TestExportFailureReason(t *testing.T) {
	st, err := fixture.New("../../fixtures")
	if err != nil {
		t.Fatal(err)
	}
	h := newHandlerWithStore(t, config.Config{Version: "test"}, st)
	user := newUser(t, h)

	ctx := context.Background()
	pending, err := st.CreateExportTask(ctx, user.UserID, time.Now().UTC().Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	ready, err := st.CreateExportTask(ctx, user.UserID, time.Now().UTC().Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishExportTask(ctx, ready.ID, model.PersonalDataExport{}); err != nil {
		t.Fatal(err)
	}

	// 阈值取未来时刻：所有当前 pending 都视为「已被中断」。
	reason := "导出任务因服务重启中断，请重新发起导出"
	if n, err := st.ReconcileExportTasks(ctx, time.Now().UTC().Add(time.Second), reason); err != nil || n != 1 {
		t.Fatalf("对账条数=%d err=%v", n, err)
	}

	rec := do(t, h, http.MethodGet, "/api/v1/me/export/"+pending.ID, "", user.Token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("查询失败任务 status=%d body=%s", rec.Code, rec.Body.String())
	}
	task := decode[exportTaskResp](t, rec)
	if task.Status != "failed" || task.FailureReason != reason || task.CompletedAt == nil {
		t.Fatalf("失败任务响应异常: %+v", task)
	}
	if task.DownloadURL != "" {
		t.Fatalf("failed 任务不应返回下载链接: %+v", task)
	}

	// ready 任务不受对账影响。
	rec = do(t, h, http.MethodGet, "/api/v1/me/export/"+ready.ID, "", user.Token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("查询 ready 任务 status=%d body=%s", rec.Code, rec.Body.String())
	}
	readyTask := decode[exportTaskResp](t, rec)
	if readyTask.Status != "ready" || readyTask.FailureReason != "" || readyTask.DownloadURL == "" {
		t.Fatalf("ready 任务被对账影响: %+v", readyTask)
	}
}
