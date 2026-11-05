package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/platojobs/PJGoIoT/internal/auth"
	"github.com/platojobs/PJGoIoT/internal/model"
	"github.com/platojobs/PJGoIoT/internal/store"
	"github.com/platojobs/PJGoIoT/internal/store/postgres"
)

// TestPostgresAccountIntegration 覆盖 v0.6.0 的配对码、数据导出、注销与近似计数。
func TestPostgresAccountIntegration(t *testing.T) {
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

	hashA := hashOf("account-a-" + uuid.NewString())
	hashB := hashOf("account-b-" + uuid.NewString())
	userA, err := st.CreateUserWithToken(ctx, hashA)
	if err != nil {
		t.Fatal(err)
	}
	userB, err := st.CreateUserWithToken(ctx, hashB)
	if err != nil {
		t.Fatal(err)
	}
	// A 会在注销测试中被删除，这里只清理 B 与可能残留的码。
	t.Cleanup(func() {
		db.Exec("DELETE FROM users WHERE id = $1", userB)
		db.Exec("DELETE FROM pairing_codes")
	})

	user, err := st.GetUser(ctx, userA)
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if user.ID != userA || user.Kind == "" || user.CreatedAt.IsZero() {
		t.Fatalf("用户信息不完整: %+v", user)
	}

	codeA1, _ := auth.NewPairingCode()
	codeA2, _ := auth.NewPairingCode()
	expiry := time.Now().UTC().Add(10 * time.Minute)
	if err := st.CreatePairingCode(ctx, userA, auth.PairingCodeHash(codeA1), expiry); err != nil {
		t.Fatal(err)
	}
	if err := st.CreatePairingCode(ctx, userA, auth.PairingCodeHash(codeA2), expiry); err != nil {
		t.Fatal(err)
	}

	t.Run("新码使旧码失效", func(t *testing.T) {
		if _, err := st.ClaimPairingCode(ctx, auth.PairingCodeHash(codeA1),
			hashOf("claim-old-"+uuid.NewString()), time.Now().UTC(), 10*time.Minute); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("旧码 err=%v", err)
		}
	})

	t.Run("claim 签发新 token 且保留原 token", func(t *testing.T) {
		newTokenHash := hashOf("claim-new-" + uuid.NewString())
		got, err := st.ClaimPairingCode(ctx, auth.PairingCodeHash(codeA2), newTokenHash, time.Now().UTC(), 10*time.Minute)
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		if got != userA {
			t.Fatalf("claim userId=%s want=%s", got, userA)
		}
		if uid, err := st.UserIDByTokenHash(ctx, newTokenHash); err != nil || uid != userA {
			t.Fatalf("新 token 不可用: uid=%s err=%v", uid, err)
		}
		if uid, err := st.UserIDByTokenHash(ctx, hashA); err != nil || uid != userA {
			t.Fatalf("原 token 失效: uid=%s err=%v", uid, err)
		}
	})

	t.Run("重放返回 404", func(t *testing.T) {
		if _, err := st.ClaimPairingCode(ctx, auth.PairingCodeHash(codeA2),
			hashOf("claim-replay-"+uuid.NewString()), time.Now().UTC(), 10*time.Minute); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("重放 err=%v", err)
		}
	})

	t.Run("过期码返回 404", func(t *testing.T) {
		code, _ := auth.NewPairingCode()
		if err := st.CreatePairingCode(ctx, userA, auth.PairingCodeHash(code), time.Now().UTC().Add(-time.Minute)); err != nil {
			t.Fatal(err)
		}
		if _, err := st.ClaimPairingCode(ctx, auth.PairingCodeHash(code),
			hashOf("claim-expired-"+uuid.NewString()), time.Now().UTC(), 10*time.Minute); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("过期 err=%v", err)
		}
	})

	t.Run("错 5 次作废", func(t *testing.T) {
		code, _ := auth.NewPairingCode()
		hash := auth.PairingCodeHash(code)
		if err := st.CreatePairingCode(ctx, userA, hash, time.Now().UTC().Add(10*time.Minute)); err != nil {
			t.Fatal(err)
		}
		// 让码过期，从而 5 次失败都会累计到该码。
		if _, err := db.Exec(`UPDATE pairing_codes SET expires_at = now() - interval '1 minute' WHERE code_hash = $1`, hash); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 5; i++ {
			_, err := st.ClaimPairingCode(ctx, hash, hashOf("wrong-"+uuid.NewString()), time.Now().UTC(), 10*time.Minute)
			if !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("第 %d 次 err=%v", i+1, err)
			}
		}
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM pairing_codes WHERE code_hash = $1`, hash).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("错 5 次后码仍在: count=%d", n)
		}
	})

	t.Run("跨用户配对", func(t *testing.T) {
		codeB, _ := auth.NewPairingCode()
		if err := st.CreatePairingCode(ctx, userB, auth.PairingCodeHash(codeB), time.Now().UTC().Add(10*time.Minute)); err != nil {
			t.Fatal(err)
		}
		got, err := st.ClaimPairingCode(ctx, auth.PairingCodeHash(codeB),
			hashOf("claim-b-"+uuid.NewString()), time.Now().UTC(), 10*time.Minute)
		if err != nil || got != userB {
			t.Fatalf("跨用户 claim uid=%s err=%v", got, err)
		}
	})

	t.Run("TTL 清理", func(t *testing.T) {
		code, _ := auth.NewPairingCode()
		if err := st.CreatePairingCode(ctx, userA, auth.PairingCodeHash(code), time.Now().UTC().Add(-time.Hour)); err != nil {
			t.Fatal(err)
		}
		n, err := st.CleanupPairingCodes(ctx, time.Now().UTC())
		if err != nil || n < 1 {
			t.Fatalf("清理 n=%d err=%v", n, err)
		}
	})

	// 准备导出数据：两台设备（其中一台为 tombstone）与 3 条读数。
	base := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	devices := []model.PushDevice{
		{
			ID: uuid.NewString(), Platform: "android", ExternalID: "AA:BB:CC:00:00:01",
			MAC: "AA:BB:CC:00:00:01", Alias: strPtr("导出设备"), Rules: []byte(`{}`),
			ScanRecords: []byte(`[]`), UpdatedAt: base,
		},
		{
			ID: uuid.NewString(), Platform: "android", ExternalID: "AA:BB:CC:00:00:02",
			MAC: "AA:BB:CC:00:00:02", Rules: []byte(`{}`), ScanRecords: []byte(`[]`),
			UpdatedAt: base.Add(time.Minute), Deleted: true,
		},
	}
	pushReadings := []model.PushReading{
		{ID: uuid.NewString(), Platform: "android", ExternalID: "AA:BB:CC:00:00:01", Metric: "temperature_celsius", Value: 21, Unit: "celsius", RecordedAt: base},
		{ID: uuid.NewString(), Platform: "android", ExternalID: "AA:BB:CC:00:00:01", Metric: "temperature_celsius", Value: 22, Unit: "celsius", RecordedAt: base.Add(10 * time.Minute)},
		{ID: uuid.NewString(), Platform: "android", ExternalID: "AA:BB:CC:00:00:01", Metric: "relative_humidity", Value: 48, Unit: "percent", RecordedAt: base.Add(20 * time.Minute)},
	}
	if _, _, err := st.Push(ctx, userA, model.PushRequest{ClientID: "export-client", Devices: devices, Readings: pushReadings},
		store.PushOptions{IdempotencyKey: "export-1", RequestHash: "export-hash"}); err != nil {
		t.Fatalf("准备导出数据 push: %v", err)
	}

	t.Run("导出设备含 tombstone", func(t *testing.T) {
		list, err := st.ListUserDevicesForExport(ctx, userA)
		if err != nil {
			t.Fatal(err)
		}
		if len(list) != 2 {
			t.Fatalf("设备数=%d", len(list))
		}
		var tombstones int
		for _, d := range list {
			if d.DeletedAt != nil {
				tombstones++
			}
		}
		if tombstones != 1 {
			t.Fatalf("tombstone=%d", tombstones)
		}
	})

	t.Run("导出读数分页与内容", func(t *testing.T) {
		var dbCount int
		if err := db.QueryRow(`SELECT count(*) FROM readings WHERE user_id = $1`, userA).Scan(&dbCount); err != nil {
			t.Fatal(err)
		}
		if dbCount != 3 {
			t.Fatalf("库内读数=%d", dbCount)
		}

		var all []model.ExportReading
		cursor := ""
		for page := 0; page < 10; page++ {
			items, next, hasMore, err := st.ExportReadings(ctx, userA, cursor, 2)
			if err != nil {
				t.Fatalf("导出第 %d 页: %v", page, err)
			}
			all = append(all, items...)
			if !hasMore {
				if next != "" {
					t.Fatalf("末页 cursor 非空: %q", next)
				}
				break
			}
			if next == "" || len(items) != 2 {
				t.Fatalf("分页游标异常: next=%q len=%d", next, len(items))
			}
			cursor = next
		}
		if len(all) != dbCount {
			t.Fatalf("导出条数=%d want=%d", len(all), dbCount)
		}
		for i := 1; i < len(all); i++ {
			if all[i].RecordedAt.Before(all[i-1].RecordedAt) {
				t.Fatalf("导出未按 recordedAt 升序: %+v", all)
			}
		}
		if all[0].ID != pushReadings[0].ID || all[1].ID != pushReadings[1].ID || all[2].ID != pushReadings[2].ID {
			t.Fatalf("导出顺序与写入不一致: %+v", all)
		}
		if all[2].Metric != "relative_humidity" || all[2].Unit != "percent" || all[2].Value != 48 {
			t.Fatalf("导出字段不一致: %+v", all[2])
		}
	})

	t.Run("非法游标", func(t *testing.T) {
		if _, _, _, err := st.ExportReadings(ctx, userA, "not-base64", 10); !errors.Is(err, store.ErrBadCursor) {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("近似计数可读", func(t *testing.T) {
		counts, err := st.ApproxCounts(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if counts.Users < 0 || counts.Devices < 0 || counts.Readings < 0 {
			t.Fatalf("计数为负: %+v", counts)
		}
	})

	t.Run("注销级联与幂等", func(t *testing.T) {
		tables := []string{"tokens", "user_devices", "change_log", "idempotency_keys", "readings", "pairing_codes"}
		nonZero := 0
		for _, table := range tables {
			var n int
			if err := db.QueryRow("SELECT count(*) FROM "+table+" WHERE user_id = $1", userA).Scan(&n); err != nil {
				t.Fatalf("统计 %s: %v", table, err)
			}
			if n > 0 {
				nonZero++
			}
		}
		if nonZero < 3 {
			t.Fatalf("注销前关联数据异常，非空表数=%d", nonZero)
		}

		if err := st.DeleteUser(ctx, userA); err != nil {
			t.Fatalf("DeleteUser: %v", err)
		}
		for _, table := range tables {
			var n int
			if err := db.QueryRow("SELECT count(*) FROM "+table+" WHERE user_id = $1", userA).Scan(&n); err != nil {
				t.Fatalf("统计 %s: %v", table, err)
			}
			if n != 0 {
				t.Fatalf("注销后 %s 仍有 %d 行", table, n)
			}
		}
		var users int
		if err := db.QueryRow(`SELECT count(*) FROM users WHERE id = $1`, userA).Scan(&users); err != nil {
			t.Fatal(err)
		}
		if users != 0 {
			t.Fatalf("注销后 users 仍有 %d 行", users)
		}
		if _, err := st.UserIDByTokenHash(ctx, hashA); !errors.Is(err, store.ErrUnauthorized) {
			t.Fatalf("注销后 token 仍有效: err=%v", err)
		}
		if err := st.DeleteUser(ctx, userA); !errors.Is(err, store.ErrUnauthorized) {
			t.Fatalf("重复注销 err=%v", err)
		}
		if _, err := st.UserIDByTokenHash(ctx, hashB); err != nil {
			t.Fatalf("其他用户受影响: %v", err)
		}
	})
}
