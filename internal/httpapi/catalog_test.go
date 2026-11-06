package httpapi_test

import (
	"net/http"
	"testing"

	"github.com/platojobs/PJGoIoT/internal/config"
)

type catalogMetric struct {
	Key                string `json:"key"`
	Unit               string `json:"unit"`
	CharacteristicUUID string `json:"characteristicUuid"`
}

type catalogDeviceType struct {
	Type        string          `json:"type"`
	DisplayName string          `json:"displayName"`
	Description string          `json:"description"`
	Metrics     []catalogMetric `json:"metrics"`
	Limitations []string        `json:"limitations"`
	UpdatedAt   string          `json:"updatedAt"`
}

type catalogResponse struct {
	Version     int64               `json:"version"`
	GeneratedAt string              `json:"generatedAt"`
	Items       []catalogDeviceType `json:"items"`
}

// TestCatalogAnonymousReadAndCache 验证匿名可读、带 token 不被拒，以及 ETag / Last-Modified 304。
func TestCatalogAnonymousReadAndCache(t *testing.T) {
	h := newFeedbackHandler(t, config.Config{})

	anon := do(t, h, http.MethodGet, "/api/v1/catalog/device-types", "", "", nil)
	if anon.Code != http.StatusOK {
		t.Fatalf("匿名读取 status=%d body=%s", anon.Code, anon.Body.String())
	}
	etag := anon.Header().Get("ETag")
	if etag != `"catalog-v1"` {
		t.Fatalf("ETag=%q", etag)
	}
	lastModified := anon.Header().Get("Last-Modified")
	if lastModified == "" {
		t.Fatal("缺少 Last-Modified")
	}
	if anon.Header().Get("Cache-Control") == "" {
		t.Fatal("缺少 Cache-Control")
	}
	body := decode[catalogResponse](t, anon)
	if len(body.Items) != 5 {
		t.Fatalf("目录项=%d，期望 5", len(body.Items))
	}
	// 至少一项必须带可解析指标与来源特征 UUID，客户端才能订阅。
	for _, item := range body.Items {
		if item.Type == "environmental_sensor" {
			if len(item.Metrics) != 3 {
				t.Fatalf("environmental_sensor metrics=%d", len(item.Metrics))
			}
			for _, m := range item.Metrics {
				if m.Key == "" || m.Unit == "" || m.CharacteristicUUID == "" {
					t.Fatalf("指标字段不完整: %+v", m)
				}
			}
		}
	}

	// 有效 token 只用于记录访问，不应改变可读性。
	anonUser := createAnonymous(t, h)
	withToken := do(t, h, http.MethodGet, "/api/v1/catalog/device-types", "", anonUser.Token, nil)
	if withToken.Code != http.StatusOK {
		t.Fatalf("带 token 读取 status=%d", withToken.Code)
	}
	// 无效/过期 token 也必须按匿名放行，不能因为可选鉴权把用户挡在门外。
	badToken := do(t, h, http.MethodGet, "/api/v1/catalog/device-types", "", "not-a-real-token", nil)
	if badToken.Code != http.StatusOK {
		t.Fatalf("无效 token 读取 status=%d body=%s", badToken.Code, badToken.Body.String())
	}

	if got := do(t, h, http.MethodGet, "/api/v1/catalog/device-types", "", "", map[string]string{"If-None-Match": etag}); got.Code != http.StatusNotModified {
		t.Fatalf("If-None-Match 命中 status=%d", got.Code)
	}
	if got := do(t, h, http.MethodGet, "/api/v1/catalog/device-types", "", "", map[string]string{"If-None-Match": `W/` + etag}); got.Code != http.StatusNotModified {
		t.Fatalf("弱 ETag 未命中 304，status=%d", got.Code)
	}
	if got := do(t, h, http.MethodGet, "/api/v1/catalog/device-types", "", "", map[string]string{"If-Modified-Since": lastModified}); got.Code != http.StatusNotModified {
		t.Fatalf("If-Modified-Since 命中 status=%d", got.Code)
	}
	if got := do(t, h, http.MethodGet, "/api/v1/catalog/device-types", "", "", map[string]string{"If-None-Match": `"catalog-v0"`}); got.Code != http.StatusOK {
		t.Fatalf("不匹配的 ETag 应返回 200，status=%d", got.Code)
	}
}

// TestAdminCatalogDisabledReturns404 验证未配置 ADMIN_TOKEN 时运营入口与不存在路由不可区分。
func TestAdminCatalogDisabledReturns404(t *testing.T) {
	h := newFeedbackHandler(t, config.Config{})
	body := validCatalogBody()
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		rec := do(t, h, method, "/api/v1/admin/catalog/device-types/environmental_sensor", body, "", nil)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s 未配置 admin token 时 status=%d body=%s", method, rec.Code, rec.Body.String())
		}
	}
}

