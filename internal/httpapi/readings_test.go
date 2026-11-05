package httpapi_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// pushResp 是 /sync/push 响应的测试视图，覆盖 v0.4.0 新增的读数计数。
type pushResp struct {
	Applied []struct {
		ServerSeq int64 `json:"serverSeq"`
	} `json:"applied"`
	Conflicts []struct {
		ID         string `json:"id"`
		Platform   string `json:"platform"`
		ExternalID string `json:"externalId"`
		Entity     string `json:"entity"`
		Reason     string `json:"reason"`
		Reading    *struct {
			ID    string  `json:"id"`
			Value float64 `json:"value"`
		} `json:"reading"`
	} `json:"conflicts"`
	NextCursor      int64 `json:"nextCursor"`
	AppliedReadings int   `json:"appliedReadings"`
	SkippedReadings int   `json:"skippedReadings"`
}

type readingsPage struct {
	Items []struct {
		ID         string  `json:"id"`
		Metric     string  `json:"metric"`
		Value      float64 `json:"value"`
		Unit       string  `json:"unit"`
		RecordedAt string  `json:"recordedAt"`
	} `json:"items"`
	NextCursor string `json:"nextCursor"`
	HasMore    bool   `json:"hasMore"`
}

type readingsSummary struct {
	Metric      string   `json:"metric"`
	Unit        string   `json:"unit"`
	Count       int64    `json:"count"`
	Min         *float64 `json:"min"`
	Max         *float64 `json:"max"`
	Avg         *float64 `json:"avg"`
	Last        *float64 `json:"last"`
	WindowStart string   `json:"windowStart"`
	WindowEnd   string   `json:"windowEnd"`
}

func pushBody(t *testing.T, devices string, readings []string) string {
	t.Helper()
	body := `{"clientId":"android-test"`
	if devices != "" {
		body += `,"devices":` + devices
	}
	if len(readings) > 0 {
		body += `,"readings":[` + strings.Join(readings, ",") + `]`
	}
	return body + "}"
}

func readingJSON(id, platform, externalID, metric, value, unit string, recordedAt time.Time) string {
	return fmt.Sprintf(`{"id":%q,"platform":%q,"externalId":%q,"metric":%q,"value":%s,"unit":%q,"recordedAt":%q}`,
		id, platform, externalID, metric, value, unit, recordedAt.UTC().Format(time.RFC3339Nano))
}

func deviceJSON(id, mac string, updatedAt time.Time) string {
	return fmt.Sprintf(`{"id":%q,"mac":%q,"updatedAt":%q}`, id, mac, updatedAt.UTC().Format(time.RFC3339))
}

