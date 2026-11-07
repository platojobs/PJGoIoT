package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/platojobs/PJGoIoT/internal/model"
	"github.com/platojobs/PJGoIoT/internal/store/postgres/sqlcgen"
)

// presenceKey 是读数上报里的设备唯一键。
type presenceKey struct {
	platform   string
	externalID string
}

// presenceRow 是 presence 更新所需的设备行快照。
type presenceRow struct {
	id          uuid.UUID
	clientID    string
	platform    string
	externalID  string
	mac         string
	alias       string
	advertised  string
	group       string
	rules       []byte
	scanRecords []byte
	updatedAt   time.Time
	serverSeq   int64
	deviceType  sql.NullString
	householdID uuid.NullUUID
	roomID      uuid.NullUUID
	markedAt    sql.NullTime
	lastSeenAt  sql.NullTime
}

// touchDevicePresenceTx 用本次 push 的读数推进设备 last_seen_at。
//
// 契约（0.21.0）：
//   - 只有「读数被服务端接受」才算活动，使用服务端接收时间 now，而不是
//     客户端上报的 recordedAt（补录旧读数不应让设备重新上线）；
//   - 只更新推送者本人名下的设备行（user_id = 当前账号），共享家庭里
//     其他成员的设备记录不被跨账号改写，保证跨用户隔离；
//   - 每次推进都写一条 entity=user_device 的 change_log，payload 是完整
//     设备快照，因此 /sync/changes 能增量拿到最新的 lastSeenAt/presence；
//   - presence 不落库，由读接口按 15 分钟窗口实时推导。
func (s *Store) touchDevicePresenceTx(ctx context.Context, tx *sql.Tx, uid uuid.UUID,
	readings []model.PushReading, now time.Time, q *sqlcgen.Queries) error {
	keys := distinctPresenceKeys(readings)
	if len(keys) == 0 {
		return nil
	}
	rows, err := loadPresenceRows(ctx, tx, uid, keys)
	if err != nil {
		return err
	}
	seenAt := now.UTC().Truncate(time.Microsecond)

	// 只推进严格更晚的时间，避免同微秒重复写入产生无意义条目。
	touched := make([]presenceRow, 0, len(rows))
	for _, row := range rows {
		if row.lastSeenAt.Valid &&
			!seenAt.After(row.lastSeenAt.Time.UTC().Truncate(time.Microsecond)) {
			continue
		}
		touched = append(touched, row)
	}
	if len(touched) == 0 {
		return nil
	}

	seqs, err := allocateChangeSeqs(ctx, tx, len(touched))
	if err != nil {
		return err
	}
	if err := updateDevicePresenceBatch(ctx, tx, uid, touched, seqs, seenAt); err != nil {
		return err
	}

	logRows := make([][]any, 0, len(touched))
	for i := range touched {
		row := &touched[i]
		d := presenceSnapshot(*row, seqs[i], seenAt)
		d = s.enrichUserDevice(ctx, q, d)
		payload, err := json.Marshal(d)
		if err != nil {
			return fmt.Errorf("序列化在线状态变更失败: %w", err)
		}
		logRows = append(logRows, []any{
			seqs[i], uid, nullUUID(row.householdID), "user_device",
			uid.String() + ":" + row.platform + ":" + row.externalID, "upsert", payload, now.UTC(),
		})
	}
	if _, err := insertChangeLogBatch(ctx, tx, logRows); err != nil {
		return fmt.Errorf("写入在线状态变更失败: %w", err)
	}
	return nil
}

// distinctPresenceKeys 按出现顺序去重读数设备键。
func distinctPresenceKeys(readings []model.PushReading) []presenceKey {
	seen := map[string]struct{}{}
	out := make([]presenceKey, 0, len(readings))
	for _, r := range readings {
		k := r.Platform + "|" + r.ExternalID
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, presenceKey{platform: r.Platform, externalID: r.ExternalID})
	}
	return out
}

