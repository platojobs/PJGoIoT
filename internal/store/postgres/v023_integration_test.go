package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/platojobs/PJGoIoT/internal/model"
	"github.com/platojobs/PJGoIoT/internal/store"
	"github.com/platojobs/PJGoIoT/internal/store/postgres"
)

// TestPostgresPushIntegration 覆盖 v0.23.0 的推送令牌与投递队列：
// 注册幂等、跨用户隔离、告警同事务入队、抢占/退避/崩溃回收、dry-run 落库、
// 令牌失效置 inactive 与过期清理。
func TestPostgresPushIntegration(t *testing.T) {
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

	t.Run("注册幂等与跨用户隔离", func(t *testing.T) {
		owner, _ := createAccount(t, st, db, "push-owner")
		cleanupAccount(t, db, owner)
		other, _ := createAccount(t, st, db, "push-other")
		cleanupAccount(t, db, other)

		now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
		in := model.PushRegistrationInput{
			Platform: "android", Token: "tok-aaa", AppVersion: "1.0.0", DeviceModel: "Pixel 8",
		}
		first, err := st.UpsertPushDevice(ctx, owner, in, now)
		if err != nil {
			t.Fatalf("首次注册失败: %v", err)
		}
		if first.Status != "active" {
			t.Fatalf("首次注册状态应为 active，得到 %q", first.Status)
		}

		in.AppVersion = "1.2.0"
		in.DeviceModel = "Pixel 9"
		second, err := st.UpsertPushDevice(ctx, owner, in, now.Add(time.Minute))
		if err != nil {
			t.Fatalf("重复注册失败: %v", err)
		}
		if second.ID != first.ID {
			t.Fatalf("同一用户同一 token 应命中同一条记录，first=%s second=%s", first.ID, second.ID)
		}
		if second.AppVersion != "1.2.0" || second.DeviceModel != "Pixel 9" {
			t.Fatalf("幂等更新未生效: %+v", second)
		}
		if n := countRows(t, db, `SELECT count(*) FROM push_devices WHERE user_id = $1`, owner); n != 1 {
			t.Fatalf("应只有一条令牌记录，得到 %d", n)
		}

		// 另一个用户使用同一 token 是独立记录（唯一约束是 user_id + token）。
		if _, err := st.UpsertPushDevice(ctx, other, model.PushRegistrationInput{
			Platform: "android", Token: "tok-aaa", AppVersion: "1.0.0",
		}, now); err != nil {
			t.Fatalf("跨用户注册同一 token 失败: %v", err)
		}

		if err := st.DeactivatePushDevice(ctx, other, first.ID, now); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("跨用户 DELETE 应为 ErrNotFound，得到 %v", err)
		}
		if err := st.DeactivatePushDevice(ctx, owner, first.ID, now); err != nil {
			t.Fatalf("本人停用失败: %v", err)
		}
		if err := st.DeactivatePushDevice(ctx, owner, first.ID, now); err != nil {
			t.Fatalf("重复停用应幂等成功，得到 %v", err)
		}
		var status string
		if err := db.QueryRow(`SELECT status FROM push_devices WHERE id = $1`, first.ID).Scan(&status); err != nil {
			t.Fatalf("读取令牌状态失败: %v", err)
		}
		if status != "inactive" {
			t.Fatalf("停用后状态应为 inactive，得到 %q", status)
		}

		// 非法参数在存储层同样被拒绝（HTTP 层会映射为 400）。
		if _, err := st.UpsertPushDevice(ctx, owner, model.PushRegistrationInput{
			Platform: "web", Token: "tok-bbb",
		}, now); !errors.Is(err, store.ErrPushDeviceInvalid) {
			t.Fatalf("非法 platform 应返回 ErrPushDeviceInvalid，得到 %v", err)
		}
		if _, err := st.UpsertPushDevice(ctx, owner, model.PushRegistrationInput{
			Platform: "ios", Token: "  ",
		}, now); !errors.Is(err, store.ErrPushDeviceInvalid) {
			t.Fatalf("空 token 应返回 ErrPushDeviceInvalid，得到 %v", err)
		}
	})

	t.Run("告警入队与投递状态机", func(t *testing.T) {
		owner, _ := createAccount(t, st, db, "push-queue")
		cleanupAccount(t, db, owner)
		other, _ := createAccount(t, st, db, "push-queue-other")
		cleanupAccount(t, db, other)

		base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
		android, err := st.UpsertPushDevice(ctx, owner, model.PushRegistrationInput{
			Platform: "android", Token: "tok-android",
		}, base)
		if err != nil {
			t.Fatalf("注册 android 令牌失败: %v", err)
		}
		ios, err := st.UpsertPushDevice(ctx, owner, model.PushRegistrationInput{
			Platform: "ios", Token: "tok-ios",
		}, base)
		if err != nil {
			t.Fatalf("注册 ios 令牌失败: %v", err)
		}
		if _, err := st.UpsertPushDevice(ctx, other, model.PushRegistrationInput{
			Platform: "android", Token: "tok-other",
		}, base); err != nil {
			t.Fatalf("注册他人令牌失败: %v", err)
		}

		alertInput := model.AlertInput{
			UserID: owner, Kind: "device_offline", Title: "设备离线",
			Body: "客厅传感器已离线", Severity: "warning",
			Payload: map[string]string{"externalId": "AA:BB"},
		}
		alert, count, err := st.CreateAlertWithDeliveries(ctx, alertInput, base)
		if err != nil {
			t.Fatalf("创建告警失败: %v", err)
		}
		if count != 2 {
			t.Fatalf("应为本人两个 active 令牌入队，得到 %d", count)
		}
		if n := countRows(t, db, `SELECT count(*) FROM push_deliveries WHERE user_id = $1`, owner); n != 2 {
			t.Fatalf("投递记录数应为 2，得到 %d", n)
		}
		if n := countRows(t, db, `SELECT count(*) FROM push_deliveries WHERE user_id = $1`, other); n != 0 {
			t.Fatalf("不应为其他用户入队，得到 %d", n)
		}
		var snapToken string
		var snapshotData []byte
		if err := db.QueryRow(`SELECT token, data FROM push_deliveries WHERE alert_id = $1 AND platform = 'android'`, alert.ID).
			Scan(&snapToken, &snapshotData); err != nil {
			t.Fatalf("读取投递快照失败: %v", err)
		}
		if snapToken != "tok-android" {
			t.Fatalf("投递快照 token 不匹配: %q", snapToken)
		}
		if string(snapshotData) == "" {
			t.Fatal("投递快照 data 不应为空")
		}

		claimed, err := st.ClaimPushDeliveries(ctx, base, 10, base.Add(-5*time.Minute))
		if err != nil {
			t.Fatalf("抢占失败: %v", err)
		}
		if len(claimed) != 2 {
			t.Fatalf("应抢占 2 条，得到 %d", len(claimed))
		}
		for _, d := range claimed {
			if d.Attempts != 1 {
				t.Fatalf("首次抢占后 attempts 应为 1，得到 %d", d.Attempts)
			}
			if d.Data["kind"] != "device_offline" || d.Data["severity"] != "warning" {
				t.Fatalf("投递 data 缺少告警元信息: %+v", d.Data)
			}
		}

		// dry-run：无凭据时记录完整载荷并置 sent，不卡队列。
		var dryID, retryID string
		for _, d := range claimed {
			if d.Platform == "android" {
				dryID = d.ID
			} else {
				retryID = d.ID
			}
		}
		if err := st.CompletePushDelivery(ctx, dryID, "fcm-dry-run", true, base); err != nil {
			t.Fatalf("写回 dry-run 成功失败: %v", err)
		}
		var dryRun bool
		var provider, status string
		if err := db.QueryRow(`SELECT dry_run, provider, status FROM push_deliveries WHERE id = $1`, dryID).
			Scan(&dryRun, &provider, &status); err != nil {
			t.Fatalf("读取 dry-run 投递失败: %v", err)
		}
		if !dryRun || provider != "fcm-dry-run" || status != "sent" {
			t.Fatalf("dry-run 落库异常: dry_run=%v provider=%q status=%q", dryRun, provider, status)
		}

		// 可重试错误：退避期内不再被抢占，到期后 attempts 递增。
		if err := st.RetryPushDelivery(ctx, retryID, base.Add(30*time.Second), "provider 503"); err != nil {
			t.Fatalf("安排重试失败: %v", err)
		}
		if again, err := st.ClaimPushDeliveries(ctx, base, 10, base.Add(-5*time.Minute)); err != nil || len(again) != 0 {
			t.Fatalf("退避期内不应再次抢占: n=%d err=%v", len(again), err)
		}
		again, err := st.ClaimPushDeliveries(ctx, base.Add(30*time.Second), 10, base.Add(25*time.Second))
		if err != nil || len(again) != 1 {
			t.Fatalf("退避到期应重新抢占: n=%d err=%v", len(again), err)
		}
		if again[0].Attempts != 2 {
			t.Fatalf("第二次抢占 attempts 应为 2，得到 %d", again[0].Attempts)
		}

		// 终态错误（unregistered）：投递 failed，且同一事务把令牌置 inactive。
		if err := st.FailPushDelivery(ctx, again[0].ID, "APNs 令牌失效: Unregistered", true, base.Add(30*time.Second)); err != nil {
			t.Fatalf("标记终态失败: %v", err)
		}
		if status := tokenStatus(t, db, ios.ID); status != "inactive" {
			t.Fatalf("失效令牌应置 inactive，得到 %q", status)
		}

		// 进程崩溃恢复：sending 超过阈值后回收为 pending 并重新抢占。
		third, _, err := st.CreateAlertWithDeliveries(ctx, alertInput, base.Add(time.Minute))
		if err != nil {
			t.Fatalf("创建第二条告警失败: %v", err)
		}
		_ = third
		crashClaim, err := st.ClaimPushDeliveries(ctx, base.Add(time.Minute), 10, base.Add(-5*time.Minute))
		if err != nil || len(crashClaim) != 1 {
			t.Fatalf("应抢占 1 条（ios 已失效）：n=%d err=%v", len(crashClaim), err)
		}
		if _, err := db.Exec(`UPDATE push_deliveries SET claimed_at = $1 WHERE status = 'sending'`,
			base.Add(-10*time.Minute)); err != nil {
			t.Fatalf("模拟停滞 sending 失败: %v", err)
		}
		recovered, err := st.ClaimPushDeliveries(ctx, base.Add(time.Minute), 10, base.Add(-5*time.Minute))
		if err != nil || len(recovered) != 1 {
			t.Fatalf("停滞发送应被回收并重新抢占：n=%d err=%v", len(recovered), err)
		}
		if recovered[0].Attempts != 2 {
			t.Fatalf("回收后 attempts 应为 2，得到 %d", recovered[0].Attempts)
		}

		// 过期清理：终态投递按 created_at、inactive 令牌按 updated_at 删除。
		if _, err := db.Exec(`UPDATE push_deliveries SET created_at = $1 WHERE id = $2`,
			base.Add(-10*24*time.Hour), dryID); err != nil {
			t.Fatalf("回填投递时间失败: %v", err)
		}
		if _, err := db.Exec(`UPDATE push_devices SET updated_at = $1 WHERE id = $2`,
			base.Add(-40*24*time.Hour), ios.ID); err != nil {
			t.Fatalf("回填令牌时间失败: %v", err)
		}
		n, err := st.CleanupPushDeliveries(ctx, base.Add(-7*24*time.Hour))
		if err != nil || n != 1 {
			t.Fatalf("应清理 1 条终态投递：n=%d err=%v", n, err)
		}
		devices, err := st.CleanupPushDevices(ctx, base.Add(-30*24*time.Hour))
		if err != nil || devices != 1 {
			t.Fatalf("应清理 1 条失效令牌：n=%d err=%v", devices, err)
		}
		if n := countRows(t, db, `SELECT count(*) FROM push_devices WHERE id = $1`, android.ID); n != 1 {
			t.Fatalf("active 令牌不应被清理，得到 %d", n)
		}
	})
}

