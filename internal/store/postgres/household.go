package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/platojobs/PJGoIoT/internal/model"
	"github.com/platojobs/PJGoIoT/internal/store"
)

// householdTimeout 是家庭类写操作的超时预算（含成员/房间/邀请码事务）。
const householdTimeout = 5 * time.Second

// householdRole 返回用户在家庭中的角色；非成员与不存在统一返回 ErrNotFound。
func householdRole(ctx context.Context, db rowQueryer, uid, householdID uuid.UUID) (string, error) {
	var role string
	err := db.QueryRowContext(ctx,
		`SELECT role FROM household_members WHERE household_id = $1 AND user_id = $2`,
		householdID, uid).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return "", store.ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("查询家庭角色失败: %w", err)
	}
	return role, nil
}

// requireHouseholdOwner 校验 owner 角色；成员返回 ErrForbidden，非成员返回 ErrNotFound。
func requireHouseholdOwner(ctx context.Context, db rowQueryer, uid, householdID uuid.UUID) error {
	role, err := householdRole(ctx, db, uid, householdID)
	if err != nil {
		return err
	}
	if role != "owner" {
		return store.ErrForbidden
	}
	return nil
}

type rowQueryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// CreateHousehold 实现 store.Store。
func (s *Store) CreateHousehold(ctx context.Context, ownerUserID string, name string) (model.Household, error) {
	uid, err := uuid.Parse(ownerUserID)
	if err != nil {
		return model.Household{}, fmt.Errorf("非法 user id: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, householdTimeout)
	defer cancel()

	hid := uuid.New()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Household{}, fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO households (id, name, owner_user_id, is_personal) VALUES ($1, $2, $3, false)`,
		hid, name, uid); err != nil {
		return model.Household{}, fmt.Errorf("创建家庭失败: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO household_members (household_id, user_id, role) VALUES ($1, $2, 'owner')`,
		hid, uid); err != nil {
		return model.Household{}, fmt.Errorf("写入家庭成员失败: %w", err)
	}
	snapshot, err := householdSnapshot(ctx, tx, hid)
	if err != nil {
		return model.Household{}, err
	}
	if err := emitHouseholdChange(ctx, tx, snapshot, "upsert", false); err != nil {
		return model.Household{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.Household{}, fmt.Errorf("提交事务失败: %w", err)
	}
	return s.GetHousehold(ctx, ownerUserID, hid.String())
}

// ListHouseholds 实现 store.Store。
func (s *Store) ListHouseholds(ctx context.Context, userID string) ([]model.Household, error) {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return nil, fmt.Errorf("非法 user id: %w", err)
	}
	ctx, cancel := queryTimeout(ctx)
	defer cancel()

	rows, err := s.db.QueryContext(ctx, householdSelectSQL+`
		WHERE m.user_id = $1
		ORDER BY h.is_personal DESC, h.created_at ASC, h.id`, uid)
	if err != nil {
		return nil, fmt.Errorf("查询家庭列表失败: %w", err)
	}
	defer rows.Close()
	out := []model.Household{}
	for rows.Next() {
		h, err := scanHousehold(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历家庭列表失败: %w", err)
	}
	return out, nil
}

// GetHousehold 实现 store.Store。
func (s *Store) GetHousehold(ctx context.Context, userID string, householdID string) (model.Household, error) {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return model.Household{}, fmt.Errorf("非法 user id: %w", err)
	}
	hid, err := uuid.Parse(householdID)
	if err != nil {
		return model.Household{}, store.ErrNotFound
	}
	ctx, cancel := queryTimeout(ctx)
	defer cancel()
	row := s.db.QueryRowContext(ctx, householdSelectSQL+`
		WHERE h.id = $2 AND m.user_id = $1`, uid, hid)
	h, err := scanHousehold(row)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Household{}, store.ErrNotFound
	}
	if err != nil {
		return model.Household{}, err
	}
	return h, nil
}

const householdSelectSQL = `
SELECT h.id, h.name, h.owner_user_id, m.role, h.is_personal, h.created_at,
       (SELECT count(*) FROM household_members x WHERE x.household_id = h.id) AS member_count,
       (SELECT count(*) FROM user_devices d WHERE d.household_id = h.id AND d.deleted_at IS NULL) AS device_count,
       (SELECT count(*) FROM household_rooms r WHERE r.household_id = h.id) AS room_count
FROM households h
JOIN household_members m ON m.household_id = h.id`

type householdScanner interface {
	Scan(dest ...any) error
}

func scanHousehold(row householdScanner) (model.Household, error) {
	var (
		h         model.Household
		id, owner uuid.UUID
	)
	if err := row.Scan(&id, &h.Name, &owner, &h.Role, &h.Personal, &h.CreatedAt, &h.MemberCount, &h.DeviceCount, &h.RoomCount); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.Household{}, err
		}
		return model.Household{}, fmt.Errorf("解析家庭失败: %w", err)
	}
	h.ID = id.String()
	h.OwnerUserID = owner.String()
	h.CreatedAt = h.CreatedAt.UTC()
	return h, nil
}

