package postgres_test

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/platojobs/PJGoIoT/internal/store/postgres"
)

// TestSharedRateLimiterAcrossInstances 验证 RATE_LIMIT_BACKEND=postgres 的共享限流：
// 两个独立的连接池（模拟两个 API 实例）对同一 key 使用同一份额度。
func TestSharedRateLimiterAcrossInstances(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("未设置 TEST_DATABASE_URL，跳过真实数据库集成测试")
	}
	runMigrations(t, dsn)

	ctx := context.Background()
	instanceA, err := postgres.New(ctx, dsn)
	if err != nil {
		t.Fatalf("连接数据库失败（实例 A）: %v", err)
	}
	t.Cleanup(func() { instanceA.Close() })
	instanceB, err := postgres.New(ctx, dsn)
	if err != nil {
		t.Fatalf("连接数据库失败（实例 B）: %v", err)
	}
	t.Cleanup(func() { instanceB.Close() })

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("打开校验连接失败: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	key := "it-rate-" + uuid.NewString()
	t.Cleanup(func() {
		db.Exec(`DELETE FROM rate_limit_buckets WHERE bucket LIKE 'it-rate-%'`)
	})

	const perMinute, burst = 6, 6
	// 6 个请求在两个实例间交替，应全部放行。
	for i := 0; i < burst; i++ {
		st := instanceA
		if i%2 == 1 {
			st = instanceB
		}
		ok, err := st.AllowRateLimit(ctx, key, perMinute, burst)
		if err != nil {
			t.Fatalf("第 %d 次共享限流查询失败: %v", i+1, err)
		}
		if !ok {
			t.Fatalf("第 %d 次在两个实例共享 burst=%d 内应放行", i+1, burst)
		}
	}

	// 第 7 次无论打到哪个实例都必须拒绝：如果计数是进程内的，另一个实例仍会放行。
	if ok, err := instanceA.AllowRateLimit(ctx, key, perMinute, burst); err != nil || ok {
		t.Fatalf("实例 A 第 7 次应被共享计数拒绝: ok=%v err=%v", ok, err)
	}
	if ok, err := instanceB.AllowRateLimit(ctx, key, perMinute, burst); err != nil || ok {
		t.Fatalf("实例 B 第 7 次应被共享计数拒绝（说明两个实例没有共享状态）: ok=%v err=%v", ok, err)
	}

	// 其他 key 不受影响。
	otherKey := key + "-other"
	if ok, err := instanceA.AllowRateLimit(ctx, otherKey, perMinute, burst); err != nil || !ok {
		t.Fatalf("不同 key 应独立计数: ok=%v err=%v", ok, err)
	}

	// 回填语义：把上次访问时间前移，桶应回满到 burst，下一次仍放行。
	if _, err := db.Exec(
		`UPDATE rate_limit_buckets SET updated_at = now() - interval '1 hour' WHERE bucket = $1`, key); err != nil {
		t.Fatalf("回填测试准备失败: %v", err)
	}
	if ok, err := instanceB.AllowRateLimit(ctx, key, perMinute, burst); err != nil || !ok {
		t.Fatalf("令牌回填后应放行: ok=%v err=%v", ok, err)
	}

	// 状态只应有一行，且 tokens 在 [0, burst] 内。
	var rows int
	var tokens float64
	var updated time.Time
	if err := db.QueryRow(
		`SELECT count(*), COALESCE(max(tokens), -1), COALESCE(max(updated_at), now()) FROM rate_limit_buckets WHERE bucket = $1`,
		key).Scan(&rows, &tokens, &updated); err != nil {
		t.Fatalf("读取限流状态失败: %v", err)
	}
	if rows != 1 {
		t.Fatalf("同一 key 应只有一行共享状态，实际 %d 行", rows)
	}
	if tokens < 0 || tokens > burst {
		t.Fatalf("tokens=%v 超出 [0,%d]", tokens, burst)
	}
}
