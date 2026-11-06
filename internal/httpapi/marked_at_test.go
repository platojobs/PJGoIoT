package httpapi_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/platojobs/PJGoIoT/internal/config"
	"github.com/platojobs/PJGoIoT/internal/model"
)

// TestSyncPushMarkedAt 覆盖 HTTP 层的 markedAt 三态解码与回读：
// 时间戳=标记、null=取消、缺失=保持；非法时间返回 400。
func TestSyncPushMarkedAt(t *testing.T) {
	h := newFeedbackHandler(t, config.Config{})
	user := newUser(t, h)

	deviceID := "11111111-1111-4111-8111-111111111111"
	now := time.Now().UTC().Add(-time.Minute)
	marked := time.Date(2026, 11, 7, 0, 0, 0, 123456000, time.UTC)

	body := func(mark string, updated time.Time) string {
		return `{"clientId":"http-mark","devices":[{"id":"` + deviceID + `","mac":"AA:BB:CC:00:00:51","alias":"标记设备","rules":{},"scanRecords":[],"updatedAt":"` +
			updated.Format(time.RFC3339Nano) + `"` + mark + `}]}`
	}
	push := func(mark string, updated time.Time) {
		t.Helper()
		rec := do(t, h, http.MethodPost, "/api/v1/sync/push", body(mark, updated), user.Token, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("push status=%d body=%s", rec.Code, rec.Body.String())
		}
	}
	read := func() model.UserDevice {
		t.Helper()
		rec := do(t, h, http.MethodGet, "/api/v1/my/devices", "", user.Token, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("读取设备 status=%d body=%s", rec.Code, rec.Body.String())
		}
		items := decode[struct {
			Items []model.UserDevice `json:"items"`
		}](t, rec).Items
		for _, d := range items {
			if d.ID == deviceID {
				return d
			}
		}
		t.Fatalf("设备未出现在列表中: %+v", items)
		return model.UserDevice{}
	}

	// 标记
	push(`,"markedAt":"`+marked.Format("2006-01-02T15:04:05.999999Z07:00")+`"`, now)
	if got := read(); got.MarkedAt == nil || !got.MarkedAt.Equal(marked) {
		t.Fatalf("markedAt=%v want=%v", got.MarkedAt, marked)
	}

	// 取消标记
	push(`,"markedAt":null`, now.Add(time.Second))
	if got := read(); got.MarkedAt != nil {
		t.Fatalf("取消标记后应为 null，得到 %v", got.MarkedAt)
	}

	// 缺省保持（不重新标记）
	push("", now.Add(2*time.Second))
	if got := read(); got.MarkedAt != nil {
		t.Fatalf("缺省 markedAt 不应改变状态，得到 %v", got.MarkedAt)
	}

	// 非法时间
	rec := do(t, h, http.MethodPost, "/api/v1/sync/push", body(`,"markedAt":"not-a-time"`, now.Add(3*time.Second)), user.Token, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法 markedAt 应 400，得到 %d %s", rec.Code, rec.Body.String())
	}
}