// RenameHousehold 实现 store.Store。
func (s *Store) RenameHousehold(ctx context.Context, userID string, householdID string, name string) error {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return fmt.Errorf("非法 user id: %w", err)
	}
	hid, err := uuid.Parse(householdID)
	if err != nil {
		return store.ErrNotFound
	}
	ctx, cancel := context.WithTimeout(ctx, householdTimeout)
	defer cancel()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()
	if err := requireHouseholdOwner(ctx, tx, uid, hid); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE households SET name = $2 WHERE id = $1`, hid, name)
	if err != nil {
		return fmt.Errorf("更新家庭失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	snapshot, err := householdSnapshot(ctx, tx, hid)
	if err != nil {
		return err
	}
	if err := emitHouseholdChange(ctx, tx, snapshot, "upsert", false); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交事务失败: %w", err)
	}
	return nil
}

// DeleteHousehold 实现 store.Store。
// 设备按“各自的 owner 的个人家庭”回迁，房间与成员由外键级联清理。
func (s *Store) DeleteHousehold(ctx context.Context, userID string, householdID string) error {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return fmt.Errorf("非法 user id: %w", err)
	}
	hid, err := uuid.Parse(householdID)
	if err != nil {
		return store.ErrNotFound
	}
	ctx, cancel := context.WithTimeout(ctx, householdTimeout)
	defer cancel()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	var personal bool
	if err := tx.QueryRowContext(ctx, `SELECT is_personal FROM households WHERE id = $1`, hid).Scan(&personal); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return store.ErrNotFound
		}
		return fmt.Errorf("查询家庭失败: %w", err)
	}
	if err := requireHouseholdOwner(ctx, tx, uid, hid); err != nil {
		return err
	}
	if personal {
		return store.ErrForbidden
	}

	// 先回迁到各设备 owner 的个人家庭；没有个人家庭的设备（理论上不存在）置 NULL。
	if _, err := tx.ExecContext(ctx, `
		UPDATE user_devices d
		SET household_id = h.id, room_id = NULL
		FROM households h
		WHERE d.household_id = $1 AND h.owner_user_id = d.user_id AND h.is_personal = true`, hid); err != nil {
		return fmt.Errorf("回迁家庭设备失败: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE user_devices SET household_id = NULL, room_id = NULL WHERE household_id = $1`, hid); err != nil {
		return fmt.Errorf("清理家庭设备归属失败: %w", err)
	}
	snapshot, err := householdSnapshot(ctx, tx, hid)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM households WHERE id = $1`, hid); err != nil {
		return fmt.Errorf("删除家庭失败: %w", err)
	}
	// 解散家庭只扇出一条 household delete：成员/房间随家庭一并消失，客户端按家庭整段移除即可。
	if err := emitHouseholdChange(ctx, tx, snapshot, "delete", true); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交事务失败: %w", err)
	}
	return nil
}

// ListHouseholdMembers 实现 store.Store。
func (s *Store) ListHouseholdMembers(ctx context.Context, userID string, householdID string) ([]model.HouseholdMember, error) {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return nil, fmt.Errorf("非法 user id: %w", err)
	}
	hid, err := uuid.Parse(householdID)
	if err != nil {
		return nil, store.ErrNotFound
	}
	ctx, cancel := queryTimeout(ctx)
	defer cancel()
	if _, err := householdRole(ctx, s.db, uid, hid); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT m.user_id, m.role, u.kind, m.joined_at, (m.user_id = $2) AS is_self
		FROM household_members m
		JOIN users u ON u.id = m.user_id
		WHERE m.household_id = $1
		ORDER BY m.joined_at ASC, m.user_id`, hid, uid)
	if err != nil {
		return nil, fmt.Errorf("查询家庭成员失败: %w", err)
	}
	defer rows.Close()
	out := []model.HouseholdMember{}
	for rows.Next() {
		var (
			m  model.HouseholdMember
			id uuid.UUID
		)
		if err := rows.Scan(&id, &m.Role, &m.Kind, &m.JoinedAt, &m.IsSelf); err != nil {
			return nil, fmt.Errorf("解析家庭成员失败: %w", err)
		}
		m.UserID = id.String()
		m.JoinedAt = m.JoinedAt.UTC()
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历家庭成员失败: %w", err)
	}
	return out, nil
}

