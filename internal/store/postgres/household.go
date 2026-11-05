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
       (SELECT count(*) FROM user_devices d WHERE d.household_id = h.id AND d.deleted_at IS NULL) AS device_count
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
	if err := row.Scan(&id, &h.Name, &owner, &h.Role, &h.Personal, &h.CreatedAt, &h.MemberCount, &h.DeviceCount); err != nil {
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
	if err := requireHouseholdOwner(ctx, s.db, uid, hid); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE households SET name = $2 WHERE id = $1`, hid, name)
	if err != nil {
		return fmt.Errorf("更新家庭失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
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
	if _, err := tx.ExecContext(ctx, `DELETE FROM households WHERE id = $1`, hid); err != nil {
		return fmt.Errorf("删除家庭失败: %w", err)
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
		SELECT m.user_id, m.role, u.kind, m.joined_at
		FROM household_members m
		JOIN users u ON u.id = m.user_id
		WHERE m.household_id = $1
		ORDER BY m.joined_at ASC, m.user_id`, hid)
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
		if err := rows.Scan(&id, &m.Role, &m.Kind, &m.JoinedAt); err != nil {
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
	if err := requireHouseholdOwner(ctx, s.db, uid, hid); err != nil {
		return model.HouseholdRoom{}, err
	}
	rid := uuid.New()
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO household_rooms (id, household_id, name, sort_order) VALUES ($1, $2, $3, $4)`,
		rid, hid, name, sortOrder); err != nil {
		return model.HouseholdRoom{}, fmt.Errorf("创建房间失败: %w", err)
	}
	var r model.HouseholdRoom
	if err := s.db.QueryRowContext(ctx, `
		SELECT id, household_id, name, sort_order, created_at FROM household_rooms WHERE id = $1`, rid).
		Scan(&rid, &hid, &r.Name, &r.SortOrder, &r.CreatedAt); err != nil {
		return model.HouseholdRoom{}, fmt.Errorf("查询房间失败: %w", err)
	}
	r.ID = rid.String()
	r.HouseholdID = hid.String()
	r.CreatedAt = r.CreatedAt.UTC()
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
	if err := requireHouseholdOwner(ctx, s.db, uid, hid); err != nil {
		return model.HouseholdRoom{}, err
	}
	if name != nil {
		if _, err := s.db.ExecContext(ctx, `UPDATE household_rooms SET name = $3 WHERE id = $1 AND household_id = $2`, rid, hid, *name); err != nil {
			return model.HouseholdRoom{}, fmt.Errorf("更新房间失败: %w", err)
		}
	}
	if sortOrder != nil {
		if _, err := s.db.ExecContext(ctx, `UPDATE household_rooms SET sort_order = $3 WHERE id = $1 AND household_id = $2`, rid, hid, *sortOrder); err != nil {
			return model.HouseholdRoom{}, fmt.Errorf("更新房间排序失败: %w", err)
		}
	}
	var r model.HouseholdRoom
	err = s.db.QueryRowContext(ctx, `
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
	return r, nil
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
	if err := requireHouseholdOwner(ctx, s.db, uid, hid); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM household_rooms WHERE id = $1 AND household_id = $2`, rid, hid)
	if err != nil {
		return fmt.Errorf("删除房间失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
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
	)
	err = tx.QueryRowContext(ctx, `
		SELECT id, alias, household_id, room_id, client_id, device_type, advertised_name, "group",
		       rules, scan_records, updated_at, deleted_at
		FROM user_devices
		WHERE user_id = $1 AND platform = $2 AND external_id = $3 AND deleted_at IS NULL
		FOR UPDATE`, uid, platform, externalID).
		Scan(&devID, &curAlias, &curHousehold, &curRoom, &clientID, &devType, &advertised, &group,
			&rules, &scanRecords, &updatedAt, &deletedAt)
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
