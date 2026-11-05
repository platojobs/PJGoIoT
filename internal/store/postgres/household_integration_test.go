package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/platojobs/PJGoIoT/internal/model"
	"github.com/platojobs/PJGoIoT/internal/store"
	"github.com/platojobs/PJGoIoT/internal/store/postgres"
)

func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("未设置 TEST_DATABASE_URL，跳过真实数据库集成测试")
	}
	return dsn
}

func cleanupUsers(t *testing.T, dsn string, ids ...string) {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Logf("清理连接失败: %v", err)
		return
	}
	defer db.Close()
	for _, id := range ids {
		if _, err := db.Exec("DELETE FROM users WHERE id = $1", id); err != nil {
			t.Logf("清理用户 %s 失败: %v", id, err)
		}
	}
}

func newHouseholdUser(t *testing.T, st *postgres.Store) string {
	t.Helper()
	ctx := context.Background()
	uid, err := st.CreateUserWithToken(ctx, hashOf("hh-user-"+uuid.NewString()), time.Now().UTC().Add(180*time.Duration(24)*time.Hour))
	if err != nil {
		t.Fatalf("创建用户失败: %v", err)
	}
	return uid
}

func TestHouseholdSharingPostgres(t *testing.T) {
	dsn := testDSN(t)
	runMigrations(t, dsn)
	ctx := context.Background()
	st, err := postgres.New(ctx, dsn)
	if err != nil {
		t.Fatalf("连接数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	ownerID := newHouseholdUser(t, st)
	memberID := newHouseholdUser(t, st)
	outsiderID := newHouseholdUser(t, st)
	t.Cleanup(func() {
		cleanupUsers(t, dsn, ownerID, memberID, outsiderID)
	})

	// 默认个人家庭
	personal, err := st.ListHouseholds(ctx, ownerID)
	if err != nil || len(personal) != 1 || !personal[0].Personal {
		t.Fatalf("默认个人家庭不符合预期: %+v err=%v", personal, err)
	}
	personalID := personal[0].ID
	if err := st.DeleteHousehold(ctx, ownerID, personalID); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("个人家庭应不可删除: err=%v", err)
	}

	home, err := st.CreateHousehold(ctx, ownerID, "老家")
	if err != nil {
		t.Fatalf("创建家庭失败: %v", err)
	}
	codeHash := hashOf("hh-code-" + uuid.NewString())
	expires := time.Now().UTC().Add(10 * time.Minute)
	if _, err := st.CreateHouseholdInvitation(ctx, ownerID, home.ID, codeHash, expires); err != nil {
		t.Fatalf("创建邀请码失败: %v", err)
	}
	joined, err := st.ClaimHouseholdInvitation(ctx, memberID, codeHash, time.Now().UTC(), 10*time.Minute)
	if err != nil || joined.ID != home.ID || joined.Role != "member" || joined.MemberCount != 2 {
		t.Fatalf("领取邀请码不符合预期: %+v err=%v", joined, err)
	}
	// 一次性
	if _, err := st.ClaimHouseholdInvitation(ctx, outsiderID, codeHash, time.Now().UTC(), 10*time.Minute); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("邀请码应一次性: err=%v", err)
	}
	// 跨家庭 404
	if _, err := st.GetHousehold(ctx, outsiderID, home.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("跨家庭读取应 NotFound: err=%v", err)
	}
	// 成员权限
	if err := st.RenameHousehold(ctx, memberID, home.ID, "改名"); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("成员改名应 Forbidden: err=%v", err)
	}

	// 设备 push 后移入家庭与房间
	deviceID := uuid.NewString()
	extID := "AA:BB:CC:00:11:22"
	alias := "客厅灯"
	group := "默认"
	now := time.Now().UTC()
	_, _, err = st.Push(ctx, ownerID, model.PushRequest{
		ClientID: "hh-integration",
		Devices: []model.PushDevice{{
			ID: deviceID, Platform: "android", ExternalID: extID, Alias: &alias, Group: &group,
			UpdatedAt: now, Rules: []byte(`{}`), ScanRecords: []byte(`[]`),
		}},
	}, store.PushOptions{IdempotencyKey: "hh-push-1", RequestHash: "hash-hh-1"})
	if err != nil {
		t.Fatalf("push 设备失败: %v", err)
	}

	room, err := st.CreateHouseholdRoom(ctx, ownerID, home.ID, "客厅", 1)
	if err != nil {
		t.Fatalf("创建房间失败: %v", err)
	}
	moved, err := st.UpdateUserDeviceMeta(ctx, ownerID, "android", extID, nil, &room.ID, &home.ID)
	if err != nil {
		t.Fatalf("移动设备失败: %v", err)
	}
	if moved.HouseholdID == nil || *moved.HouseholdID != home.ID || moved.RoomID == nil || *moved.RoomID != room.ID {
		t.Fatalf("设备归属不符合预期: %+v", moved)
	}

	// 成员可见设备与读数
	if _, err := st.GetUserDeviceByKey(ctx, memberID, "android", extID); err != nil {
		t.Fatalf("成员应可见共享设备: %v", err)
	}
	if _, err := st.GetUserDeviceByKey(ctx, outsiderID, "android", extID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("跨家庭设备应 NotFound: err=%v", err)
	}
	if _, err := st.UpdateUserDeviceMeta(ctx, memberID, "android", extID, &alias, nil, nil); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("成员改他人设备应 NotFound: err=%v", err)
	}

	readingID := uuid.NewString()
	value := 23.5
	_, _, err = st.Push(ctx, ownerID, model.PushRequest{
		ClientID: "hh-integration",
		Readings: []model.PushReading{{
			ID: readingID, Platform: "android", ExternalID: extID, Metric: "temperature",
			Value: value, Unit: "C", RecordedAt: now,
		}},
	}, store.PushOptions{IdempotencyKey: "hh-push-reading", RequestHash: "hash-hh-reading"})
	if err != nil {
		t.Fatalf("push 读数失败: %v", err)
	}
	page, err := st.ListReadings(ctx, memberID, "android", extID, model.ReadingsQuery{Limit: 10})
	if err != nil || len(page.Items) == 0 {
		t.Fatalf("成员应可读共享设备读数: %+v err=%v", page, err)
	}
	if _, err := st.ListReadings(ctx, outsiderID, "android", extID, model.ReadingsQuery{Limit: 10}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("跨家庭读数应 NotFound: err=%v", err)
	}

	// 成员的增量同步可见共享家庭变更
	changes, err := st.Changes(ctx, memberID, 0, 100)
	if err != nil {
		t.Fatalf("成员拉取变更失败: %v", err)
	}
	found := false
	for _, ch := range changes.Changes {
		if ch.Entity == "user_device" && ch.Op == "upsert" && ch.EntityID == ownerID+":android:"+extID {
			found = true
		}
	}
	if !found {
		t.Fatalf("成员未见共享设备变更: %+v", changes.Changes)
	}
	if _, err := st.Changes(ctx, outsiderID, 0, 100); err != nil {
		t.Fatalf("无关用户拉取变更失败: %v", err)
	}

	// 删除房间 → 设备 room_id 置 NULL
	if err := st.DeleteHouseholdRoom(ctx, ownerID, home.ID, room.ID); err != nil {
		t.Fatalf("删除房间失败: %v", err)
	}
	got, err := st.GetUserDeviceByKey(ctx, ownerID, "android", extID)
	if err != nil || got.RoomID != nil {
		t.Fatalf("删除房间后 roomId 应清空: %+v err=%v", got, err)
	}

	// 删除家庭 → 设备回到个人家庭，成员失去可见性
	if err := st.DeleteHousehold(ctx, ownerID, home.ID); err != nil {
		t.Fatalf("删除家庭失败: %v", err)
	}
	got, err = st.GetUserDeviceByKey(ctx, ownerID, "android", extID)
	if err != nil || got.HouseholdID == nil || *got.HouseholdID != personalID {
		t.Fatalf("删除家庭后设备应回到个人家庭: %+v err=%v", got, err)
	}
	if _, err := st.GetUserDeviceByKey(ctx, memberID, "android", extID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("删除家庭后成员不应再看到设备: err=%v", err)
	}
}
