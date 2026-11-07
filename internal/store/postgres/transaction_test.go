package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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

// TestPushTransactionBoundaries 验证 /sync/push 的事务边界：
// 整批原子、计数与实际写入严格对应，以及任意阶段崩溃都不留下部分状态。
func TestPushTransactionBoundaries(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("未设置 TEST_DATABASE_URL，跳过真实数据库集成测试")
	}
	runMigrations(t, dsn)

	st, err := postgres.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("连接数据库失败: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("打开校验连接失败: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	newUser := func(t *testing.T) string {
		t.Helper()
		uid, err := st.CreateUserWithToken(context.Background(), hashOf("tx-"+uuid.NewString()),
			time.Now().UTC().Add(180*24*time.Hour))
		if err != nil {
			t.Fatalf("创建用户失败: %v", err)
		}
		t.Cleanup(func() { db.Exec("DELETE FROM users WHERE id = $1", uid) })
		return uid
	}
	counts := func(t *testing.T, uid string) (devices, readings, changes, idem int) {
		t.Helper()
		queries := []struct {
			q    string
			dest *int
		}{
			{"SELECT count(*) FROM user_devices WHERE user_id = $1", &devices},
			{"SELECT count(*) FROM readings WHERE user_id = $1", &readings},
			{"SELECT count(*) FROM change_log WHERE user_id = $1", &changes},
			{"SELECT count(*) FROM idempotency_keys WHERE user_id = $1", &idem},
		}
		for _, item := range queries {
			if err := db.QueryRow(item.q, uid).Scan(item.dest); err != nil {
				t.Fatalf("统计失败 %q: %v", item.q, err)
			}
		}
		return
	}
	device := func(mac string) model.PushDevice {
		return model.PushDevice{
			ID: uuid.NewString(), MAC: mac, Rules: []byte(`{}`), ScanRecords: []byte(`[]`),
			UpdatedAt: time.Now().UTC(),
		}
	}
	reading := func(mac string, at time.Time) model.PushReading {
		return model.PushReading{
			ID: uuid.NewString(), Platform: "android", ExternalID: mac,
			Metric: "temperature_celsius", Value: 21.5, Unit: "celsius", RecordedAt: at,
		}
	}

	t.Run("同批重复设备 id 明确拒绝且无残留", func(t *testing.T) {
		uid := newUser(t)
		id := uuid.NewString()
		req := model.PushRequest{Devices: []model.PushDevice{
			{ID: id, MAC: "AA:BB:CC:00:10:01", Rules: []byte(`{}`), ScanRecords: []byte(`[]`), UpdatedAt: time.Now().UTC()},
			{ID: id, MAC: "AA:BB:CC:00:10:02", Rules: []byte(`{}`), ScanRecords: []byte(`[]`), UpdatedAt: time.Now().UTC()},
		}}
		if _, _, err := st.Push(context.Background(), uid, req, store.PushOptions{}); !errors.Is(err, store.ErrDuplicateDevice) {
			t.Fatalf("期望 ErrDuplicateDevice，实际 err=%v", err)
		}
		if d, _, _, _ := counts(t, uid); d != 0 {
			t.Fatalf("回滚后仍有设备 %d 行", d)
		}
	})

	t.Run("同批重复唯一键明确拒绝且无残留", func(t *testing.T) {
		uid := newUser(t)
		req := model.PushRequest{Devices: []model.PushDevice{
			device("AA:BB:CC:00:10:03"), device("AA:BB:CC:00:10:03"),
		}}
		if _, _, err := st.Push(context.Background(), uid, req, store.PushOptions{}); !errors.Is(err, store.ErrDuplicateDevice) {
			t.Fatalf("期望 ErrDuplicateDevice，实际 err=%v", err)
		}
		if d, _, c, _ := counts(t, uid); d != 0 || c != 0 {
			t.Fatalf("回滚后残留 devices=%d change_log=%d", d, c)
		}
	})

	t.Run("读数校验失败时设备与变更日志一并回滚", func(t *testing.T) {
		uid := newUser(t)
		now := time.Now().UTC()
		req := model.PushRequest{
			Devices:  []model.PushDevice{device("AA:BB:CC:00:10:04")},
			Readings: []model.PushReading{reading("AA:BB:CC:00:10:04", now.Add(time.Hour))},
		}
		if _, _, err := st.Push(context.Background(), uid, req, store.PushOptions{}); err == nil {
			t.Fatal("超前 1 小时的读数应被拒绝")
		}
		if d, r, c, _ := counts(t, uid); d != 0 || r != 0 || c != 0 {
			t.Fatalf("回滚后残留 devices=%d readings=%d change_log=%d", d, r, c)
		}
	})

	// 崩溃注入：每个阶段返回错误，验证 defer tx.Rollback 不留下任何部分写入。
	for _, stage := range []string{"devices_written", "change_log_written", "readings_written", "idempotency_written", "before_commit"} {
		stage := stage
		t.Run("崩溃注入 "+stage, func(t *testing.T) {
			uid := newUser(t)
			restore := postgres.SetFaultHook(func(s string) error {
				if s == stage {
					return fmt.Errorf("injected crash at %s", s)
				}
				return nil
			})
			defer restore()

			now := time.Now().UTC()
			req := model.PushRequest{
				Devices:  []model.PushDevice{device("AA:BB:CC:00:10:05")},
				Readings: []model.PushReading{reading("AA:BB:CC:00:10:05", now.Add(-time.Minute))},
			}
			opts := store.PushOptions{IdempotencyKey: "crash-" + uuid.NewString(), RequestHash: "hash"}
			if _, _, err := st.Push(context.Background(), uid, req, opts); err == nil {
				t.Fatalf("阶段 %s 应返回错误", stage)
			}
			if d, r, c, i := counts(t, uid); d != 0 || r != 0 || c != 0 || i != 0 {
				t.Fatalf("回滚后残留 devices=%d readings=%d change_log=%d idempotency=%d", d, r, c, i)
			}

			// 崩溃后同一 Idempotency-Key 重试：幂等记录未提交，应作为新请求处理并成功。
			restore()
			res, replayed, err := st.Push(context.Background(), uid, req, opts)
			if err != nil || replayed {
				t.Fatalf("重试应成功且非重放，replayed=%v err=%v", replayed, err)
			}
			if d, _, _, i := counts(t, uid); d != 1 || i != 1 {
				t.Fatalf("重试后 devices=%d idempotency=%d", d, i)
			}
			if len(res.Applied) != 1 || res.AppliedReadings != 1 {
				t.Fatalf("重试结果异常: %+v", res)
			}
		})
	}

	t.Run("计数与实际写入严格对应", func(t *testing.T) {
		uid := newUser(t)
		now := time.Now().UTC()
		dup := reading("AA:BB:CC:00:10:06", now.Add(-2*time.Minute))
		req := model.PushRequest{
			Devices: []model.PushDevice{
				device("AA:BB:CC:00:10:06"), device("AA:BB:CC:00:10:07"), device("AA:BB:CC:00:10:08"),
			},
			Readings: []model.PushReading{
				dup, dup, // 同批重复读数：一条写入、一条跳过
				reading("AA:BB:CC:00:10:07", now.Add(-time.Minute)),
			},
		}
		res, replayed, err := st.Push(context.Background(), uid, req, store.PushOptions{})
		if err != nil || replayed {
			t.Fatalf("push 失败 err=%v replayed=%v", err, replayed)
		}
		applied := 0
		for _, item := range res.Applied {
			if item.Status == "applied" {
				applied++
			}
		}
		if applied != len(req.Devices) {
			t.Fatalf("applied 设备数=%d，期望 %d", applied, len(req.Devices))
		}
		if res.AppliedReadings+res.SkippedReadings != len(req.Readings) {
			t.Fatalf("读数计数不守恒: applied=%d skipped=%d total=%d",
				res.AppliedReadings, res.SkippedReadings, len(req.Readings))
		}
		devices, readings, changes, _ := counts(t, uid)
		if devices != applied {
			t.Fatalf("设备实际行数=%d applied=%d", devices, applied)
		}
		if readings != res.AppliedReadings {
			t.Fatalf("读数实际行数=%d appliedReadings=%d", readings, res.AppliedReadings)
		}
		if changes != applied+2 {
			// 3 台设备各 1 条；读数命中的 2 台已存设备各追加 1 条 lastSeenAt 变更
			//（同批重复读数只算一次）。
			t.Fatalf("change_log 行数=%d 期望 %d（设备 %d + 在线状态 2）", changes, applied+2, applied)
		}
	})

	t.Run("并发 push 不丢变更且游标单调", func(t *testing.T) {
		uid := newUser(t)
		const n = 8
		var wg sync.WaitGroup
		errCh := make(chan error, n)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				req := model.PushRequest{Devices: []model.PushDevice{
					device(fmt.Sprintf("AA:BB:CC:00:11:%02X", i)),
				}}
				if _, _, err := st.Push(context.Background(), uid, req, store.PushOptions{}); err != nil {
					errCh <- err
				}
			}(i)
		}
		wg.Wait()
		close(errCh)
		for err := range errCh {
			t.Fatalf("并发 push 失败: %v", err)
		}

		if devices, _, changes, _ := counts(t, uid); devices != n || changes != n {
			t.Fatalf("并发后 devices=%d change_log=%d，期望 %d", devices, changes, n)
		}
		page, err := st.Changes(context.Background(), uid, 0, 100)
		if err != nil {
			t.Fatalf("读取变更失败: %v", err)
		}
		if len(page.Changes) != n {
			t.Fatalf("变更条数=%d，期望 %d", len(page.Changes), n)
		}
		for i := 1; i < len(page.Changes); i++ {
			if page.Changes[i].Seq <= page.Changes[i-1].Seq {
				t.Fatalf("seq 未严格递增: %d -> %d", page.Changes[i-1].Seq, page.Changes[i].Seq)
			}
		}
	})
}