// RemoveHouseholdMember 实现 store.Store。
// 被移除成员的设备回到其个人家庭并清空房间，避免设备继续暴露给原家庭。
func (s *Store) RemoveHouseholdMember(ctx context.Context, userID string, householdID string, targetUserID string) error {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return fmt.Errorf("非法 user id: %w", err)
	}
	hid, err := uuid.Parse(householdID)
	if err != nil {
		return store.ErrNotFound
	}
	tid, err := uuid.Parse(targetUserID)
	if err != nil {
		return store.ErrNotFound
	}
	ctx, cancel := context.WithTimeout(ctx, householdTimeout)
	defer cancel()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()
	if err := requireHouseholdOwner(ctx, tx, uid, hid); err != nil {
		return err
	}
	var targetRole string
	err = tx.QueryRowContext(ctx, `
		SELECT role FROM household_members WHERE household_id = $1 AND user_id = $2 FOR UPDATE`,
		hid, tid).Scan(&targetRole)
	if errors.Is(err, sql.ErrNoRows) {
		return store.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("查询成员失败: %w", err)
	}
	if targetRole == "owner" || tid == uid {
		return store.ErrHouseholdOwnerCannotRemoveSelf
	}
	if err := moveMemberDevicesToPersonal(ctx, tx, tid, hid); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM household_members WHERE household_id = $1 AND user_id = $2`, hid, tid); err != nil {
		return fmt.Errorf("移除成员失败: %w", err)
	}
	// 移除后快照只含剩余成员：被移除者不会收到这条（含后续所有）结构变更。
	snapshot, err := householdSnapshot(ctx, tx, hid)
	if err != nil {
		return err
	}
	if err := emitHouseholdChange(ctx, tx, snapshot, "upsert", false); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交事务失败: %w", err)
	}
	return nil
}

// LeaveHousehold 实现 store.Store。
func (s *Store) LeaveHousehold(ctx context.Context, userID string, householdID string) error {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return fmt.Errorf("非法 user id: %w", err)
	}
	hid, err := uuid.Parse(householdID)
	if err != nil {
		return store.ErrNotFound
	}
	ctx, cancel := context.WithTimeout(ctx, householdTimeout)
	defer cancel()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	var (
		personal   bool
		role       string
		otherCount int
	)
	err = tx.QueryRowContext(ctx, `
		SELECT is_personal FROM households WHERE id = $1 FOR UPDATE`, hid).Scan(&personal)
	if errors.Is(err, sql.ErrNoRows) {
		return store.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("查询家庭失败: %w", err)
	}
	err = tx.QueryRowContext(ctx, `
		SELECT role FROM household_members WHERE household_id = $1 AND user_id = $2`, hid, uid).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return store.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("查询家庭角色失败: %w", err)
	}
	if personal {
		return store.ErrPersonalHouseholdImmutable
	}

	if role == "owner" {
		if err := tx.QueryRowContext(ctx, `
			SELECT count(*) FROM household_members WHERE household_id = $1 AND user_id <> $2`, hid, uid).
			Scan(&otherCount); err != nil {
			return fmt.Errorf("查询其他成员失败: %w", err)
		}
		if otherCount > 0 {
			return store.ErrHouseholdOwnerMustTransfer
		}
		// owner 独自退出：家庭整体删除，设备回到个人家庭。
		if err := moveMemberDevicesToPersonal(ctx, tx, uid, hid); err != nil {
			return err
		}
		snapshot, err := householdSnapshot(ctx, tx, hid)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM households WHERE id = $1`, hid); err != nil {
			return fmt.Errorf("删除家庭失败: %w", err)
		}
		if err := emitHouseholdChange(ctx, tx, snapshot, "delete", true); err != nil {
			return err
		}
	} else {
		// 退出者本人收到一条 delete（其他设备上的同账号也能立刻移除该家庭），
		// 剩余成员收到成员数变化后的 upsert。
		before, err := householdSnapshot(ctx, tx, hid)
		if err != nil {
			return err
		}
		if err := moveMemberDevicesToPersonal(ctx, tx, uid, hid); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM household_members WHERE household_id = $1 AND user_id = $2`, hid, uid); err != nil {
			return fmt.Errorf("退出家庭失败: %w", err)
		}
		for _, r := range before {
			if r.UserID != uid {
				continue
			}
			p := r.Payload
			p.Deleted = true
			if err := insertStructuralChanges(ctx, tx, []uuid.UUID{uid}, "household", p.ID, "delete", p); err != nil {
				return err
			}
		}
		after, err := householdSnapshot(ctx, tx, hid)
		if err != nil {
			return err
		}
		if err := emitHouseholdChange(ctx, tx, after, "upsert", false); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交事务失败: %w", err)
	}
	return nil
}

// TransferHouseholdOwnership 实现 store.Store。
//
// 并发方案：事务内先对 households 行加 FOR UPDATE 行锁，再做角色校验与两处写入，
// 因此同时发起的两笔转让会被串行化：后到者拿到锁后重新读到「自己已不是 owner」而返回 ErrForbidden，
// 不会出现两个 owner 或零 owner。最后对 households 用 owner_user_id 条件更新兜底，
// 若影响行数为 0（极端情况下角色行与 households 不一致），整笔回滚并返回 ErrForbidden。
func (s *Store) TransferHouseholdOwnership(ctx context.Context, userID string, householdID string, targetUserID string) error {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return fmt.Errorf("非法 user id: %w", err)
	}
	hid, err := uuid.Parse(householdID)
	if err != nil {
		return store.ErrNotFound
	}
	tid, err := uuid.Parse(targetUserID)
	if err != nil {
		return store.ErrHouseholdTargetNotMember
	}
	ctx, cancel := context.WithTimeout(ctx, householdTimeout)
	defer cancel()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	var (
		personal bool
		ownerID  uuid.UUID
	)
	err = tx.QueryRowContext(ctx, `
		SELECT is_personal, owner_user_id FROM households WHERE id = $1 FOR UPDATE`, hid).
		Scan(&personal, &ownerID)
	if errors.Is(err, sql.ErrNoRows) {
		return store.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("查询家庭失败: %w", err)
	}
	// 行锁释放后重新确认调用者角色：并发转让的后到者会在这里拿到 ErrForbidden。
	if err := requireHouseholdOwner(ctx, tx, uid, hid); err != nil {
		return err
	}
	if personal {
		return store.ErrPersonalHouseholdImmutable
	}
	if tid == uid {
		return store.ErrHouseholdTargetAlreadyOwner
	}

	var targetRole string
	err = tx.QueryRowContext(ctx, `
		SELECT role FROM household_members WHERE household_id = $1 AND user_id = $2 FOR UPDATE`,
		hid, tid).Scan(&targetRole)
	if errors.Is(err, sql.ErrNoRows) {
		return store.ErrHouseholdTargetNotMember
	}
	if err != nil {
		return fmt.Errorf("查询成员失败: %w", err)
	}
	if targetRole == "owner" {
		return store.ErrHouseholdTargetAlreadyOwner
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE household_members SET role = 'member' WHERE household_id = $1 AND user_id = $2`, hid, uid); err != nil {
		return fmt.Errorf("降级原 owner 失败: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE household_members SET role = 'owner' WHERE household_id = $1 AND user_id = $2`, hid, tid); err != nil {
		return fmt.Errorf("升级新 owner 失败: %w", err)
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE households SET owner_user_id = $2 WHERE id = $1 AND owner_user_id = $3`, hid, tid, ownerID)
	if err != nil {
		return fmt.Errorf("更新家庭 owner 失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return store.ErrForbidden
	}
	snapshot, err := householdSnapshot(ctx, tx, hid)
	if err != nil {
		return err
	}
	if err := emitHouseholdChange(ctx, tx, snapshot, "upsert", false); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交事务失败: %w", err)
	}
	return nil
}

// moveMemberDevicesToPersonal 把某成员在该家庭的设备迁回其个人家庭并清空房间。
func moveMemberDevicesToPersonal(ctx context.Context, tx *sql.Tx, memberID, householdID uuid.UUID) error {
	if _, err := tx.ExecContext(ctx, `
		UPDATE user_devices d
		SET household_id = h.id, room_id = NULL
		FROM households h
		WHERE d.user_id = $1 AND d.household_id = $2
		  AND h.owner_user_id = $1 AND h.is_personal = true`, memberID, householdID); err != nil {
		return fmt.Errorf("回迁成员设备失败: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE user_devices SET household_id = NULL, room_id = NULL
		WHERE user_id = $1 AND household_id = $2`, memberID, householdID); err != nil {
		return fmt.Errorf("清理成员设备归属失败: %w", err)
	}
	return nil
}

