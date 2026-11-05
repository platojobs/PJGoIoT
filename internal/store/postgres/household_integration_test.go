package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"sync"
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

func TestHouseholdManagementPostgres(t *testing.T) {
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
	otherID := newHouseholdUser(t, st)
	t.Cleanup(func() { cleanupUsers(t, dsn, ownerID, memberID, otherID) })

	ownerPersonal := mustPersonalHousehold(t, st, ownerID)
	memberPersonal := mustPersonalHousehold(t, st, memberID)

	home, err := st.CreateHousehold(ctx, ownerID, "管理家庭")
	if err != nil {
		t.Fatalf("创建家庭失败: %v", err)
	}
	join := func(userID string) {
		t.Helper()
		codeHash := hashOf("mgmt-code-" + uuid.NewString())
		if _, err := st.CreateHouseholdInvitation(ctx, ownerID, home.ID, codeHash, time.Now().UTC().Add(10*time.Minute)); err != nil {
			t.Fatalf("创建邀请码失败: %v", err)
		}
		if _, err := st.ClaimHouseholdInvitation(ctx, userID, codeHash, time.Now().UTC(), 10*time.Minute); err != nil {
			t.Fatalf("领取邀请码失败: %v", err)
		}
	}

	// 邀请码管理：列表只暴露前缀，撤销后不可领取
	codeHash := hashOf("mgmt-code-listed-" + uuid.NewString())
	if _, err := st.CreateHouseholdInvitation(ctx, ownerID, home.ID, codeHash, time.Now().UTC().Add(10*time.Minute)); err != nil {
		t.Fatalf("创建邀请码失败: %v", err)
	}
	list, err := st.ListHouseholdInvitations(ctx, ownerID, home.ID)
	if err != nil || len(list) != 1 || list[0].Status != "active" || len(list[0].CodeHashPrefix) != 8 {
		t.Fatalf("邀请码列表不符合预期: %+v err=%v", list, err)
	}
	if err := st.RevokeHouseholdInvitation(ctx, ownerID, home.ID, list[0].ID); err != nil {
		t.Fatalf("撤销邀请码失败: %v", err)
	}
	if _, err := st.ClaimHouseholdInvitation(ctx, memberID, codeHash, time.Now().UTC(), 10*time.Minute); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("撤销后的邀请码应失效: err=%v", err)
	}

	join(memberID)
	if _, err := st.ListHouseholdInvitations(ctx, memberID, home.ID); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("非 owner 查邀请码应 Forbidden: err=%v", err)
	}

	// roomCount 与整段重排
	r1, err := st.CreateHouseholdRoom(ctx, ownerID, home.ID, "客厅", 1)
	if err != nil {
		t.Fatalf("创建房间失败: %v", err)
	}
	r2, err := st.CreateHouseholdRoom(ctx, ownerID, home.ID, "卧室", 2)
	if err != nil {
		t.Fatalf("创建房间失败: %v", err)
	}
	r3, err := st.CreateHouseholdRoom(ctx, ownerID, home.ID, "书房", 3)
	if err != nil {
		t.Fatalf("创建房间失败: %v", err)
	}
	homes, err := st.ListHouseholds(ctx, ownerID)
	if err != nil {
		t.Fatalf("查询家庭失败: %v", err)
	}
	if len(homes) != 2 || homes[1].RoomCount != 3 {
		t.Fatalf("roomCount 应为 3: %+v", homes)
	}
	rooms, err := st.ReorderHouseholdRooms(ctx, ownerID, home.ID, []string{r3.ID, r1.ID, r2.ID})
	if err != nil || len(rooms) != 3 || rooms[0].ID != r3.ID || rooms[2].SortOrder != 2 {
		t.Fatalf("整段重排不符合预期: %+v err=%v", rooms, err)
	}
	if _, err := st.ReorderHouseholdRooms(ctx, ownerID, home.ID, []string{r1.ID}); !errors.Is(err, store.ErrRoomOrderMismatch) {
		t.Fatalf("缺项重排应冲突: err=%v", err)
	}

	// 成员设备随成员移除回到个人家庭
	memberDevice := uuid.NewString()
	memberExt := "AA:BB:CC:00:22:33"
	alias := "成员设备"
	now := time.Now().UTC()
	_, _, err = st.Push(ctx, memberID, model.PushRequest{
		ClientID: "mgmt-integration",
		Devices: []model.PushDevice{{
			ID: memberDevice, Platform: "android", ExternalID: memberExt, Alias: &alias,
			UpdatedAt: now, Rules: []byte(`{}`), ScanRecords: []byte(`[]`),
		}},
	}, store.PushOptions{IdempotencyKey: "mgmt-push-member", RequestHash: "hash-mgmt-member"})
	if err != nil {
		t.Fatalf("push 成员设备失败: %v", err)
	}
	if _, err := st.UpdateUserDeviceMeta(ctx, memberID, "android", memberExt, nil, nil, &home.ID); err != nil {
		t.Fatalf("成员设备移入家庭失败: %v", err)
	}
	if err := st.RemoveHouseholdMember(ctx, ownerID, home.ID, memberID); err != nil {
		t.Fatalf("移除成员失败: %v", err)
	}
	if _, err := st.GetHousehold(ctx, memberID, home.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("被移除成员应 NotFound: err=%v", err)
	}
	// 被移除成员的 token 不能再读到该家庭的任何数据。
	if _, err := st.ListHouseholdMembers(ctx, memberID, home.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("被移除成员读成员列表应 NotFound: err=%v", err)
	}
	if _, err := st.ListHouseholdRooms(ctx, memberID, home.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("被移除成员读房间应 NotFound: err=%v", err)
	}
	if _, err := st.ListHouseholdInvitations(ctx, memberID, home.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("被移除成员读邀请码应 NotFound: err=%v", err)
	}
	got, err := st.GetUserDeviceByKey(ctx, memberID, "android", memberExt)
	if err != nil || got.HouseholdID == nil || *got.HouseholdID != memberPersonal {
		t.Fatalf("被移除成员设备应回个人家庭: %+v err=%v", got, err)
	}
	if err := st.RemoveHouseholdMember(ctx, ownerID, home.ID, ownerID); !errors.Is(err, store.ErrHouseholdOwnerCannotRemoveSelf) {
		t.Fatalf("owner 移除自己应被拒绝: err=%v", err)
	}

	// 成员退出
	join(memberID)
	if err := st.LeaveHousehold(ctx, memberID, home.ID); err != nil {
		t.Fatalf("成员退出失败: %v", err)
	}
	if _, err := st.GetHousehold(ctx, memberID, home.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("退出后应 NotFound: err=%v", err)
	}

	// owner 有其他成员时拒绝退出；成员退出后 owner 单独退出删除家庭
	join(otherID)
	if err := st.LeaveHousehold(ctx, ownerID, home.ID); !errors.Is(err, store.ErrHouseholdOwnerMustTransfer) {
		t.Fatalf("owner 有其他成员应拒退: err=%v", err)
	}
	if err := st.LeaveHousehold(ctx, otherID, home.ID); err != nil {
		t.Fatalf("成员退出失败: %v", err)
	}
	if err := st.LeaveHousehold(ctx, ownerID, home.ID); err != nil {
		t.Fatalf("owner 单独退出失败: %v", err)
	}
	if _, err := st.GetHousehold(ctx, ownerID, home.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("owner 退出后家庭应删除: err=%v", err)
	}

	// 个人家庭不可退出
	if err := st.LeaveHousehold(ctx, ownerID, ownerPersonal); !errors.Is(err, store.ErrPersonalHouseholdImmutable) {
		t.Fatalf("个人家庭退出应被拒绝: err=%v", err)
	}
}