// TestReadingsPushIdempotency 覆盖批量写入、重复 id 幂等与冲突语义。
func TestReadingsPushIdempotency(t *testing.T) {
	h := newHandler(t, 0)
	user := newUser(t, h)
	now := time.Now().UTC().Add(-time.Minute)

	deviceID := "11111111-1111-4111-8111-111111111111"
	readings := []string{
		readingJSON("22222222-2222-4222-8222-222222222222", "android", "AA:BB:CC:00:00:01", "temperature_celsius", "21.5", "celsius", now),
		readingJSON("33333333-3333-4333-8333-333333333333", "android", "AA:BB:CC:00:00:01", "relative_humidity", "48", "percent", now.Add(-30*time.Second)),
	}

	body := pushBody(t, "["+deviceJSON(deviceID, "AA:BB:CC:00:00:01", now)+"]", readings)
	rec := do(t, h, http.MethodPost, "/api/v1/sync/push", body, user.Token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("首次 push status=%d body=%s", rec.Code, rec.Body.String())
	}
	first := decode[pushResp](t, rec)
	if first.AppliedReadings != 2 || first.SkippedReadings != 0 {
		t.Fatalf("首次 applied=%d skipped=%d", first.AppliedReadings, first.SkippedReadings)
	}
	if len(first.Conflicts) != 0 {
		t.Fatalf("首次不应有冲突: %+v", first.Conflicts)
	}

	// 完全重复：计 skipped，不产生冲突。
	rec = do(t, h, http.MethodPost, "/api/v1/sync/push", body, user.Token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("重复 push status=%d body=%s", rec.Code, rec.Body.String())
	}
	dup := decode[pushResp](t, rec)
	if dup.AppliedReadings != 0 || dup.SkippedReadings != 2 || len(dup.Conflicts) != 0 {
		t.Fatalf("重复 push applied=%d skipped=%d conflicts=%d", dup.AppliedReadings, dup.SkippedReadings, len(dup.Conflicts))
	}

	// 同 id 不同内容：skipped + conflict(entity=reading, reason=duplicate_id)。
	changed := []string{readingJSON("22222222-2222-4222-8222-222222222222", "android", "AA:BB:CC:00:00:01", "temperature_celsius", "99", "celsius", now)}
	rec = do(t, h, http.MethodPost, "/api/v1/sync/push", pushBody(t, "[]", changed), user.Token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("冲突 push status=%d body=%s", rec.Code, rec.Body.String())
	}
	conflict := decode[pushResp](t, rec)
	if conflict.SkippedReadings != 1 || len(conflict.Conflicts) != 1 {
		t.Fatalf("冲突 push skipped=%d conflicts=%d", conflict.SkippedReadings, len(conflict.Conflicts))
	}
	c := conflict.Conflicts[0]
	if c.Entity != "reading" || c.Reason != "duplicate_id" || c.Reading == nil || c.Reading.Value != 99 {
		t.Fatalf("冲突内容不符合契约: %+v", c)
	}

	// 原始数据未被覆盖。
	page := listReadings(t, h, user.Token, "/api/v1/my/devices/android/AA:BB:CC:00:00:01/readings")
	if len(page.Items) != 2 {
		t.Fatalf("读数条数=%d", len(page.Items))
	}
	if page.Items[0].Metric != "temperature_celsius" || page.Items[0].Value != 21.5 {
		t.Fatalf("原读数被覆盖: %+v", page.Items[0])
	}
}

// TestReadingsPushOptional 覆盖「readings 缺失时行为完全不变」与隐式设备。
func TestReadingsPushOptional(t *testing.T) {
	h := newHandler(t, 0)
	user := newUser(t, h)
	now := time.Now().UTC()

	// 旧客户端：不带 readings。
	body := pushBody(t, "["+deviceJSON("11111111-1111-4111-8111-111111111111", "AA:BB:CC:00:00:01", now)+"]", nil)
	rec := do(t, h, http.MethodPost, "/api/v1/sync/push", body, user.Token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("push status=%d body=%s", rec.Code, rec.Body.String())
	}
	got := decode[pushResp](t, rec)
	if got.AppliedReadings != 0 || got.SkippedReadings != 0 {
		t.Fatalf("未提交读数时计数应为 0: applied=%d skipped=%d", got.AppliedReadings, got.SkippedReadings)
	}

	// 隐式设备：iOS 设备未在 user_devices 中，读数仍可写入。
	iosID := "44444444-4444-4444-8444-444444444444"
	reading := readingJSON("55555555-5555-4555-8555-555555555555", "ios", iosID, "battery_percent", "88", "percent", now)
	rec = do(t, h, http.MethodPost, "/api/v1/sync/push", pushBody(t, "", []string{reading}), user.Token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("隐式设备 push status=%d body=%s", rec.Code, rec.Body.String())
	}
	if decode[pushResp](t, rec).AppliedReadings != 1 {
		t.Fatalf("隐式设备读数未写入")
	}
	// 设备未注册 → 查询 404（写入与查询的语义差异见 openapi）。
	rec = do(t, h, http.MethodGet, "/api/v1/my/devices/ios/"+iosID+"/readings", "", user.Token, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未注册设备查询 status=%d body=%s", rec.Code, rec.Body.String())
	}
	// 注册设备后即可查询。
	body = `{"clientId":"ios-test","devices":[{"id":"66666666-6666-4666-8666-666666666666","platform":"ios","externalId":"` + iosID + `","updatedAt":"` + now.Format(time.RFC3339) + `"}]}`
	rec = do(t, h, http.MethodPost, "/api/v1/sync/push", body, user.Token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("注册设备 status=%d body=%s", rec.Code, rec.Body.String())
	}
	page := listReadings(t, h, user.Token, "/api/v1/my/devices/ios/"+iosID+"/readings")
	if len(page.Items) != 1 || page.Items[0].Metric != "battery_percent" {
		t.Fatalf("注册后应能查询到隐式设备读数: %+v", page.Items)
	}
}

