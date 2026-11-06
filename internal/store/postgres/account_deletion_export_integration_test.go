package postgres_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/platojobs/PJGoIoT/internal/auth"
	"github.com/platojobs/PJGoIoT/internal/model"
	"github.com/platojobs/PJGoIoT/internal/store"
	"github.com/platojobs/PJGoIoT/internal/store/postgres"
)

// TestPostgresAccountDeletionExportIntegration 覆盖 v0.19.0 的账号删除与异步个人数据导出。
func TestPostgresAccountDeletionExportIntegration(t *testing.T) {
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

	t.Run("删除令牌与逐表清空", func(t *testing.T) {
		userID, tokenHash := createAccount(t, st, db, "deletion")
		cleanupAccount(t, db, userID)

		base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
		if _, _, err := st.Push(ctx, userID, model.PushRequest{
			ClientID: "deletion-it",
			Devices: []model.PushDevice{{
				ID: uuid.NewString(), Platform: "android", ExternalID: "AA:BB:CC:00:00:21",
				MAC: "AA:BB:CC:00:00:21", Alias: strPtr("待删设备"), Rules: []byte(`{}`),
				ScanRecords: []byte(`[]`), UpdatedAt: base,
			}},
			Readings: []model.PushReading{
				{ID: uuid.NewString(), Platform: "android", ExternalID: "AA:BB:CC:00:00:21", Metric: "temperature_celsius", Value: 21, Unit: "celsius", RecordedAt: base},
				{ID: uuid.NewString(), Platform: "android", ExternalID: "AA:BB:CC:00:00:21", Metric: "temperature_celsius", Value: 23, Unit: "celsius", RecordedAt: base.Add(time.Minute)},
			},
		}, store.PushOptions{IdempotencyKey: "deletion-push", RequestHash: "deletion-hash"}); err != nil {
			t.Fatalf("准备数据 push: %v", err)
		}

		code, _ := auth.NewPairingCode()
		if err := st.CreatePairingCode(ctx, userID, auth.PairingCodeHash(code), time.Now().UTC().Add(10*time.Minute)); err != nil {
			t.Fatalf("配对码: %v", err)
		}
		if err := st.RecordCatalogAccess(ctx, userID, 1); err != nil {
			t.Fatalf("目录访问记录: %v", err)
		}
		feedback, _, err := st.CreateFeedback(ctx, userID, model.FeedbackSubmission{
			Kind: "bug", Body: "注销测试反馈", Platform: "android",
			AppVersion: "1.0", BuildType: "release", OSVersion: "14", DeviceModel: "Pixel",
			Contact: strPtr("user@example.com"), Diagnostics: strPtr("diag-text"),
		}, store.FeedbackOptions{IdempotencyKey: "deletion-feedback", RequestHash: "fb-hash"})
		if err != nil {
			t.Fatalf("反馈: %v", err)
		}
		// 产生一条状态审计记录，验证删除范围覆盖 feedback_status_events。
		if _, err := st.UpdateFeedbackStatus(ctx, feedback.FeedbackID, "received", nil); err != nil {
			t.Fatalf("推进反馈状态: %v", err)
		}
		household, err := st.CreateHousehold(ctx, userID, "注销临时家庭")
		if err != nil {
			t.Fatalf("创建家庭: %v", err)
		}
		if _, err := st.CreateHouseholdRoom(ctx, userID, household.ID, "客厅", 0); err != nil {
			t.Fatalf("创建房间: %v", err)
		}
		inviteCode, _ := auth.NewPairingCode()
		if _, err := st.CreateHouseholdInvitation(ctx, userID, household.ID, auth.PairingCodeHash(inviteCode), time.Now().UTC().Add(time.Hour)); err != nil {
			t.Fatalf("创建邀请: %v", err)
		}
		if _, err := st.CreateExportTask(ctx, userID, time.Now().UTC().Add(time.Hour)); err != nil {
			t.Fatalf("创建导出任务: %v", err)
		}
		// 场景也必须纳入删除范围。
		if _, err := st.CreateScene(ctx, userID, model.SceneInput{
			Name: "注销前场景",
			Trigger: model.SceneTrigger{Kind: "device_offline", Device: &model.SceneDeviceRef{
				Platform: "android", ExternalID: "AA:BB:CC:00:00:21",
			}},
			Actions: []model.SceneAction{{Kind: "log", Message: "离线", Level: "warn"}},
		}); err != nil {
			t.Fatalf("创建场景: %v", err)
		}

		// 错误令牌与过期令牌必须是明确错误。
		if err := st.DeleteAccount(ctx, hashOf("no-such-"+uuid.NewString()), time.Now().UTC()); !errors.Is(err, store.ErrDeletionTokenInvalid) {
			t.Fatalf("错误令牌 err=%v", err)
		}
		expiredHash := hashOf("expired-" + uuid.NewString())
		if err := st.CreateDeletionToken(ctx, userID, expiredHash, time.Now().UTC().Add(-time.Minute)); err != nil {
			t.Fatal(err)
		}
		if err := st.DeleteAccount(ctx, expiredHash, time.Now().UTC()); !errors.Is(err, store.ErrDeletionTokenExpired) {
			t.Fatalf("过期令牌 err=%v", err)
		}

		validHash := hashOf("valid-" + uuid.NewString())
		if err := st.CreateDeletionToken(ctx, userID, validHash, time.Now().UTC().Add(10*time.Minute)); err != nil {
			t.Fatal(err)
		}

		counts := map[string]int{
			"tokens":                       countRows(t, db, `SELECT count(*) FROM tokens WHERE user_id = $1`, userID),
			"user_devices":                 countRows(t, db, `SELECT count(*) FROM user_devices WHERE user_id = $1`, userID),
			"readings":                     countRows(t, db, `SELECT count(*) FROM readings WHERE user_id = $1`, userID),
			"scenes":                       countRows(t, db, `SELECT count(*) FROM scenes WHERE user_id = $1`, userID),
			"change_log":                   countRows(t, db, `SELECT count(*) FROM change_log WHERE user_id = $1`, userID),
			"idempotency_keys":             countRows(t, db, `SELECT count(*) FROM idempotency_keys WHERE user_id = $1`, userID),
			"pairing_codes":                countRows(t, db, `SELECT count(*) FROM pairing_codes WHERE user_id = $1`, userID),
			"feedback":                     countRows(t, db, `SELECT count(*) FROM feedback WHERE user_id = $1`, userID),
			"feedback_status_events":       countRows(t, db, `SELECT count(*) FROM feedback_status_events WHERE feedback_id = $1`, feedback.FeedbackID),
			"device_type_catalog_accesses": countRows(t, db, `SELECT count(*) FROM device_type_catalog_accesses WHERE user_id = $1`, userID),
			"personal_data_export_tasks":   countRows(t, db, `SELECT count(*) FROM personal_data_export_tasks WHERE user_id = $1`, userID),
			"households":                   countRows(t, db, `SELECT count(*) FROM households WHERE id = $1`, household.ID),
			"household_members":            countRows(t, db, `SELECT count(*) FROM household_members WHERE user_id = $1`, userID),
			"household_rooms":              countRows(t, db, `SELECT count(*) FROM household_rooms WHERE household_id = $1`, household.ID),
			"household_invitations":        countRows(t, db, `SELECT count(*) FROM household_invitations WHERE household_id = $1`, household.ID),
		}
		nonZero := 0
		for _, n := range counts {
			if n > 0 {
				nonZero++
			}
		}
		if nonZero != len(counts) {
			t.Fatalf("删除前关联数据不足，非空表数=%d/%d: %+v", nonZero, len(counts), counts)
		}

		if err := st.DeleteAccount(ctx, validHash, time.Now().UTC()); err != nil {
			t.Fatalf("DeleteAccount: %v", err)
		}
		for table, query := range map[string]string{
			"tokens":                       `SELECT count(*) FROM tokens WHERE user_id = $1`,
			"user_devices":                 `SELECT count(*) FROM user_devices WHERE user_id = $1`,
			"readings":                     `SELECT count(*) FROM readings WHERE user_id = $1`,
			"scenes":                       `SELECT count(*) FROM scenes WHERE user_id = $1`,
			"change_log":                   `SELECT count(*) FROM change_log WHERE user_id = $1`,
			"idempotency_keys":             `SELECT count(*) FROM idempotency_keys WHERE user_id = $1`,
			"pairing_codes":                `SELECT count(*) FROM pairing_codes WHERE user_id = $1`,
			"feedback":                     `SELECT count(*) FROM feedback WHERE user_id = $1`,
			"device_type_catalog_accesses": `SELECT count(*) FROM device_type_catalog_accesses WHERE user_id = $1`,
			"personal_data_export_tasks":   `SELECT count(*) FROM personal_data_export_tasks WHERE user_id = $1`,
			"household_members":            `SELECT count(*) FROM household_members WHERE user_id = $1`,
		} {
			arg := userID
			if table == "feedback" {
				arg = userID
			}
			if n := countRows(t, db, query, arg); n != 0 {
				t.Fatalf("删除后 %s 仍有 %d 行", table, n)
			}
		}
		for table, query := range map[string]string{
			"feedback_status_events": `SELECT count(*) FROM feedback_status_events WHERE feedback_id = $1`,
			"households":             `SELECT count(*) FROM households WHERE id = $1`,
			"household_rooms":        `SELECT count(*) FROM household_rooms WHERE household_id = $1`,
			"household_invitations":  `SELECT count(*) FROM household_invitations WHERE household_id = $1`,
		} {
			arg := household.ID
			if table == "feedback_status_events" {
				arg = feedback.FeedbackID
			}
			if n := countRows(t, db, query, arg); n != 0 {
				t.Fatalf("删除后 %s 仍有 %d 行", table, n)
			}
		}
		if n := countRows(t, db, `SELECT count(*) FROM users WHERE id = $1`, userID); n != 0 {
			t.Fatalf("删除后 users 仍有 %d 行", n)
		}
		if n := countRows(t, db, `SELECT count(*) FROM households WHERE owner_user_id = $1`, userID); n != 0 {
			t.Fatalf("删除后仍有 %d 个 owner 家庭", n)
		}
		if _, err := st.UserIDByTokenHash(ctx, tokenHash); !errors.Is(err, store.ErrUnauthorized) {
			t.Fatalf("删除后旧 token 仍有效: err=%v", err)
		}
		// 重放：账号已不存在，返回 404 语义且没有未知错误。
		if err := st.DeleteAccount(ctx, validHash, time.Now().UTC()); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("重放删除 err=%v", err)
		}
		// 清理后墓碑消失，重放变成「令牌无效」而不是内部错误。
		n, err := st.CleanupDeletionTokens(ctx, time.Now().UTC().Add(time.Hour))
		if err != nil || n < 1 {
			t.Fatalf("清理删除令牌 n=%d err=%v", n, err)
		}
		if err := st.DeleteAccount(ctx, validHash, time.Now().UTC()); !errors.Is(err, store.ErrDeletionTokenInvalid) {
			t.Fatalf("清理后重放 err=%v", err)
		}
	})

	t.Run("owner 有成员时拦截，转让后可删", func(t *testing.T) {
		ownerID, _ := createAccount(t, st, db, "owner")
		memberID, _ := createAccount(t, st, db, "member")
		cleanupAccount(t, db, ownerID)
		cleanupAccount(t, db, memberID)

		household, err := st.CreateHousehold(ctx, ownerID, "共享家庭")
		if err != nil {
			t.Fatal(err)
		}
		code, _ := auth.NewPairingCode()
		if _, err := st.CreateHouseholdInvitation(ctx, ownerID, household.ID, auth.PairingCodeHash(code), time.Now().UTC().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if _, err := st.ClaimHouseholdInvitation(ctx, memberID, auth.PairingCodeHash(code), time.Now().UTC(), time.Minute); err != nil {
			t.Fatal(err)
		}

		tokenHash := hashOf("owner-" + uuid.NewString())
		if err := st.CreateDeletionToken(ctx, ownerID, tokenHash, time.Now().UTC().Add(10*time.Minute)); err != nil {
			t.Fatal(err)
		}
		if err := st.DeleteAccount(ctx, tokenHash, time.Now().UTC()); !errors.Is(err, store.ErrAccountDeletionBlocked) {
			t.Fatalf("owner 有成员应拦截: err=%v", err)
		}
		// 拦截后账号与家庭完整保留。
		if _, err := st.GetUser(ctx, ownerID); err != nil {
			t.Fatalf("拦截后 owner 不可用: %v", err)
		}
		if _, err := st.GetHousehold(ctx, memberID, household.ID); err != nil {
			t.Fatalf("拦截后家庭不可用: %v", err)
		}
		// 未消耗的令牌仍可用于转让后的删除。
		if err := st.TransferHouseholdOwnership(ctx, ownerID, household.ID, memberID); err != nil {
			t.Fatal(err)
		}
		if err := st.DeleteAccount(ctx, tokenHash, time.Now().UTC()); err != nil {
			t.Fatalf("转让后删除: %v", err)
		}
		if _, err := st.GetUser(ctx, ownerID); !errors.Is(err, store.ErrUnauthorized) {
			t.Fatalf("owner 未删除: %v", err)
		}
		// 家庭成员与家庭本身仍在，不会留下孤儿家庭。
		list, err := st.ListHouseholds(ctx, memberID)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, h := range list {
			if h.ID == household.ID && h.Role == "owner" && h.MemberCount == 1 {
				found = true
			}
		}
		if !found {
			t.Fatalf("转让后的家庭状态异常: %+v", list)
		}
	})

	t.Run("异步导出内容与一次性链接", func(t *testing.T) {
		exportUser, _ := createAccount(t, st, db, "export")
		otherUser, _ := createAccount(t, st, db, "other")
		cleanupAccount(t, db, exportUser)
		cleanupAccount(t, db, otherUser)

		base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
		if _, _, err := st.Push(ctx, exportUser, model.PushRequest{
			ClientID: "export-it",
			Devices: []model.PushDevice{{
				ID: uuid.NewString(), Platform: "android", ExternalID: "AA:BB:CC:00:00:31",
				MAC: "AA:BB:CC:00:00:31", Alias: strPtr("导出设备"), Rules: []byte(`{}`),
				ScanRecords: []byte(`[]`), UpdatedAt: base,
			}},
			Readings: []model.PushReading{
				{ID: uuid.NewString(), Platform: "android", ExternalID: "AA:BB:CC:00:00:31", Metric: "temperature_celsius", Value: 21, Unit: "celsius", RecordedAt: base},
				{ID: uuid.NewString(), Platform: "android", ExternalID: "AA:BB:CC:00:00:31", Metric: "temperature_celsius", Value: 23, Unit: "celsius", RecordedAt: base.Add(10 * time.Minute)},
				{ID: uuid.NewString(), Platform: "android", ExternalID: "AA:BB:CC:00:00:31", Metric: "relative_humidity", Value: 48, Unit: "percent", RecordedAt: base.Add(20 * time.Minute)},
			},
		}, store.PushOptions{IdempotencyKey: "export-push", RequestHash: "export-hash"}); err != nil {
			t.Fatalf("导出用户 push: %v", err)
		}
		// 他人数据：设备、读数、反馈，都不能出现在导出中。
		if _, _, err := st.Push(ctx, otherUser, model.PushRequest{
			Devices: []model.PushDevice{{
				ID: uuid.NewString(), Platform: "android", ExternalID: "AA:BB:CC:00:00:32",
				MAC: "AA:BB:CC:00:00:32", Rules: []byte(`{}`), ScanRecords: []byte(`[]`), UpdatedAt: base,
			}},
			Readings: []model.PushReading{{
				ID: uuid.NewString(), Platform: "android", ExternalID: "AA:BB:CC:00:00:32",
				Metric: "temperature_celsius", Value: 99, Unit: "celsius", RecordedAt: base,
			}},
		}, store.PushOptions{}); err != nil {
			t.Fatalf("他人 push: %v", err)
		}

		feedback, _, err := st.CreateFeedback(ctx, exportUser, model.FeedbackSubmission{
			Kind: "bug", Body: "导出反馈正文", Platform: "android",
			AppVersion: "1.2.3", BuildType: "release", OSVersion: "14", DeviceModel: "Pixel",
			Contact: strPtr("export@example.com"), Diagnostics: strPtr("diagnostics-text"),
		}, store.FeedbackOptions{IdempotencyKey: "export-feedback", RequestHash: "fb"})
		if err != nil {
			t.Fatalf("导出反馈: %v", err)
		}
		internalNote := "内部备注-只给运营看-" + uuid.NewString()
		if _, err := st.UpdateFeedbackStatus(ctx, feedback.FeedbackID, "received", &internalNote); err != nil {
			t.Fatalf("运营备注: %v", err)
		}

		household, err := st.CreateHousehold(ctx, exportUser, "导出的家")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.CreateHouseholdRoom(ctx, exportUser, household.ID, "书房", 1); err != nil {
			t.Fatal(err)
		}

		// 场景：本人一条、他人一条，导出只能出现本人场景。
		ownSceneName := "导出场景-" + uuid.NewString()
		if _, err := st.CreateScene(ctx, exportUser, model.SceneInput{
			Name: ownSceneName,
			Trigger: model.SceneTrigger{Kind: "rssi_below", Device: &model.SceneDeviceRef{
				Platform: "android", ExternalID: "AA:BB:CC:00:00:31",
			}, RSSIThreshold: intPtr(-70)},
			Actions: []model.SceneAction{{Kind: "mark_device", Mark: "attention"}},
		}); err != nil {
			t.Fatalf("导出用户场景: %v", err)
		}
		otherSceneName := "他人场景-" + uuid.NewString()
		if _, err := st.CreateScene(ctx, otherUser, model.SceneInput{
			Name: otherSceneName,
			Trigger: model.SceneTrigger{Kind: "rssi_below", Device: &model.SceneDeviceRef{
				Platform: "android", ExternalID: "AA:BB:CC:00:00:32",
			}, RSSIThreshold: intPtr(-80)},
			Actions: []model.SceneAction{{Kind: "log", Message: "他人设备弱信号", Level: "info"}},
		}); err != nil {
			t.Fatalf("他人场景: %v", err)
		}

		task, err := st.CreateExportTask(ctx, exportUser, time.Now().UTC().Add(24*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.PrepareExportDownload(ctx, exportUser, task.ID, hashOf("early-"+uuid.NewString()), time.Now().UTC().Add(time.Minute)); !errors.Is(err, store.ErrExportTaskNotReady) {
			t.Fatalf("pending 任务应不可下载: %v", err)
		}

		artifact, err := st.BuildPersonalDataExport(ctx, exportUser)
		if err != nil {
			t.Fatalf("构建导出: %v", err)
		}
		if err := st.FinishExportTask(ctx, task.ID, artifact); err != nil {
			t.Fatalf("完成导出任务: %v", err)
		}
		status, err := st.GetExportTask(ctx, exportUser, task.ID)
		if err != nil || status.Status != "ready" {
			t.Fatalf("任务状态=%+v err=%v", status, err)
		}
		if _, err := st.GetExportTask(ctx, otherUser, task.ID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("跨用户任务查询 err=%v", err)
		}

		downloadTokenHash := hashOf("download-" + uuid.NewString())
		expiresAt := time.Now().UTC().Add(10 * time.Minute)
		prepared, err := st.PrepareExportDownload(ctx, exportUser, task.ID, downloadTokenHash, expiresAt)
		if err != nil || prepared.DownloadExpiresAt == nil || !prepared.DownloadExpiresAt.After(time.Now().UTC()) {
			t.Fatalf("签发下载令牌: %+v err=%v", prepared, err)
		}

		got, err := st.ConsumeExportDownload(ctx, downloadTokenHash, time.Now().UTC())
		if err != nil {
			t.Fatalf("消费下载令牌: %v", err)
		}
		if got.User.ID != exportUser {
			t.Fatalf("导出 user=%s want=%s", got.User.ID, exportUser)
		}
		if len(got.Devices) != 1 {
			t.Fatalf("导出设备数=%d", len(got.Devices))
		}
		if len(got.Readings) != 2 {
			t.Fatalf("读数聚合数=%d: %+v", len(got.Readings), got.Readings)
		}
		var temp *model.ExportReadingAggregate
		for i := range got.Readings {
			if got.Readings[i].Metric == "temperature_celsius" {
				temp = &got.Readings[i]
			}
		}
		if temp == nil || temp.Count != 2 || temp.Min != 21 || temp.Max != 23 || temp.Avg != 22 ||
			!temp.FirstAt.Equal(base) || !temp.LastAt.Equal(base.Add(10*time.Minute)) {
			t.Fatalf("温度聚合异常: %+v", temp)
		}
		var exportedHousehold *model.ExportHousehold
		for i := range got.Households {
			if got.Households[i].ID == household.ID {
				exportedHousehold = &got.Households[i]
			}
		}
		if exportedHousehold == nil || exportedHousehold.Role != "owner" || len(exportedHousehold.Rooms) != 1 ||
			exportedHousehold.Rooms[0].Name != "书房" {
			t.Fatalf("家庭导出异常: %+v", got.Households)
		}
		if len(got.Feedback) != 1 || got.Feedback[0].Contact == nil || *got.Feedback[0].Contact != "export@example.com" ||
			got.Feedback[0].Diagnostics == nil || *got.Feedback[0].Diagnostics != "diagnostics-text" ||
			got.Feedback[0].Status != "received" {
			t.Fatalf("反馈导出异常: %+v", got.Feedback)
		}
		if len(got.Scenes) != 1 || got.Scenes[0].Name != ownSceneName ||
			got.Scenes[0].Trigger.Kind != "rssi_below" || got.Scenes[0].DeviceMissing {
			t.Fatalf("场景导出异常: %+v", got.Scenes)
		}

		raw, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{internalNote, "internalNote", "internal_note", "AA:BB:CC:00:00:32", otherUser, otherSceneName} {
			if strings.Contains(string(raw), forbidden) {
				t.Fatalf("导出泄露禁止内容 %q: %s", forbidden, raw)
			}
		}

		// 一次性：重放 410；错误令牌 403；过期 410。
		if _, err := st.ConsumeExportDownload(ctx, downloadTokenHash, time.Now().UTC()); !errors.Is(err, store.ErrExportDownloadExpired) {
			t.Fatalf("重放下载 err=%v", err)
		}
		if _, err := st.ConsumeExportDownload(ctx, hashOf("bad-"+uuid.NewString()), time.Now().UTC()); !errors.Is(err, store.ErrExportDownloadInvalid) {
			t.Fatalf("错误下载令牌 err=%v", err)
		}
		expiredTokenHash := hashOf("expired-dl-" + uuid.NewString())
		if _, err := st.PrepareExportDownload(ctx, exportUser, task.ID, expiredTokenHash, time.Now().UTC().Add(-time.Minute)); err != nil {
			t.Fatal(err)
		}
		if _, err := st.ConsumeExportDownload(ctx, expiredTokenHash, time.Now().UTC()); !errors.Is(err, store.ErrExportDownloadExpired) {
			t.Fatalf("过期下载令牌 err=%v", err)
		}

		// 产物保留期清理：过期任务整行删除，再查询 404。
		expiredTask, err := st.CreateExportTask(ctx, exportUser, time.Now().UTC().Add(-time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		n, err := st.CleanupExportTasks(ctx, time.Now().UTC())
		if err != nil || n < 1 {
			t.Fatalf("清理导出任务 n=%d err=%v", n, err)
		}
		if _, err := st.GetExportTask(ctx, exportUser, expiredTask.ID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("清理后任务仍可查: %v", err)
		}
	})
}

func createAccount(t *testing.T, st store.Store, db *sql.DB, prefix string) (string, string) {
	t.Helper()
	hash := hashOf(prefix + "-" + uuid.NewString())
	id, err := st.CreateUserWithToken(context.Background(), hash, time.Now().UTC().Add(180*24*time.Hour))
	if err != nil {
		t.Fatalf("创建账号 %s: %v", prefix, err)
	}
	return id, hash
}

// cleanupAccount 兜底清理测试账号；账号已被删除时是无操作。
func cleanupAccount(t *testing.T, db *sql.DB, userID string) {
	t.Helper()
	t.Cleanup(func() {
		db.Exec(`DELETE FROM users WHERE id = $1`, userID)
	})
}

func countRows(t *testing.T, db *sql.DB, query string, arg any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query, arg).Scan(&n); err != nil {
		t.Fatalf("统计失败 %s: %v", query, err)
	}
	return n
}
