package postgres

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/platojobs/PJGoIoT/internal/auth"
	"github.com/platojobs/PJGoIoT/internal/model"
	"github.com/platojobs/PJGoIoT/internal/store"
	"github.com/platojobs/PJGoIoT/internal/store/postgres/sqlcgen"
)

// GetUser 实现 store.Store。
func (s *Store) GetUser(ctx context.Context, userID string) (model.User, error) {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return model.User{}, fmt.Errorf("非法 user id: %w", err)
	}
	var user model.User
	var id uuid.UUID
	if err := s.db.QueryRowContext(ctx, `SELECT id, kind, created_at FROM users WHERE id = $1`, uid).
		Scan(&id, &user.Kind, &user.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.User{}, store.ErrUnauthorized
		}
		return model.User{}, fmt.Errorf("查询用户失败: %w", err)
	}
	user.ID = id.String()
	user.CreatedAt = user.CreatedAt.UTC()
	return user, nil
}

// DeleteUser 实现 store.Store。
// users 上的外键均为 ON DELETE CASCADE，删除用户行即级联清理 tokens、user_devices、
// change_log、idempotency_keys、readings 与 pairing_codes。
func (s *Store) DeleteUser(ctx context.Context, userID string) error {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return fmt.Errorf("非法 user id: %w", err)
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM users WHERE id = $1`, uid)
	if err != nil {
		return fmt.Errorf("删除用户失败: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("读取删除结果失败: %w", err)
	}
	if affected == 0 {
		// 已删除的用户再次注销仍应得到 401（token 已随用户删除而失效）。
		return store.ErrUnauthorized
	}
	return nil
}

// ListUserDevicesForExport 实现 store.Store。
// 导出包含已删除（tombstone）设备，保证用户能拿到自己全部写入过的记录。
func (s *Store) ListUserDevicesForExport(ctx context.Context, userID string) ([]model.UserDevice, error) {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return nil, fmt.Errorf("非法 user id: %w", err)
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, client_id, platform, external_id, mac, alias, advertised_name, "group",
		       rules, scan_records, updated_at, deleted_at, server_seq, device_type
		FROM user_devices
		WHERE user_id = $1
		ORDER BY updated_at DESC, id ASC`, uid)
	if err != nil {
		return nil, fmt.Errorf("导出用户设备失败: %w", err)
	}
	defer rows.Close()

	out := make([]model.UserDevice, 0)
	for rows.Next() {
		var row sqlcgen.UserDevice
		if err := rows.Scan(&row.ID, &row.ClientID, &row.Platform, &row.ExternalID, &row.Mac,
			&row.Alias, &row.AdvertisedName, &row.Group, &row.Rules, &row.ScanRecords,
			&row.UpdatedAt, &row.DeletedAt, &row.ServerSeq, &row.DeviceType); err != nil {
			return nil, fmt.Errorf("解析导出设备失败: %w", err)
		}
		out = append(out, s.enrichUserDevice(ctx, s.q, mapUserDevice(row)))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历导出设备失败: %w", err)
	}
	return out, nil
}

// ExportReadings 实现 store.Store。
func (s *Store) ExportReadings(ctx context.Context, userID string, cursor string, limit int) ([]model.ExportReading, string, bool, error) {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return nil, "", false, fmt.Errorf("非法 user id: %w", err)
	}
	if limit <= 0 {
		limit = 5000
	}
	var afterAt sql.NullTime
	afterID := uuid.Nil
	if cursor != "" {
		at, id, err := decodeExportCursor(cursor)
		if err != nil {
			return nil, "", false, err
		}
		afterAt = sql.NullTime{Time: at, Valid: true}
		afterID = id
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, platform, external_id, metric, value, unit, recorded_at, device_type
		FROM readings
		WHERE user_id = $1
		  AND ($2::timestamptz IS NULL OR recorded_at > $2 OR (recorded_at = $2 AND id > $3))
		ORDER BY recorded_at ASC, id ASC
		LIMIT $4`, uid, afterAt, afterID, limit+1)
	if err != nil {
		return nil, "", false, fmt.Errorf("导出读数失败: %w", err)
	}
	defer rows.Close()

	items := make([]model.ExportReading, 0, limit)
	for rows.Next() {
		var item model.ExportReading
		var id uuid.UUID
		var deviceType sql.NullString
		if err := rows.Scan(&id, &item.Platform, &item.ExternalID, &item.Metric, &item.Value,
			&item.Unit, &item.RecordedAt, &deviceType); err != nil {
			return nil, "", false, fmt.Errorf("解析导出读数失败: %w", err)
		}
		item.ID = id.String()
		item.RecordedAt = item.RecordedAt.UTC()
		item.DeviceType = strings.TrimSpace(deviceType.String)
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, "", false, fmt.Errorf("遍历导出读数失败: %w", err)
	}

	if len(items) > limit {
		items = items[:limit]
		last := items[len(items)-1]
		return items, encodeExportCursor(last.RecordedAt, last.ID), true, nil
	}
	return items, "", false, nil
}

// CreatePairingCode 实现 store.Store：同一用户的新码使旧码失效。
func (s *Store) CreatePairingCode(ctx context.Context, userID string, codeHash string, expiresAt time.Time) error {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return fmt.Errorf("非法 user id: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `DELETE FROM pairing_codes WHERE user_id = $1`, uid); err != nil {
		return fmt.Errorf("回收旧配对码失败: %w", err)
	}
	// 顺带清理过期行（含失败尝试记录），避免表无限增长。
	if _, err := tx.ExecContext(ctx, `DELETE FROM pairing_codes WHERE expires_at < now()`); err != nil {
		return fmt.Errorf("清理过期配对码失败: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO pairing_codes (code_hash, user_id, attempts, expires_at)
		VALUES ($1, $2, 0, $3)`, codeHash, uid, expiresAt.UTC()); err != nil {
		return fmt.Errorf("写入配对码失败: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交事务失败: %w", err)
	}
	return nil
}

