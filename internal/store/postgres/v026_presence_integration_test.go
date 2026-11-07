package postgres_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/platojobs/PJGoIoT/internal/model"
	"github.com/platojobs/PJGoIoT/internal/store"
	"github.com/platojobs/PJGoIoT/internal/store/postgres"
)

// TestDevicePresencePostgres 用真实 Postgres 验证设备在线状态契约 0.21.0：
// 落库与回读、读数上报推导、增量同步可见、窗口内外、从未上报、跨用户隔离。
func TestDevicePresencePostgres(t *testing.T) {
	dsn := testDSN(t)
	runMigrations(t, dsn)
	ctx := context.Background()
	st, err := postgres.New(ctx, dsn)
	if err != nil {
		t.Fatalf("连接数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	userA := newHouseholdUser(t, st)
	userB := newHouseholdUser(t, st)
	t.Cleanup(func() { cleanupUsers(t, dsn, userA, userB) })

	raw, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("打开原始连接失败: %v", err)
	}
	defer raw.Close()

	const ext = "AA:BB:CC:00:00:61"
	entityID := func(user string) string { return user + ":android:" + ext }
	base := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Microsecond)
	deviceA := uuid.NewString()
	deviceB := uuid.NewString()

	upsert := func(user, deviceID string) model.PushResult {
		t.Helper()
		res, _, err := st.Push(ctx, user, model.PushRequest{
			ClientID: "presence-it",
			Devices: []model.PushDevice{{
				ID: deviceID, Platform: "android", ExternalID: ext, MAC: ext,
				Alias: strPtr("在线状态设备"), Rules: json.RawMessage(`{}`),
				ScanRecords: json.RawMessage(`[]`), UpdatedAt: base,
			}},
		}, store.PushOptions{})
		if err != nil {
			t.Fatalf("设备 push 失败: %v", err)
		}
		return res
	}

	pushReadings := func(user string, readingIDs ...string) model.PushResult {
		t.Helper()
		items := make([]model.PushReading, 0, len(readingIDs))
		for i, id := range readingIDs {
			items = append(items, model.PushReading{
				ID: id, Platform: "android", ExternalID: ext, Metric: "temperature",
				Value: 21 + float64(i), Unit: "C", RecordedAt: base, // 旧 recordedAt 不应让设备重新上线
			})
		}
		res, _, err := st.Push(ctx, user, model.PushRequest{
			ClientID: "presence-it", Readings: items,
		}, store.PushOptions{})
		if err != nil {
			t.Fatalf("读数 push 失败: %v", err)
		}
		return res
	}

	getDevice := func(user, deviceID string) model.UserDevice {
		t.Helper()
		devices, err := st.ListUserDevices(ctx, user)
		if err != nil {
			t.Fatalf("读取设备失败: %v", err)
		}
		for _, d := range devices {
			if d.ID == deviceID {
				return d
			}
		}
		t.Fatalf("设备 %s 未找到", deviceID)
		return model.UserDevice{}
	}

	countChanges := func(user string) int {
		t.Helper()
		var n int
		if err := raw.QueryRowContext(ctx, `SELECT count(*) FROM change_log
			WHERE user_id = $1 AND entity = 'user_device' AND entity_id = $2`,
			user, entityID(user)).Scan(&n); err != nil {
			t.Fatalf("统计 change_log 失败: %v", err)
		}
		return n
	}

	rawLastSeen := func(user, deviceID string) sql.NullTime {
		t.Helper()
		var seen sql.NullTime
		if err := raw.QueryRowContext(ctx, `SELECT last_seen_at FROM user_devices
			WHERE user_id = $1 AND id = $2`, user, deviceID).Scan(&seen); err != nil {
			t.Fatalf("读取 last_seen_at 失败: %v", err)
		}
		return seen
	}

	t.Run("从未上报：lastSeenAt 为 null 且 presence=unknown", func(t *testing.T) {
		res := upsert(userA, deviceA)
		if len(res.Applied) != 1 || res.Applied[0].Status != "applied" {
			t.Fatalf("设备写入结果异常: %+v", res)
		}
		d := getDevice(userA, deviceA)
		if d.LastSeenAt != nil || d.Presence != model.PresenceUnknown {
			t.Fatalf("从未上报应为 unknown/null，实际 presence=%q lastSeenAt=%v", d.Presence, d.LastSeenAt)
		}
		if seen := rawLastSeen(userA, deviceA); seen.Valid {
			t.Fatalf("数据库 last_seen_at 应为 NULL，实际 %s", seen.Time)
		}
		if n := countChanges(userA); n != 1 {
			t.Fatalf("设备创建 change_log=%d want=1", n)
		}
		upsert(userB, deviceB) // 同 externalId，不同账号
	})

	t.Run("读数上报推进 lastSeenAt 并立即可见", func(t *testing.T) {
		before := time.Now().UTC()
		res := pushReadings(userA, uuid.NewString(), uuid.NewString())
		if res.AppliedReadings != 2 {
			t.Fatalf("appliedReadings=%d want=2", res.AppliedReadings)
		}
		d := getDevice(userA, deviceA)
		if d.LastSeenAt == nil || d.Presence != model.PresenceOnline {
			t.Fatalf("读数后应为 online，实际 presence=%q lastSeenAt=%v", d.Presence, d.LastSeenAt)
		}
		if d.LastSeenAt.Before(before) || d.LastSeenAt.After(time.Now().UTC().Add(time.Second)) {
			t.Fatalf("lastSeenAt=%s 不是服务端接收时间", d.LastSeenAt)
		}
		if d.LastSeenAt.Nanosecond()%1000 != 0 {
			t.Fatalf("lastSeenAt 未截断到微秒: %s", d.LastSeenAt.Format(time.RFC3339Nano))
		}
		if !d.UpdatedAt.Equal(base) {
			t.Fatalf("读数不应改变设备 updatedAt: %s", d.UpdatedAt)
		}
		seen := rawLastSeen(userA, deviceA)
		if !seen.Valid || !seen.Time.UTC().Equal(d.LastSeenAt.UTC()) {
			t.Fatalf("数据库 last_seen_at=%v 与接口 %v 不一致", seen, d.LastSeenAt)
		}
		if n := countChanges(userA); n != 2 {
			t.Fatalf("读数后 change_log=%d want=2（设备创建 1 + 在线状态 1）", n)
		}
	})

	t.Run("增量同步可见且 payload 带 lastSeenAt", func(t *testing.T) {
		page, err := st.Changes(ctx, userA, 0, 100)
		if err != nil {
			t.Fatalf("拉取变更失败: %v", err)
		}
		var found bool
		for _, ch := range page.Changes {
			if ch.Entity != "user_device" || ch.EntityID != entityID(userA) {
				continue
			}
			var d model.UserDevice
			if err := json.Unmarshal(ch.Payload, &d); err != nil {
				t.Fatalf("解析 payload 失败: %v", err)
			}
			if d.LastSeenAt != nil && d.Presence == model.PresenceOnline {
				found = true
			}
		}
		if !found {
			t.Fatalf("增量变更中未见带 lastSeenAt 的 user_device 条目: %+v", page.Changes)
		}
	})

	t.Run("再次上报推进 lastSeenAt 并新增一条变更", func(t *testing.T) {
		time.Sleep(2 * time.Millisecond) // 保证接收时间严格前进
		prev := getDevice(userA, deviceA)
		pushReadings(userA, uuid.NewString())
		cur := getDevice(userA, deviceA)
		if !cur.LastSeenAt.After(*prev.LastSeenAt) {
			t.Fatalf("lastSeenAt 未推进: prev=%s cur=%s", prev.LastSeenAt, cur.LastSeenAt)
		}
		if n := countChanges(userA); n != 3 {
			t.Fatalf("第二次上报后 change_log=%d want=3", n)
		}
	})

	t.Run("窗口边界：刚在窗口内 online，刚超出 offline", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			offset string
			want   string
		}{
			{"窗口内 14m50s", "14 minutes 50 seconds", model.PresenceOnline},
			{"超出 15m10s", "15 minutes 10 seconds", model.PresenceOffline},
		} {
			if _, err := raw.ExecContext(ctx, `UPDATE user_devices SET last_seen_at = now() - $1::interval
				WHERE user_id = $2 AND id = $3`, tc.offset, userA, deviceA); err != nil {
				t.Fatalf("设置 last_seen_at 失败: %v", err)
			}
			d := getDevice(userA, deviceA)
			if d.Presence != tc.want {
				t.Fatalf("%s presence=%q want=%q (lastSeenAt=%s)", tc.name, d.Presence, tc.want, d.LastSeenAt)
			}
			if d.LastSeenAt == nil {
				t.Fatalf("%s 不应丢失 lastSeenAt", tc.name)
			}
		}
		// 窗口外再次上报会重新上线并新增变更。
		pushReadings(userA, uuid.NewString())
		if d := getDevice(userA, deviceA); d.Presence != model.PresenceOnline {
			t.Fatalf("重新上报后 presence=%q want=online", d.Presence)
		}
	})

	t.Run("跨用户隔离：A 的读数不影响 B，B 的读数不影响 A", func(t *testing.T) {
		beforeA := getDevice(userA, deviceA).LastSeenAt
		// B 的读数只命中 B 自己的设备行。
		pushReadings(userB, uuid.NewString())
		afterA := getDevice(userA, deviceA).LastSeenAt
		if !beforeA.Equal(*afterA) {
			t.Fatalf("A 的 lastSeenAt 被 B 的读数改动: before=%s after=%s", beforeA, afterA)
		}
		db := getDevice(userB, deviceB)
		if db.LastSeenAt == nil || db.Presence != model.PresenceOnline {
			t.Fatalf("B 设备应 online，实际 presence=%q lastSeenAt=%v", db.Presence, db.LastSeenAt)
		}
		if n := countChanges(userB); n != 2 {
			t.Fatalf("B 的 change_log=%d want=2（B 自己的设备创建 + 在线状态）", n)
		}
		if n := countChanges(userA); n < 2 {
			t.Fatalf("A 的 change_log 不应被 B 影响，实际 %d", n)
		}
	})
}