func TestHouseholdOwnershipPostgres(t *testing.T) {
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
	otherID := newHouseholdUser(t, st)
	outsiderID := newHouseholdUser(t, st)
	t.Cleanup(func() { cleanupUsers(t, dsn, ownerID, memberID, otherID, outsiderID) })

	ownerPersonal := mustPersonalHousehold(t, st, ownerID)
	home, err := st.CreateHousehold(ctx, ownerID, "转让家庭")
	if err != nil {
		t.Fatalf("创建家庭失败: %v", err)
	}
	join := func(userID string) {
		t.Helper()
		codeHash := hashOf("transfer-code-" + uuid.NewString())
		if _, err := st.CreateHouseholdInvitation(ctx, ownerID, home.ID, codeHash, time.Now().UTC().Add(10*time.Minute)); err != nil {
			t.Fatalf("创建邀请码失败: %v", err)
		}
		if _, err := st.ClaimHouseholdInvitation(ctx, userID, codeHash, time.Now().UTC(), 10*time.Minute); err != nil {
			t.Fatalf("领取邀请码失败: %v", err)
		}
	}
	join(memberID)
	join(otherID)

	// 目标非成员 → ErrHouseholdTargetNotMember
	if err := st.TransferHouseholdOwnership(ctx, ownerID, home.ID, outsiderID); !errors.Is(err, store.ErrHouseholdTargetNotMember) {
		t.Fatalf("目标非成员应 TargetNotMember: err=%v", err)
	}
	// 个人家庭不可转让
	if err := st.TransferHouseholdOwnership(ctx, ownerID, ownerPersonal, memberID); !errors.Is(err, store.ErrPersonalHouseholdImmutable) {
		t.Fatalf("个人家庭转让应拒绝: err=%v", err)
	}
	// 不能转让给自己
	if err := st.TransferHouseholdOwnership(ctx, ownerID, home.ID, ownerID); !errors.Is(err, store.ErrHouseholdTargetAlreadyOwner) {
		t.Fatalf("转让给自己应拒绝: err=%v", err)
	}
	// 非 owner 不能转让
	if err := st.TransferHouseholdOwnership(ctx, memberID, home.ID, otherID); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("成员转让应 Forbidden: err=%v", err)
	}

	// 成功转让：单 owner、owner_user_id 同步
	if err := st.TransferHouseholdOwnership(ctx, ownerID, home.ID, memberID); err != nil {
		t.Fatalf("转让失败: %v", err)
	}
	owners, ownerUserID := householdOwnerState(t, dsn, home.ID)
	if owners != 1 || ownerUserID != memberID {
		t.Fatalf("转让后应恰好一个 owner 且指向目标: owners=%d owner_user_id=%s", owners, ownerUserID)
	}

	// 成员列表 isSelf
	members, err := st.ListHouseholdMembers(ctx, memberID, home.ID)
	if err != nil {
		t.Fatalf("查询成员失败: %v", err)
	}
	if len(members) != 3 {
		t.Fatalf("成员数应为 3: %+v", members)
	}
	for _, m := range members {
		if m.IsSelf != (m.UserID == memberID) {
			t.Fatalf("isSelf 标记不正确: %+v", m)
		}
		wantRole := "member"
		if m.UserID == memberID {
			wantRole = "owner"
		}
		if m.Role != wantRole {
			t.Fatalf("角色不正确: %+v", m)
		}
	}

	// 旧 owner 边界：读 200（角色 member），结构性写 403，可正常退出
	view, err := st.GetHousehold(ctx, ownerID, home.ID)
	if err != nil || view.Role != "member" || view.OwnerUserID != memberID {
		t.Fatalf("旧 owner 视图不正确: %+v err=%v", view, err)
	}
	if err := st.RenameHousehold(ctx, ownerID, home.ID, "旧 owner 改名"); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("旧 owner 改名应 Forbidden: err=%v", err)
	}
	if _, err := st.CreateHouseholdRoom(ctx, ownerID, home.ID, "旧 owner 房间", 0); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("旧 owner 建房应 Forbidden: err=%v", err)
	}
	if err := st.LeaveHousehold(ctx, ownerID, home.ID); err != nil {
		t.Fatalf("旧 owner 转让后退出失败: %v", err)
	}
	if _, err := st.GetHousehold(ctx, ownerID, home.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("旧 owner 退出后应 NotFound: err=%v", err)
	}
	// 新 owner 还有成员时不能退出
	if err := st.LeaveHousehold(ctx, memberID, home.ID); !errors.Is(err, store.ErrHouseholdOwnerMustTransfer) {
		t.Fatalf("新 owner 有其他成员应拒退: err=%v", err)
	}
}