// TestReadingsValidation 覆盖 metric/value/unit/recordedAt 与批量上限校验。
func TestReadingsValidation(t *testing.T) {
	h := newHandler(t, 0)
	user := newUser(t, h)
	now := time.Now().UTC()

	tooMany := make([]string, 0, 501)
	for i := 0; i < 501; i++ {
		tooMany = append(tooMany, readingJSON(fmt.Sprintf("77777777-7777-4777-8777-%012d", i), "android", "AA:BB:CC:00:00:01", "temperature_celsius", "1", "celsius", now))
	}

	cases := []struct {
		name string
		body string
		code string
	}{
		{"metric 大写", pushBody(t, "", []string{readingJSON("22222222-2222-4222-8222-222222222222", "android", "AA:BB:CC:00:00:01", "Temperature", "1", "celsius", now)}), "BAD_REQUEST"},
		{"metric 空", pushBody(t, "", []string{readingJSON("22222222-2222-4222-8222-222222222222", "android", "AA:BB:CC:00:00:01", "", "1", "celsius", now)}), "BAD_REQUEST"},
		{"metric 超长", pushBody(t, "", []string{readingJSON("22222222-2222-4222-8222-222222222222", "android", "AA:BB:CC:00:00:01", strings.Repeat("a", 33), "1", "celsius", now)}), "BAD_REQUEST"},
		{"unit 超长", pushBody(t, "", []string{readingJSON("22222222-2222-4222-8222-222222222222", "android", "AA:BB:CC:00:00:01", "temperature_celsius", "1", strings.Repeat("u", 17), now)}), "BAD_REQUEST"},
		{"未来时间", pushBody(t, "", []string{readingJSON("22222222-2222-4222-8222-222222222222", "android", "AA:BB:CC:00:00:01", "temperature_celsius", "1", "celsius", now.Add(10*time.Minute))}), "BAD_REQUEST"},
		{"非 RFC3339 时间", pushBody(t, "", []string{`{"id":"22222222-2222-4222-8222-222222222222","platform":"android","externalId":"AA:BB:CC:00:00:01","metric":"temperature_celsius","value":1,"unit":"celsius","recordedAt":"yesterday"}`}), "BAD_REQUEST"},
		{"NaN 字面量", pushBody(t, "", []string{`{"id":"22222222-2222-4222-8222-222222222222","platform":"android","externalId":"AA:BB:CC:00:00:01","metric":"temperature_celsius","value":NaN,"unit":"celsius","recordedAt":"` + now.Format(time.RFC3339) + `"}`}), "INVALID_JSON"},
		{"无穷大指数", pushBody(t, "", []string{`{"id":"22222222-2222-4222-8222-222222222222","platform":"android","externalId":"AA:BB:CC:00:00:01","metric":"temperature_celsius","value":1e999,"unit":"celsius","recordedAt":"` + now.Format(time.RFC3339) + `"}`}), "INVALID_JSON"},
		{"Infinity 字符串", pushBody(t, "", []string{`{"id":"22222222-2222-4222-8222-222222222222","platform":"android","externalId":"AA:BB:CC:00:00:01","metric":"temperature_celsius","value":"Infinity","unit":"celsius","recordedAt":"` + now.Format(time.RFC3339) + `"}`}), "INVALID_JSON"},
		{"readings 非数组", `{"clientId":"x","readings":{}}`, "BAD_REQUEST"},
		{"超过 500 条", pushBody(t, "", tooMany), "BAD_REQUEST"},
		{"非法 externalId", pushBody(t, "", []string{readingJSON("22222222-2222-4222-8222-222222222222", "android", "not-a-mac", "temperature_celsius", "1", "celsius", now)}), "BAD_REQUEST"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, h, http.MethodPost, "/api/v1/sync/push", tc.body, user.Token, nil)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			code := decode[errResponse](t, rec).Error.Code
			if code != tc.code && !(tc.code == "INVALID_JSON" && code == "BAD_REQUEST") {
				t.Fatalf("code=%q want=%q", code, tc.code)
			}
		})
	}

	// 允许 value=0 与空 unit。
	ok := pushBody(t, "", []string{readingJSON("88888888-8888-4888-8888-888888888888", "android", "AA:BB:CC:00:00:01", "counter", "0", "", now)})
	if rec := do(t, h, http.MethodPost, "/api/v1/sync/push", ok, user.Token, nil); rec.Code != http.StatusOK {
		t.Fatalf("value=0/unit 空串应被接受: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// TestReadingsListPagination 覆盖倒序、游标分页、过滤与越权。
func TestReadingsListPagination(t *testing.T) {
	h := newHandler(t, 0)
	user := newUser(t, h)
	base := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Microsecond)

	readings := make([]string, 0, 5)
	for i := 0; i < 5; i++ {
		readings = append(readings, readingJSON(
			fmt.Sprintf("99999999-9999-4999-8999-%012d", i), "android", "AA:BB:CC:00:00:01",
			"temperature_celsius", fmt.Sprintf("%d", 10+i), "celsius", base.Add(time.Duration(i)*time.Minute)))
	}
	body := pushBody(t, "["+deviceJSON("11111111-1111-4111-8111-111111111111", "AA:BB:CC:00:00:01", base)+"]", readings)
	if rec := do(t, h, http.MethodPost, "/api/v1/sync/push", body, user.Token, nil); rec.Code != http.StatusOK {
		t.Fatalf("push status=%d body=%s", rec.Code, rec.Body.String())
	}

	path := "/api/v1/my/devices/android/AA:BB:CC:00:00:01/readings?limit=2"
	page := listReadings(t, h, user.Token, path)
	if len(page.Items) != 2 || !page.HasMore || page.NextCursor == "" {
		t.Fatalf("第一页 items=%d hasMore=%v cursor=%q", len(page.Items), page.HasMore, page.NextCursor)
	}
	if page.Items[0].Value != 14 || page.Items[1].Value != 13 {
		t.Fatalf("未按 recordedAt 倒序: %+v", page.Items)
	}

	page2 := listReadings(t, h, user.Token, path+"&cursor="+page.NextCursor)
	if len(page2.Items) != 2 || !page2.HasMore {
		t.Fatalf("第二页 items=%d hasMore=%v", len(page2.Items), page2.HasMore)
	}
	if page2.Items[0].Value != 12 || page2.Items[1].Value != 11 {
		t.Fatalf("第二页内容错误: %+v", page2.Items)
	}

	page3 := listReadings(t, h, user.Token, path+"&cursor="+page2.NextCursor)
	if len(page3.Items) != 1 || page3.HasMore || page3.NextCursor != "" {
		t.Fatalf("第三页 items=%d hasMore=%v cursor=%q", len(page3.Items), page3.HasMore, page3.NextCursor)
	}
	if page3.Items[0].Value != 10 {
		t.Fatalf("第三页内容错误: %+v", page3.Items)
	}

	// metric 过滤 + 时间窗口。
	filtered := listReadings(t, h, user.Token,
		"/api/v1/my/devices/android/AA:BB:CC:00:00:01/readings?metric=relative_humidity")
	if len(filtered.Items) != 0 {
		t.Fatalf("metric 过滤应无数据: %+v", filtered.Items)
	}
	windowed := listReadings(t, h, user.Token, fmt.Sprintf(
		"/api/v1/my/devices/android/AA:BB:CC:00:00:01/readings?from=%s&to=%s",
		base.Add(time.Minute).Format(time.RFC3339Nano), base.Add(3*time.Minute).Format(time.RFC3339Nano)))
	if len(windowed.Items) != 3 {
		t.Fatalf("时间窗口条数=%d", len(windowed.Items))
	}

	// 非法参数。
	for _, p := range []string{
		"/api/v1/my/devices/android/AA:BB:CC:00:00:01/readings?limit=501",
		"/api/v1/my/devices/android/AA:BB:CC:00:00:01/readings?limit=0",
		"/api/v1/my/devices/android/AA:BB:CC:00:00:01/readings?cursor=@@",
		"/api/v1/my/devices/android/AA:BB:CC:00:00:01/readings?from=not-a-time",
	} {
		if rec := do(t, h, http.MethodGet, p, "", user.Token, nil); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s status=%d body=%s", p, rec.Code, rec.Body.String())
		}
	}

	// 不存在 / 跨用户 → 404。
	if rec := do(t, h, http.MethodGet, "/api/v1/my/devices/android/AA:BB:CC:00:00:02/readings", "", user.Token, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("不存在设备 status=%d", rec.Code)
	}
	other := newUser(t, h)
	if rec := do(t, h, http.MethodGet, "/api/v1/my/devices/android/AA:BB:CC:00:00:01/readings", "", other.Token, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("跨用户 status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// TestReadingsSummary 覆盖聚合值与无数据语义。
func TestReadingsSummary(t *testing.T) {
	h := newHandler(t, 0)
	user := newUser(t, h)
	now := time.Now().UTC()
	base := now.Add(-30 * time.Minute).Truncate(time.Microsecond)

	readings := []string{
		readingJSON("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "android", "AA:BB:CC:00:00:01", "temperature_celsius", "10", "celsius", base.Add(-30*time.Minute)),
		readingJSON("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", "android", "AA:BB:CC:00:00:01", "temperature_celsius", "20", "celsius", base.Add(-20*time.Minute)),
		readingJSON("cccccccc-cccc-4ccc-8ccc-cccccccccccc", "android", "AA:BB:CC:00:00:01", "temperature_celsius", "30", "celsius", base),
	}
	body := pushBody(t, "["+deviceJSON("11111111-1111-4111-8111-111111111111", "AA:BB:CC:00:00:01", base)+"]", readings)
	if rec := do(t, h, http.MethodPost, "/api/v1/sync/push", body, user.Token, nil); rec.Code != http.StatusOK {
		t.Fatalf("push status=%d body=%s", rec.Code, rec.Body.String())
	}

	path := "/api/v1/my/devices/android/AA:BB:CC:00:00:01/readings/summary?metric=temperature_celsius&hours=24"
	summary := getSummary(t, h, user.Token, path)
	if summary.Count != 3 || summary.Min == nil || *summary.Min != 10 ||
		summary.Max == nil || *summary.Max != 30 || summary.Avg == nil || *summary.Avg != 20 ||
		summary.Last == nil || *summary.Last != 30 || summary.Unit != "celsius" {
		t.Fatalf("聚合结果错误: %+v", summary)
	}
	if summary.WindowStart == "" || summary.WindowEnd == "" {
		t.Fatalf("缺少窗口时间: %+v", summary)
	}

	// 无数据：count=0，数值为 null。
	empty := getSummary(t, h, user.Token, "/api/v1/my/devices/android/AA:BB:CC:00:00:01/readings/summary?metric=relative_humidity&hours=24")
	if empty.Count != 0 || empty.Min != nil || empty.Max != nil || empty.Avg != nil || empty.Last != nil {
		t.Fatalf("无数据聚合错误: %+v", empty)
	}

	// 窗口外数据不参与聚合（上面 3 条均在 1 小时内，hours=1 仍应全部命中；hours 校验见下）。
	if rec := do(t, h, http.MethodGet, "/api/v1/my/devices/android/AA:BB:CC:00:00:01/readings/summary?metric=temperature_celsius&hours=2161", "", user.Token, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("hours 超限 status=%d", rec.Code)
	}
	if rec := do(t, h, http.MethodGet, "/api/v1/my/devices/android/AA:BB:CC:00:00:01/readings/summary", "", user.Token, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("缺少 metric status=%d", rec.Code)
	}
	other := newUser(t, h)
	if rec := do(t, h, http.MethodGet, path, "", other.Token, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("跨用户 summary status=%d", rec.Code)
	}
}

func listReadings(t *testing.T, h http.Handler, token, path string) readingsPage {
	t.Helper()
	rec := do(t, h, http.MethodGet, path, "", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s status=%d body=%s", path, rec.Code, rec.Body.String())
	}
	return decode[readingsPage](t, rec)
}

func getSummary(t *testing.T, h http.Handler, token, path string) readingsSummary {
	t.Helper()
	rec := do(t, h, http.MethodGet, path, "", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s status=%d body=%s", path, rec.Code, rec.Body.String())
	}
	var out readingsSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析 summary 失败: %v body=%s", err, rec.Body.String())
	}
	return out
}