// ClaimPairingCode 实现 store.Store。
//
// 失败（不存在、过期、已用过、哈希不一致）统一返回 ErrNotFound，不区分原因。
// 每次失败都对提交的码计数；同一码累计 5 次即删除作废。
func (s *Store) ClaimPairingCode(ctx context.Context, codeHash string, tokenHash string, now time.Time, ttl time.Duration, tokenExpiresAt time.Time) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	var storedHash string
	var owner uuid.NullUUID
	var attempts int
	var expiresAt time.Time
	err = tx.QueryRowContext(ctx, `
		SELECT code_hash, user_id, attempts, expires_at
		FROM pairing_codes WHERE code_hash = $1 FOR UPDATE`, codeHash).
		Scan(&storedHash, &owner, &attempts, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		if err := recordPairingFailure(ctx, tx, codeHash, now, ttl); err != nil {
			return "", err
		}
		if err := tx.Commit(); err != nil {
			return "", fmt.Errorf("提交事务失败: %w", err)
		}
		return "", store.ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("查询配对码失败: %w", err)
	}

	// 失败尝试记录行（user_id IS NULL）或已满 5 次：继续计数并最终删除。
	if !owner.Valid || attempts >= maxPairingAttempts {
		if err := incrementPairingFailure(ctx, tx, codeHash, attempts, now, ttl, true); err != nil {
			return "", err
		}
		if err := tx.Commit(); err != nil {
			return "", fmt.Errorf("提交事务失败: %w", err)
		}
		return "", store.ErrNotFound
	}

	// 常量时间比较，避免通过耗时差异推断哈希前缀。
	if !auth.EqualPairingCodeHash(storedHash, codeHash) {
		if err := incrementPairingFailure(ctx, tx, codeHash, attempts, now, ttl, false); err != nil {
			return "", err
		}
		if err := tx.Commit(); err != nil {
			return "", fmt.Errorf("提交事务失败: %w", err)
		}
		return "", store.ErrNotFound
	}

	expired := !expiresAt.After(now)
	if expired {
		// 过期码只计数，不延长过期时间，避免失败尝试把过期码“救活”。
		if err := incrementPairingFailure(ctx, tx, codeHash, attempts, now, ttl, false); err != nil {
			return "", err
		}
		if err := tx.Commit(); err != nil {
			return "", fmt.Errorf("提交事务失败: %w", err)
		}
		return "", store.ErrNotFound
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO tokens (user_id, token_hash, expires_at) VALUES ($1, $2, $3)`, owner.UUID, tokenHash, tokenExpiresAt.UTC()); err != nil {
		return "", fmt.Errorf("签发 token 失败: %w", err)
	}
	// 一次性：用掉即删除，重放命中「不存在」分支并返回 404。
	if _, err := tx.ExecContext(ctx, `DELETE FROM pairing_codes WHERE code_hash = $1`, codeHash); err != nil {
		return "", fmt.Errorf("删除已用配对码失败: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("提交事务失败: %w", err)
	}
	return owner.UUID.String(), nil
}

// maxPairingAttempts 是同一配对码允许的最大失败次数。
const maxPairingAttempts = 5

// recordPairingFailure 为不存在的码写入失败计数行；并发时递增已有计数。
func recordPairingFailure(ctx context.Context, tx *sql.Tx, codeHash string, now time.Time, ttl time.Duration) error {
	var attempts int
	err := tx.QueryRowContext(ctx, `
		INSERT INTO pairing_codes (code_hash, user_id, attempts, expires_at)
		VALUES ($1, NULL, 1, $2)
		ON CONFLICT (code_hash) DO UPDATE
		SET attempts = LEAST(pairing_codes.attempts + 1, $3),
		    expires_at = GREATEST(pairing_codes.expires_at, EXCLUDED.expires_at)
		RETURNING attempts`, codeHash, now.Add(ttl).UTC(), maxPairingAttempts).Scan(&attempts)
	if err != nil {
		return fmt.Errorf("记录配对码失败尝试失败: %w", err)
	}
	if attempts >= maxPairingAttempts {
		if _, err := tx.ExecContext(ctx, `DELETE FROM pairing_codes WHERE code_hash = $1`, codeHash); err != nil {
			return fmt.Errorf("作废配对码失败: %w", err)
		}
	}
	return nil
}

// incrementPairingFailure 对已有的码（含失败记录行）递增失败次数，满 5 次删除。
// extendExpiry 仅用于失败记录行（user_id IS NULL），让其 TTL 覆盖计数窗口；
// 对真实码（尤其过期码）绝不延长，否则失败尝试会把过期码重新“救活”。
func incrementPairingFailure(ctx context.Context, tx *sql.Tx, codeHash string, current int, now time.Time, ttl time.Duration, extendExpiry bool) error {
	next := current + 1
	if next >= maxPairingAttempts {
		if _, err := tx.ExecContext(ctx, `DELETE FROM pairing_codes WHERE code_hash = $1`, codeHash); err != nil {
			return fmt.Errorf("作废配对码失败: %w", err)
		}
		return nil
	}
	if extendExpiry {
		if _, err := tx.ExecContext(ctx, `
			UPDATE pairing_codes SET attempts = $2, expires_at = GREATEST(expires_at, $3)
			WHERE code_hash = $1`, codeHash, next, now.Add(ttl).UTC()); err != nil {
			return fmt.Errorf("更新配对码失败次数失败: %w", err)
		}
		return nil
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE pairing_codes SET attempts = $2 WHERE code_hash = $1`, codeHash, next); err != nil {
		return fmt.Errorf("更新配对码失败次数失败: %w", err)
	}
	return nil
}

