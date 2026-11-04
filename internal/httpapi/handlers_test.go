package httpapi_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/platojobs/PJGoIoT/internal/config"
	"github.com/platojobs/PJGoIoT/internal/httpapi"
	"github.com/platojobs/PJGoIoT/internal/store/fixture"
)

func newHandler(t *testing.T, rateLimit int) http.Handler {
	t.Helper()
	st, err := fixture.New("../../fixtures")
	if err != nil {
		t.Fatalf("加载 fixtures 失败: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return httpapi.New(config.Config{
		Version:            "test",
		CORSAllowedOrigins: []string{"*"},
		RateLimitPerMin:    rateLimit,
		RateLimitBurst:     rateLimit,
	}, st, logger)
}

func do(t *testing.T, h http.Handler, method, path, body, token string, headers map[string]string) *httptest.ResponseRecorder {
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
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析响应失败: %v, body=%s", err, rec.Body.String())
	}
	return out
}

type anonResponse struct {
	UserID string `json:"userId"`
	Token  string `json:"token"`
}

type errResponse struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type itemsResponse[T any] struct {
	Items []T `json:"items"`
}

func newUser(t *testing.T, h http.Handler) anonResponse {
	t.Helper()
	rec := do(t, h, http.MethodPost, "/api/v1/auth/anonymous", "", "", nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("创建匿名用户失败: status=%d body=%s", rec.Code, rec.Body.String())
	}
	return decode[anonResponse](t, rec)
}

