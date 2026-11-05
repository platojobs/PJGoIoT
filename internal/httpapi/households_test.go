package httpapi_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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
	RoomCount   int    `json:"roomCount"`
}

type invitationInfoResp struct {
	ID             string    `json:"id"`
	CodeHashPrefix string    `json:"codeHashPrefix"`
	Status         string    `json:"status"`
	ExpiresAt      time.Time `json:"expiresAt"`
}

type roomResp struct {
	ID          string `json:"id"`
	HouseholdID string `json:"householdId"`
	Name        string `json:"name"`
	SortOrder   int    `json:"sortOrder"`
}

type memberResp struct {
	UserID string `json:"userId"`
	Role   string `json:"role"`
	Kind   string `json:"kind"`
	IsSelf bool   `json:"isSelf"`
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

func TestHouseholdOwnershipTransfer(t *testing.T) {
	h := newHandler(t, 0)
	owner := newUser(t, h)
	member := newUser(t, h)
	outsider := newUser(t, h)

	personal := decode[itemsResponse[householdResp]](t, do(t, h, http.MethodGet, "/api/v1/households", "", owner.Token, nil)).Items[0]
	if !personal.Personal {
		t.Fatalf("首个家庭应为个人家庭: %+v", personal)
	}

	rec := do(t, h, http.MethodPost, "/api/v1/households", `{"name":"转让测试"}`, owner.Token, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("创建家庭 status=%d body=%s", rec.Code, rec.Body.String())
	}
	home := decode[householdResp](t, rec)

	inv := decode[invitationResp](t, do(t, h, http.MethodPost, "/api/v1/households/"+home.ID+"/invitations", "", owner.Token, nil))
	if rec := do(t, h, http.MethodPost, "/api/v1/households/invitations/claim", `{"code":"`+inv.Code+`"}`, member.Token, nil); rec.Code != http.StatusOK {
		t.Fatalf("成员加入失败 status=%d body=%s", rec.Code, rec.Body.String())
	}

	transfer := func(token, target string, want int) *httptest.ResponseRecorder {
		t.Helper()
		rec := do(t, h, http.MethodPost, "/api/v1/households/"+home.ID+"/transfer-ownership", `{"userId":"`+target+`"}`, token, nil)
		if rec.Code != want {
			t.Fatalf("转让所有权 status=%d want=%d body=%s", rec.Code, want, rec.Body.String())
		}
		return rec
	}

	// 权限与参数边界
	transfer(member.Token, owner.UserID, http.StatusForbidden)
	transfer(outsider.Token, owner.UserID, http.StatusNotFound)
	transfer(owner.Token, outsider.UserID, http.StatusNotFound)
	transfer(owner.Token, owner.UserID, http.StatusBadRequest)
	if rec := do(t, h, http.MethodPost, "/api/v1/households/"+personal.ID+"/transfer-ownership",
		`{"userId":"`+member.UserID+`"}`, owner.Token, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("个人家庭转让应 400: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := do(t, h, http.MethodPost, "/api/v1/households/"+home.ID+"/transfer-ownership", `{"userId":"not-a-uuid"}`, owner.Token, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("非法 userId 应 400: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// 成功转让：调用者身份已变为 member
	rec = transfer(owner.Token, member.UserID, http.StatusOK)
	after := decode[householdResp](t, rec)
	if after.Role != "member" || after.OwnerUserID != member.UserID {
		t.Fatalf("转让后家庭视图不符合预期: %+v", after)
	}

	// 旧 owner：读 200，结构性写 403
	if rec := do(t, h, http.MethodGet, "/api/v1/households/"+home.ID, "", owner.Token, nil); rec.Code != http.StatusOK {
		t.Fatalf("旧 owner 读取应 200: status=%d body=%s", rec.Code, rec.Body.String())
	}
	oldView := decode[householdResp](t, do(t, h, http.MethodGet, "/api/v1/households/"+home.ID, "", owner.Token, nil))
	if oldView.Role != "member" || oldView.OwnerUserID != member.UserID {
		t.Fatalf("旧 owner 视图不正确: %+v", oldView)
	}
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodPatch, "/api/v1/households/" + home.ID, `{"name":"旧 owner 改名"}`},
		{http.MethodDelete, "/api/v1/households/" + home.ID, ""},
		{http.MethodPost, "/api/v1/households/" + home.ID + "/rooms", `{"name":"旧 owner 建房间"}`},
		{http.MethodPost, "/api/v1/households/" + home.ID + "/invitations", ""},
		{http.MethodDelete, "/api/v1/households/" + home.ID + "/members/" + member.UserID, ""},
	} {
		if rec := do(t, h, tc.method, tc.path, tc.body, owner.Token, nil); rec.Code != http.StatusForbidden {
			t.Fatalf("旧 owner %s %s 应 403: status=%d body=%s", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}

	// 新 owner：成员列表只有本人 isSelf=true
	members := decode[itemsResponse[memberResp]](t, do(t, h, http.MethodGet, "/api/v1/households/"+home.ID+"/members", "", member.Token, nil))
	if len(members.Items) != 2 {
		t.Fatalf("成员数应为 2: %+v", members.Items)
	}
	for _, m := range members.Items {
		if wantSelf := m.UserID == member.UserID; m.IsSelf != wantSelf {
			t.Fatalf("isSelf 标记不正确: %+v", m)
		}
		if wantRole := map[bool]string{true: "owner", false: "member"}[m.UserID == member.UserID]; m.Role != wantRole {
			t.Fatalf("角色不正确: %+v", m)
		}
	}

	// owner 带其他成员退出：409 且提示先转让所有权
	rec = do(t, h, http.MethodPost, "/api/v1/households/"+home.ID+"/leave", "", member.Token, nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("owner 有其他成员退出应 409: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if body := decode[errResponse](t, rec); !strings.Contains(body.Error.Message, "转让所有权") {
		t.Fatalf("409 提示应包含转让所有权: %s", body.Error.Message)
	}

	// 转让后旧 owner 恢复退出能力
	if rec := do(t, h, http.MethodPost, "/api/v1/households/"+home.ID+"/leave", "", owner.Token, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("旧 owner 转让后退出应 204: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := do(t, h, http.MethodGet, "/api/v1/households/"+home.ID, "", owner.Token, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("旧 owner 退出后应 404: status=%d", rec.Code)
	}
	if rec := do(t, h, http.MethodPost, "/api/v1/households/"+home.ID+"/leave", "", member.Token, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("新 owner 独退应 204: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHouseholdRoomSetMismatchCode(t *testing.T) {
	h := newHandler(t, 0)
	owner := newUser(t, h)
	rec := do(t, h, http.MethodPost, "/api/v1/households", `{"name":"排序码"}`, owner.Token, nil)
	home := decode[householdResp](t, rec)
	r1 := decode[roomResp](t, do(t, h, http.MethodPost, "/api/v1/households/"+home.ID+"/rooms", `{"name":"客厅"}`, owner.Token, nil))
	decode[roomResp](t, do(t, h, http.MethodPost, "/api/v1/households/"+home.ID+"/rooms", `{"name":"卧室"}`, owner.Token, nil))

	rec = do(t, h, http.MethodPut, "/api/v1/households/"+home.ID+"/rooms/order",
		fmt.Sprintf(`{"roomIds":[%q]}`, r1.ID), owner.Token, nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("缺项重排应 409: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if body := decode[errResponse](t, rec); body.Error.Code != "ROOM_SET_MISMATCH" {
		t.Fatalf("应返回可机读 code ROOM_SET_MISMATCH: %s", rec.Body.String())
	}
}

func TestHouseholdMemberAndInvitationManagement(t *testing.T) {
	h := newHandler(t, 0)
	owner := newUser(t, h)
	member := newUser(t, h)
	other := newUser(t, h)

	rec := do(t, h, http.MethodPost, "/api/v1/households", `{"name":"管理测试"}`, owner.Token, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("创建家庭 status=%d body=%s", rec.Code, rec.Body.String())
	}
	home := decode[householdResp](t, rec)

	createInvite := func() invitationResp {
		t.Helper()
		rec := do(t, h, http.MethodPost, "/api/v1/households/"+home.ID+"/invitations", "", owner.Token, nil)
		if rec.Code != http.StatusCreated {
			t.Fatalf("签发邀请码 status=%d body=%s", rec.Code, rec.Body.String())
		}
		return decode[invitationResp](t, rec)
	}
	claim := func(code, token string, want int) {
		t.Helper()
		rec := do(t, h, http.MethodPost, "/api/v1/households/invitations/claim", `{"code":"`+code+`"}`, token, nil)
		if rec.Code != want {
			t.Fatalf("领取邀请码 status=%d want=%d body=%s", rec.Code, want, rec.Body.String())
		}
	}

	// 邀请码管理：列表只给哈希前缀，撤销后不可领取
	inv1 := createInvite()
	listed := decode[itemsResponse[invitationInfoResp]](t, do(t, h, http.MethodGet, "/api/v1/households/"+home.ID+"/invitations", "", owner.Token, nil))
	if len(listed.Items) != 1 || listed.Items[0].Status != "active" || len(listed.Items[0].CodeHashPrefix) != 8 {
		t.Fatalf("邀请码列表不符合预期: %+v", listed.Items)
	}
	if rec := do(t, h, http.MethodDelete, "/api/v1/households/"+home.ID+"/invitations/"+listed.Items[0].ID, "", owner.Token, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("撤销邀请码 status=%d body=%s", rec.Code, rec.Body.String())
	}
	claim(inv1.Code, member.Token, http.StatusNotFound)

	// member 不能查看邀请码管理列表
	inv2 := createInvite()
	claim(inv2.Code, member.Token, http.StatusOK)
	if rec := do(t, h, http.MethodGet, "/api/v1/households/"+home.ID+"/invitations", "", member.Token, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("member 查邀请码应 403: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// 成员设备随成员移除回到个人家庭
	memberPersonal := decode[itemsResponse[householdResp]](t, do(t, h, http.MethodGet, "/api/v1/households", "", member.Token, nil)).Items[0].ID
	memberDevice := "66666666-6666-4666-8666-666666666666"
	pushTestDevice(t, h, member.Token, memberDevice, "ios", "77777777-7777-4777-8777-777777777777", "成员设备")
	rec = do(t, h, http.MethodPatch, "/api/v1/my/devices/ios/77777777-7777-4777-8777-777777777777", `{"householdId":"`+home.ID+`"}`, member.Token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("成员设备移入家庭 status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := do(t, h, http.MethodDelete, "/api/v1/households/"+home.ID+"/members/"+member.UserID, "", owner.Token, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("owner 移除成员 status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := do(t, h, http.MethodGet, "/api/v1/households/"+home.ID, "", member.Token, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("被移除成员读取家庭应 404: status=%d", rec.Code)
	}
	memberDevices := decode[itemsResponse[model.UserDevice]](t, do(t, h, http.MethodGet, "/api/v1/my/devices", "", member.Token, nil))
	if len(memberDevices.Items) != 1 || memberDevices.Items[0].HouseholdID == nil || *memberDevices.Items[0].HouseholdID != memberPersonal {
		t.Fatalf("被移除成员的设备应回到个人家庭: %+v", memberDevices.Items)
	}
	if rec := do(t, h, http.MethodDelete, "/api/v1/households/"+home.ID+"/members/"+owner.UserID, "", owner.Token, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("owner 移除自己应 400: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// 成员自己退出
	inv3 := createInvite()
	claim(inv3.Code, member.Token, http.StatusOK)
	if rec := do(t, h, http.MethodPost, "/api/v1/households/"+home.ID+"/leave", "", member.Token, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("成员退出 status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := do(t, h, http.MethodGet, "/api/v1/households/"+home.ID, "", member.Token, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("退出后读取家庭应 404: status=%d", rec.Code)
	}

	// 房间整段重排 + 房间数
	r1 := decode[roomResp](t, do(t, h, http.MethodPost, "/api/v1/households/"+home.ID+"/rooms", `{"name":"客厅","sortOrder":1}`, owner.Token, nil))
	r2 := decode[roomResp](t, do(t, h, http.MethodPost, "/api/v1/households/"+home.ID+"/rooms", `{"name":"卧室","sortOrder":2}`, owner.Token, nil))
	r3 := decode[roomResp](t, do(t, h, http.MethodPost, "/api/v1/households/"+home.ID+"/rooms", `{"name":"书房","sortOrder":3}`, owner.Token, nil))
	homes := decode[itemsResponse[householdResp]](t, do(t, h, http.MethodGet, "/api/v1/households", "", owner.Token, nil))
	foundHome := false
	for _, item := range homes.Items {
		if item.ID == home.ID {
			foundHome = true
			if item.RoomCount != 3 {
				t.Fatalf("roomCount 应为 3: %+v", item)
			}
		}
	}
	if !foundHome {
		t.Fatalf("家庭列表缺少测试家庭: %+v", homes.Items)
	}
	body := fmt.Sprintf(`{"roomIds":[%q,%q,%q]}`, r3.ID, r1.ID, r2.ID)
	rec = do(t, h, http.MethodPut, "/api/v1/households/"+home.ID+"/rooms/order", body, owner.Token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("整段重排 status=%d body=%s", rec.Code, rec.Body.String())
	}
	ordered := decode[itemsResponse[roomResp]](t, rec)
	if len(ordered.Items) != 3 || ordered.Items[0].ID != r3.ID || ordered.Items[1].ID != r1.ID ||
		ordered.Items[2].ID != r2.ID || ordered.Items[0].SortOrder != 0 || ordered.Items[2].SortOrder != 2 {
		t.Fatalf("重排结果不符合预期: %+v", ordered.Items)
	}
	// 缺项必须冲突，不产生部分排序
	if rec := do(t, h, http.MethodPut, "/api/v1/households/"+home.ID+"/rooms/order",
		fmt.Sprintf(`{"roomIds":[%q]}`, r1.ID), owner.Token, nil); rec.Code != http.StatusConflict {
		t.Fatalf("缺项重排应 409: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// owner 有其他成员时不能退出
	inv4 := createInvite()
	claim(inv4.Code, other.Token, http.StatusOK)
	if rec := do(t, h, http.MethodPost, "/api/v1/households/"+home.ID+"/leave", "", owner.Token, nil); rec.Code != http.StatusConflict {
		t.Fatalf("owner 有其他成员退出应 409: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := do(t, h, http.MethodPost, "/api/v1/households/"+home.ID+"/leave", "", other.Token, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("other 退出 status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := do(t, h, http.MethodPost, "/api/v1/households/"+home.ID+"/leave", "", owner.Token, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("owner 单独退出 status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := do(t, h, http.MethodGet, "/api/v1/households/"+home.ID, "", owner.Token, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("owner 退出后家庭应删除: status=%d", rec.Code)
	}

	// 个人家庭不可退出
	personal := decode[itemsResponse[householdResp]](t, do(t, h, http.MethodGet, "/api/v1/households", "", owner.Token, nil)).Items[0].ID
	if rec := do(t, h, http.MethodPost, "/api/v1/households/"+personal+"/leave", "", owner.Token, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("个人家庭退出应 400: status=%d body=%s", rec.Code, rec.Body.String())
	}
}
