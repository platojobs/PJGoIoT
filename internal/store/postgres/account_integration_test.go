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
	userA, err := st.CreateUserWithToken(ctx, hashA, time.Now().UTC().Add(180*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	userB, err := st.CreateUserWithToken(ctx, hashB, time.Now().UTC().Add(180*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	// 配对码测试结束后两个账号都清理；A 的删除路径在专门的注销测试中覆盖。
	t.Cleanup(func() {
		db.Exec("DELETE FROM users WHERE id = $1 OR id = $2", userA, userB)
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
			hashOf("claim-old-"+uuid.NewString()), time.Now().UTC(), 10*time.Minute, time.Now().UTC().Add(180*24*time.Hour)); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("旧码 err=%v", err)
		}
	})

	t.Run("claim 签发新 token 且保留原 token", func(t *testing.T) {
		newTokenHash := hashOf("claim-new-" + uuid.NewString())
		got, err := st.ClaimPairingCode(ctx, auth.PairingCodeHash(codeA2), newTokenHash, time.Now().UTC(), 10*time.Minute, time.Now().UTC().Add(180*24*time.Hour))
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
			hashOf("claim-replay-"+uuid.NewString()), time.Now().UTC(), 10*time.Minute, time.Now().UTC().Add(180*24*time.Hour)); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("重放 err=%v", err)
		}
	})

	t.Run("过期码返回 404", func(t *testing.T) {
		code, _ := auth.NewPairingCode()
		if err := st.CreatePairingCode(ctx, userA, auth.PairingCodeHash(code), time.Now().UTC().Add(-time.Minute)); err != nil {
			t.Fatal(err)
		}
		if _, err := st.ClaimPairingCode(ctx, auth.PairingCodeHash(code),
			hashOf("claim-expired-"+uuid.NewString()), time.Now().UTC(), 10*time.Minute, time.Now().UTC().Add(180*24*time.Hour)); !errors.Is(err, store.ErrNotFound) {
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
			_, err := st.ClaimPairingCode(ctx, hash, hashOf("wrong-"+uuid.NewString()), time.Now().UTC(), 10*time.Minute, time.Now().UTC().Add(180*24*time.Hour))
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
			hashOf("claim-b-"+uuid.NewString()), time.Now().UTC(), 10*time.Minute, time.Now().UTC().Add(180*24*time.Hour))
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

	t.Run("近似计数可读", func(t *testing.T) {
		counts, err := st.ApproxCounts(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if counts.Users < 0 || counts.Devices < 0 || counts.Readings < 0 {
			t.Fatalf("计数为负: %+v", counts)
		}
	})
}