func TestPublicEndpoints(t *testing.T) {
	h := newHandler(t, 0)

	cases := []struct {
		name       string
		method     string
		path       string
		wantStatus int
		wantCode   string
	}{
		{"健康检查", http.MethodGet, "/api/v1/health", http.StatusOK, ""},
		{"设备类型列表", http.MethodGet, "/api/v1/device-types", http.StatusOK, ""},
		{"设备类型详情", http.MethodGet, "/api/v1/device-types/nordic_uart", http.StatusOK, ""},
		{"未知设备类型详情", http.MethodGet, "/api/v1/device-types/unknown", http.StatusNotFound, httpapi.CodeNotFound},
		{"固件列表", http.MethodGet, "/api/v1/device-types/nordic_uart/firmwares", http.StatusOK, ""},
		{"配件列表", http.MethodGet, "/api/v1/device-types/environmental_sensor/accessories", http.StatusOK, ""},
		{"设备类型聚合", http.MethodGet, "/api/v1/device-types/environmental_sensor/catalog", http.StatusOK, ""},
		{"未知设备类型聚合", http.MethodGet, "/api/v1/device-types/unknown/catalog", http.StatusNotFound, httpapi.CodeNotFound},
		{"公共设备-大写-MAC", http.MethodGet, "/api/v1/devices/AA:BB:CC:00:00:01", http.StatusOK, ""},
		{"公共设备-未知-MAC", http.MethodGet, "/api/v1/devices/aa:bb:cc:00:00:99", http.StatusNotFound, httpapi.CodeNotFound},
		{"公共设备-非法-MAC", http.MethodGet, "/api/v1/devices/not-a-mac", http.StatusBadRequest, httpapi.CodeInvalidMAC},
		{"公共设备-8字节-MAC", http.MethodGet, "/api/v1/devices/aa:bb:cc:dd:ee:ff:00:11", http.StatusBadRequest, httpapi.CodeInvalidMAC},
		{"未知设备类型固件", http.MethodGet, "/api/v1/device-types/unknown/firmwares", http.StatusNotFound, httpapi.CodeNotFound},
		{"未知设备类型配件", http.MethodGet, "/api/v1/device-types/unknown/accessories", http.StatusNotFound, httpapi.CodeNotFound},
		{"未知接口", http.MethodGet, "/api/v1/nope", http.StatusNotFound, httpapi.CodeNotFound},
		{"方法不允许", http.MethodPost, "/api/v1/health", http.StatusMethodNotAllowed, httpapi.CodeMethodNotAllowed},
		{"登录未实现", http.MethodPost, "/api/v1/auth/login", http.StatusNotImplemented, httpapi.CodeNotImplemented},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, h, tc.method, tc.path, "", "", nil)
			if rec.Code != tc.wantStatus {
				t.Fatalf("状态码 = %d, want %d, body=%s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.wantCode != "" {
				body := decode[errResponse](t, rec)
				if body.Error.Code != tc.wantCode {
					t.Fatalf("错误码 = %q, want %q", body.Error.Code, tc.wantCode)
				}
			}
			if rec.Code == http.StatusOK && tc.path != "/api/v1/health" {
				if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
					t.Fatalf("Content-Type = %q", ct)
				}
			}
		})
	}

	t.Run("公共设备结构", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, "/api/v1/devices/AA:BB:CC:00:00:01", "", "", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d", rec.Code)
		}
		var device struct {
			MAC             string `json:"mac"`
			Vendor          string `json:"vendor"`
			DeviceType      string `json:"deviceType"`
			FirmwareVersion string `json:"firmwareVersion"`
			GATTServices    []struct {
				UUID string `json:"uuid"`
			} `json:"gattServices"`
			Compatibility json.RawMessage `json:"compatibility"`
		}
		device = decode[struct {
			MAC             string `json:"mac"`
			Vendor          string `json:"vendor"`
			DeviceType      string `json:"deviceType"`
			FirmwareVersion string `json:"firmwareVersion"`
			GATTServices    []struct {
				UUID string `json:"uuid"`
			} `json:"gattServices"`
			Compatibility json.RawMessage `json:"compatibility"`
		}](t, rec)
		if device.MAC != "aa:bb:cc:00:00:01" {
			t.Fatalf("MAC 未规范化: %q", device.MAC)
		}
		if len(device.GATTServices) == 0 || device.FirmwareVersion == "" {
			t.Fatalf("公共设备字段缺失: %+v", device)
		}
	})

	t.Run("设备类型聚合包含固件与配件", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, "/api/v1/device-types/environmental_sensor/catalog", "", "", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		catalog := decode[struct {
			DeviceType struct {
				Type string `json:"type"`
			} `json:"deviceType"`
			Firmwares   []json.RawMessage `json:"firmwares"`
			Accessories []json.RawMessage `json:"accessories"`
		}](t, rec)
		if catalog.DeviceType.Type != "environmental_sensor" {
			t.Fatalf("deviceType.type=%q", catalog.DeviceType.Type)
		}
		if len(catalog.Firmwares) == 0 || len(catalog.Accessories) == 0 {
			t.Fatalf("聚合结果缺少固件或配件: %s", rec.Body.String())
		}
	})

	t.Run("CORS 预检", func(t *testing.T) {
		rec := do(t, h, http.MethodOptions, "/api/v1/sync/push", "", "", map[string]string{
			"Origin":                        "https://example.com",
			"Access-Control-Request-Method": "POST",
		})
		if rec.Code != http.StatusNoContent {
			t.Fatalf("status=%d", rec.Code)
		}
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
			t.Fatalf("Allow-Origin = %q", got)
		}
	})
}

func TestAuthAndPrivateAccess(t *testing.T) {
	h := newHandler(t, 0)

	t.Run("缺少 token", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, "/api/v1/my/devices", "", "", nil)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status=%d", rec.Code)
		}
		if code := decode[errResponse](t, rec).Error.Code; code != httpapi.CodeUnauthorized {
			t.Fatalf("code=%q", code)
		}
	})

	t.Run("无效 token", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, "/api/v1/my/devices", "", "pjiot_invalid", nil)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status=%d", rec.Code)
		}
	})

	t.Run("匿名用户与空设备列表", func(t *testing.T) {
		user := newUser(t, h)
		if user.UserID == "" || user.Token == "" {
			t.Fatalf("匿名响应缺字段: %+v", user)
		}
		rec := do(t, h, http.MethodGet, "/api/v1/my/devices", "", user.Token, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		items := decode[itemsResponse[json.RawMessage]](t, rec)
		if len(items.Items) != 0 {
			t.Fatalf("新用户设备应为空: %s", rec.Body.String())
		}
	})

	t.Run("注销后 token 失效", func(t *testing.T) {
		user := newUser(t, h)
		rec := do(t, h, http.MethodPost, "/api/v1/auth/logout", "", user.Token, nil)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("logout status=%d", rec.Code)
		}
		rec = do(t, h, http.MethodGet, "/api/v1/my/devices", "", user.Token, nil)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("注销后 status=%d", rec.Code)
		}
	})
}