// loadPresenceRows 一次取出本批读数命中的本人设备行（FOR UPDATE 与 push 串行锁配合）。
func loadPresenceRows(ctx context.Context, tx *sql.Tx, uid uuid.UUID, keys []presenceKey) ([]presenceRow, error) {
	args := []any{uid}
	exprs := make([]string, 0, len(keys))
	for i, k := range keys {
		exprs = append(exprs, fmt.Sprintf("($%d::text, $%d::text)", 2+i*2, 3+i*2))
		args = append(args, k.platform, k.externalID)
	}
	query := `SELECT id, client_id, platform, external_id, COALESCE(mac, ''), alias, advertised_name,
		"group", rules, scan_records, updated_at, server_seq, device_type, household_id, room_id,
		marked_at, last_seen_at
		FROM user_devices
		WHERE user_id = $1 AND deleted_at IS NULL
		  AND (platform, external_id) IN (` + strings.Join(exprs, ", ") + `)
		FOR UPDATE`
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("查询在线状态设备失败: %w", err)
	}
	defer rows.Close()

	out := make([]presenceRow, 0, len(keys))
	for rows.Next() {
		var row presenceRow
		if err := rows.Scan(&row.id, &row.clientID, &row.platform, &row.externalID, &row.mac,
			&row.alias, &row.advertised, &row.group, &row.rules, &row.scanRecords, &row.updatedAt,
			&row.serverSeq, &row.deviceType, &row.householdID, &row.roomID, &row.markedAt,
			&row.lastSeenAt); err != nil {
			return nil, fmt.Errorf("解析在线状态设备失败: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历在线状态设备失败: %w", err)
	}
	return out, nil
}

// updateDevicePresenceBatch 一条 UPDATE 推进全部命中的设备。
func updateDevicePresenceBatch(ctx context.Context, tx *sql.Tx, uid uuid.UUID,
	rows []presenceRow, seqs []int64, seenAt time.Time) error {
	values := make([][]any, 0, len(rows))
	for i, row := range rows {
		values = append(values, []any{row.id, seenAt, seqs[i]})
	}
	placeholders, args := buildBatchValues(2, values, []string{"uuid", "timestamptz", "bigint"})
	args = append([]any{uid}, args...)
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`
		UPDATE user_devices AS d
		SET last_seen_at = v.seen_at, server_seq = v.seq
		FROM (VALUES %s) AS v(id, seen_at, seq)
		WHERE d.user_id = $1 AND d.id = v.id`, placeholders), args...); err != nil {
		return fmt.Errorf("更新设备在线状态失败: %w", err)
	}
	return nil
}

// presenceSnapshot 把设备行还原成 change_log 的 UserDevice payload。
func presenceSnapshot(row presenceRow, seq int64, seenAt time.Time) model.UserDevice {
	d := model.UserDevice{
		ID:             row.id.String(),
		ClientID:       row.clientID,
		Platform:       row.platform,
		ExternalID:     row.externalID,
		MAC:            row.mac,
		Alias:          row.alias,
		AdvertisedName: row.advertised,
		Group:          row.group,
		Rules:          row.rules,
		ScanRecords:    row.scanRecords,
		UpdatedAt:      row.updatedAt.UTC(),
		ServerSeq:      seq,
		Presence:       model.PresenceOnline,
	}
	seen := seenAt.UTC().Truncate(time.Microsecond)
	d.LastSeenAt = &seen
	if row.deviceType.Valid && strings.TrimSpace(row.deviceType.String) != "" {
		v := strings.TrimSpace(row.deviceType.String)
		d.DeviceType = &v
	}
	if row.householdID.Valid {
		v := row.householdID.UUID.String()
		d.HouseholdID = &v
	}
	if row.roomID.Valid {
		v := row.roomID.UUID.String()
		d.RoomID = &v
	}
	if row.markedAt.Valid {
		v := row.markedAt.Time.UTC()
		d.MarkedAt = &v
	}
	return d
}
