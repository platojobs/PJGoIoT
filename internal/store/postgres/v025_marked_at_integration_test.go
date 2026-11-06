package postgres_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/platojobs/PJGoIoT/internal/model"
	"github.com/platojobs/PJGoIoT/internal/store"
	"github.com/platojobs/PJGoIoT/internal/store/postgres"
)

// TestDeviceMarkedAtPostgres 用真实 Postgres 验证设备标记：
// 标记/取消标记写库并进入 change_log；重复同一时间戳不产生新条目；
// 旧写入不覆盖新标记；跨用户互不可见。
func TestDeviceMarkedAtPostgres(t *testing.T) {
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

	const ext = "AA:BB:CC:00:00:41"
	base := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Microsecond)
	entityID := userA + ":android:" + ext

	push := func(user, deviceID string, updatedAt time.Time, mark model.OptionalTime) model.PushResult {
		t.Helper()
		res, _, err := st.Push(ctx, user, model.PushRequest{
			ClientID: "mark-it",
			Devices: []model.PushDevice{{
				ID: deviceID, Platform: "android", ExternalID: ext, MAC: ext,
				Alias: strPtr("标记设备"), Rules: json.RawMessage(`{}`),
				ScanRecords: json.RawMessage(`[]`), UpdatedAt: updatedAt, MarkedAt: mark,
			}},
		}, store.PushOptions{})
		if err != nil {
			t.Fatalf("push 失败: %v", err)
		}
		return res
	}

	countChanges := func(user string) int {
		t.Helper()
		var n int
		if err := raw.QueryRowContext(ctx, `SELECT count(*) FROM change_log
			WHERE user_id = $1 AND entity = 'user_device' AND entity_id = $2`,
			user, user+":android:"+ext).Scan(&n); err != nil {
			t.Fatalf("统计 change_log 失败: %v", err)
		}
		return n
	}

	markedAt := func(user, deviceID string) *time.Time {
		t.Helper()
		devices, err := st.ListUserDevices(ctx, user)
		if err != nil {
			t.Fatalf("读取设备失败: %v", err)
		}
		for _, d := range devices {
			if d.ID == deviceID {
				return d.MarkedAt
			}
		}
		t.Fatalf("设备 %s 未找到", deviceID)
		return nil
	}

	// 1) 标记：写库 + 一条 change_log。
	deviceID := uuid.NewString()
	res := push(userA, deviceID, base, model.OptionalTime{Set: true, Time: ptrTimeValue(base)})
	if len(res.Applied) != 1 || res.Applied[0].Status != "applied" {
		t.Fatalf("首次标记应 applied: %+v", res.Applied)
	}
	if got := markedAt(userA, deviceID); got == nil || !got.Equal(base) {
		t.Fatalf("marked_at=%v want=%v", got, base)
	}
	if n := countChanges(userA); n != 1 {
		t.Fatalf("首次标记后 change_log=%d want=1", n)
	}

	// 2) 重复上报完全相同的时间戳与内容：unchanged，不新增条目。
	res = push(userA, deviceID, base, model.OptionalTime{Set: true, Time: ptrTimeValue(base.Add(500 * time.Nanosecond))})
	if len(res.Applied) != 1 || res.Applied[0].Status != "unchanged" {
		t.Fatalf("重复标记应 unchanged: %+v", res.Applied)
	}
	if n := countChanges(userA); n != 1 {
		t.Fatalf("重复标记不应新增 change_log，得到 %d", n)
	}

	// 3) 取消标记：null 写库为 NULL，新增一条。
	unmarkAt := base.Add(time.Second)
	res = push(userA, deviceID, unmarkAt, model.OptionalTime{Set: true})
	if len(res.Applied) != 1 || res.Applied[0].Status != "applied" {
		t.Fatalf("取消标记应 applied: %+v", res.Applied)
	}
	if got := markedAt(userA, deviceID); got != nil {
		t.Fatalf("取消标记后 marked_at 应为 nil，得到 %v", got)
	}
	if n := countChanges(userA); n != 2 {
		t.Fatalf("取消标记后 change_log=%d want=2", n)
	}

	// 4) 旧写入（更早 updatedAt）不得覆盖取消后的状态。
	res = push(userA, deviceID, base, model.OptionalTime{Set: true, Time: ptrTimeValue(base)})
	if len(res.Conflicts) != 1 || len(res.Applied) != 0 {
		t.Fatalf("旧标记应冲突: applied=%+v conflicts=%+v", res.Applied, res.Conflicts)
	}
	if got := markedAt(userA, deviceID); got != nil {
		t.Fatalf("旧标记不应写库，得到 %v", got)
	}
	if n := countChanges(userA); n != 2 {
		t.Fatalf("冲突不应新增 change_log，得到 %d", n)
	}

	// 5) 再次标记：新增一条，并可通过 /sync/changes 的 payload 还原。
	newMark := base.Add(2 * time.Second)
	res = push(userA, deviceID, newMark, model.OptionalTime{Set: true, Time: ptrTimeValue(newMark)})
	if len(res.Applied) != 1 || res.Applied[0].Status != "applied" {
		t.Fatalf("再次标记应 applied: %+v", res.Applied)
	}
	if n := countChanges(userA); n != 3 {
		t.Fatalf("再次标记后 change_log=%d want=3", n)
	}
	page, err := st.Changes(ctx, userA, 0, 100)
	if err != nil {
		t.Fatalf("读取变更失败: %v", err)
	}
	var payload model.UserDevice
	found := false
	for _, ch := range page.Changes {
		if ch.Entity == "user_device" && ch.EntityID == entityID && ch.Op == "upsert" {
			if err := json.Unmarshal(ch.Payload, &payload); err != nil {
				t.Fatalf("解析 change_log payload 失败: %v", err)
			}
			found = true
		}
	}
	if !found || payload.MarkedAt == nil || !payload.MarkedAt.Equal(newMark) {
		t.Fatalf("change_log payload 未携带 markedAt: %+v", payload.MarkedAt)
	}

	// 6) 缺省 markedAt 保持原值，不把标记清掉。
	res = push(userA, deviceID, newMark.Add(time.Second), model.OptionalTime{})
	if len(res.Applied) != 1 || res.Applied[0].Status != "applied" {
		t.Fatalf("缺省 markedAt 的新写入应 applied: %+v", res.Applied)
	}
	if got := markedAt(userA, deviceID); got == nil || !got.Equal(newMark) {
		t.Fatalf("缺省 markedAt 不应清除标记，得到 %v", got)
	}

	// 7) 跨用户隔离：同 externalId 在另一个用户名下是另一条记录、另一条变更。
	otherMark := base.Add(4 * time.Second)
	res = push(userB, uuid.NewString(), otherMark, model.OptionalTime{Set: true, Time: ptrTimeValue(otherMark)})
	if len(res.Applied) != 1 {
		t.Fatalf("用户 B 标记失败: %+v", res)
	}
	if n := countChanges(userB); n != 1 {
		t.Fatalf("用户 B change_log=%d want=1", n)
	}
	if got := markedAt(userA, deviceID); got == nil || !got.Equal(newMark) {
		t.Fatalf("用户 B 的写入不应影响用户 A，得到 %v", got)
	}
}

func ptrTimeValue(t time.Time) *time.Time { return &t }