// TestPostgresScenePaginationIntegration 覆盖 /scenes 的可选游标分页：
// 不传 limit 返回全量；limit+游标可完整遍历；非法游标返回 ErrBadCursor。
func TestPostgresScenePaginationIntegration(t *testing.T) {
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

	userID, _ := createAccount(t, st, db, "scene-page")
	cleanupAccount(t, db, userID)

	ext := "AA:BB:CC:00:00:51"
	if _, _, err := st.Push(ctx, userID, model.PushRequest{
		ClientID: "scene-page",
		Devices: []model.PushDevice{{
			ID: uuid.NewString(), Platform: "android", ExternalID: ext, MAC: ext,
			Alias: strPtr("分页设备"), Rules: []byte(`{}`), ScanRecords: []byte(`[]`),
			UpdatedAt: time.Now().UTC(), Deleted: false,
		}},
	}, store.PushOptions{}); err != nil {
		t.Fatalf("上报设备失败: %v", err)
	}
	trigger := model.SceneTrigger{Kind: "device_offline", Device: &model.SceneDeviceRef{
		Platform: "android", ExternalID: ext,
	}}
	ids := make([]string, 0, 3)
	for i := 0; i < 3; i++ {
		created, err := st.CreateScene(ctx, userID, model.SceneInput{
			Name:    "分页场景",
			Trigger: trigger,
			Actions: []model.SceneAction{{Kind: "notify", Title: "离线", Body: "{{alias}}"}},
		})
		if err != nil {
			t.Fatalf("创建场景失败: %v", err)
		}
		ids = append(ids, created.ID)
		// 固定 updated_at，确保游标顺序可复现（DESC updated_at, ASC id）。
		if _, err := db.Exec(`UPDATE scenes SET updated_at = $2 WHERE id = $1`,
			created.ID, time.Date(2026, 11, 1, 0, i, 0, 0, time.UTC)); err != nil {
			t.Fatalf("回填场景时间失败: %v", err)
		}
	}

	full, err := st.ListScenes(ctx, userID, store.SceneQuery{})
	if err != nil {
		t.Fatalf("全量列表失败: %v", err)
	}
	if len(full.Items) != 3 || full.HasMore || full.NextCursor != "" {
		t.Fatalf("不传 limit 应返回全量且无分页字段: %+v", full)
	}

	var got []string
	cursor := ""
	for page := 0; page < 4; page++ {
		res, err := st.ListScenes(ctx, userID, store.SceneQuery{Limit: 1, Cursor: cursor})
		if err != nil {
			t.Fatalf("分页第 %d 页失败: %v", page+1, err)
		}
		if len(res.Items) != 1 {
			t.Fatalf("limit=1 每页应返回 1 条，得到 %d", len(res.Items))
		}
		got = append(got, res.Items[0].ID)
		if !res.HasMore {
			break
		}
		cursor = res.NextCursor
	}
	if len(got) != 3 {
		t.Fatalf("游标遍历应覆盖 3 条，得到 %d: %v", len(got), got)
	}
	seen := map[string]bool{}
	for _, id := range got {
		if seen[id] {
			t.Fatalf("游标遍历出现重复: %v", got)
		}
		seen[id] = true
	}
	// 期望顺序：updated_at 最新（i=2）在前。
	if got[0] != ids[2] || got[2] != ids[0] {
		t.Fatalf("分页顺序不符合 updated_at DESC: %v", got)
	}

	if _, err := st.ListScenes(ctx, userID, store.SceneQuery{Limit: 1, Cursor: "not-a-cursor"}); !errors.Is(err, store.ErrBadCursor) {
		t.Fatalf("非法游标应为 ErrBadCursor，得到 %v", err)
	}
}

func tokenStatus(t *testing.T, db *sql.DB, id string) string {
	t.Helper()
	var status string
	if err := db.QueryRow(`SELECT status FROM push_devices WHERE id = $1`, id).Scan(&status); err != nil {
		t.Fatalf("读取令牌状态失败: %v", err)
	}
	return status
}
