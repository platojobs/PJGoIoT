package postgres_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"math"
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
	"github.com/platojobs/PJGoIoT/internal/validate"
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
	userA, err := st.CreateUserWithToken(context.Background(), hashA, time.Now().UTC().Add(180*24*time.Hour))
	if err != nil {
		t.Fatalf("创建用户 A 失败: %v", err)
	}
	userB, err := st.CreateUserWithToken(context.Background(), hashB, time.Now().UTC().Add(180*24*time.Hour))
	if err != nil {
		t.Fatalf("创建用户 B 失败: %v", err)
	}
	hashC := hashOf("token-c-" + uuid.NewString())
	userC, err := st.CreateUserWithToken(context.Background(), hashC, time.Now().UTC().Add(180*24*time.Hour))
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
		ID: deviceID, MAC: "aa:bb:cc:00:00:01", Alias: strPtr("设备 A"), Group: strPtr("默认"),
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
		old.Devices[0].Alias = strPtr("过期别名")
		res, _, err := st.Push(context.Background(), userA, old, store.PushOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Conflicts) != 1 || res.Conflicts[0].Server == nil || res.Conflicts[0].Server.Alias != "设备 A" {
			t.Fatalf("冲突处理异常: %+v", res.Conflicts)
		}
	})

	t.Run("新时间覆盖并写入变更", func(t *testing.T) {
		newer := req
		newer.Devices[0].UpdatedAt = now.Add(45 * time.Second)
		newer.Devices[0].Alias = strPtr("设备 A2")
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
			ID: uuid.NewString(), MAC: macB, Alias: strPtr("B 的设备"),
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
			{ID: androidID, Platform: "android", ExternalID: "AA:BB:CC:00:00:03", MAC: "aa:bb:cc:00:00:03", Alias: strPtr("Android 设备"), Rules: []byte(`{}`), ScanRecords: []byte(`[]`), UpdatedAt: now},
			{ID: iosID, Platform: "ios", ExternalID: iosExternal, Alias: strPtr("iOS 设备"), Rules: []byte(`{}`), ScanRecords: []byte(`[]`), UpdatedAt: now},
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
			Alias: strPtr("Android 改名"), Rules: []byte(`{}`), ScanRecords: []byte(`[]`), UpdatedAt: now.Add(time.Minute),
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
			Alias: strPtr("换 id 后"), Rules: []byte(`{}`), ScanRecords: []byte(`[]`), UpdatedAt: now.Add(2 * time.Minute),
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

	t.Run("公共元数据补全与缺失降级", func(t *testing.T) {
		// 000004 迁移为种子公共设备补齐了型号/厂商/固件/兼容性。
		android, err := st.GetUserDeviceByKey(context.Background(), userC, "android", "AA:BB:CC:00:00:03")
		if err != nil {
			t.Fatalf("查询 Android 设备失败: %v", err)
		}
		if android.DeviceType == nil || *android.DeviceType != "nordic_uart" {
			t.Fatalf("Android deviceType=%v", android.DeviceType)
		}
		if android.Vendor == nil || *android.Vendor != "Nordic Semiconductor" {
			t.Fatalf("Android vendor=%v", android.Vendor)
		}
		if android.FirmwareVersion == nil || *android.FirmwareVersion != "nRF Connect SDK v2.9.0" {
			t.Fatalf("Android firmwareVersion=%v", android.FirmwareVersion)
		}
		if len(android.Compatibility) == 0 || string(android.Compatibility) == "{}" {
			t.Fatalf("Android compatibility=%s", android.Compatibility)
		}

		// iOS 未上报型号且无 MAC：字段降级为 null，读取本身仍成功。
		ios, err := st.GetUserDeviceByKey(context.Background(), userC, "ios", "E621E1F8-C36C-495A-93FC-0C247A3E6E5F")
		if err != nil {
			t.Fatalf("查询 iOS 设备失败: %v", err)
		}
		if ios.DeviceType != nil || ios.Vendor != nil || ios.FirmwareVersion != nil || len(ios.Compatibility) != 0 {
			t.Fatalf("iOS 缺失关联时应为 null: %+v", ios)
		}

		// 客户端上报型号但无 MAC：按 device_types 回退补全厂商与型号级兼容性。
		typedExternal := "0F1E2D3C-4B5A-4968-8777-666655554444"
		typedReq := model.PushRequest{ClientID: "typed", Devices: []model.PushDevice{{
			ID: uuid.NewString(), Platform: "ios", ExternalID: typedExternal,
			DeviceType: "environmental_sensor", Alias: strPtr("上报型号"),
			Rules: []byte(`{}`), ScanRecords: []byte(`[]`), UpdatedAt: now.Add(3 * time.Minute),
		}}}
		if _, _, err := st.Push(context.Background(), userC, typedReq, store.PushOptions{}); err != nil {
			t.Fatalf("上报型号 push 失败: %v", err)
		}
		typed, err := st.GetUserDeviceByKey(context.Background(), userC, "ios", typedExternal)
		if err != nil {
			t.Fatalf("查询上报型号设备失败: %v", err)
		}
		if typed.DeviceType == nil || *typed.DeviceType != "environmental_sensor" {
			t.Fatalf("deviceType=%v", typed.DeviceType)
		}
		if typed.Vendor == nil || *typed.Vendor != "Bluetooth SIG" {
			t.Fatalf("vendor=%v", typed.Vendor)
		}
		if typed.FirmwareVersion != nil {
			t.Fatalf("无 MAC 时 firmwareVersion 应为 null: %v", typed.FirmwareVersion)
		}
		if len(typed.Compatibility) == 0 || string(typed.Compatibility) == "{}" {
			t.Fatalf("compatibility=%s", typed.Compatibility)
		}

		// 未注册型号：写入与读取都不报错，关联字段为 null。
		unknownExternal := "9A8B7C6D-5E4F-4A3B-8C2D-1E0F9A8B7C6D"
		unknownReq := model.PushRequest{ClientID: "unknown", Devices: []model.PushDevice{{
			ID: uuid.NewString(), Platform: "ios", ExternalID: unknownExternal,
			DeviceType: "unregistered_model", Alias: strPtr("未注册"),
			Rules: []byte(`{}`), ScanRecords: []byte(`[]`), UpdatedAt: now.Add(4 * time.Minute),
		}}}
		if _, _, err := st.Push(context.Background(), userC, unknownReq, store.PushOptions{}); err != nil {
			t.Fatalf("未注册型号 push 不应报错: %v", err)
		}
		unknown, err := st.GetUserDeviceByKey(context.Background(), userC, "ios", unknownExternal)
		if err != nil {
			t.Fatalf("未注册型号查询不应报错: %v", err)
		}
		if unknown.DeviceType == nil || *unknown.DeviceType != "unregistered_model" {
			t.Fatalf("deviceType=%v", unknown.DeviceType)
		}
		if unknown.Vendor != nil || unknown.FirmwareVersion != nil || len(unknown.Compatibility) != 0 {
			t.Fatalf("未注册型号关联字段应为 null: %+v", unknown)
		}
	})

	t.Run("可缺省字段语义与广播名隔离", func(t *testing.T) {
		const external = "ABC12345-1234-4ABC-8DEF-1234567890AB"
		deviceID := uuid.NewString()
		ctx := context.Background()

		base := model.PushRequest{ClientID: "presence", Devices: []model.PushDevice{{
			ID: deviceID, Platform: "ios", ExternalID: external,
			Alias: strPtr("我的设备"), AdvertisedName: strPtr("PJ-Sensor"), Group: strPtr("客厅"),
			Rules: []byte(`{}`), ScanRecords: []byte(`[]`), UpdatedAt: now.Add(-40 * time.Minute),
		}}}
		if _, _, err := st.Push(ctx, userC, base, store.PushOptions{}); err != nil {
			t.Fatalf("初始化设备失败: %v", err)
		}

		// 只上报广播名：alias/group 保持原值。
		advertisedOnly := model.PushRequest{Devices: []model.PushDevice{{
			ID: deviceID, Platform: "ios", ExternalID: external,
			AdvertisedName: strPtr("PJ-Sensor-2"),
			Rules:          []byte(`{}`), ScanRecords: []byte(`[]`), UpdatedAt: now.Add(-39 * time.Minute),
		}}}
		if _, _, err := st.Push(ctx, userC, advertisedOnly, store.PushOptions{}); err != nil {
			t.Fatalf("上报广播名失败: %v", err)
		}
		got, err := st.GetUserDeviceByKey(ctx, userC, "ios", external)
		if err != nil {
			t.Fatalf("查询失败: %v", err)
		}
		if got.Alias != "我的设备" || got.Group != "客厅" || got.AdvertisedName != "PJ-Sensor-2" {
			t.Fatalf("广播名更新影响了别名或分组: %+v", got)
		}

		// 只上报别名：广播名保持原值。
		aliasOnly := model.PushRequest{Devices: []model.PushDevice{{
			ID: deviceID, Platform: "ios", ExternalID: external,
			Alias: strPtr("用户改名"),
			Rules: []byte(`{}`), ScanRecords: []byte(`[]`), UpdatedAt: now.Add(-38 * time.Minute),
		}}}
		if _, _, err := st.Push(ctx, userC, aliasOnly, store.PushOptions{}); err != nil {
			t.Fatalf("上报别名失败: %v", err)
		}
		got, err = st.GetUserDeviceByKey(ctx, userC, "ios", external)
		if err != nil {
			t.Fatalf("查询失败: %v", err)
		}
		if got.Alias != "用户改名" || got.AdvertisedName != "PJ-Sensor-2" || got.Group != "客厅" {
			t.Fatalf("别名更新影响了广播名: %+v", got)
		}

		// group 空串：显式清除；后续缺省 group 不得让旧分组复活。
		clearGroup := model.PushRequest{Devices: []model.PushDevice{{
			ID: deviceID, Platform: "ios", ExternalID: external,
			Group: strPtr(""),
			Rules: []byte(`{}`), ScanRecords: []byte(`[]`), UpdatedAt: now.Add(-37 * time.Minute),
		}}}
		if _, _, err := st.Push(ctx, userC, clearGroup, store.PushOptions{}); err != nil {
			t.Fatalf("清空分组失败: %v", err)
		}
		got, err = st.GetUserDeviceByKey(ctx, userC, "ios", external)
		if err != nil || got.Group != "" {
			t.Fatalf("分组未被清空: group=%q err=%v", got.Group, err)
		}

		keepGroup := model.PushRequest{Devices: []model.PushDevice{{
			ID: deviceID, Platform: "ios", ExternalID: external,
			AdvertisedName: strPtr("PJ-Sensor-3"),
			Rules:          []byte(`{}`), ScanRecords: []byte(`[]`), UpdatedAt: now.Add(-36 * time.Minute),
		}}}
		if _, _, err := st.Push(ctx, userC, keepGroup, store.PushOptions{}); err != nil {
			t.Fatalf("缺省 group 写入失败: %v", err)
		}
		got, err = st.GetUserDeviceByKey(ctx, userC, "ios", external)
		if err != nil {
			t.Fatalf("查询失败: %v", err)
		}
		if got.Group != "" || got.Alias != "用户改名" || got.AdvertisedName != "PJ-Sensor-3" {
			t.Fatalf("缺省字段未保持或分组复活: %+v", got)
		}
	})
	t.Run("读数链路", func(t *testing.T) {
		const external = "AA:BB:CC:00:00:09"
		ctx := context.Background()
		base := time.Now().UTC().Add(-time.Hour)

		// 设备与首批读数同批写入，验证读数按 id 幂等。
		readings := make([]model.PushReading, 0, 5)
		for i := 0; i < 5; i++ {
			readings = append(readings, model.PushReading{
				ID: uuid.NewString(), Platform: "android", ExternalID: external,
				Metric: "temperature_celsius", Value: float64(10 + i), Unit: "celsius",
				RecordedAt: base.Add(time.Duration(i) * time.Minute),
			})
		}
		batch := model.PushRequest{
			ClientID: "readings-it",
			Devices: []model.PushDevice{{
				ID: uuid.NewString(), MAC: external, Rules: []byte(`{}`), ScanRecords: []byte(`[]`), UpdatedAt: now,
			}},
			Readings: readings,
		}
		res, _, err := st.Push(ctx, userB, batch, store.PushOptions{})
		if err != nil {
			t.Fatalf("读数 push 失败: %v", err)
		}
		if res.AppliedReadings != 5 || res.SkippedReadings != 0 || len(res.Conflicts) != 0 {
			t.Fatalf("首次读数 applied=%d skipped=%d conflicts=%d", res.AppliedReadings, res.SkippedReadings, len(res.Conflicts))
		}

		// 完全重复：skipped，无冲突。
		dup, _, err := st.Push(ctx, userB, model.PushRequest{Readings: readings}, store.PushOptions{})
		if err != nil {
			t.Fatalf("重复 push 失败: %v", err)
		}
		if dup.AppliedReadings != 0 || dup.SkippedReadings != 5 || len(dup.Conflicts) != 0 {
			t.Fatalf("重复读数 applied=%d skipped=%d conflicts=%d", dup.AppliedReadings, dup.SkippedReadings, len(dup.Conflicts))
		}

		// 同 id 不同内容：skipped + conflict。
		changed := readings[1]
		changed.Value = 99
		conflictRes, _, err := st.Push(ctx, userB, model.PushRequest{Readings: []model.PushReading{changed}}, store.PushOptions{})
		if err != nil {
			t.Fatalf("冲突 push 失败: %v", err)
		}
		if conflictRes.SkippedReadings != 1 || len(conflictRes.Conflicts) != 1 {
			t.Fatalf("冲突读数 skipped=%d conflicts=%d", conflictRes.SkippedReadings, len(conflictRes.Conflicts))
		}
		c := conflictRes.Conflicts[0]
		if c.Entity != "reading" || c.Reason != "duplicate_id" || c.Reading == nil || c.Reading.Value != 99 {
			t.Fatalf("冲突内容异常: %+v", c)
		}

		// 校验：NaN / Inf / 未来时间 / 非法 metric 在存储层同样被拒绝。
		bad := []model.PushReading{
			{ID: uuid.NewString(), Platform: "android", ExternalID: external, Metric: "nan_metric", Value: math.NaN(), Unit: "", RecordedAt: base},
			{ID: uuid.NewString(), Platform: "android", ExternalID: external, Metric: "inf_metric", Value: math.Inf(1), Unit: "", RecordedAt: base},
			{ID: uuid.NewString(), Platform: "android", ExternalID: external, Metric: "future_metric", Value: 1, Unit: "", RecordedAt: time.Now().UTC().Add(10 * time.Minute)},
			{ID: uuid.NewString(), Platform: "android", ExternalID: external, Metric: "Temperature", Value: 1, Unit: "", RecordedAt: base},
		}
		for i, r := range bad {
			if _, _, err := st.Push(ctx, userB, model.PushRequest{Readings: []model.PushReading{r}}, store.PushOptions{}); err == nil {
				t.Fatalf("非法读数 %d 未被拒绝", i)
			}
		}

		// 倒序游标分页。
		page1, err := st.ListReadings(ctx, userB, "android", external, model.ReadingsQuery{Limit: 2})
		if err != nil {
			t.Fatalf("第一页失败: %v", err)
		}
		if len(page1.Items) != 2 || !page1.HasMore || page1.NextCursor == "" {
			t.Fatalf("第一页 items=%d hasMore=%v cursor=%q", len(page1.Items), page1.HasMore, page1.NextCursor)
		}
		if page1.Items[0].Value != 14 || page1.Items[1].Value != 13 {
			t.Fatalf("未按 recordedAt 倒序: %+v", page1.Items)
		}
		page2, err := st.ListReadings(ctx, userB, "android", external, model.ReadingsQuery{Limit: 2, Cursor: page1.NextCursor})
		if err != nil {
			t.Fatalf("第二页失败: %v", err)
		}
		if len(page2.Items) != 2 || !page2.HasMore || page2.Items[0].Value != 12 || page2.Items[1].Value != 11 {
			t.Fatalf("第二页异常: %+v hasMore=%v", page2.Items, page2.HasMore)
		}
		page3, err := st.ListReadings(ctx, userB, "android", external, model.ReadingsQuery{Limit: 2, Cursor: page2.NextCursor})
		if err != nil {
			t.Fatalf("第三页失败: %v", err)
		}
		if len(page3.Items) != 1 || page3.HasMore || page3.NextCursor != "" || page3.Items[0].Value != 10 {
			t.Fatalf("第三页异常: %+v hasMore=%v cursor=%q", page3.Items, page3.HasMore, page3.NextCursor)
		}
		if _, err := st.ListReadings(ctx, userB, "android", external, model.ReadingsQuery{Cursor: "not-a-cursor"}); !errors.Is(err, store.ErrBadCursor) {
			t.Fatalf("非法游标 err=%v", err)
		}
		filtered, err := st.ListReadings(ctx, userB, "android", external, model.ReadingsQuery{Metric: "relative_humidity", Limit: 10})
		if err != nil {
			t.Fatalf("metric 过滤失败: %v", err)
		}
		if len(filtered.Items) != 0 {
			t.Fatalf("metric 过滤结果=%+v", filtered.Items)
		}

		// 跨用户与不存在设备统一 404。
		if _, err := st.ListReadings(ctx, userA, "android", external, model.ReadingsQuery{Limit: 10}); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("跨用户 err=%v", err)
		}
		if _, err := st.ListReadings(ctx, userB, "android", "AA:BB:CC:00:00:0A", model.ReadingsQuery{Limit: 10}); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("不存在设备 err=%v", err)
		}
		if _, err := st.ReadingsSummary(ctx, userA, "android", external, "temperature_celsius", base, time.Now().UTC()); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("跨用户 summary err=%v", err)
		}

		// 聚合：10+11+12+13+14。
		summary, err := st.ReadingsSummary(ctx, userB, "android", external, "temperature_celsius", base.Add(-time.Minute), time.Now().UTC())
		if err != nil {
			t.Fatalf("聚合失败: %v", err)
		}
		if summary.Count != 5 || summary.Min == nil || *summary.Min != 10 ||
			summary.Max == nil || *summary.Max != 14 || summary.Avg == nil || *summary.Avg != 12 ||
			summary.Last == nil || *summary.Last != 14 || summary.Unit != "celsius" {
			t.Fatalf("聚合结果异常: %+v", summary)
		}
		empty, err := st.ReadingsSummary(ctx, userB, "android", external, "relative_humidity", base.Add(-time.Minute), time.Now().UTC())
		if err != nil {
			t.Fatalf("无数据聚合失败: %v", err)
		}
		if empty.Count != 0 || empty.Min != nil || empty.Max != nil || empty.Avg != nil || empty.Last != nil {
			t.Fatalf("无数据聚合异常: %+v", empty)
		}

		// 数据库 CHECK 兜底：绕过应用层直接写入 NaN 必须失败。
		if _, err := cleanupDB.Exec(`
			INSERT INTO readings (id, user_id, platform, external_id, metric, value, unit, recorded_at)
			VALUES ($1, $2, 'android', $3, 'db_nan_metric', 'NaN'::double precision, '', $4)`,
			uuid.NewString(), userB, external, base); err == nil {
			t.Fatalf("数据库 CHECK 未拒绝 NaN")
		}

		// 90 天清理：直接插入一条 91 天前的读数，清理后仅旧数据被删。
		legacyID := uuid.NewString()
		if _, err := cleanupDB.Exec(`
			INSERT INTO readings (id, user_id, platform, external_id, metric, value, unit, recorded_at)
			VALUES ($1, $2, 'android', $3, 'legacy_metric', 1, '', $4)`,
			legacyID, userB, external, time.Now().UTC().Add(-91*24*time.Hour)); err != nil {
			t.Fatalf("插入历史读数失败: %v", err)
		}
		deleted, err := st.CleanupReadings(ctx, time.Now().UTC().Add(-90*24*time.Hour))
		if err != nil {
			t.Fatalf("清理失败: %v", err)
		}
		if deleted < 1 {
			t.Fatalf("清理条数=%d", deleted)
		}
		legacy, err := st.ListReadings(ctx, userB, "android", external, model.ReadingsQuery{Metric: "legacy_metric", Limit: 10})
		if err != nil {
			t.Fatalf("清理后查询失败: %v", err)
		}
		if len(legacy.Items) != 0 {
			t.Fatalf("历史读数未被清理: %+v", legacy.Items)
		}
		kept, err := st.ListReadings(ctx, userB, "android", external, model.ReadingsQuery{Limit: 10})
		if err != nil || len(kept.Items) != 5 {
			t.Fatalf("清理误删近期数据: len=%d err=%v", len(kept.Items), err)
		}
	})
	t.Run("读数分桶序列与指标清单", func(t *testing.T) {
		const external = "AA:BB:CC:00:00:0B"
		ctx := context.Background()

		// 固定 UTC 时间，跨整点与跨日边界各有读数，便于断言 UTC 对齐。
		day := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
		mk := func(metric string, value float64, unit string, at time.Time) model.PushReading {
			return model.PushReading{
				ID: uuid.NewString(), Platform: "android", ExternalID: external,
				Metric: metric, Value: value, Unit: unit, RecordedAt: at,
			}
		}
		readings := []model.PushReading{
			mk("temperature_celsius", 10, "celsius", day.Add(59*time.Minute)),
			mk("temperature_celsius", 20, "celsius", day.Add(1*time.Hour)),
			mk("temperature_celsius", 30, "fahrenheit", day.Add(90*time.Minute)),
			mk("temperature_celsius", 40, "celsius", day.Add(1*time.Hour+59*time.Minute)),
			mk("temperature_celsius", 50, "celsius", day.Add(2*time.Hour)),
			mk("temperature_celsius", 60, "celsius", day.Add(24*time.Hour)),
			mk("relative_humidity", 55, "%", day.Add(27*time.Hour)),
		}
		if _, _, err := st.Push(ctx, userB, model.PushRequest{
			ClientID: "series-it",
			Devices: []model.PushDevice{{
				ID: uuid.NewString(), MAC: external, Rules: []byte(`{}`), ScanRecords: []byte(`[]`), UpdatedAt: now,
			}},
			Readings: readings,
		}, store.PushOptions{}); err != nil {
			t.Fatalf("序列测试数据写入失败: %v", err)
		}

		// 1h：按整点对齐，只返回有数据的桶；桶内 min/max/avg/last 全部来自单条 SQL。
		oneHour, err := st.ReadingsSeries(ctx, userB, "android", external, "temperature_celsius",
			day, day.Add(3*time.Hour), "1h")
		if err != nil {
			t.Fatalf("1h 序列失败: %v", err)
		}
		if oneHour.Bucket != "1h" || oneHour.Metric != "temperature_celsius" || oneHour.Unit != "celsius" {
			t.Fatalf("1h 元信息异常: %+v", oneHour)
		}
		if len(oneHour.Buckets) != 3 {
			t.Fatalf("1h 桶数=%d，期望 3", len(oneHour.Buckets))
		}
		starts := []time.Time{day, day.Add(time.Hour), day.Add(2 * time.Hour)}
		for i, b := range oneHour.Buckets {
			if !b.Start.Equal(starts[i]) || !b.End.Equal(starts[i].Add(time.Hour)) {
				t.Fatalf("1h 第 %d 桶边界异常: start=%s end=%s", i, b.Start, b.End)
			}
		}
		b1 := oneHour.Buckets[1]
		if b1.Count != 3 || *b1.Min != 20 || *b1.Max != 40 || *b1.Avg != 30 || *b1.Last != 40 {
			t.Fatalf("1h 桶内聚合异常: %+v", b1)
		}
		if oneHour.Buckets[0].Count != 1 || *oneHour.Buckets[0].Last != 10 ||
			oneHour.Buckets[2].Count != 1 || *oneHour.Buckets[2].Last != 50 {
			t.Fatalf("1h 边界桶异常: %+v", oneHour.Buckets)
		}

		// 1d：对齐 UTC 零点；10-31T00:00Z 的读数归入 10-31 桶，计数与原始条数一致。
		oneDay, err := st.ReadingsSeries(ctx, userB, "android", external, "temperature_celsius",
			day, day.Add(48*time.Hour), "1d")
		if err != nil {
			t.Fatalf("1d 序列失败: %v", err)
		}
		if len(oneDay.Buckets) != 2 {
			t.Fatalf("1d 桶数=%d，期望 2", len(oneDay.Buckets))
		}
		d0, d1 := oneDay.Buckets[0], oneDay.Buckets[1]
		if !d0.Start.Equal(day) || !d0.End.Equal(day.Add(24*time.Hour)) ||
			!d1.Start.Equal(day.Add(24*time.Hour)) || !d1.End.Equal(day.Add(48*time.Hour)) {
			t.Fatalf("1d 桶边界异常: %+v", oneDay.Buckets)
		}
		if d0.Count != 5 || *d0.Min != 10 || *d0.Max != 50 || *d0.Avg != 30 || *d0.Last != 50 {
			t.Fatalf("10-30 桶异常: %+v", d0)
		}
		if d1.Count != 1 || *d1.Min != 60 || *d1.Max != 60 || *d1.Avg != 60 || *d1.Last != 60 {
			t.Fatalf("10-31 桶异常: %+v", d1)
		}
		if sum := d0.Count + d1.Count; sum != int64(len(readings)-1) {
			t.Fatalf("分桶计数合计=%d，原始温度读数=%d", sum, len(readings)-1)
		}

		// 空区间返回空 buckets，而不是 404 或 null。
		emptySeries, err := st.ReadingsSeries(ctx, userB, "android", external, "temperature_celsius",
			day.Add(-48*time.Hour), day.Add(-24*time.Hour), "1h")
		if err != nil {
			t.Fatalf("空区间失败: %v", err)
		}
		if len(emptySeries.Buckets) != 0 {
			t.Fatalf("空区间 buckets=%+v", emptySeries.Buckets)
		}

		// 非法参数在存储层同样被拒绝，供 HTTP 层映射为 400。
		if _, err := st.ReadingsSeries(ctx, userB, "android", external, "temperature_celsius",
			day, day.Add(time.Hour), "5m"); !errors.Is(err, validate.ErrInvalidBucket) {
			t.Fatalf("非法 bucket err=%v", err)
		}
		if _, err := st.ReadingsSeries(ctx, userB, "android", external, "temperature_celsius",
			day.Add(-100*24*time.Hour), day, "1h"); !errors.Is(err, validate.ErrInvalidSeriesRange) {
			t.Fatalf("超 90 天 err=%v", err)
		}
		if _, err := st.ReadingsSeries(ctx, userB, "android", external, "temperature_celsius",
			day, day.Add(10*time.Hour), "1m"); !errors.Is(err, validate.ErrTooManyBuckets) {
			t.Fatalf("超 500 桶 err=%v", err)
		}

		// 跨用户与不存在设备统一 404。
		if _, err := st.ReadingsSeries(ctx, userA, "android", external, "temperature_celsius",
			day, day.Add(3*time.Hour), "1h"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("跨用户序列 err=%v", err)
		}
		if _, err := st.ReadingsSeries(ctx, userB, "android", "AA:BB:CC:00:00:0C", "temperature_celsius",
			day, day.Add(3*time.Hour), "1h"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("不存在设备序列 err=%v", err)
		}
		if _, err := st.ReadingsMetrics(ctx, userA, "android", external); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("跨用户指标清单 err=%v", err)
		}

		// 指标清单：按 lastAt 倒序；unit 取最近一条；count/firstAt/lastAt 正确。
		metrics, err := st.ReadingsMetrics(ctx, userB, "android", external)
		if err != nil {
			t.Fatalf("指标清单失败: %v", err)
		}
		if len(metrics.Items) != 2 {
			t.Fatalf("指标数量=%d，期望 2", len(metrics.Items))
		}
		humidity, temp := metrics.Items[0], metrics.Items[1]
		if humidity.Metric != "relative_humidity" || humidity.Unit != "%" || humidity.Count != 1 ||
			!humidity.FirstAt.Equal(day.Add(27*time.Hour)) || !humidity.LastAt.Equal(day.Add(27*time.Hour)) {
			t.Fatalf("湿度指标异常: %+v", humidity)
		}
		if temp.Metric != "temperature_celsius" || temp.Unit != "celsius" || temp.Count != 6 ||
			!temp.FirstAt.Equal(day.Add(59*time.Minute)) || !temp.LastAt.Equal(day.Add(24*time.Hour)) {
			t.Fatalf("温度指标异常: %+v", temp)
		}

		// 无读数的已注册设备：指标清单返回空数组。
		const emptyDevice = "AA:BB:CC:00:00:0D"
		if _, _, err := st.Push(ctx, userB, model.PushRequest{Devices: []model.PushDevice{{
			ID: uuid.NewString(), MAC: emptyDevice, Rules: []byte(`{}`), ScanRecords: []byte(`[]`), UpdatedAt: now,
		}}}, store.PushOptions{}); err != nil {
			t.Fatalf("空设备写入失败: %v", err)
		}
		emptyMetrics, err := st.ReadingsMetrics(ctx, userB, "android", emptyDevice)
		if err != nil {
			t.Fatalf("空调指标清单失败: %v", err)
		}
		if len(emptyMetrics.Items) != 0 {
			t.Fatalf("空调指标清单=%+v", emptyMetrics.Items)
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

// strPtr 用于构造可缺省字段：非 nil 表示显式上报，nil 表示未上报。
func strPtr(s string) *string { return &s }

func intPtr(v int) *int { return &v }
