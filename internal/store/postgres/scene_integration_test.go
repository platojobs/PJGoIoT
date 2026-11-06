package postgres_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/platojobs/PJGoIoT/internal/model"
	"github.com/platojobs/PJGoIoT/internal/store"
	"github.com/platojobs/PJGoIoT/internal/store/postgres"
)

// TestPostgresSceneIntegration 覆盖 v0.20.0 的场景上云：
// 本人限定、字段级 LWW、非法组合、deviceMissing、软删 tombstone 与 change_log 可见性。
func TestPostgresSceneIntegration(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("未设置 TEST_DATABASE_URL，跳过真实数据库集成测试")
	}
	runMigrations(t, dsn)

	ctx := context.Background()
	st, err := postgres.New(ctx, dsn)
	if err != nil {
		t.Fatalf("连接数据库失败: %v", err)
	}
	defer st.Close()

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	defer db.Close()

	// pushSceneDevice 上报一台 android 设备，供场景引用。
	pushSceneDevice := func(t *testing.T, userID, externalID string, at time.Time, deleted bool) {
		t.Helper()
		if _, _, err := st.Push(ctx, userID, model.PushRequest{
			ClientID: "scene-it",
			Devices: []model.PushDevice{{
				ID: uuid.NewString(), Platform: "android", ExternalID: externalID, MAC: externalID,
				Alias: strPtr("场景设备"), Rules: []byte(`{}`), ScanRecords: []byte(`[]`),
				UpdatedAt: at, Deleted: deleted,
			}},
		}, store.PushOptions{}); err != nil {
			t.Fatalf("上报设备失败: %v", err)
		}
	}

	deviceOffline := func(externalID string) model.SceneTrigger {
		return model.SceneTrigger{Kind: "device_offline", Device: &model.SceneDeviceRef{
			Platform: "android", ExternalID: externalID,
		}}
	}
	notifyAction := []model.SceneAction{{Kind: "notify", Title: "设备离线", Body: "{{alias}} 已离线"}}

	t.Run("CRUD 与本人限定", func(t *testing.T) {
		ownerID, _ := createAccount(t, st, db, "scene-owner")
		cleanupAccount(t, db, ownerID)
		otherID, _ := createAccount(t, st, db, "scene-other")
		cleanupAccount(t, db, otherID)

		ext := "AA:BB:CC:00:00:31"
		pushSceneDevice(t, ownerID, ext, time.Now().UTC().Add(-time.Hour), false)

		created, err := st.CreateScene(ctx, ownerID, model.SceneInput{
			Name: "回家开灯", Trigger: deviceOffline(ext), Actions: notifyAction,
		})
		if err != nil {
			t.Fatalf("创建场景失败: %v", err)
		}
		if created.Version != 1 || created.DeviceMissing || created.CooldownSeconds != 300 || !created.Enabled {
			t.Fatalf("创建结果异常: %+v", created)
		}

		list, err := st.ListScenes(ctx, ownerID)
		if err != nil || len(list) != 1 || list[0].ID != created.ID {
			t.Fatalf("本人列表异常: items=%+v err=%v", list, err)
		}
		if _, err := st.GetScene(ctx, ownerID, created.ID); err != nil {
			t.Fatalf("本人读取失败: %v", err)
		}
		if _, err := st.GetScene(ctx, otherID, created.ID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("跨用户读取应为 404，得到 %v", err)
		}
		if list, err := st.ListScenes(ctx, otherID); err != nil || len(list) != 0 {
			t.Fatalf("跨用户列表应为空: items=%+v err=%v", list, err)
		}
		if _, err := st.UpdateScene(ctx, otherID, created.ID, model.ScenePatch{
			Name: &model.SceneField[string]{Value: "劫持", UpdatedAt: time.Now().UTC()},
		}, time.Now().UTC()); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("跨用户更新应为 404，得到 %v", err)
		}
		if err := st.DeleteScene(ctx, otherID, created.ID, time.Now().UTC()); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("跨用户删除应为 404，得到 %v", err)
		}
	})

	t.Run("字段级 LWW 并发与过期写入", func(t *testing.T) {
		userID, _ := createAccount(t, st, db, "scene-lww")
		cleanupAccount(t, db, userID)

		created, err := st.CreateScene(ctx, userID, model.SceneInput{
			Name: "定时提醒",
			Trigger: model.SceneTrigger{
				Kind: "schedule", TimeOfDay: "08:00", Timezone: "Asia/Shanghai",
			},
			Actions: []model.SceneAction{{Kind: "log", Message: "早上好", Level: "info"}},
		})
		if err != nil {
			t.Fatalf("创建定时场景失败: %v", err)
		}

		base := time.Now().UTC()
		var wg sync.WaitGroup
		errs := make(chan error, 2)
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, err := st.UpdateScene(ctx, userID, created.ID, model.ScenePatch{
				Name: &model.SceneField[string]{Value: "晨间提醒", UpdatedAt: base.Add(1 * time.Second)},
			}, time.Now().UTC())
			errs <- err
		}()
		go func() {
			defer wg.Done()
			_, err := st.UpdateScene(ctx, userID, created.ID, model.ScenePatch{
				Enabled: &model.SceneField[bool]{Value: false, UpdatedAt: base.Add(2 * time.Second)},
			}, time.Now().UTC())
			errs <- err
		}()
		wg.Wait()
		close(errs)
		for e := range errs {
			if e != nil {
				t.Fatalf("并发字段更新失败: %v", e)
			}
		}

		got, err := st.GetScene(ctx, userID, created.ID)
		if err != nil {
			t.Fatalf("并发更新后读取失败: %v", err)
		}
		if got.Name != "晨间提醒" || got.Enabled {
			t.Fatalf("两个请求改不同字段被互相覆盖: %+v", got)
		}
		if got.Version != 3 {
			t.Fatalf("两次应用应让 version=3，得到 %d", got.Version)
		}

		// 过期 name + 较新 cooldown：只有 cooldown 被应用，name 保持并发写入的结果。
		updated, err := st.UpdateScene(ctx, userID, created.ID, model.ScenePatch{
			Name:            &model.SceneField[string]{Value: "旧名字", UpdatedAt: base},
			CooldownSeconds: &model.SceneField[int]{Value: 120, UpdatedAt: base.Add(3 * time.Second)},
		}, time.Now().UTC())
		if err != nil {
			t.Fatalf("混合补丁更新失败: %v", err)
		}
		if updated.Name != "晨间提醒" || updated.CooldownSeconds != 120 || updated.Version != 4 {
			t.Fatalf("字段级 LWW 结果异常: %+v", updated)
		}

		// 同时间戳、同内容视为幂等 no-op：不改 version、不写 change_log。
		beforeVersion := updated.Version
		same, err := st.UpdateScene(ctx, userID, created.ID, model.ScenePatch{
			Name: &model.SceneField[string]{Value: "晨间提醒", UpdatedAt: base.Add(1 * time.Second)},
		}, time.Now().UTC())
		if err != nil {
			t.Fatalf("幂等补丁失败: %v", err)
		}
		if same.Version != beforeVersion {
			t.Fatalf("幂等补丁不应改变 version: %d -> %d", beforeVersion, same.Version)
		}
		var sceneChanges int
		if err := db.QueryRow(`SELECT COUNT(*) FROM change_log
			WHERE user_id = $1 AND entity = 'scene' AND entity_id = $2`, userID, created.ID).
			Scan(&sceneChanges); err != nil {
			t.Fatalf("统计场景变更失败: %v", err)
		}
		// create + name + enabled + cooldown 各一条，幂等补丁不写。
		if sceneChanges != 4 {
			t.Fatalf("场景变更条数异常: got=%d want=4", sceneChanges)
		}
	})

	t.Run("非法组合与设备归属", func(t *testing.T) {
		userID, _ := createAccount(t, st, db, "scene-invalid")
		cleanupAccount(t, db, userID)

		ext := "AA:BB:CC:00:00:41"
		pushSceneDevice(t, userID, ext, time.Now().UTC().Add(-time.Hour), false)

		// 引用不存在的设备。
		if _, err := st.CreateScene(ctx, userID, model.SceneInput{
			Name: "孤儿设备", Trigger: deviceOffline("AA:BB:CC:00:00:99"), Actions: notifyAction,
		}); !errors.Is(err, store.ErrSceneDeviceNotFound) {
			t.Fatalf("未知设备应为 ErrSceneDeviceNotFound，得到 %v", err)
		}

		// 设备类型存在，但该类型不支持该指标。
		metricTrigger := model.SceneTrigger{
			Kind:       "metric_threshold",
			Device:     &model.SceneDeviceRef{Platform: "android", ExternalID: ext},
			DeviceType: "smart_scale",
			Metric:     &model.SceneMetricCondition{Metric: "temperature_celsius", Comparator: "lt", Threshold: 18, Consecutive: 3},
		}
		if _, err := st.CreateScene(ctx, userID, model.SceneInput{
			Name: "指标越界", Trigger: metricTrigger, Actions: notifyAction,
		}); !errors.Is(err, store.ErrSceneMetricNotSupported) {
			t.Fatalf("不支持指标应为 ErrSceneMetricNotSupported，得到 %v", err)
		}

		// 设备类型不存在。
		metricTrigger.DeviceType = "not_a_real_type"
		if _, err := st.CreateScene(ctx, userID, model.SceneInput{
			Name: "未知类型", Trigger: metricTrigger, Actions: notifyAction,
		}); !errors.Is(err, store.ErrSceneMetricNotSupported) {
			t.Fatalf("未知设备类型应为 ErrSceneMetricNotSupported，得到 %v", err)
		}

		// 合法指标应可创建。
		metricTrigger.DeviceType = "environmental_sensor"
		metricTrigger.Metric.Metric = "temperature_celsius"
		ok, err := st.CreateScene(ctx, userID, model.SceneInput{
			Name: "低温提醒", Trigger: metricTrigger, Actions: notifyAction,
		})
		if err != nil {
			t.Fatalf("合法指标场景创建失败: %v", err)
		}
		if ok.DeviceMissing {
			t.Fatalf("设备存在时 deviceMissing 应为 false: %+v", ok)
		}
	})

	t.Run("设备删除后 deviceMissing 且场景保留", func(t *testing.T) {
		userID, _ := createAccount(t, st, db, "scene-missing")
		cleanupAccount(t, db, userID)

		ext := "AA:BB:CC:00:00:51"
		pushSceneDevice(t, userID, ext, time.Now().UTC().Add(-time.Hour), false)
		created, err := st.CreateScene(ctx, userID, model.SceneInput{
			Name: "离线告警", Trigger: deviceOffline(ext), Actions: notifyAction,
		})
		if err != nil {
			t.Fatalf("创建场景失败: %v", err)
		}
		if created.DeviceMissing {
			t.Fatalf("设备存在时 deviceMissing 应为 false")
		}

		pushSceneDevice(t, userID, ext, time.Now().UTC(), true)

		got, err := st.GetScene(ctx, userID, created.ID)
		if err != nil {
			t.Fatalf("设备删除后场景不应消失: %v", err)
		}
		if !got.DeviceMissing {
			t.Fatalf("设备删除后 deviceMissing 应为 true: %+v", got)
		}
		if got.Version != created.Version {
			t.Fatalf("设备删除不应改动场景版本: %d -> %d", created.Version, got.Version)
		}
	})

	t.Run("软删 tombstone 与 change_log 可见性", func(t *testing.T) {
		userID, _ := createAccount(t, st, db, "scene-delete")
		cleanupAccount(t, db, userID)
		otherID, _ := createAccount(t, st, db, "scene-delete-other")
		cleanupAccount(t, db, otherID)

		ext := "AA:BB:CC:00:00:61"
		pushSceneDevice(t, userID, ext, time.Now().UTC().Add(-time.Hour), false)
		created, err := st.CreateScene(ctx, userID, model.SceneInput{
			Name: "待删场景", Trigger: deviceOffline(ext), Actions: notifyAction,
		})
		if err != nil {
			t.Fatalf("创建场景失败: %v", err)
		}

		var payload []byte
		var version int64
		if err := db.QueryRow(`SELECT payload, (payload->>'version')::bigint FROM change_log
			WHERE user_id = $1 AND entity = 'scene' AND entity_id = $2 AND op = 'upsert'
			ORDER BY seq DESC LIMIT 1`, userID, created.ID).Scan(&payload, &version); err != nil {
			t.Fatalf("读取 upsert 变更失败: %v", err)
		}
		var payloadScene model.Scene
		if err := json.Unmarshal(payload, &payloadScene); err != nil {
			t.Fatalf("解析 upsert payload 失败: %v", err)
		}
		if payloadScene.ID != created.ID || payloadScene.Trigger.Kind != "device_offline" || payloadScene.Version != 1 {
			t.Fatalf("upsert payload 不是完整可重建状态: %+v", payloadScene)
		}

		if err := st.DeleteScene(ctx, userID, created.ID, time.Now().UTC()); err != nil {
			t.Fatalf("删除场景失败: %v", err)
		}
		if _, err := st.GetScene(ctx, userID, created.ID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("软删后读取应为 404，得到 %v", err)
		}
		if list, err := st.ListScenes(ctx, userID); err != nil || len(list) != 0 {
			t.Fatalf("软删后列表应为空: items=%+v err=%v", list, err)
		}

		var deletedAt sql.NullTime
		var rowVersion int64
		if err := db.QueryRow(`SELECT deleted_at, version FROM scenes WHERE id = $1`, created.ID).
			Scan(&deletedAt, &rowVersion); err != nil {
			t.Fatalf("读取场景行失败: %v", err)
		}
		if !deletedAt.Valid || rowVersion != 2 {
			t.Fatalf("软删落库异常: deletedAt=%v version=%d", deletedAt, rowVersion)
		}

		var deletePayload []byte
		if err := db.QueryRow(`SELECT payload FROM change_log
			WHERE user_id = $1 AND entity = 'scene' AND entity_id = $2 AND op = 'delete'
			ORDER BY seq DESC LIMIT 1`, userID, created.ID).Scan(&deletePayload); err != nil {
			t.Fatalf("读取 delete 变更失败: %v", err)
		}
		var tombstone model.Scene
		if err := json.Unmarshal(deletePayload, &tombstone); err != nil {
			t.Fatalf("解析 delete payload 失败: %v", err)
		}
		if tombstone.DeletedAt == nil || tombstone.Version != 2 {
			t.Fatalf("delete payload 缺少 tombstone 信息: %+v", tombstone)
		}

		// 其他用户的 changes 不应看到该场景（scene 的 household_id 恒为 NULL）。
		page, err := st.Changes(ctx, otherID, 0, 100)
		if err != nil {
			t.Fatalf("读取其他用户变更失败: %v", err)
		}
		for _, ch := range page.Changes {
			if ch.Entity == "scene" {
				t.Fatalf("跨用户 changes 泄露场景: %+v", ch)
			}
		}
		// 本人 changes 能看到 upsert 与 delete。
		own, err := st.Changes(ctx, userID, 0, 100)
		if err != nil {
			t.Fatalf("读取本人变更失败: %v", err)
		}
		var upserts, deletes int
		for _, ch := range own.Changes {
			if ch.Entity != "scene" || ch.EntityID != created.ID {
				continue
			}
			if ch.Op == "upsert" {
				upserts++
			}
			if ch.Op == "delete" {
				deletes++
			}
		}
		if upserts != 1 || deletes != 1 {
			t.Fatalf("场景变更可见性异常: upserts=%d deletes=%d", upserts, deletes)
		}
	})
}