// CreateHouseholdInvitation 实现 store.Store。
func (s *Store) CreateHouseholdInvitation(ctx context.Context, userID string, householdID string, codeHash string, expiresAt time.Time) (model.HouseholdInvitation, error) {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return model.HouseholdInvitation{}, fmt.Errorf("非法 user id: %w", err)
	}
	hid, err := uuid.Parse(householdID)
	if err != nil {
		return model.HouseholdInvitation{}, store.ErrNotFound
	}
	ctx, cancel := context.WithTimeout(ctx, householdTimeout)
	defer cancel()
	if err := requireHouseholdOwner(ctx, s.db, uid, hid); err != nil {
		return model.HouseholdInvitation{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.HouseholdInvitation{}, fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()
	// 惰性清理该家庭已过期的邀请码。
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM household_invitations WHERE household_id = $1 AND expires_at < now()`, hid); err != nil {
		return model.HouseholdInvitation{}, fmt.Errorf("清理过期邀请码失败: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO household_invitations (code_hash, household_id, invited_by_user_id, expires_at)
		VALUES ($1, $2, $3, $4)`, codeHash, hid, uid, expiresAt.UTC()); err != nil {
		return model.HouseholdInvitation{}, fmt.Errorf("写入邀请码失败: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return model.HouseholdInvitation{}, fmt.Errorf("提交事务失败: %w", err)
	}
	return model.HouseholdInvitation{HouseholdID: hid.String(), ExpiresAt: expiresAt.UTC()}, nil
}

// ListHouseholdInvitations 实现 store.Store：只返回哈希前缀、过期时间与状态。
func (s *Store) ListHouseholdInvitations(ctx context.Context, userID string, householdID string) ([]model.HouseholdInvitationInfo, error) {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return nil, fmt.Errorf("非法 user id: %w", err)
	}
	hid, err := uuid.Parse(householdID)
	if err != nil {
		return nil, store.ErrNotFound
	}
	ctx, cancel := context.WithTimeout(ctx, householdTimeout)
	defer cancel()
	if err := requireHouseholdOwner(ctx, s.db, uid, hid); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, code_hash, attempts, expires_at, created_at
		FROM household_invitations
		WHERE household_id = $1
		ORDER BY created_at DESC, id`, hid)
	if err != nil {
		return nil, fmt.Errorf("查询邀请码失败: %w", err)
	}
	defer rows.Close()
	now := time.Now().UTC()
	out := []model.HouseholdInvitationInfo{}
	for rows.Next() {
		var (
			id                   uuid.UUID
			hash                 string
			attempts             int
			expiresAt, createdAt time.Time
		)
		if err := rows.Scan(&id, &hash, &attempts, &expiresAt, &createdAt); err != nil {
			return nil, fmt.Errorf("解析邀请码失败: %w", err)
		}
		out = append(out, model.HouseholdInvitationInfo{
			ID:             id.String(),
			CodeHashPrefix: hashPrefix(hash),
			Status:         invitationStatus(attempts, expiresAt, now),
			Attempts:       attempts,
			ExpiresAt:      expiresAt.UTC(),
			CreatedAt:      createdAt.UTC(),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历邀请码失败: %w", err)
	}
	return out, nil
}

// RevokeHouseholdInvitation 实现 store.Store：删除未使用的邀请码；已用码在领取时已删除。
func (s *Store) RevokeHouseholdInvitation(ctx context.Context, userID string, householdID string, invitationID string) error {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return fmt.Errorf("非法 user id: %w", err)
	}
	hid, err := uuid.Parse(householdID)
	if err != nil {
		return store.ErrNotFound
	}
	iid, err := uuid.Parse(invitationID)
	if err != nil {
		return store.ErrNotFound
	}
	ctx, cancel := context.WithTimeout(ctx, householdTimeout)
	defer cancel()
	if err := requireHouseholdOwner(ctx, s.db, uid, hid); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM household_invitations WHERE id = $1 AND household_id = $2`, iid, hid)
	if err != nil {
		return fmt.Errorf("撤销邀请码失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// hashPrefix 返回哈希前 8 位，仅用于管理列表展示，不能还原明文。
func hashPrefix(hash string) string {
	const n = 8
	if len(hash) <= n {
		return hash
	}
	return hash[:n]
}

// invitationStatus 计算邀请码状态：attempts 达上限为 locked，否则过期/有效。
func invitationStatus(attempts int, expiresAt, now time.Time) string {
	if attempts >= maxHouseholdInvitationAttempts {
		return "locked"
	}
	if !expiresAt.After(now) {
		return "expired"
	}
	return "active"
}

// maxHouseholdInvitationAttempts 是同一邀请码允许的最大失败次数。
const maxHouseholdInvitationAttempts = 5

// ClaimHouseholdInvitation 实现 store.Store。
func (s *Store) ClaimHouseholdInvitation(ctx context.Context, userID string, codeHash string, now time.Time, ttl time.Duration) (model.Household, error) {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return model.Household{}, fmt.Errorf("非法 user id: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, householdTimeout)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Household{}, fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	var (
		storedHash string
		hid        uuid.NullUUID
		attempts   int
		expiresAt  time.Time
	)
	err = tx.QueryRowContext(ctx, `
		SELECT code_hash, household_id, attempts, expires_at
		FROM household_invitations WHERE code_hash = $1 FOR UPDATE`, codeHash).
		Scan(&storedHash, &hid, &attempts, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		if err := recordHouseholdInvitationFailure(ctx, tx, codeHash, now, ttl); err != nil {
			return model.Household{}, err
		}
		if err := tx.Commit(); err != nil {
			return model.Household{}, fmt.Errorf("提交事务失败: %w", err)
		}
		return model.Household{}, store.ErrNotFound
	}
	if err != nil {
		return model.Household{}, fmt.Errorf("查询邀请码失败: %w", err)
	}
	if !hid.Valid || attempts >= maxHouseholdInvitationAttempts || !expiresAt.After(now) {
		if err := incrementHouseholdInvitationFailure(ctx, tx, codeHash, attempts, now, ttl); err != nil {
			return model.Household{}, err
		}
		if err := tx.Commit(); err != nil {
			return model.Household{}, fmt.Errorf("提交事务失败: %w", err)
		}
		return model.Household{}, store.ErrNotFound
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO household_members (household_id, user_id, role) VALUES ($1, $2, 'member')
		ON CONFLICT (household_id, user_id) DO NOTHING`, hid.UUID, uid); err != nil {
		return model.Household{}, fmt.Errorf("加入家庭失败: %w", err)
	}
	// 一次性：用掉即删除，重放命中「不存在」分支。
	if _, err := tx.ExecContext(ctx, `DELETE FROM household_invitations WHERE code_hash = $1`, codeHash); err != nil {
		return model.Household{}, fmt.Errorf("删除已用邀请码失败: %w", err)
	}
	// 新成员不会收到加入之前的历史结构变更（历史行里没有他的 user_id），首次基线走 GET /households。
	snapshot, err := householdSnapshot(ctx, tx, hid.UUID)
	if err != nil {
		return model.Household{}, err
	}
	if err := emitHouseholdChange(ctx, tx, snapshot, "upsert", false); err != nil {
		return model.Household{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.Household{}, fmt.Errorf("提交事务失败: %w", err)
	}
	return s.GetHousehold(ctx, userID, hid.UUID.String())
}

func recordHouseholdInvitationFailure(ctx context.Context, tx *sql.Tx, codeHash string, now time.Time, ttl time.Duration) error {
	var attempts int
	err := tx.QueryRowContext(ctx, `
		INSERT INTO household_invitations (code_hash, household_id, invited_by_user_id, attempts, expires_at)
		VALUES ($1, NULL, NULL, 1, $2)
		ON CONFLICT (code_hash) DO UPDATE
		SET attempts = LEAST(household_invitations.attempts + 1, $3),
		    expires_at = GREATEST(household_invitations.expires_at, EXCLUDED.expires_at)
		RETURNING attempts`, codeHash, now.Add(ttl).UTC(), maxHouseholdInvitationAttempts).Scan(&attempts)
	if err != nil {
		return fmt.Errorf("记录邀请码失败尝试失败: %w", err)
	}
	if attempts >= maxHouseholdInvitationAttempts {
		if _, err := tx.ExecContext(ctx, `DELETE FROM household_invitations WHERE code_hash = $1`, codeHash); err != nil {
			return fmt.Errorf("作废邀请码失败: %w", err)
		}
	}
	return nil
}

func incrementHouseholdInvitationFailure(ctx context.Context, tx *sql.Tx, codeHash string, attempts int, now time.Time, ttl time.Duration) error {
	if attempts >= maxHouseholdInvitationAttempts {
		_, err := tx.ExecContext(ctx, `DELETE FROM household_invitations WHERE code_hash = $1`, codeHash)
		return err
	}
	_, err := tx.ExecContext(ctx, `
		UPDATE household_invitations
		SET attempts = LEAST(attempts + 1, $2)
		WHERE code_hash = $1`, codeHash, maxHouseholdInvitationAttempts)
	if err != nil {
		return fmt.Errorf("更新邀请码失败次数失败: %w", err)
	}
	return nil
}

// ListHouseholdRooms 实现 store.Store。
func (s *Store) ListHouseholdRooms(ctx context.Context, userID string, householdID string) ([]model.HouseholdRoom, error) {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return nil, fmt.Errorf("非法 user id: %w", err)
	}
	hid, err := uuid.Parse(householdID)
	if err != nil {
		return nil, store.ErrNotFound
	}
	ctx, cancel := queryTimeout(ctx)
	defer cancel()
	if _, err := householdRole(ctx, s.db, uid, hid); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, household_id, name, sort_order, created_at
		FROM household_rooms WHERE household_id = $1
		ORDER BY sort_order ASC, created_at ASC, id`, hid)
	if err != nil {
		return nil, fmt.Errorf("查询房间失败: %w", err)
	}
	defer rows.Close()
	out := []model.HouseholdRoom{}
	for rows.Next() {
		r, err := scanRoom(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历房间失败: %w", err)
	}
	return out, nil
}

type roomScanner interface{ Scan(dest ...any) error }

func scanRoom(row roomScanner) (model.HouseholdRoom, error) {
	var (
		r           model.HouseholdRoom
		id, hidUUID uuid.UUID
	)
	if err := row.Scan(&id, &hidUUID, &r.Name, &r.SortOrder, &r.CreatedAt); err != nil {
		return model.HouseholdRoom{}, fmt.Errorf("解析房间失败: %w", err)
	}
	r.ID = id.String()
	r.HouseholdID = hidUUID.String()
	r.CreatedAt = r.CreatedAt.UTC()
	return r, nil
}

// CreateHouseholdRoom 实现 store.Store。
func (s *Store) CreateHouseholdRoom(ctx context.Context, userID string, householdID string, name string, sortOrder int) (model.HouseholdRoom, error) {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return model.HouseholdRoom{}, fmt.Errorf("非法 user id: %w", err)
	}
	hid, err := uuid.Parse(householdID)
	if err != nil {
		return model.HouseholdRoom{}, store.ErrNotFound
	}
	ctx, cancel := context.WithTimeout(ctx, householdTimeout)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.HouseholdRoom{}, fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()
	if err := requireHouseholdOwner(ctx, tx, uid, hid); err != nil {
		return model.HouseholdRoom{}, err
	}
	rid := uuid.New()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO household_rooms (id, household_id, name, sort_order) VALUES ($1, $2, $3, $4)`,
		rid, hid, name, sortOrder); err != nil {
		return model.HouseholdRoom{}, fmt.Errorf("创建房间失败: %w", err)
	}
	var r model.HouseholdRoom
	if err := tx.QueryRowContext(ctx, `
		SELECT id, household_id, name, sort_order, created_at FROM household_rooms WHERE id = $1`, rid).
		Scan(&rid, &hid, &r.Name, &r.SortOrder, &r.CreatedAt); err != nil {
		return model.HouseholdRoom{}, fmt.Errorf("查询房间失败: %w", err)
	}
	r.ID = rid.String()
	r.HouseholdID = hid.String()
	r.CreatedAt = r.CreatedAt.UTC()
	if err := emitRoomChange(ctx, tx, hid, "upsert", false, r); err != nil {
		return model.HouseholdRoom{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.HouseholdRoom{}, fmt.Errorf("提交事务失败: %w", err)
	}
	return r, nil
}

// UpdateHouseholdRoom 实现 store.Store。
func (s *Store) UpdateHouseholdRoom(ctx context.Context, userID string, householdID string, roomID string, name *string, sortOrder *int) (model.HouseholdRoom, error) {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return model.HouseholdRoom{}, fmt.Errorf("非法 user id: %w", err)
	}
	hid, err := uuid.Parse(householdID)
	if err != nil {
		return model.HouseholdRoom{}, store.ErrNotFound
	}
	rid, err := uuid.Parse(roomID)
	if err != nil {
		return model.HouseholdRoom{}, store.ErrNotFound
	}
	ctx, cancel := context.WithTimeout(ctx, householdTimeout)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.HouseholdRoom{}, fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()
	if err := requireHouseholdOwner(ctx, tx, uid, hid); err != nil {
		return model.HouseholdRoom{}, err
	}
	if name != nil {
		if _, err := tx.ExecContext(ctx, `UPDATE household_rooms SET name = $3 WHERE id = $1 AND household_id = $2`, rid, hid, *name); err != nil {
			return model.HouseholdRoom{}, fmt.Errorf("更新房间失败: %w", err)
		}
	}
	if sortOrder != nil {
		if _, err := tx.ExecContext(ctx, `UPDATE household_rooms SET sort_order = $3 WHERE id = $1 AND household_id = $2`, rid, hid, *sortOrder); err != nil {
			return model.HouseholdRoom{}, fmt.Errorf("更新房间排序失败: %w", err)
		}
	}
	var r model.HouseholdRoom
	err = tx.QueryRowContext(ctx, `
		SELECT id, household_id, name, sort_order, created_at FROM household_rooms WHERE id = $1 AND household_id = $2`, rid, hid).
		Scan(&rid, &hid, &r.Name, &r.SortOrder, &r.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return model.HouseholdRoom{}, store.ErrNotFound
	}
	if err != nil {
		return model.HouseholdRoom{}, fmt.Errorf("查询房间失败: %w", err)
	}
	r.ID = rid.String()
	r.HouseholdID = hid.String()
	r.CreatedAt = r.CreatedAt.UTC()
	if err := emitRoomChange(ctx, tx, hid, "upsert", false, r); err != nil {
		return model.HouseholdRoom{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.HouseholdRoom{}, fmt.Errorf("提交事务失败: %w", err)
	}
	return r, nil
}

// ReorderHouseholdRooms 实现 store.Store：整段顺序提交，事务内按数组顺序重写 0..n-1。
// 行锁串行化并发重排，最后一个提交者获胜，不会出现交错的中间排序。
func (s *Store) ReorderHouseholdRooms(ctx context.Context, userID string, householdID string, roomIDs []string) ([]model.HouseholdRoom, error) {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return nil, fmt.Errorf("非法 user id: %w", err)
	}
	hid, err := uuid.Parse(householdID)
	if err != nil {
		return nil, store.ErrNotFound
	}
	ctx, cancel := context.WithTimeout(ctx, householdTimeout)
	defer cancel()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()
	if err := requireHouseholdOwner(ctx, tx, uid, hid); err != nil {
		return nil, err
	}

	rows, err := tx.QueryContext(ctx, `
		SELECT id FROM household_rooms WHERE household_id = $1 FOR UPDATE`, hid)
	if err != nil {
		return nil, fmt.Errorf("锁定房间失败: %w", err)
	}
	current := map[uuid.UUID]bool{}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("解析房间 id 失败: %w", err)
		}
		current[id] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历房间失败: %w", err)
	}
	if len(roomIDs) != len(current) {
		return nil, store.ErrRoomOrderMismatch
	}
	ordered := make([]uuid.UUID, 0, len(roomIDs))
	seen := map[uuid.UUID]bool{}
	for _, raw := range roomIDs {
		id, err := uuid.Parse(raw)
		if err != nil || !current[id] || seen[id] {
			return nil, store.ErrRoomOrderMismatch
		}
		seen[id] = true
		ordered = append(ordered, id)
	}
	for i, id := range ordered {
		if _, err := tx.ExecContext(ctx, `
			UPDATE household_rooms SET sort_order = $1 WHERE id = $2 AND household_id = $3`, i, id, hid); err != nil {
			return nil, fmt.Errorf("更新房间排序失败: %w", err)
		}
	}
	roomRows, err := tx.QueryContext(ctx, `
		SELECT id, household_id, name, sort_order, created_at
		FROM household_rooms WHERE household_id = $1
		ORDER BY sort_order ASC, created_at ASC, id`, hid)
	if err != nil {
		return nil, fmt.Errorf("查询房间失败: %w", err)
	}
	out := []model.HouseholdRoom{}
	for roomRows.Next() {
		r, err := scanRoom(roomRows)
		if err != nil {
			roomRows.Close()
			return nil, err
		}
		out = append(out, r)
	}
	roomRows.Close()
	if err := roomRows.Err(); err != nil {
		return nil, fmt.Errorf("遍历房间失败: %w", err)
	}
	// 每间房各发一条 upsert，客户端按 sortOrder 重新排列即可。
	for _, r := range out {
		if err := emitRoomChange(ctx, tx, hid, "upsert", false, r); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("提交事务失败: %w", err)
	}
	return out, nil
}

// DeleteHouseholdRoom 实现 store.Store。room_id 由外键 ON DELETE SET NULL 自动清空。
func (s *Store) DeleteHouseholdRoom(ctx context.Context, userID string, householdID string, roomID string) error {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return fmt.Errorf("非法 user id: %w", err)
	}
	hid, err := uuid.Parse(householdID)
	if err != nil {
		return store.ErrNotFound
	}
	rid, err := uuid.Parse(roomID)
	if err != nil {
		return store.ErrNotFound
	}
	ctx, cancel := context.WithTimeout(ctx, householdTimeout)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()
	if err := requireHouseholdOwner(ctx, tx, uid, hid); err != nil {
		return err
	}
	// 删除前先取快照，因为删除后行已不存在，payload 需要带着 name/sortOrder。
	var r model.HouseholdRoom
	err = tx.QueryRowContext(ctx, `
		SELECT id, household_id, name, sort_order, created_at
		FROM household_rooms WHERE id = $1 AND household_id = $2 FOR UPDATE`, rid, hid).
		Scan(&rid, &hid, &r.Name, &r.SortOrder, &r.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return store.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("查询房间失败: %w", err)
	}
	r.ID = rid.String()
	r.HouseholdID = hid.String()
	r.CreatedAt = r.CreatedAt.UTC()
	if _, err := tx.ExecContext(ctx, `DELETE FROM household_rooms WHERE id = $1 AND household_id = $2`, rid, hid); err != nil {
		return fmt.Errorf("删除房间失败: %w", err)
	}
	if err := emitRoomChange(ctx, tx, hid, "delete", true, r); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交事务失败: %w", err)
	}
	return nil
}

// UpdateUserDeviceMeta 实现 store.Store。
func (s *Store) UpdateUserDeviceMeta(ctx context.Context, userID string, platform string, externalID string, alias *string, roomID *string, householdID *string) (model.UserDevice, error) {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return model.UserDevice{}, fmt.Errorf("非法 user id: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, householdTimeout)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.UserDevice{}, fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	q := s.q.WithTx(tx)
	if err := q.LockIdempotencyKey(ctx, "push:"+userID); err != nil {
		return model.UserDevice{}, fmt.Errorf("锁定用户写入失败: %w", err)
	}

	var (
		devID        uuid.UUID
		curAlias     string
		curHousehold uuid.NullUUID
		curRoom      uuid.NullUUID
		clientID     string
		devType      sql.NullString
		advertised   string
		group        string
		rules        []byte
		scanRecords  []byte
		updatedAt    time.Time
		deletedAt    sql.NullTime
		curMarkedAt  sql.NullTime
		curLastSeen  sql.NullTime
	)
	err = tx.QueryRowContext(ctx, `
		SELECT id, alias, household_id, room_id, client_id, device_type, advertised_name, "group",
		       rules, scan_records, updated_at, deleted_at, marked_at, last_seen_at
		FROM user_devices
		WHERE user_id = $1 AND platform = $2 AND external_id = $3 AND deleted_at IS NULL
		FOR UPDATE`, uid, platform, externalID).
		Scan(&devID, &curAlias, &curHousehold, &curRoom, &clientID, &devType, &advertised, &group,
			&rules, &scanRecords, &updatedAt, &deletedAt, &curMarkedAt, &curLastSeen)
	if errors.Is(err, sql.ErrNoRows) {
		return model.UserDevice{}, store.ErrNotFound
	}
	if err != nil {
		return model.UserDevice{}, fmt.Errorf("查询设备失败: %w", err)
	}

	personalID, err := personalHouseholdID(ctx, tx, uid)
	if err != nil {
		return model.UserDevice{}, err
	}
	targetHousehold := curHousehold
	if householdID != nil {
		if *householdID == "" {
			targetHousehold = uuid.NullUUID{UUID: personalID, Valid: true}
		} else {
			hid, parseErr := uuid.Parse(*householdID)
			if parseErr != nil {
				return model.UserDevice{}, store.ErrNotFound
			}
			if _, roleErr := householdRole(ctx, tx, uid, hid); roleErr != nil {
				return model.UserDevice{}, roleErr
			}
			targetHousehold = uuid.NullUUID{UUID: hid, Valid: true}
		}
	}

	targetRoom := curRoom
	if roomID != nil {
		if *roomID == "" {
			targetRoom = uuid.NullUUID{}
		} else {
			rid, parseErr := uuid.Parse(*roomID)
			if parseErr != nil {
				return model.UserDevice{}, store.ErrNotFound
			}
			// 房间必须属于目标家庭。
			var roomHousehold uuid.UUID
			if err := tx.QueryRowContext(ctx, `SELECT household_id FROM household_rooms WHERE id = $1`, rid).Scan(&roomHousehold); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return model.UserDevice{}, store.ErrNotFound
				}
				return model.UserDevice{}, fmt.Errorf("查询房间失败: %w", err)
			}
			if !targetHousehold.Valid || roomHousehold != targetHousehold.UUID {
				return model.UserDevice{}, store.ErrForbidden
			}
			targetRoom = uuid.NullUUID{UUID: rid, Valid: true}
		}
	} else if householdID != nil && targetRoom.Valid {
		// 家庭变了但未指定房间：原房间不属于新家庭时清空。
		var roomHousehold uuid.UUID
		if err := tx.QueryRowContext(ctx, `SELECT household_id FROM household_rooms WHERE id = $1`, targetRoom.UUID).Scan(&roomHousehold); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				targetRoom = uuid.NullUUID{}
			} else {
				return model.UserDevice{}, fmt.Errorf("查询房间失败: %w", err)
			}
		} else if !targetHousehold.Valid || roomHousehold != targetHousehold.UUID {
			targetRoom = uuid.NullUUID{}
		}
	}

	newAlias := curAlias
	if alias != nil {
		newAlias = *alias
	}
	now := time.Now().UTC()
	seq, err := allocOneChangeSeq(ctx, tx)
	if err != nil {
		return model.UserDevice{}, err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE user_devices
		SET alias = $2, household_id = $3, room_id = $4, updated_at = $5, server_seq = $6
		WHERE id = $1`, devID, newAlias, nullUUID(targetHousehold), nullUUID(targetRoom), now, seq); err != nil {
		return model.UserDevice{}, fmt.Errorf("更新设备失败: %w", err)
	}

	var hhStr, roomStr *string
	if targetHousehold.Valid {
		v := targetHousehold.UUID.String()
		hhStr = &v
	}
	if targetRoom.Valid {
		v := targetRoom.UUID.String()
		roomStr = &v
	}
	d := model.UserDevice{
		ID: devID.String(), ClientID: clientID, Platform: platform, ExternalID: externalID,
		Alias: newAlias, AdvertisedName: advertised, Group: group,
		Rules: rules, ScanRecords: scanRecords, UpdatedAt: now, ServerSeq: seq,
		HouseholdID: hhStr, RoomID: roomStr,
	}
	if curMarkedAt.Valid {
		v := curMarkedAt.Time.UTC()
		d.MarkedAt = &v
	}
	if curLastSeen.Valid {
		v := curLastSeen.Time.UTC()
		d.LastSeenAt = &v
	}
	d.Presence = model.DerivePresence(now, d.LastSeenAt)
	if devType.Valid && devType.String != "" {
		v := devType.String
		d.DeviceType = &v
	}
	d = s.enrichUserDevice(ctx, q, d)

	payload, err := json.Marshal(d)
	if err != nil {
		return model.UserDevice{}, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO change_log (seq, user_id, household_id, entity, entity_id, op, payload, server_ts)
		VALUES ($1, $2, $3, 'user_device', $4, 'upsert', $5, now())`,
		seq, uid, nullUUID(targetHousehold), userID+":"+platform+":"+externalID, payload); err != nil {
		return model.UserDevice{}, fmt.Errorf("写入变更日志失败: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return model.UserDevice{}, fmt.Errorf("提交事务失败: %w", err)
	}
	return d, nil
}

func personalHouseholdID(ctx context.Context, db rowQueryer, uid uuid.UUID) (uuid.UUID, error) {
	var id uuid.UUID
	err := db.QueryRowContext(ctx,
		`SELECT id FROM households WHERE owner_user_id = $1 AND is_personal = true`, uid).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return uuid.Nil, fmt.Errorf("用户缺少个人家庭: %w", store.ErrNotFound)
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("查询个人家庭失败: %w", err)
	}
	return id, nil
}

func allocOneChangeSeq(ctx context.Context, db rowQueryer) (int64, error) {
	var seq int64
	if err := db.QueryRowContext(ctx,
		`SELECT nextval(pg_get_serial_sequence('change_log', 'seq'))::bigint`).Scan(&seq); err != nil {
		return 0, fmt.Errorf("分配变更序列失败: %w", err)
	}
	return seq, nil
}

func nullUUID(v uuid.NullUUID) any {
	if !v.Valid {
		return nil
	}
	return v.UUID
}

// householdRecipient 是一个结构变更接收者及其专属 payload（role 随接收者不同）。
type householdRecipient struct {
	UserID  uuid.UUID
	Payload model.HouseholdChangePayload
}

// householdSnapshot 返回家庭当前成员的接收者列表与各自视角的 household payload。
// 调用时机决定语义：在成员变更之后调用，payload 的 memberCount/role 即为变更后的快照。
func householdSnapshot(ctx context.Context, db rowQueryer, hid uuid.UUID) ([]householdRecipient, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT m.user_id, h.name, h.owner_user_id, m.role,
		       (SELECT count(*) FROM household_members x WHERE x.household_id = h.id) AS member_count
		FROM household_members m
		JOIN households h ON h.id = m.household_id
		WHERE h.id = $1
		ORDER BY m.joined_at, m.user_id`, hid)
	if err != nil {
		return nil, fmt.Errorf("查询家庭快照失败: %w", err)
	}
	defer rows.Close()
	out := []householdRecipient{}
	for rows.Next() {
		var (
			uid, owner uuid.UUID
			p          model.HouseholdChangePayload
		)
		if err := rows.Scan(&uid, &p.Name, &owner, &p.Role, &p.MemberCount); err != nil {
			return nil, fmt.Errorf("解析家庭快照失败: %w", err)
		}
		p.ID = hid.String()
		p.OwnerUserID = owner.String()
		out = append(out, householdRecipient{UserID: uid, Payload: p})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历家庭快照失败: %w", err)
	}
	return out, nil
}

// householdMemberIDs 返回家庭当前成员 id，用于扇出房间类结构变更。
func householdMemberIDs(ctx context.Context, db rowQueryer, hid uuid.UUID) ([]uuid.UUID, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT user_id FROM household_members WHERE household_id = $1 ORDER BY joined_at, user_id`, hid)
	if err != nil {
		return nil, fmt.Errorf("查询家庭成员失败: %w", err)
	}
	defer rows.Close()
	out := []uuid.UUID{}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("解析家庭成员失败: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历家庭成员失败: %w", err)
	}
	return out, nil
}

// insertStructuralChanges 为每个接收者各写一条 change_log。
//
// 扇出设计（v0.15.0）：结构变更按「写入时刻的成员集合」逐人写一行，
// user_id = 接收者、household_id 留空。因此
//   - 被移除的成员不会再收到离开之后的结构变更（那些行里没有他的 user_id）；
//   - 新成员加入后不会回放历史结构（历史的行没有他的 user_id），首次基线走 GET /households；
//   - 已写入的行不会被成员变动影响（不会因退出而突然变成「未清理的缺口」触发 410）。
func insertStructuralChanges(ctx context.Context, tx *sql.Tx, recipients []uuid.UUID, entity, entityID, op string, payload any) error {
	if len(recipients) == 0 {
		return nil
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("序列化结构变更失败: %w", err)
	}
	for _, uid := range recipients {
		seq, err := allocOneChangeSeq(ctx, tx)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO change_log (seq, user_id, household_id, entity, entity_id, op, payload, server_ts)
			VALUES ($1, $2, NULL, $3, $4, $5, $6, now())`,
			seq, uid, entity, entityID, op, raw); err != nil {
			return fmt.Errorf("写入结构变更失败: %w", err)
		}
	}
	return nil
}

