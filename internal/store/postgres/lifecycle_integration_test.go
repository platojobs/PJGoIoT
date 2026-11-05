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

// TestCleanupAndRetentionLifecycle 验证保留边界、410 语义、清理行数与 token 过期。
// 需要真实 PostgreSQL，通过 TEST_DATABASE_URL 启用。
func TestCleanupAndRetentionLifecycle(t *testing.T) {
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

	tokenExpiry := time.Now().UTC().Add(180 * 24 * time.Hour).Truncate(time.Microsecond)
	hash := hashOf("lifecycle-" + uuid.NewString())
	userID, err := st.CreateUserWithToken(ctx, hash, tokenExpiry)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Exec("DELETE FROM users WHERE id = $1", userID) })

	// expiresAt 与库一致。
	var stored time.Time
	if err := db.QueryRow(`SELECT expires_at FROM tokens WHERE token_hash = $1`, hash).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored.UTC().Sub(tokenExpiry).Abs() > time.Microsecond {
		t.Fatalf("tokens.expires_at=%s want=%s", stored.UTC(), tokenExpiry)
	}

	// 三次 push 产生三条 change_log（seq 连续）。
	now := time.Now().UTC().Truncate(time.Microsecond)
	var seqs []int64
	for i := 0; i < 3; i++ {
		mac := "AA:BB:CC:00:01:0" + string(rune('A'+i))
		res, _, err := st.Push(ctx, userID, model.PushRequest{Devices: []model.PushDevice{{
			ID: uuid.NewString(), MAC: mac, Rules: []byte(`{}`), ScanRecords: []byte(`[]`), UpdatedAt: now,
		}}}, store.PushOptions{})
		if err != nil {
			t.Fatalf("push %d: %v", i, err)
		}
		seqs = append(seqs, res.Applied[0].ServerSeq)
	}
	// 再写一条读数，用于验证清理不影响 readings。
	readingMAC := "AA:BB:CC:00:01:0A"
	if _, _, err := st.Push(ctx, userID, model.PushRequest{Readings: []model.PushReading{{
		ID: uuid.NewString(), Platform: "android", ExternalID: readingMAC,
		Metric: "temperature_celsius", Value: 21.5, Unit: "celsius", RecordedAt: now,
	}}}, store.PushOptions{}); err != nil {
		t.Fatalf("push reading: %v", err)
	}

	page, err := st.Changes(ctx, userID, 0, 100)
	if err != nil {
		t.Fatalf("初始 changes: %v", err)
	}
	if len(page.Changes) != 3 {
		t.Fatalf("初始变更数=%d want=3", len(page.Changes))
	}

	// 把最老一条变更的 server_ts 推到 100 天前，再按 90 天保留期清理。
	if _, err := db.Exec(`UPDATE change_log SET server_ts = now() - interval '100 days' WHERE user_id = $1 AND seq = $2`, userID, seqs[0]); err != nil {
		t.Fatal(err)
	}
	deleted, err := st.CleanupChangeLog(ctx, time.Now().UTC().Add(-90*24*time.Hour))
	if err != nil {
		t.Fatalf("CleanupChangeLog: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("change_log 删除=%d want=1", deleted)
	}

	// 旧 cursor 跨过保留边界必须 410（ErrCursorExpired），不能静默空页。
	// 用一个早于被删 seq 的正游标模拟“客户端曾同步到边界之前”。
	if _, err := st.Changes(ctx, userID, seqs[0]-1, 100); !errors.Is(err, store.ErrCursorExpired) {
		t.Fatalf("旧 cursor err=%v want ErrCursorExpired", err)
	}
	// 新 cursor（等于被删 seq）仍可正常拉取。
	page, err = st.Changes(ctx, userID, seqs[0], 100)
	if err != nil {
		t.Fatalf("新 cursor changes: %v", err)
	}
	if len(page.Changes) != 2 || page.NextCursor != seqs[2] {
		t.Fatalf("新 cursor 变更数=%d next=%d", len(page.Changes), page.NextCursor)
	}

	// 清理不影响 user_devices / readings。
	var devices, readings int
	if err := db.QueryRow(`SELECT count(*) FROM user_devices WHERE user_id = $1`, userID).Scan(&devices); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM readings WHERE user_id = $1`, userID).Scan(&readings); err != nil {
		t.Fatal(err)
	}
	if devices != 3 || readings != 1 {
		t.Fatalf("devices=%d readings=%d，清理误删了用户数据", devices, readings)
	}

	// 幂等键清理后，同 key 不再按缓存重放，也不会误报 409，而是按新请求处理。
	body := model.PushRequest{Devices: []model.PushDevice{{
		ID: uuid.NewString(), MAC: "AA:BB:CC:00:01:0B", Rules: []byte(`{}`), ScanRecords: []byte(`[]`), UpdatedAt: now,
	}}}
	opts := store.PushOptions{IdempotencyKey: "lifecycle-key", RequestHash: "hash-1"}
	if _, replayed, err := st.Push(ctx, userID, body, opts); err != nil || replayed {
		t.Fatalf("首次 push replayed=%v err=%v", replayed, err)
	}
	if _, replayed, err := st.Push(ctx, userID, body, opts); err != nil || !replayed {
		t.Fatalf("重放 replayed=%v err=%v", replayed, err)
	}
	if _, err := st.CleanupIdempotencyKeys(ctx, time.Now().UTC()); err != nil {
		t.Fatalf("CleanupIdempotencyKeys: %v", err)
	}
	// 清理后同 key 同请求按新请求重新处理（设备 LWW 命中 unchanged），不会误报 409。
	if _, replayed, err := st.Push(ctx, userID, body, opts); err != nil || replayed {
		t.Fatalf("清理后同 key replayed=%v err=%v want replayed=false", replayed, err)
	}

	// 过期 token 不可用；CleanupTokens 删除后行数下降。
	expiredHash := hashOf("expired-" + uuid.NewString())
	expiredUser, err := st.CreateUserWithToken(ctx, expiredHash, time.Now().UTC().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Exec("DELETE FROM users WHERE id = $1", expiredUser) })
	if _, err := st.UserIDByTokenHash(ctx, expiredHash); !errors.Is(err, store.ErrUnauthorized) {
		t.Fatalf("过期 token err=%v want ErrUnauthorized", err)
	}
	n, err := st.CleanupTokens(ctx, time.Now().UTC())
	if err != nil {
		t.Fatalf("CleanupTokens: %v", err)
	}
	if n < 1 {
		t.Fatalf("CleanupTokens 删除=%d want >=1", n)
	}
	var left int
	if err := db.QueryRow(`SELECT count(*) FROM tokens WHERE user_id = $1`, expiredUser).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Fatalf("过期 token 未清理: %d", left)
	}
}