// TestAdminCatalogUpdateAndDelete 验证管理员鉴权、更新后 ETag 变化、软删与非法类型 404。
func TestAdminCatalogUpdateAndDelete(t *testing.T) {
	h := newFeedbackHandler(t, config.Config{AdminToken: "ops-secret"})
	path := "/api/v1/admin/catalog/device-types/environmental_sensor"
	body := validCatalogBody()

	unauthorized := do(t, h, http.MethodPut, path, body, "", nil)
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("缺少 admin token status=%d", unauthorized.Code)
	}
	wrong := do(t, h, http.MethodPut, path, body, "", map[string]string{"X-Admin-Token": "wrong"})
	if wrong.Code != http.StatusUnauthorized {
		t.Fatalf("错误 admin token status=%d", wrong.Code)
	}

	ok := do(t, h, http.MethodPut, path, body, "", map[string]string{"X-Admin-Token": "ops-secret"})
	if ok.Code != http.StatusOK {
		t.Fatalf("更新 status=%d body=%s", ok.Code, ok.Body.String())
	}
	updatedEtag := ok.Header().Get("ETag")
	if updatedEtag == `"catalog-v1"` || updatedEtag == "" {
		t.Fatalf("更新后 ETag 未变化: %q", updatedEtag)
	}
	updated := decode[catalogDeviceType](t, ok)
	if updated.DisplayName != "环境传感器（运营更新）" {
		t.Fatalf("displayName=%q", updated.DisplayName)
	}

	// 目录整体 ETag 必须随之变化，且新 ETag 可命中 304。
	list := do(t, h, http.MethodGet, "/api/v1/catalog/device-types", "", "", nil)
	if list.Header().Get("ETag") != updatedEtag {
		t.Fatalf("列表 ETag=%q，更新响应 ETag=%q", list.Header().Get("ETag"), updatedEtag)
	}
	if got := do(t, h, http.MethodGet, "/api/v1/catalog/device-types", "", "", map[string]string{"If-None-Match": updatedEtag}); got.Code != http.StatusNotModified {
		t.Fatalf("新 ETag 未命中 304，status=%d", got.Code)
	}
	var after = decode[catalogResponse](t, list)
	for _, item := range after.Items {
		if item.Type == "environmental_sensor" && item.DisplayName != "环境传感器（运营更新）" {
			t.Fatalf("目录未反映更新: %q", item.DisplayName)
		}
	}

	// 非法类型（不存在）返回 404。
	missing := do(t, h, http.MethodPut, "/api/v1/admin/catalog/device-types/not_a_type", body, "", map[string]string{"X-Admin-Token": "ops-secret"})
	if missing.Code != http.StatusNotFound {
		t.Fatalf("不存在类型 status=%d body=%s", missing.Code, missing.Body.String())
	}

	del := do(t, h, http.MethodDelete, path, "", "", map[string]string{"X-Admin-Token": "ops-secret"})
	if del.Code != http.StatusNoContent {
		t.Fatalf("删除 status=%d body=%s", del.Code, del.Body.String())
	}
	if del.Header().Get("ETag") == updatedEtag {
		t.Fatalf("删除后 ETag 未变化: %q", del.Header().Get("ETag"))
	}
	finalList := decode[catalogResponse](t, do(t, h, http.MethodGet, "/api/v1/catalog/device-types", "", "", nil))
	if len(finalList.Items) != 4 {
		t.Fatalf("软删后目录项=%d，期望 4", len(finalList.Items))
	}
	for _, item := range finalList.Items {
		if item.Type == "environmental_sensor" {
			t.Fatal("软删项仍出现在目录中")
		}
	}
	// 已删除的项不能再次更新。
	if again := do(t, h, http.MethodPut, path, body, "", map[string]string{"X-Admin-Token": "ops-secret"}); again.Code != http.StatusNotFound {
		t.Fatalf("已软删项再次更新 status=%d", again.Code)
	}
	// DELETE 幂等性不做承诺：第二次删除同样是 404。
	if again := do(t, h, http.MethodDelete, path, "", "", map[string]string{"X-Admin-Token": "ops-secret"}); again.Code != http.StatusNotFound {
		t.Fatalf("已软删项再次删除 status=%d", again.Code)
	}
}

func validCatalogBody() string {
	return `{"displayName":"环境传感器（运营更新）","description":"更新后的说明",` +
		`"gattServices":[{"uuid":"0000181a-0000-1000-8000-00805f9b34fb","name":"Environmental Sensing"}],` +
		`"metrics":[{"key":"temperature_celsius","unit":"celsius","characteristicUuid":"00002a6e-0000-1000-8000-00805f9b34fb"}],` +
		`"limitations":["仅解析标准特征"],` +
		`"compatibility":{"platforms":["android","ios"],"protocol":"ble-gatt"}}`
}

func createAnonymous(t *testing.T, h http.Handler) anonResponse {
	t.Helper()
	rec := do(t, h, http.MethodPost, "/api/v1/auth/anonymous", "", "", nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("创建匿名用户失败: status=%d body=%s", rec.Code, rec.Body.String())
	}
	return decode[anonResponse](t, rec)
}
