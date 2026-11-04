package postgres_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	migratepgx "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/platojobs/PJGoIoT/db/migrations"
	"github.com/platojobs/PJGoIoT/internal/model"
	"github.com/platojobs/PJGoIoT/internal/store"
	"github.com/platojobs/PJGoIoT/internal/store/postgres"
)

// TestPostgresStoreIntegration 需要真实 PostgreSQL，通过 TEST_DATABASE_URL 启用。
// 本机或 CI 没有数据库时会 Skip，保证 go test ./... 始终可运行。
func TestPostgresStoreIntegration(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("未设置 TEST_DATABASE_URL，跳过真实数据库集成测试")
	}
	runMigrations(t, dsn)

	st, err := postgres.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("连接数据库失败: %v", err)
	}
	defer st.Close()

	cleanupDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("打开清理连接失败: %v", err)
	}
	defer cleanupDB.Close()

	hashA := hashOf("token-a-" + uuid.NewString())
	hashB := hashOf("token-b-" + uuid.NewString())
	userA, err := st.CreateUserWithToken(context.Background(), hashA)
	if err != nil {
		t.Fatalf("创建用户 A 失败: %v", err)
	}
	userB, err := st.CreateUserWithToken(context.Background(), hashB)
	if err != nil {
		t.Fatalf("创建用户 B 失败: %v", err)
	}
	hashC := hashOf("token-c-" + uuid.NewString())
	userC, err := st.CreateUserWithToken(context.Background(), hashC)
	if err != nil {
		t.Fatalf("创建用户 C 失败: %v", err)
	}
	t.Cleanup(func() {
		cleanupDB.Exec("DELETE FROM users WHERE id = $1 OR id = $2 OR id = $3", userA, userB, userC)
	})

	t.Run("token 解析", func(t *testing.T) {
		got, err := st.UserIDByTokenHash(context.Background(), hashA)
		if err != nil || got != userA {
			t.Fatalf("user=%s err=%v", got, err)
		}
	})

	deviceID := uuid.NewString()
	now := time.Now().UTC()
	req := model.PushRequest{ClientID: "integration-client", Devices: []model.PushDevice{{
		ID: deviceID, MAC: "aa:bb:cc:00:00:01", Alias: "设备 A", Group: "默认",
		Rules: []byte(`{"sampleRate":10}`), ScanRecords: []byte(`[]`),
		UpdatedAt: now,
	}}}
	opts := store.PushOptions{IdempotencyKey: "it-001", RequestHash: "hash-1"}

	result, replayed, err := st.Push(context.Background(), userA, req, opts)
	if err != nil {
		t.Fatalf("push 失败: %v", err)
	}
	if replayed || len(result.Applied) != 1 || len(result.Conflicts) != 0 {
		t.Fatalf("push 结果异常: %+v replayed=%v", result, replayed)
	}
	firstSeq := result.Applied[0].ServerSeq

	t.Run("重复 Idempotency-Key 命中缓存", func(t *testing.T) {
		cached, replayed, err := st.Push(context.Background(), userA, req, opts)
		if err != nil || !replayed {
			t.Fatalf("replayed=%v err=%v", replayed, err)
		}
		if cached.Applied[0].ServerSeq != firstSeq || cached.NextCursor != result.NextCursor {
			t.Fatalf("幂等结果不一致: %+v", cached)
		}
		page, err := st.Changes(context.Background(), userA, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Changes) != 1 {
			t.Fatalf("幂等重放产生重复变更: %d", len(page.Changes))
		}
	})

	t.Run("不同请求体使用同一幂等键返回冲突", func(t *testing.T) {
		_, _, err := st.Push(context.Background(), userA, req, store.PushOptions{
			IdempotencyKey: "it-001", RequestHash: "hash-2",
		})
		if !errors.Is(err, store.ErrIdempotencyConflict) {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("旧时间冲突不覆盖", func(t *testing.T) {
		old := req
		old.Devices[0].UpdatedAt = now.Add(-time.Minute)
		old.Devices[0].Alias = "过期别名"
		res, _, err := st.Push(context.Background(), userA, old, store.PushOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Conflicts) != 1 || res.Conflicts[0].Server.Alias != "设备 A" {
			t.Fatalf("冲突处理异常: %+v", res.Conflicts)
		}
	})

	t.Run("新时间覆盖并写入变更", func(t *testing.T) {
		newer := req
		newer.Devices[0].UpdatedAt = now.Add(45 * time.Second)
		newer.Devices[0].Alias = "设备 A2"
		res, _, err := st.Push(context.Background(), userA, newer, store.PushOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Applied) != 1 || res.Applied[0].Status != "applied" {
			t.Fatalf("更新结果异常: %+v", res)
		}
		got, err := st.GetUserDevice(context.Background(), userA, "aa:bb:cc:00:00:01")
		if err != nil || got.Alias != "设备 A2" {
			t.Fatalf("设备=%+v err=%v", got, err)
		}
	})

	t.Run("删除只写 tombstone", func(t *testing.T) {
		del := req
		del.Devices[0].UpdatedAt = now.Add(90 * time.Second)
		del.Devices[0].Deleted = true
		res, _, err := st.Push(context.Background(), userA, del, store.PushOptions{})
		if err != nil || len(res.Applied) != 1 {
			t.Fatalf("删除失败: %+v err=%v", res, err)
		}
		if _, err := st.GetUserDevice(context.Background(), userA, "aa:bb:cc:00:00:01"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("删除后仍可查询: %v", err)
		}
		page, err := st.Changes(context.Background(), userA, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		last := page.Changes[len(page.Changes)-1]
		if last.Op != "delete" {
			t.Fatalf("最后一条变更 op=%s", last.Op)
		}
	})

	t.Run("变更分页", func(t *testing.T) {
		page, err := st.Changes(context.Background(), userA, 0, 2)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Changes) != 2 || !page.HasMore {
			t.Fatalf("分页异常: len=%d hasMore=%v", len(page.Changes), page.HasMore)
		}
		page2, err := st.Changes(context.Background(), userA, page.NextCursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		if len(page2.Changes) != 1 || page2.HasMore {
			t.Fatalf("第二页异常: len=%d hasMore=%v", len(page2.Changes), page2.HasMore)
		}
	})

	t.Run("跨用户隔离", func(t *testing.T) {
		const macB = "aa:bb:cc:00:00:02"
		otherReq := model.PushRequest{Devices: []model.PushDevice{{
			ID: uuid.NewString(), MAC: macB, Alias: "B 的设备",
			Rules: []byte(`{}`), ScanRecords: []byte(`[]`), UpdatedAt: now,
		}}}
		if _, _, err := st.Push(context.Background(), userB, otherReq, store.PushOptions{}); err != nil {
			t.Fatalf("B push 失败: %v", err)
		}
		if _, err := st.GetUserDevice(context.Background(), userA, macB); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("A 越权访问 B 的设备: %v", err)
		}
		if _, err := st.GetUserDevice(context.Background(), userB, macB); err != nil {
			t.Fatalf("B 访问自己的设备失败: %v", err)
		}
		pageA, _ := st.Changes(context.Background(), userA, 0, 100)
		pageB, _ := st.Changes(context.Background(), userB, 0, 100)
		if len(pageA.Changes) != 3 || len(pageB.Changes) != 1 {
			t.Fatalf("变更隔离异常: A=%d B=%d", len(pageA.Changes), len(pageB.Changes))
		}
	})

	t.Run("同一用户 Android 与 iOS 复合键互不覆盖", func(t *testing.T) {
		androidID := uuid.NewString()
		iosID := uuid.NewString()
		iosExternal := "E621E1F8-C36C-495A-93FC-0C247A3E6E5F"
		userCReq := model.PushRequest{ClientID: "multi-platform", Devices: []model.PushDevice{
			{ID: androidID, Platform: "android", ExternalID: "AA:BB:CC:00:00:03", MAC: "aa:bb:cc:00:00:03", Alias: "Android 设备", Rules: []byte(`{}`), ScanRecords: []byte(`[]`), UpdatedAt: now},
			{ID: iosID, Platform: "ios", ExternalID: iosExternal, Alias: "iOS 设备", Rules: []byte(`{}`), ScanRecords: []byte(`[]`), UpdatedAt: now},
		}}
		res, _, err := st.Push(context.Background(), userC, userCReq, store.PushOptions{})
		if err != nil || len(res.Applied) != 2 {
			t.Fatalf("多平台 push 失败: %+v err=%v", res, err)
		}
		list, err := st.ListUserDevices(context.Background(), userC)
		if err != nil || len(list) != 2 {
			t.Fatalf("设备数量=%d err=%v", len(list), err)
		}
		for _, d := range list {
			switch d.Platform {
			case "android":
				if d.ExternalID != "AA:BB:CC:00:00:03" || d.MAC != "aa:bb:cc:00:00:03" {
					t.Fatalf("Android 记录异常: %+v", d)
				}
			case "ios":
				if d.ExternalID != iosExternal || d.MAC != "" {
					t.Fatalf("iOS 记录异常: %+v", d)
				}
			default:
				t.Fatalf("未知 platform: %+v", d)
			}
		}

		// 按复合键查询单台设备。
		gotAndroid, err := st.GetUserDeviceByKey(context.Background(), userC, "android", "AA:BB:CC:00:00:03")
		if err != nil || gotAndroid.Platform != "android" || gotAndroid.MAC != "aa:bb:cc:00:00:03" {
			t.Fatalf("按复合键查 Android 失败: %+v err=%v", gotAndroid, err)
		}
		gotIOS, err := st.GetUserDeviceByKey(context.Background(), userC, "ios", iosExternal)
		if err != nil || gotIOS.Platform != "ios" || gotIOS.MAC != "" {
			t.Fatalf("按复合键查 iOS 失败: %+v err=%v", gotIOS, err)
		}
		if _, err := st.GetUserDeviceByKey(context.Background(), userA, "ios", iosExternal); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("跨用户复合键查询应 404，err=%v", err)
		}

		// 更新 Android 设备不应影响 iOS 记录。
		update := userCReq
		update.Devices = []model.PushDevice{{
			ID: androidID, Platform: "android", ExternalID: "AA:BB:CC:00:00:03", MAC: "aa:bb:cc:00:00:03",
			Alias: "Android 改名", Rules: []byte(`{}`), ScanRecords: []byte(`[]`), UpdatedAt: now.Add(time.Minute),
		}}
		if _, _, err := st.Push(context.Background(), userC, update, store.PushOptions{}); err != nil {
			t.Fatalf("更新 Android 失败: %v", err)
		}
		list, _ = st.ListUserDevices(context.Background(), userC)
		for _, d := range list {
			if d.Platform == "ios" && d.Alias != "iOS 设备" {
				t.Fatalf("iOS 设备被覆盖: %+v", d)
			}
		}

		// 复合键相同但客户端 id 变化：应沿用已有记录而不是报唯一键冲突。
		rekey := model.PushRequest{ClientID: "rekey", Devices: []model.PushDevice{{
			ID: uuid.NewString(), Platform: "android", ExternalID: "AA:BB:CC:00:00:03",
			Alias: "换 id 后", Rules: []byte(`{}`), ScanRecords: []byte(`[]`), UpdatedAt: now.Add(2 * time.Minute),
		}}}
		res, _, err = st.Push(context.Background(), userC, rekey, store.PushOptions{})
		if err != nil || len(res.Applied) != 1 {
			t.Fatalf("同一复合键换 id 失败: %+v err=%v", res, err)
		}
		list, _ = st.ListUserDevices(context.Background(), userC)
		if len(list) != 2 {
			t.Fatalf("换 id 后设备数量=%d", len(list))
		}
	})
}

func runMigrations(t *testing.T, dsn string) {
	t.Helper()
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		t.Fatalf("加载迁移失败: %v", err)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	defer db.Close()
	driver, err := migratepgx.WithInstance(db, &migratepgx.Config{})
	if err != nil {
		t.Fatalf("初始化迁移驱动失败: %v", err)
	}
	m, err := migrate.NewWithInstance("iofs", src, "pgx5", driver)
	if err != nil {
		t.Fatalf("初始化迁移失败: %v", err)
	}
	defer m.Close()
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("执行迁移失败: %v", err)
	}
}

func hashOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