// emitHouseholdChange 在事务内按当前成员集合扇出 household 结构变更。
// deleted=true 时 op 为 delete，payload 里的 Deleted 置真；调用方需在此之前取快照。
func emitHouseholdChange(ctx context.Context, tx *sql.Tx, snapshot []householdRecipient, op string, deleted bool) error {
	if len(snapshot) == 0 {
		return nil
	}
	recipients := make([]uuid.UUID, 0, len(snapshot))
	payloads := make([]model.HouseholdChangePayload, 0, len(snapshot))
	for _, r := range snapshot {
		p := r.Payload
		p.Deleted = deleted
		recipients = append(recipients, r.UserID)
		payloads = append(payloads, p)
	}
	for i, uid := range recipients {
		if err := insertStructuralChanges(ctx, tx, []uuid.UUID{uid}, "household", payloads[i].ID, op, payloads[i]); err != nil {
			return err
		}
	}
	return nil
}

// emitRoomChange 在事务内按当前成员集合扇出 room 结构变更。
func emitRoomChange(ctx context.Context, tx *sql.Tx, hid uuid.UUID, op string, deleted bool, room model.HouseholdRoom) error {
	recipients, err := householdMemberIDs(ctx, tx, hid)
	if err != nil {
		return err
	}
	payload := model.RoomChangePayload{
		ID: room.ID, HouseholdID: room.HouseholdID, Name: room.Name,
		SortOrder: room.SortOrder, Deleted: deleted,
	}
	return insertStructuralChanges(ctx, tx, recipients, "room", room.ID, op, payload)
}