func TestPushValidation(t *testing.T) {
	h := newHandler(t, 0)
	user := newUser(t, h)
	validTime := time.Now().UTC().Format(time.RFC3339)

	cases := []struct {
		name string
		body string
	}{
		{"非法 JSON", `{"clientId":`},
		{"空设备", `{"clientId":"android-test","devices":[]}`},
		{"非法 UUID", `{"devices":[{"id":"not-uuid","mac":"aa:bb:cc:00:00:01","updatedAt":"` + validTime + `"}]}`},
		{"非法 MAC", `{"devices":[{"id":"11111111-1111-4111-8111-111111111111","mac":"zz:zz:zz:zz:zz:zz","updatedAt":"` + validTime + `"}]}`},
		{"8 字节 MAC", `{"devices":[{"id":"11111111-1111-4111-8111-111111111111","mac":"aa:bb:cc:dd:ee:ff:00:11","updatedAt":"` + validTime + `"}]}`},
		{"rules 非对象", `{"devices":[{"id":"11111111-1111-4111-8111-111111111111","mac":"aa:bb:cc:00:00:01","rules":[],"updatedAt":"` + validTime + `"}]}`},
		{"scanRecords 非数组", `{"devices":[{"id":"11111111-1111-4111-8111-111111111111","mac":"aa:bb:cc:00:00:01","scanRecords":{},"updatedAt":"` + validTime + `"}]}`},
		{"缺少 updatedAt", `{"devices":[{"id":"11111111-1111-4111-8111-111111111111","mac":"aa:bb:cc:00:00:01"}]}`},
		{"非法 platform", `{"devices":[{"id":"11111111-1111-4111-8111-111111111111","platform":"windows","mac":"aa:bb:cc:00:00:01","updatedAt":"` + validTime + `"}]}`},
		{"Android 无 mac 且无 externalId", `{"devices":[{"id":"11111111-1111-4111-8111-111111111111","platform":"android","updatedAt":"` + validTime + `"}]}`},
		{"iOS 缺少 externalId", `{"devices":[{"id":"11111111-1111-4111-8111-111111111111","platform":"ios","updatedAt":"` + validTime + `"}]}`},
		{"iOS externalId 非 UUID", `{"devices":[{"id":"11111111-1111-4111-8111-111111111111","platform":"ios","externalId":"peripheral-1","updatedAt":"` + validTime + `"}]}`},
		{"mac 与 externalId 不一致", `{"devices":[{"id":"11111111-1111-4111-8111-111111111111","platform":"android","externalId":"AA:BB:CC:00:00:01","mac":"aa:bb:cc:00:00:02","updatedAt":"` + validTime + `"}]}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, h, http.MethodPost, "/api/v1/sync/push", tc.body, user.Token, nil)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if code := decode[errResponse](t, rec).Error.Code; code != httpapi.CodeBadRequest && code != httpapi.CodeInvalidJSON {
				t.Fatalf("code=%q", code)
			}
		})
	}
}

func TestSyncIdempotencyConflictAndTombstone(t *testing.T) {
	h := newHandler(t, 0)
	user := newUser(t, h)
	deviceID := "11111111-1111-4111-8111-111111111111"
	now := time.Now().UTC()
	body := `{"clientId":"android-test","devices":[{"id":"` + deviceID + `","mac":"AA:BB:CC:00:00:01","alias":"客厅开发板","group":"客厅","rules":{"sampleRate":10},"scanRecords":[{"rssi":-55}],"updatedAt":"` + now.Format(time.RFC3339) + `"}]}`

	headers := map[string]string{"Idempotency-Key": "push-001"}
	rec := do(t, h, http.MethodPost, "/api/v1/sync/push", body, user.Token, headers)
	if rec.Code != http.StatusOK {
		t.Fatalf("push status=%d body=%s", rec.Code, rec.Body.String())
	}
	first := decode[struct {
		Applied    []struct{ ServerSeq int64 }
		Conflicts  []json.RawMessage
		NextCursor int64 `json:"nextCursor"`
	}](t, rec)
	if len(first.Applied) != 1 || len(first.Conflicts) != 0 || first.NextCursor < 1 {
		t.Fatalf("首次 push 结果异常: %s", rec.Body.String())
	}
	seq := first.Applied[0].ServerSeq

	t.Run("相同 Idempotency-Key 与请求体不重复写入", func(t *testing.T) {
		rec := do(t, h, http.MethodPost, "/api/v1/sync/push", body, user.Token, headers)
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d", rec.Code)
		}
		if rec.Header().Get("Idempotent-Replayed") != "true" {
			t.Fatalf("缺少 Idempotent-Replayed 响应头")
		}
		replayed := decode[struct {
			Applied    []struct{ ServerSeq int64 }
			NextCursor int64 `json:"nextCursor"`
		}](t, rec)
		if replayed.Applied[0].ServerSeq != seq || replayed.NextCursor != first.NextCursor {
			t.Fatalf("幂等响应不一致: %s", rec.Body.String())
		}

		changes := decode[struct {
			Changes []json.RawMessage
		}](t, do(t, h, http.MethodGet, "/api/v1/sync/changes?cursor=0&limit=100", "", user.Token, nil))
		if len(changes.Changes) != 1 {
			t.Fatalf("幂等重放产生了重复变更: %d", len(changes.Changes))
		}
	})

	t.Run("相同 Idempotency-Key 不同请求体返回 409", func(t *testing.T) {
		changed := strings.ReplaceAll(body, "客厅开发板", "书房开发板")
		rec := do(t, h, http.MethodPost, "/api/v1/sync/push", changed, user.Token, headers)
		if rec.Code != http.StatusConflict {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		if code := decode[errResponse](t, rec).Error.Code; code != httpapi.CodeIdempotencyConflict {
			t.Fatalf("code=%q", code)
		}
	})

	t.Run("旧时间冲突不静默覆盖", func(t *testing.T) {
		older := now.Add(-time.Minute).Format(time.RFC3339)
		conflictBody := `{"clientId":"android-test","devices":[{"id":"` + deviceID + `","mac":"AA:BB:CC:00:00:01","alias":"服务端应保留","group":"客厅","rules":{"sampleRate":10},"scanRecords":[{"rssi":-55}],"updatedAt":"` + older + `"}]}`
		rec := do(t, h, http.MethodPost, "/api/v1/sync/push", conflictBody, user.Token, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d", rec.Code)
		}
		result := decode[struct {
			Conflicts []struct {
				Reason string `json:"reason"`
				Server struct {
					Alias string `json:"alias"`
				} `json:"server"`
			} `json:"conflicts"`
		}](t, rec)
		if len(result.Conflicts) != 1 || result.Conflicts[0].Server.Alias != "客厅开发板" {
			t.Fatalf("冲突结果异常: %s", rec.Body.String())
		}
	})

	t.Run("删除写入 tombstone", func(t *testing.T) {
		newer := time.Now().UTC().Add(30 * time.Second).Format(time.RFC3339)
		deleteBody := `{"clientId":"android-test","devices":[{"id":"` + deviceID + `","mac":"AA:BB:CC:00:00:01","alias":"客厅开发板","group":"客厅","rules":{"sampleRate":10},"scanRecords":[{"rssi":-55}],"updatedAt":"` + newer + `","deleted":true}]}`
		rec := do(t, h, http.MethodPost, "/api/v1/sync/push", deleteBody, user.Token, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		if len(decode[struct {
			Conflicts []json.RawMessage `json:"conflicts"`
		}](t, rec).Conflicts) != 0 {
			t.Fatalf("删除被误判冲突: %s", rec.Body.String())
		}

		my := do(t, h, http.MethodGet, "/api/v1/my/devices", "", user.Token, nil)
		if items := decode[itemsResponse[json.RawMessage]](t, my); len(items.Items) != 0 {
			t.Fatalf("删除后设备列表未清空: %s", my.Body.String())
		}

		changes := decode[struct {
			Changes []struct {
				Op      string          `json:"op"`
				Payload json.RawMessage `json:"payload"`
			}
		}](t, do(t, h, http.MethodGet, "/api/v1/sync/changes?cursor="+intToStr(seq)+"&limit=10", "", user.Token, nil))
		if len(changes.Changes) != 1 || changes.Changes[0].Op != "delete" {
			t.Fatalf("tombstone 变更异常: %+v", changes.Changes)
		}
	})
}

func TestSyncChangesPagination(t *testing.T) {
	h := newHandler(t, 0)
	user := newUser(t, h)

	for i, mac := range []string{"AA:BB:CC:00:00:01", "AA:BB:CC:00:00:02", "AA:BB:CC:00:00:03"} {
		id := []string{
			"11111111-1111-4111-8111-111111111111",
			"22222222-2222-4222-8222-222222222222",
			"33333333-3333-4333-8333-333333333333",
		}[i]
		body := `{"devices":[{"id":"` + id + `","mac":"` + mac + `","alias":"设备` + intToStr(int64(i)) + `","updatedAt":"` + time.Now().UTC().Add(time.Duration(i)*time.Second).Format(time.RFC3339) + `"}]}`
		rec := do(t, h, http.MethodPost, "/api/v1/sync/push", body, user.Token, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("push %d status=%d body=%s", i, rec.Code, rec.Body.String())
		}
	}

	rec := do(t, h, http.MethodGet, "/api/v1/sync/changes?cursor=0&limit=2", "", user.Token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	page := decode[struct {
		Changes []struct {
			Seq int64 `json:"seq"`
		} `json:"changes"`
		NextCursor int64 `json:"nextCursor"`
		HasMore    bool  `json:"hasMore"`
	}](t, rec)
	if len(page.Changes) != 2 || !page.HasMore {
		t.Fatalf("分页异常: %s", rec.Body.String())
	}

	next := do(t, h, http.MethodGet, "/api/v1/sync/changes?cursor="+intToStr(page.NextCursor)+"&limit=2", "", user.Token, nil)
	page2 := decode[struct {
		Changes []struct {
			Seq int64 `json:"seq"`
		} `json:"changes"`
		NextCursor int64 `json:"nextCursor"`
		HasMore    bool  `json:"hasMore"`
	}](t, next)
	if len(page2.Changes) != 1 || page2.HasMore {
		t.Fatalf("第二页异常: %s", next.Body.String())
	}
}

func TestPlatformKeyIsolation(t *testing.T) {
	h := newHandler(t, 0)
	user := newUser(t, h)
	now := time.Now().UTC()

	androidID := "11111111-1111-4111-8111-111111111111"
	iosID := "22222222-2222-4222-8222-222222222222"
	iosExternalID := "E621E1F8-C36C-495A-93FC-0C247A3E6E5F"

	androidBody := `{"clientId":"android-client","devices":[{"id":"` + androidID + `","platform":"android","externalId":"AA:BB:CC:00:00:01","alias":"Android 设备","updatedAt":"` + now.Format(time.RFC3339) + `"}]}`
	iosBody := `{"clientId":"ios-client","devices":[{"id":"` + iosID + `","platform":"ios","externalId":"` + iosExternalID + `","alias":"iOS 设备","updatedAt":"` + now.Format(time.RFC3339) + `"}]}`

	rec := do(t, h, http.MethodPost, "/api/v1/sync/push", androidBody, user.Token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("Android push status=%d body=%s", rec.Code, rec.Body.String())
	}
	applied := decode[struct {
		Applied []struct {
			Platform   string `json:"platform"`
			ExternalID string `json:"externalId"`
		} `json:"applied"`
	}](t, rec)
	if applied.Applied[0].Platform != "android" || applied.Applied[0].ExternalID != "AA:BB:CC:00:00:01" {
		t.Fatalf("Android 键未规范化: %s", rec.Body.String())
	}

	rec = do(t, h, http.MethodPost, "/api/v1/sync/push", iosBody, user.Token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("iOS push status=%d body=%s", rec.Code, rec.Body.String())
	}

	t.Run("同一用户下 Android 与 iOS 同型号设备是两条记录", func(t *testing.T) {
		my := decode[itemsResponse[struct {
			ID         string `json:"id"`
			Platform   string `json:"platform"`
			ExternalID string `json:"externalId"`
			MAC        string `json:"mac"`
			Alias      string `json:"alias"`
		}]](t, do(t, h, http.MethodGet, "/api/v1/my/devices", "", user.Token, nil))
		if len(my.Items) != 2 {
			t.Fatalf("设备数量=%d body=%s", len(my.Items), rec.Body.String())
		}
		seen := map[string]bool{}
		for _, d := range my.Items {
			seen[d.Platform] = true
			if d.Platform == "android" && d.ExternalID != "AA:BB:CC:00:00:01" {
				t.Fatalf("Android externalId=%q", d.ExternalID)
			}
			if d.Platform == "ios" {
				if d.ExternalID != iosExternalID || d.MAC != "" {
					t.Fatalf("iOS 记录异常: %+v", d)
				}
			}
		}
		if !seen["android"] || !seen["ios"] {
			t.Fatalf("平台记录缺失: %+v", my.Items)
		}
	})

	t.Run("更新 Android 设备不影响 iOS 设备", func(t *testing.T) {
		newer := now.Add(time.Minute)
		update := `{"clientId":"android-client","devices":[{"id":"` + androidID + `","platform":"android","externalId":"AA:BB:CC:00:00:01","alias":"Android 设备改名","updatedAt":"` + newer.Format(time.RFC3339) + `"}]}`
		rec := do(t, h, http.MethodPost, "/api/v1/sync/push", update, user.Token, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		my := decode[itemsResponse[struct {
			Platform string `json:"platform"`
			Alias    string `json:"alias"`
		}]](t, do(t, h, http.MethodGet, "/api/v1/my/devices", "", user.Token, nil))
		for _, d := range my.Items {
			if d.Platform == "ios" && d.Alias != "iOS 设备" {
				t.Fatalf("iOS 设备被覆盖: %+v", d)
			}
			if d.Platform == "android" && d.Alias != "Android 设备改名" {
				t.Fatalf("Android 设备未更新: %+v", d)
			}
		}
	})

	t.Run("变更事件带 platform/externalId 与复合 entityId", func(t *testing.T) {
		page := decode[struct {
			Changes []struct {
				EntityID string `json:"entityId"`
				Payload  struct {
					Platform   string `json:"platform"`
					ExternalID string `json:"externalId"`
				} `json:"payload"`
			} `json:"changes"`
		}](t, do(t, h, http.MethodGet, "/api/v1/sync/changes?cursor=0&limit=100", "", user.Token, nil))
		found := false
		for _, ch := range page.Changes {
			if ch.Payload.Platform == "ios" {
				found = true
				if ch.Payload.ExternalID != iosExternalID {
					t.Fatalf("iOS 变更 externalId=%q", ch.Payload.ExternalID)
				}
				want := user.UserID + ":ios:" + iosExternalID
				if ch.EntityID != want {
					t.Fatalf("entityId=%q want %q", ch.EntityID, want)
				}
			}
		}
		if !found {
			t.Fatalf("未找到 iOS 变更: %s", rec.Body.String())
		}
	})
}

func TestCrossUserIsolation(t *testing.T) {
	h := newHandler(t, 0)
	userA := newUser(t, h)
	userB := newUser(t, h)

	deviceID := "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	mac := "AA:BB:CC:00:00:02"
	pushBody := `{"clientId":"b-client","devices":[{"id":"` + deviceID + `","mac":"` + mac + `","alias":"B 的设备","updatedAt":"` + time.Now().UTC().Format(time.RFC3339) + `"}]}`
	rec := do(t, h, http.MethodPost, "/api/v1/sync/push", pushBody, userB.Token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("B push status=%d", rec.Code)
	}

	t.Run("A 的设备列表不包含 B 的设备", func(t *testing.T) {
		my := do(t, h, http.MethodGet, "/api/v1/my/devices", "", userA.Token, nil)
		items := decode[itemsResponse[json.RawMessage]](t, my)
		if len(items.Items) != 0 {
			t.Fatalf("越权可见: %s", my.Body.String())
		}
	})

	t.Run("A 按 MAC 访问 B 的设备必须 404", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, "/api/v1/my/devices/"+mac, "", userA.Token, nil)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		if code := decode[errResponse](t, rec).Error.Code; code != httpapi.CodeNotFound {
			t.Fatalf("code=%q", code)
		}
	})

	t.Run("A 按复合键访问 B 的设备必须 404", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, "/api/v1/my/devices/android/"+mac, "", userA.Token, nil)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("A 的增量变更不包含 B 的记录", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, "/api/v1/sync/changes?cursor=0&limit=100", "", userA.Token, nil)
		page := decode[struct {
			Changes []json.RawMessage `json:"changes"`
		}](t, rec)
		if len(page.Changes) != 0 {
			t.Fatalf("越权看到 B 的变更: %s", rec.Body.String())
		}
	})

	t.Run("B 自己能访问", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, "/api/v1/my/devices/"+mac, "", userB.Token, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d", rec.Code)
		}
		rec = do(t, h, http.MethodGet, "/api/v1/my/devices/android/"+mac, "", userB.Token, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("复合键查询 status=%d body=%s", rec.Code, rec.Body.String())
		}
	})
}

// TestMyDeviceByKey 覆盖按 (platform, externalId) 查询单台设备的新路径。
// 该路径让 iOS 不必依赖 MAC；旧 /my/devices/{mac} 保持可用。
func TestMyDeviceByKey(t *testing.T) {
	h := newHandler(t, 0)
	user := newUser(t, h)
	now := time.Now().UTC()

	androidID := "33333333-3333-4333-8333-333333333333"
	iosID := "44444444-4444-4444-8444-444444444444"
	androidMAC := "AA:BB:CC:00:00:05"
	iosExternalID := "E621E1F8-C36C-495A-93FC-0C247A3E6E5F"

	body := `{"clientId":"key-lookup","devices":[
		{"id":"` + androidID + `","platform":"android","externalId":"` + androidMAC + `","alias":"Android","updatedAt":"` + now.Format(time.RFC3339) + `"},
		{"id":"` + iosID + `","platform":"ios","externalId":"` + iosExternalID + `","alias":"iOS","updatedAt":"` + now.Format(time.RFC3339) + `"}]}`
	rec := do(t, h, http.MethodPost, "/api/v1/sync/push", body, user.Token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("push status=%d body=%s", rec.Code, rec.Body.String())
	}

	t.Run("Android 按复合键查询并规范化大小写", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, "/api/v1/my/devices/android/"+strings.ToLower(androidMAC), "", user.Token, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		device := decode[struct {
			Platform   string `json:"platform"`
			ExternalID string `json:"externalId"`
			MAC        string `json:"mac"`
		}](t, rec)
		if device.Platform != "android" || device.ExternalID != androidMAC || device.MAC != strings.ToLower(androidMAC) {
			t.Fatalf("设备键异常: %+v", device)
		}
	})

	t.Run("iOS 按复合键查询且无 MAC", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, "/api/v1/my/devices/ios/"+strings.ToLower(iosExternalID), "", user.Token, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		device := decode[struct {
			Platform   string `json:"platform"`
			ExternalID string `json:"externalId"`
			MAC        string `json:"mac"`
		}](t, rec)
		if device.Platform != "ios" || device.ExternalID != iosExternalID || device.MAC != "" {
			t.Fatalf("iOS 设备键异常: %+v", device)
		}
	})

	t.Run("平台参数大小写不敏感", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, "/api/v1/my/devices/IOS/"+iosExternalID, "", user.Token, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("非法 platform 返回 400", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, "/api/v1/my/devices/windows/"+androidMAC, "", user.Token, nil)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		if code := decode[errResponse](t, rec).Error.Code; code != httpapi.CodeBadRequest {
			t.Fatalf("code=%q", code)
		}
	})

	t.Run("Android 非法 externalId 返回 400", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, "/api/v1/my/devices/android/not-a-mac", "", user.Token, nil)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status=%d", rec.Code)
		}
		if code := decode[errResponse](t, rec).Error.Code; code != httpapi.CodeInvalidMAC {
			t.Fatalf("code=%q", code)
		}
	})

	t.Run("iOS 非法 externalId 返回 400", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, "/api/v1/my/devices/ios/not-a-uuid", "", user.Token, nil)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status=%d", rec.Code)
		}
	})

	t.Run("不存在的键返回 404", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, "/api/v1/my/devices/ios/00000000-0000-4000-8000-000000000000", "", user.Token, nil)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status=%d", rec.Code)
		}
	})

	t.Run("旧 MAC 路径仍可用", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, "/api/v1/my/devices/"+androidMAC, "", user.Token, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("删除后按复合键返回 404", func(t *testing.T) {
		deleteBody := `{"devices":[{"id":"` + androidID + `","platform":"android","externalId":"` + androidMAC + `","deleted":true,"updatedAt":"` + now.Add(time.Minute).Format(time.RFC3339) + `"}]}`
		if rec := do(t, h, http.MethodPost, "/api/v1/sync/push", deleteBody, user.Token, nil); rec.Code != http.StatusOK {
			t.Fatalf("删除 push status=%d body=%s", rec.Code, rec.Body.String())
		}
		rec := do(t, h, http.MethodGet, "/api/v1/my/devices/android/"+androidMAC, "", user.Token, nil)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("删除后 status=%d body=%s", rec.Code, rec.Body.String())
		}
	})
}

// TestUnauthorizedDoesNotIssueToken 固化 401 契约：
// token 失效时只返回错误体，绝不自动新建匿名账号或下发新 token。
func TestUnauthorizedDoesNotIssueToken(t *testing.T) {
	h := newHandler(t, 0)
	rec := do(t, h, http.MethodGet, "/api/v1/my/devices/ios/E621E1F8-C36C-495A-93FC-0C247A3E6E5F", "", "pjiot_revoked", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if code := decode[errResponse](t, rec).Error.Code; code != httpapi.CodeUnauthorized {
		t.Fatalf("code=%q", code)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if _, ok := raw["token"]; ok {
		t.Fatalf("401 响应不应下发新 token: %s", rec.Body.String())
	}
	if _, ok := raw["userId"]; ok {
		t.Fatalf("401 响应不应返回 userId: %s", rec.Body.String())
	}
}

func TestRateLimit(t *testing.T) {
	h := newHandler(t, 1) // 每分钟 1 次，burst 1
	first := do(t, h, http.MethodGet, "/api/v1/device-types", "", "", nil)
	if first.Code != http.StatusOK {
		t.Fatalf("first status=%d", first.Code)
	}
	second := do(t, h, http.MethodGet, "/api/v1/device-types", "", "", nil)
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("second status=%d", second.Code)
	}
	if code := decode[errResponse](t, second).Error.Code; code != httpapi.CodeRateLimited {
		t.Fatalf("code=%q", code)
	}
	// 健康探针不受限流影响
	if rec := do(t, h, http.MethodGet, "/api/v1/health", "", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("health status=%d", rec.Code)
	}
}

func intToStr(v int64) string {
	return strconv.FormatInt(v, 10)
}