// CleanupPairingCodes 实现 store.Store。
func (s *Store) CleanupPairingCodes(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM pairing_codes WHERE expires_at < $1`, before.UTC())
	if err != nil {
		return 0, fmt.Errorf("清理配对码失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("读取清理结果失败: %w", err)
	}
	return n, nil
}

// CleanupChangeLog 实现 store.Store。
func (s *Store) CleanupChangeLog(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM change_log WHERE server_ts < $1`, before.UTC())
	if err != nil {
		return 0, fmt.Errorf("清理变更日志失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("读取清理结果失败: %w", err)
	}
	return n, nil
}

// CleanupIdempotencyKeys 实现 store.Store。
func (s *Store) CleanupIdempotencyKeys(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM idempotency_keys WHERE created_at < $1`, before.UTC())
	if err != nil {
		return 0, fmt.Errorf("清理幂等键失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("读取清理结果失败: %w", err)
	}
	return n, nil
}

// CleanupTokens 实现 store.Store。
func (s *Store) CleanupTokens(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM tokens WHERE expires_at < $1`, before.UTC())
	if err != nil {
		return 0, fmt.Errorf("清理过期 token 失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("读取清理结果失败: %w", err)
	}
	return n, nil
}

// ApproxCounts 实现 store.Store：读取 pg_class.reltuples，不对大表执行 COUNT。
func (s *Store) ApproxCounts(ctx context.Context) (model.StoreCounts, error) {
	var counts model.StoreCounts
	err := s.db.QueryRowContext(ctx, `
		SELECT
			GREATEST(COALESCE((SELECT reltuples::bigint FROM pg_class WHERE oid = 'users'::regclass), 0), 0),
			GREATEST(COALESCE((SELECT reltuples::bigint FROM pg_class WHERE oid = 'user_devices'::regclass), 0), 0),
			GREATEST(COALESCE((SELECT reltuples::bigint FROM pg_class WHERE oid = 'readings'::regclass), 0), 0)`).
		Scan(&counts.Users, &counts.Devices, &counts.Readings)
	if err != nil {
		return model.StoreCounts{}, fmt.Errorf("查询近似行数失败: %w", err)
	}
	return counts, nil
}

// encodeExportCursor 把 (recordedAt, id) 编码为升序导出的不透明游标。
func encodeExportCursor(at time.Time, id string) string {
	raw := at.UTC().Format(time.RFC3339Nano) + "|" + id
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeExportCursor(cursor string) (time.Time, uuid.UUID, error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return time.Time{}, uuid.Nil, store.ErrBadCursor
	}
	parts := strings.SplitN(string(raw), "|", 2)
	if len(parts) != 2 {
		return time.Time{}, uuid.Nil, store.ErrBadCursor
	}
	at, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return time.Time{}, uuid.Nil, store.ErrBadCursor
	}
	id, err := uuid.Parse(parts[1])
	if err != nil {
		return time.Time{}, uuid.Nil, store.ErrBadCursor
	}
	return at.UTC(), id, nil
}
