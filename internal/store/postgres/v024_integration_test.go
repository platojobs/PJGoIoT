package postgres_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/platojobs/PJGoIoT/internal/store/postgres"
)

// TestChangesMultiHouseholdPaginationPostgres 校验多家庭可见性查询：
//   - 本人分支与家庭分支同时命中同一条记录（个人家庭、本人写给共享家庭）时，
//     外层去重不会重复返回，也不会漏行；
//   - 小页长连续翻页时 seq 严格单调递增，翻页结果与直接按可见性谓词查询的全集一致。
func TestChangesMultiHouseholdPaginationPostgres(t *testing.T) {
	dsn := testDSN(t)
	runMigrations(t, dsn)
	ctx := context.Background()
	st, err := postgres.New(ctx, dsn)
	if err != nil {
		t.Fatalf("连接数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	userID := newHouseholdUser(t, st)
	ownerID := newHouseholdUser(t, st)
	t.Cleanup(func() { cleanupUsers(t, dsn, userID, ownerID) })

	personalID := mustPersonalHousehold(t, st, userID)

	// 5 个共享家庭，userID 全部加入（模拟多家庭成员上限场景）。
	shared := make([]string, 0, 5)
	for i := 0; i < 5; i++ {
		home, err := st.CreateHousehold(ctx, ownerID, fmt.Sprintf("分页家庭-%d", i))
		if err != nil {
			t.Fatalf("创建家庭失败: %v", err)
		}
		codeHash := hashOf("paging-code-" + uuid.NewString())
		if _, err := st.CreateHouseholdInvitation(ctx, ownerID, home.ID, codeHash, time.Now().UTC().Add(10*time.Minute)); err != nil {
			t.Fatalf("创建邀请码失败: %v", err)
		}
		if _, err := st.ClaimHouseholdInvitation(ctx, userID, codeHash, time.Now().UTC(), 10*time.Minute); err != nil {
			t.Fatalf("领取邀请码失败: %v", err)
		}
		shared = append(shared, home.ID)
	}

	raw, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("打开原始连接失败: %v", err)
	}
	defer raw.Close()

	insert := func(user uuid.UUID, household *string, entityID string) {
		t.Helper()
		if _, err := raw.ExecContext(ctx, `INSERT INTO change_log
			(user_id, household_id, entity, entity_id, op, payload)
			VALUES ($1, $2, 'user_device', $3, 'upsert', '{"n":1}'::jsonb)`,
			user, household, entityID); err != nil {
			t.Fatalf("写入变更失败: %v", err)
		}
	}

	// 仅本人可见（household_id 为 NULL）
	for i := 0; i < 20; i++ {
		insert(uuid.MustParse(userID), nil, fmt.Sprintf("only-user-%d", i))
	}
	// 本人写给个人家庭：两个分支都会命中，必须去重
	for i := 0; i < 20; i++ {
		hid := personalID
		insert(uuid.MustParse(userID), &hid, fmt.Sprintf("personal-dup-%d", i))
	}
	// 本人写给共享家庭：两个分支都会命中，必须去重
	for i := 0; i < 20; i++ {
		hid := shared[0]
		insert(uuid.MustParse(userID), &hid, fmt.Sprintf("shared-dup-%d", i))
	}
	// 其他成员写给共享家庭：仅家庭分支可见
	for _, hid := range shared {
		for i := 0; i < 20; i++ {
			h := hid
			insert(uuid.MustParse(ownerID), &h, fmt.Sprintf("shared-%s-%d", hid, i))
		}
	}

	// 参照集合：与业务语义完全一致的可见性谓词，按 seq 排序。
	rows, err := raw.QueryContext(ctx, `
		SELECT c.seq FROM change_log c
		WHERE c.user_id = $1 OR c.household_id IN (
			SELECT hm.household_id FROM household_members hm WHERE hm.user_id = $1)
		ORDER BY c.seq`, userID)
	if err != nil {
		t.Fatalf("参照查询失败: %v", err)
	}
	defer rows.Close()
	var want []int64
	for rows.Next() {
		var seq int64
		if err := rows.Scan(&seq); err != nil {
			t.Fatalf("读取参照 seq 失败: %v", err)
		}
		want = append(want, seq)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("参照查询遍历失败: %v", err)
	}

	// 用 limit=7 逐页拉取，验证逐页无重复且与参照集合完全一致。
	const limit = 7
	var got []int64
	cursor := int64(0)
	for pages := 0; ; pages++ {
		if pages > len(want)/limit+3 {
			t.Fatalf("翻页次数异常，可能存在死循环: 已取 %d 条，期望 %d 条", len(got), len(want))
		}
		page, err := st.Changes(ctx, userID, cursor, limit)
		if err != nil {
			t.Fatalf("拉取变更失败 cursor=%d: %v", cursor, err)
		}
		if len(page.Changes) > limit {
			t.Fatalf("单页返回 %d 条，超过 limit=%d", len(page.Changes), limit)
		}
		if !page.HasMore && len(page.Changes) != len(want)-len(got) {
			t.Fatalf("末页数量异常: page=%d remaining=%d", len(page.Changes), len(want)-len(got))
		}
		for _, ch := range page.Changes {
			if len(got) > 0 && ch.Seq <= got[len(got)-1] {
				t.Fatalf("seq 非严格递增: prev=%d cur=%d", got[len(got)-1], ch.Seq)
			}
			got = append(got, ch.Seq)
		}
		if !page.HasMore {
			break
		}
		if page.NextCursor != got[len(got)-1] {
			t.Fatalf("NextCursor=%d 与最后一条 seq=%d 不一致", page.NextCursor, got[len(got)-1])
		}
		cursor = page.NextCursor
	}

	if len(got) != len(want) {
		t.Fatalf("翻页共取 %d 条，期望 %d 条（存在漏行或重复）", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 条不一致: got=%d want=%d", i, got[i], want[i])
		}
	}
}