func TestHouseholdOwnershipTransferConcurrentPostgres(t *testing.T) {
	dsn := testDSN(t)
	runMigrations(t, dsn)
	ctx := context.Background()
	st, err := postgres.New(ctx, dsn)
	if err != nil {
		t.Fatalf("连接数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	ownerID := newHouseholdUser(t, st)
	aID := newHouseholdUser(t, st)
	bID := newHouseholdUser(t, st)
	t.Cleanup(func() { cleanupUsers(t, dsn, ownerID, aID, bID) })

	home, err := st.CreateHousehold(ctx, ownerID, "并发转让")
	if err != nil {
		t.Fatalf("创建家庭失败: %v", err)
	}
	for _, uid := range []string{aID, bID} {
		codeHash := hashOf("conc-code-" + uuid.NewString())
		if _, err := st.CreateHouseholdInvitation(ctx, ownerID, home.ID, codeHash, time.Now().UTC().Add(10*time.Minute)); err != nil {
			t.Fatalf("创建邀请码失败: %v", err)
		}
		if _, err := st.ClaimHouseholdInvitation(ctx, uid, codeHash, time.Now().UTC(), 10*time.Minute); err != nil {
			t.Fatalf("领取邀请码失败: %v", err)
		}
	}

	// 两笔并发转让（同一 owner 转给不同目标）：只有一笔能成功。
	results := make([]error, 2)
	var wg sync.WaitGroup
	for i, target := range []string{aID, bID} {
		wg.Add(1)
		go func(idx int, targetUserID string) {
			defer wg.Done()
			results[idx] = st.TransferHouseholdOwnership(context.Background(), ownerID, home.ID, targetUserID)
		}(i, target)
	}
	wg.Wait()

	success, forbidden := 0, 0
	for _, err := range results {
		switch {
		case err == nil:
			success++
		case errors.Is(err, store.ErrForbidden):
			forbidden++
		default:
			t.Fatalf("并发转让出现非预期错误: %v", err)
		}
	}
	if success != 1 || forbidden != 1 {
		t.Fatalf("并发转让应一胜一 403: success=%d forbidden=%d results=%+v", success, forbidden, results)
	}

	owners, ownerUserID := householdOwnerState(t, dsn, home.ID)
	if owners != 1 {
		t.Fatalf("并发转让后必须恰好一个 owner: owners=%d", owners)
	}
	if ownerUserID != aID && ownerUserID != bID {
		t.Fatalf("owner_user_id 应指向获胜目标: %s", ownerUserID)
	}
}

// householdOwnerState 返回家庭内 owner 角色行数与 households.owner_user_id，用于校验单 owner 不变量。
func householdOwnerState(t *testing.T, dsn, householdID string) (int, string) {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("连接数据库失败: %v", err)
	}
	defer db.Close()
	var owners int
	if err := db.QueryRow(`SELECT count(*) FROM household_members WHERE household_id = $1 AND role = 'owner'`, householdID).Scan(&owners); err != nil {
		t.Fatalf("统计 owner 失败: %v", err)
	}
	var ownerUserID string
	if err := db.QueryRow(`SELECT owner_user_id::text FROM households WHERE id = $1`, householdID).Scan(&ownerUserID); err != nil {
		t.Fatalf("查询 owner_user_id 失败: %v", err)
	}
	return owners, ownerUserID
}

func mustPersonalHousehold(t *testing.T, st *postgres.Store, userID string) string {
	t.Helper()
	homes, err := st.ListHouseholds(context.Background(), userID)
	if err != nil || len(homes) == 0 {
		t.Fatalf("查询个人家庭失败: %+v err=%v", homes, err)
	}
	for _, h := range homes {
		if h.Personal {
			return h.ID
		}
	}
	t.Fatalf("未找到个人家庭: %+v", homes)
	return ""
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
