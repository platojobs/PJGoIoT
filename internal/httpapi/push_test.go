package httpapi_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/platojobs/PJGoIoT/internal/config"
	"github.com/platojobs/PJGoIoT/internal/model"
)

// TestPushDeviceEndpoints 覆盖 POST/DELETE /push/devices：鉴权、幂等注册、
// 参数校验与「停用而非硬删」的语义。
func TestPushDeviceEndpoints(t *testing.T) {
	h := newFeedbackHandler(t, config.Config{})
	user := newUser(t, h)

	body := `{"platform":"android","token":"tok-http","appVersion":"1.0.0","deviceModel":"Pixel"}`
	if rec := do(t, h, http.MethodPost, "/api/v1/push/devices", body, "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("未带 token 应 401，得到 %d", rec.Code)
	}
	first := do(t, h, http.MethodPost, "/api/v1/push/devices", body, user.Token, nil)
	if first.Code != http.StatusOK {
		t.Fatalf("注册设备失败: %d %s", first.Code, first.Body.String())
	}
	device := decode[model.PushRegistration](t, first)
	if device.Status != "active" || device.Platform != "android" {
		t.Fatalf("注册响应异常: %+v", device)
	}

	updated := do(t, h, http.MethodPost, "/api/v1/push/devices",
		`{"platform":"android","token":"tok-http","appVersion":"2.0.0"}`, user.Token, nil)
	if updated.Code != http.StatusOK {
		t.Fatalf("重复注册应 200，得到 %d", updated.Code)
	}
	again := decode[model.PushRegistration](t, updated)
	if again.ID != device.ID || again.AppVersion != "2.0.0" {
		t.Fatalf("同一 token 应幂等更新同一条记录: first=%+v again=%+v", device, again)
	}

	for _, tc := range []struct{ name, body string }{
		{"非法平台", `{"platform":"web","token":"tok-http"}`},
		{"空 token", `{"platform":"ios","token":""}`},
	} {
		rec := do(t, h, http.MethodPost, "/api/v1/push/devices", tc.body, user.Token, nil)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s 应 400，得到 %d %s", tc.name, rec.Code, rec.Body.String())
		}
	}

	other := newUser(t, h)
	if rec := do(t, h, http.MethodDelete, "/api/v1/push/devices/"+device.ID, "", other.Token, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("跨用户删除应 404，得到 %d", rec.Code)
	}
	if rec := do(t, h, http.MethodDelete, "/api/v1/push/devices/"+device.ID, "", user.Token, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("本人删除应 204，得到 %d", rec.Code)
	}
	// 已停用时幂等成功，仍然不是硬删。
	if rec := do(t, h, http.MethodDelete, "/api/v1/push/devices/"+device.ID, "", user.Token, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("重复删除应幂等 204，得到 %d", rec.Code)
	}
}

// TestAdminAlertEndpoint 覆盖运营侧告警入口：鉴权、参数校验与投递条数。
func TestAdminAlertEndpoint(t *testing.T) {
	h := newFeedbackHandler(t, config.Config{AdminToken: "ops-secret"})
	user := newUser(t, h)

	rec := do(t, h, http.MethodPost, "/api/v1/push/devices",
		`{"platform":"android","token":"tok-alert"}`, user.Token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("注册设备失败: %d %s", rec.Code, rec.Body.String())
	}

	alertBody := `{"userId":"` + user.UserID + `","kind":"device_offline","title":"设备离线","body":"客厅传感器离线","severity":"warning","payload":{"externalId":"AA:BB"}}`
	if rec := do(t, h, http.MethodPost, "/api/v1/admin/alerts", alertBody, "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("缺少 X-Admin-Token 应 401，得到 %d", rec.Code)
	}
	if rec := do(t, h, http.MethodPost, "/api/v1/admin/alerts", alertBody, "", map[string]string{"X-Admin-Token": "wrong"}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("错误 X-Admin-Token 应 401，得到 %d", rec.Code)
	}

	created := do(t, h, http.MethodPost, "/api/v1/admin/alerts", alertBody, "", map[string]string{"X-Admin-Token": "ops-secret"})
	if created.Code != http.StatusCreated {
		t.Fatalf("创建告警失败: %d %s", created.Code, created.Body.String())
	}
	resp := decode[struct {
		Alert         model.Alert `json:"alert"`
		DeliveryCount int         `json:"deliveryCount"`
	}](t, created)
	if resp.DeliveryCount != 1 || resp.Alert.Severity != "warning" {
		t.Fatalf("告警响应异常: %+v", resp)
	}

	for _, tc := range []struct{ name, body string }{
		{"未知用户", `{"userId":"00000000-0000-0000-0000-000000000000","kind":"x","title":"t","body":"b"}`},
		{"非法 severity", `{"userId":"` + user.UserID + `","kind":"x","title":"t","body":"b","severity":"fatal"}`},
		{"空 title", `{"userId":"` + user.UserID + `","kind":"x","title":"","body":"b"}`},
	} {
		rec := do(t, h, http.MethodPost, "/api/v1/admin/alerts", tc.body, "", map[string]string{"X-Admin-Token": "ops-secret"})
		if tc.name == "未知用户" {
			if rec.Code != http.StatusNotFound {
				t.Fatalf("%s 应 404，得到 %d %s", tc.name, rec.Code, rec.Body.String())
			}
			continue
		}
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s 应 400，得到 %d %s", tc.name, rec.Code, rec.Body.String())
		}
	}

	// 未配置 ADMIN_TOKEN：路由整体 404，不暴露接口存在性。
	noAdmin := newFeedbackHandler(t, config.Config{})
	if rec := do(t, noAdmin, http.MethodPost, "/api/v1/admin/alerts", alertBody, "", map[string]string{"X-Admin-Token": "any"}); rec.Code != http.StatusNotFound {
		t.Fatalf("未配置 ADMIN_TOKEN 应 404，得到 %d", rec.Code)
	}
}

