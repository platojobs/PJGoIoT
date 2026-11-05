package httpapi_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/platojobs/PJGoIoT/internal/model"
)

type householdResp struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	OwnerUserID string `json:"ownerUserId"`
	Role        string `json:"role"`
	Personal    bool   `json:"personal"`
	MemberCount int    `json:"memberCount"`
	DeviceCount int    `json:"deviceCount"`
}

type roomResp struct {
	ID          string `json:"id"`
	HouseholdID string `json:"householdId"`
	Name        string `json:"name"`
	SortOrder   int    `json:"sortOrder"`
}

type invitationResp struct {
	Code      string    `json:"code"`
	ExpiresAt time.Time `json:"expiresAt"`
}

func pushTestDevice(t *testing.T, h http.Handler, token, id, platform, externalID, alias string) {
	t.Helper()
	body := fmt.Sprintf(`{"clientId":"household-test","devices":[{"id":%q,"platform":%q,"externalId":%q,"alias":%q,"updatedAt":%q}]}`,
		id, platform, externalID, alias, time.Now().UTC().Format(time.RFC3339Nano))
	if rec := do(t, h, http.MethodPost, "/api/v1/sync/push", body, token, nil); rec.Code != http.StatusOK {
		t.Fatalf("push 失败: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHouseholdLifecycleAndPermissions(t *testing.T) {
	h := newHandler(t, 0)
	owner := newUser(t, h)
	member := newUser(t, h)
	outsider := newUser(t, h)

	deviceID := "33333333-3333-4333-8333-333333333333"
	pushTestDevice(t, h, owner.Token, deviceID, "android", "CC:DD:00:00:00:01", "客厅灯")

	// 默认家庭
	list := decode[itemsResponse[householdResp]](t, do(t, h, http.MethodGet, "/api/v1/households", "", owner.Token, nil))
	if len(list.Items) != 1 || !list.Items[0].Personal || list.Items[0].Role != "owner" || list.Items[0].DeviceCount != 1 {
		t.Fatalf("默认家庭不符合预期: %+v", list.Items)
	}
	personalID := list.Items[0].ID

	// 创建家庭
	rec := do(t, h, http.MethodPost, "/api/v1/households", `{"name":"老家"}`, owner.Token, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("创建家庭 status=%d body=%s", rec.Code, rec.Body.String())
	}
	home := decode[householdResp](t, rec)
	if home.Personal || home.Role != "owner" || home.Name != "老家" {
		t.Fatalf("创建家庭响应不符合预期: %+v", home)
	}

	// 邀请与领取
	rec = do(t, h, http.MethodPost, "/api/v1/households/"+home.ID+"/invitations", "", owner.Token, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("创建邀请码 status=%d body=%s", rec.Code, rec.Body.String())
	}
	inv := decode[invitationResp](t, rec)
	rec = do(t, h, http.MethodPost, "/api/v1/households/invitations/claim", `{"code":"`+inv.Code+`"}`, member.Token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("领取邀请码 status=%d body=%s", rec.Code, rec.Body.String())
	}
	joined := decode[householdResp](t, rec)
	if joined.Role != "member" || joined.MemberCount != 2 {
		t.Fatalf("领取后角色/成员数不符合预期: %+v", joined)
	}

	// 一次性
	rec = do(t, h, http.MethodPost, "/api/v1/households/invitations/claim", `{"code":"`+inv.Code+`"}`, outsider.Token, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("邀请码应一次性失效: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// 成员权限：可读、不可写家庭结构
	if rec := do(t, h, http.MethodGet, "/api/v1/households/"+home.ID+"/members", "", member.Token, nil); rec.Code != http.StatusOK {
		t.Fatalf("成员读成员列表 status=%d", rec.Code)
	}
	if rec := do(t, h, http.MethodPatch, "/api/v1/households/"+home.ID, `{"name":"改名"}`, member.Token, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("成员改家庭名应 403: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := do(t, h, http.MethodDelete, "/api/v1/households/"+home.ID, "", member.Token, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("成员删家庭应 403: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := do(t, h, http.MethodPost, "/api/v1/households/"+home.ID+"/rooms", `{"name":"客厅"}`, member.Token, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("成员建房间应 403: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := do(t, h, http.MethodGet, "/api/v1/households/"+personalID, "", member.Token, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("跨家庭读取应 404: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// owner 建房间，设备移入家庭房间
	rec = do(t, h, http.MethodPost, "/api/v1/households/"+home.ID+"/rooms", `{"name":"客厅","sortOrder":1}`, owner.Token, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("创建房间 status=%d body=%s", rec.Code, rec.Body.String())
	}
	room := decode[roomResp](t, rec)
	rec = do(t, h, http.MethodPatch, "/api/v1/my/devices/android/CC:DD:00:00:00:01",
		`{"householdId":"`+home.ID+`","roomId":"`+room.ID+`"}`, owner.Token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("移动设备 status=%d body=%s", rec.Code, rec.Body.String())
	}
	moved := decode[model.UserDevice](t, rec)
	if moved.HouseholdID == nil || *moved.HouseholdID != home.ID || moved.RoomID == nil || *moved.RoomID != room.ID {
		t.Fatalf("设备归属不符合预期: %+v", moved)
	}

	// 家庭成员可见设备与读数，但不能改他人设备
	devices := decode[itemsResponse[model.UserDevice]](t, do(t, h, http.MethodGet, "/api/v1/my/devices", "", member.Token, nil))
	if len(devices.Items) != 1 || devices.Items[0].ID != deviceID {
		t.Fatalf("家庭成员应看到共享设备: %+v", devices.Items)
	}
	if rec := do(t, h, http.MethodPatch, "/api/v1/my/devices/android/CC:DD:00:00:00:01", `{"alias":"成员改名"}`, member.Token, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("成员改他人设备应 404: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// 成员自己的设备仍可管理
	memberDevice := "44444444-4444-4444-8444-444444444444"
	pushTestDevice(t, h, member.Token, memberDevice, "ios", "55555555-5555-4555-8555-555555555555", "成员设备")
	if rec := do(t, h, http.MethodPatch, "/api/v1/my/devices/ios/55555555-5555-4555-8555-555555555555", `{"alias":"成员别名"}`, member.Token, nil); rec.Code != http.StatusOK {
		t.Fatalf("成员改自己的设备应成功: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// 三态：空串清除房间；householdId 空串回到个人家庭
	rec = do(t, h, http.MethodPatch, "/api/v1/my/devices/android/CC:DD:00:00:00:01", `{"roomId":""}`, owner.Token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("清空 roomId status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := decode[model.UserDevice](t, rec); got.RoomID != nil {
		t.Fatalf("roomId 应被清空: %+v", got)
	}
	rec = do(t, h, http.MethodPatch, "/api/v1/my/devices/android/CC:DD:00:00:00:01", `{"householdId":""}`, owner.Token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("清空 householdId status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := decode[model.UserDevice](t, rec); got.HouseholdID == nil || *got.HouseholdID != personalID {
		t.Fatalf("应回到个人家庭: %+v", got)
	}

	// 同步可见范围：成员拉取变更能看到共享设备
	changes := decode[struct {
		Changes []struct {
			Seq      int64           `json:"seq"`
			Entity   string          `json:"entity"`
			EntityID string          `json:"entityId"`
			Op       string          `json:"op"`
			Payload  json.RawMessage `json:"payload"`
		} `json:"changes"`
	}](t, do(t, h, http.MethodGet, "/api/v1/sync/changes?cursor=0&limit=100", "", member.Token, nil))
	found := false
	for _, ch := range changes.Changes {
		if ch.Entity == "user_device" && ch.Op == "upsert" {
			var d model.UserDevice
			if err := json.Unmarshal(ch.Payload, &d); err == nil && d.ID == deviceID {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("成员未见共享设备变更: %+v", changes.Changes)
	}

	// 删除家庭：成员失去可见性，设备回到 owner 个人家庭
	if rec := do(t, h, http.MethodDelete, "/api/v1/households/"+home.ID, "", owner.Token, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("owner 删家庭 status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := do(t, h, http.MethodGet, "/api/v1/households/"+home.ID, "", member.Token, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("删除后成员读取应 404: status=%d", rec.Code)
	}
	if rec := do(t, h, http.MethodGet, "/api/v1/my/devices", "", member.Token, nil); rec.Code != http.StatusOK {
		t.Fatalf("成员设备列表 status=%d", rec.Code)
	}
	memberDevices := decode[itemsResponse[model.UserDevice]](t, do(t, h, http.MethodGet, "/api/v1/my/devices", "", member.Token, nil))
	for _, d := range memberDevices.Items {
		if d.ID == deviceID {
			t.Fatalf("删除家庭后成员不应再看到原共享设备: %+v", d)
		}
	}
	ownerDevices := decode[itemsResponse[model.UserDevice]](t, do(t, h, http.MethodGet, "/api/v1/my/devices", "", owner.Token, nil))
	if len(ownerDevices.Items) == 0 || ownerDevices.Items[0].HouseholdID == nil || *ownerDevices.Items[0].HouseholdID != personalID {
		t.Fatalf("设备应回到 owner 个人家庭: %+v", ownerDevices.Items)
	}

	// 个人家庭不可删除
	if rec := do(t, h, http.MethodDelete, "/api/v1/households/"+personalID, "", owner.Token, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("个人家庭删除应 403: status=%d body=%s", rec.Code, rec.Body.String())
	}
}
