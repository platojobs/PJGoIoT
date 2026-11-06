package httpapi_test

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/platojobs/PJGoIoT/internal/config"
	"github.com/platojobs/PJGoIoT/internal/httpapi"
	"github.com/platojobs/PJGoIoT/internal/model"
	"github.com/platojobs/PJGoIoT/internal/store/fixture"
)

// TestScenesHTTP 覆盖场景接口的鉴权、字段级 LWW 与错误码矩阵（fixture 存储）。
func TestScenesHTTP(t *testing.T) {
	st, err := fixture.New("../../fixtures")
	if err != nil {
		t.Fatal(err)
	}
	h := newHandlerWithStore(t, config.Config{Version: "test"}, st)
	owner := newUser(t, h)
	other := newUser(t, h)

	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	body := pushBody(t, "["+deviceJSON("11111111-1111-4111-8111-111111111111", "AA:BB:CC:00:00:01", base)+"]", nil)
	if rec := do(t, h, http.MethodPost, "/api/v1/sync/push", body, owner.Token, nil); rec.Code != http.StatusOK {
		t.Fatalf("push status=%d body=%s", rec.Code, rec.Body.String())
	}

	// 未认证：所有场景路径都必须 401。
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/scenes"},
		{http.MethodPost, "/api/v1/scenes"},
		{http.MethodGet, "/api/v1/scenes/no-such"},
		{http.MethodPut, "/api/v1/scenes/no-such"},
		{http.MethodDelete, "/api/v1/scenes/no-such"},
	} {
		if rec := do(t, h, tc.method, tc.path, "", "", nil); rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s 未认证 status=%d", tc.method, tc.path, rec.Code)
		}
	}

	createBody := `{"name":"回家提醒","trigger":{"kind":"device_offline","device":{"platform":"android","externalId":"AA:BB:CC:00:00:01"},"offlineSeconds":120},"actions":[{"kind":"notify","title":"设备离线","body":"{{alias}} 已离线"},{"kind":"log","message":"离线告警","level":"warn"}]}`
	rec := do(t, h, http.MethodPost, "/api/v1/scenes", createBody, owner.Token, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("创建场景 status=%d body=%s", rec.Code, rec.Body.String())
	}
	created := decode[model.Scene](t, rec)
	if created.ID == "" || created.Version != 1 || created.DeviceMissing || created.CooldownSeconds != 300 ||
		created.Trigger.Kind != "device_offline" || len(created.Actions) != 2 {
		t.Fatalf("创建响应异常: %+v", created)
	}

	list := decode[sceneListResp](t, do(t, h, http.MethodGet, "/api/v1/scenes", "", owner.Token, nil))
	if len(list.Items) != 1 || list.Items[0].ID != created.ID {
		t.Fatalf("列表异常: %+v", list.Items)
	}
	if rec := do(t, h, http.MethodGet, "/api/v1/scenes/"+created.ID, "", other.Token, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("跨用户读取 status=%d", rec.Code)
	}

	// 两个字段各自更新，字段级 LWW 不互相覆盖。
	ts1 := time.Now().UTC().Add(1 * time.Second).Format(time.RFC3339Nano)
	ts2 := time.Now().UTC().Add(2 * time.Second).Format(time.RFC3339Nano)
	rec = do(t, h, http.MethodPut, "/api/v1/scenes/"+created.ID,
		fmt.Sprintf(`{"name":{"value":"晨间提醒","updatedAt":%q},"enabled":{"value":false,"updatedAt":%q}}`, ts1, ts2),
		owner.Token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("字段更新 status=%d body=%s", rec.Code, rec.Body.String())
	}
	updated := decode[model.Scene](t, rec)
	if updated.Name != "晨间提醒" || updated.Enabled || updated.Version != 2 {
		t.Fatalf("字段更新结果异常: %+v", updated)
	}

	// 过期 name + 更新的 cooldown：只应用 cooldown。
	rec = do(t, h, http.MethodPut, "/api/v1/scenes/"+created.ID,
		fmt.Sprintf(`{"name":{"value":"旧名字","updatedAt":%q},"cooldownSeconds":{"value":60,"updatedAt":%q}}`, ts1, ts2),
		owner.Token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("混合更新 status=%d body=%s", rec.Code, rec.Body.String())
	}
	mixed := decode[model.Scene](t, rec)
	if mixed.Name != "晨间提醒" || mixed.CooldownSeconds != 60 || mixed.Version != 3 {
		t.Fatalf("混合更新结果异常: %+v", mixed)
	}

	// 非法组合矩阵。
	cases := []struct {
		name string
		body string
		code string
	}{
		{"空名称", `{"name":"  ","trigger":{"kind":"schedule","timeOfDay":"08:00"},"actions":[{"kind":"log","message":"x"}]}`, httpapi.CodeSceneNameInvalid},
		{"冷却过短", `{"name":"场景","cooldownSeconds":10,"trigger":{"kind":"schedule","timeOfDay":"08:00"},"actions":[{"kind":"log","message":"x"}]}`, httpapi.CodeSceneCooldownOutOfRange},
		{"RSSI 越界", `{"name":"场景","trigger":{"kind":"rssi_below","device":{"platform":"android","externalId":"AA:BB:CC:00:00:01"},"rssiThreshold":20},"actions":[{"kind":"log","message":"x"}]}`, httpapi.CodeSceneTriggerInvalid},
		{"比较符非法", `{"name":"场景","trigger":{"kind":"metric_threshold","device":{"platform":"android","externalId":"AA:BB:CC:00:00:01"},"deviceType":"environmental_sensor","metric":{"metric":"temperature_celsius","comparator":"eq","threshold":20,"consecutive":3}},"actions":[{"kind":"log","message":"x"}]}`, httpapi.CodeSceneTriggerInvalid},
		{"连续条数超限", `{"name":"场景","trigger":{"kind":"metric_threshold","device":{"platform":"android","externalId":"AA:BB:CC:00:00:01"},"deviceType":"environmental_sensor","metric":{"metric":"temperature_celsius","comparator":"lt","threshold":20,"consecutive":31}},"actions":[{"kind":"log","message":"x"}]}`, httpapi.CodeSceneTriggerInvalid},
		{"定时多余字段", `{"name":"场景","trigger":{"kind":"schedule","timeOfDay":"08:00","rssiThreshold":-70},"actions":[{"kind":"log","message":"x"}]}`, httpapi.CodeSceneTriggerInvalid},
		{"设备不存在", `{"name":"场景","trigger":{"kind":"device_offline","device":{"platform":"android","externalId":"AA:BB:CC:00:00:99"}},"actions":[{"kind":"log","message":"x"}]}`, httpapi.CodeSceneDeviceNotFound},
		{"指标不被支持", `{"name":"场景","trigger":{"kind":"metric_threshold","device":{"platform":"android","externalId":"AA:BB:CC:00:00:01"},"deviceType":"smart_scale","metric":{"metric":"temperature_celsius","comparator":"lt","threshold":20,"consecutive":3}},"actions":[{"kind":"log","message":"x"}]}`, httpapi.CodeSceneMetricNotSupported},
		{"设备类型不存在", `{"name":"场景","trigger":{"kind":"metric_threshold","device":{"platform":"android","externalId":"AA:BB:CC:00:00:01"},"deviceType":"unknown_type","metric":{"metric":"temperature_celsius","comparator":"lt","threshold":20,"consecutive":3}},"actions":[{"kind":"log","message":"x"}]}`, httpapi.CodeSceneMetricNotSupported},
		{"动作为空", `{"name":"场景","trigger":{"kind":"schedule","timeOfDay":"08:00"},"actions":[]}`, httpapi.CodeSceneActionInvalid},
		{"动作重复", `{"name":"场景","trigger":{"kind":"schedule","timeOfDay":"08:00"},"actions":[{"kind":"log","message":"a"},{"kind":"log","message":"b"}]}`, httpapi.CodeSceneActionInvalid},
		{"通知缺正文", `{"name":"场景","trigger":{"kind":"schedule","timeOfDay":"08:00"},"actions":[{"kind":"notify","title":"t"}]}`, httpapi.CodeSceneActionInvalid},
	}
	for _, tc := range cases {
		rec := do(t, h, http.MethodPost, "/api/v1/scenes", tc.body, owner.Token, nil)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s status=%d body=%s", tc.name, rec.Code, rec.Body.String())
		}
		if got := decode[errResponse](t, rec); got.Error.Code != tc.code {
			t.Fatalf("%s code=%q want=%q", tc.name, got.Error.Code, tc.code)
		}
	}

	// 补丁缺少 updatedAt / 空补丁：400 BAD_REQUEST。
	rec = do(t, h, http.MethodPut, "/api/v1/scenes/"+created.ID, `{"name":{"value":"x"}}`, owner.Token, nil)
	if rec.Code != http.StatusBadRequest || decode[errResponse](t, rec).Error.Code != httpapi.CodeBadRequest {
		t.Fatalf("缺少 updatedAt status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = do(t, h, http.MethodPut, "/api/v1/scenes/"+created.ID, `{}`, owner.Token, nil)
	if rec.Code != http.StatusBadRequest || decode[errResponse](t, rec).Error.Code != httpapi.CodeBadRequest {
		t.Fatalf("空补丁 status=%d body=%s", rec.Code, rec.Body.String())
	}

	// 软删除：204，随后 GET/DELETE 均 404。
	if rec := do(t, h, http.MethodDelete, "/api/v1/scenes/"+created.ID, "", owner.Token, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("删除 status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := do(t, h, http.MethodGet, "/api/v1/scenes/"+created.ID, "", owner.Token, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("软删后读取 status=%d", rec.Code)
	}
	if rec := do(t, h, http.MethodDelete, "/api/v1/scenes/"+created.ID, "", owner.Token, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("重复删除 status=%d", rec.Code)
	}
	if list := decode[sceneListResp](t, do(t, h, http.MethodGet, "/api/v1/scenes", "", owner.Token, nil)); len(list.Items) != 0 {
		t.Fatalf("软删后列表仍返回: %+v", list.Items)
	}
}

type sceneListResp struct {
	Items []model.Scene `json:"items"`
}