type scenePageResponse struct {
	Items      []model.Scene `json:"items"`
	NextCursor *string       `json:"nextCursor"`
	HasMore    *bool         `json:"hasMore"`
}

// TestScenesPagination 覆盖 /scenes 的可选分页：不传参数保持全量、limit 与游标可用、
// 非法参数 400。场景使用 schedule 触发条件，不依赖设备。
func TestScenesPagination(t *testing.T) {
	h := newFeedbackHandler(t, config.Config{})
	user := newUser(t, h)

	ids := make([]string, 0, 3)
	for i := 0; i < 3; i++ {
		body := `{"name":"场景` + string(rune('A'+i)) + `","trigger":{"kind":"schedule","timeOfDay":"08:0` +
			string(rune('0'+i)) + `"},"actions":[{"kind":"log","message":"m"}]}`
		rec := do(t, h, http.MethodPost, "/api/v1/scenes", body, user.Token, nil)
		if rec.Code != http.StatusCreated {
			t.Fatalf("创建场景失败: %d %s", rec.Code, rec.Body.String())
		}
		ids = append(ids, decode[model.Scene](t, rec).ID)
	}

	full := do(t, h, http.MethodGet, "/api/v1/scenes", "", user.Token, nil)
	if full.Code != http.StatusOK {
		t.Fatalf("全量列表失败: %d", full.Code)
	}
	if strings.Contains(full.Body.String(), "nextCursor") || strings.Contains(full.Body.String(), "hasMore") {
		t.Fatalf("不传 limit 时不应出现分页字段: %s", full.Body.String())
	}
	all := decode[scenePageResponse](t, full)
	if len(all.Items) != 3 {
		t.Fatalf("不传 limit 应返回全量 3 条，得到 %d", len(all.Items))
	}

	cursor := ""
	seen := map[string]bool{}
	for page := 0; page < 4; page++ {
		path := "/api/v1/scenes?limit=1"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		rec := do(t, h, http.MethodGet, path, "", user.Token, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("分页请求失败: %d %s", rec.Code, rec.Body.String())
		}
		res := decode[scenePageResponse](t, rec)
		if len(res.Items) != 1 {
			t.Fatalf("limit=1 每页应 1 条，得到 %d", len(res.Items))
		}
		if res.HasMore == nil {
			t.Fatal("分页响应缺少 hasMore")
		}
		if seen[res.Items[0].ID] {
			t.Fatalf("游标遍历出现重复: %v", res.Items[0].ID)
		}
		seen[res.Items[0].ID] = true
		if !*res.HasMore {
			break
		}
		if res.NextCursor == nil || *res.NextCursor == "" {
			t.Fatal("hasMore=true 时必须返回 nextCursor")
		}
		cursor = *res.NextCursor
	}
	if len(seen) != 3 {
		t.Fatalf("游标遍历应覆盖 3 条，得到 %d", len(seen))
	}
	for _, id := range ids {
		if !seen[id] {
			t.Fatalf("游标遍历缺少场景 %s", id)
		}
	}

	for _, path := range []string{
		"/api/v1/scenes?limit=0",
		"/api/v1/scenes?limit=abc",
		"/api/v1/scenes?limit=1&cursor=not-a-cursor",
	} {
		if rec := do(t, h, http.MethodGet, path, "", user.Token, nil); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s 应 400，得到 %d %s", path, rec.Code, rec.Body.String())
		}
	}
}
