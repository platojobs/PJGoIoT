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

	"github.com/platojobs/PJGoIoT/internal/model"
	"github.com/platojobs/PJGoIoT/internal/store"
	"github.com/platojobs/PJGoIoT/internal/store/postgres/sqlcgen"
	"github.com/platojobs/PJGoIoT/internal/validate"
)

// readingRow 是 readings 表在存储层内部的表示。
type readingRow struct {
	ID         uuid.UUID
	Platform   string
	ExternalID string
	Metric     string
	Value      float64
	Unit       string
	RecordedAt time.Time
	DeviceType sql.NullString
}

// pushReadingsTx 在 push 事务内按 id 幂等写入读数。
// 已存在同 id 且内容一致：计 skipped；内容不同：计 skipped 并追加 reading 冲突。
func pushReadingsTx(ctx context.Context, tx *sql.Tx, uid uuid.UUID, readings []model.PushReading, now time.Time, result *model.PushResult, fallback *sqlcgen.Queries) error {
	for i := range readings {
		r := &readings[i]
		canonical, err := normalizePushReading(r, now)
		if err != nil {
			return fmt.Errorf("读数 %d 非法: %w", i, err)
		}

		deviceType := canonical.DeviceType
		if deviceType == "" {
			deviceType = lookupReadingDeviceType(ctx, tx, fallback, uid, canonical.Platform, canonical.ExternalID)
		}

		readingID, _ := uuid.Parse(canonical.ID)
		inserted, err := insertReading(ctx, tx, uid, readingID, canonical, deviceType)
		if err != nil {
			return err
		}
		if inserted {
			result.AppliedReadings++
			continue
		}

		// 主键冲突：读取已存在记录比较内容。
		existing, err := selectReading(ctx, tx, readingID, uid)
		if err != nil {
			return err
		}
		result.SkippedReadings++
		if existing == nil || !sameReadingContent(existing, canonical, deviceType) {
			result.Conflicts = append(result.Conflicts, model.Conflict{
				ID: canonical.ID, Platform: canonical.Platform, ExternalID: canonical.ExternalID,
				Entity: "reading", Reason: "duplicate_id",
				Reading: &model.Reading{
					ID: canonical.ID, Metric: canonical.Metric, Value: canonical.Value,
					Unit: canonical.Unit, RecordedAt: canonical.RecordedAt,
				},
			})
		}
	}
	return nil
}

// normalizePushReading 存储层再次规范化，避免绕过 HTTP 层写入非法读数。
func normalizePushReading(r *model.PushReading, now time.Time) (model.PushReading, error) {
	out := *r
	id, err := uuid.Parse(out.ID)
	if err != nil {
		return out, fmt.Errorf("id 必须是 UUID: %w", err)
	}
	out.ID = id.String()

	platform, externalID, _, err := validate.ResolveDeviceKey(out.Platform, out.ExternalID, "")
	if err != nil {
		return out, err
	}
	out.Platform, out.ExternalID = platform, externalID

	if out.Metric, err = validate.NormalizeMetric(out.Metric); err != nil {
		return out, err
	}
	if err := validate.CheckFiniteValue(out.Value); err != nil {
		return out, err
	}
	if out.Unit, err = validate.NormalizeUnit(out.Unit); err != nil {
		return out, err
	}
	if err := validate.CheckRecordedAt(out.RecordedAt, now); err != nil {
		return out, err
	}
	out.RecordedAt = out.RecordedAt.UTC().Truncate(time.Microsecond)
	out.DeviceType = strings.ToLower(strings.TrimSpace(out.DeviceType))
	return out, nil
}

// lookupReadingDeviceType 为隐式设备推导型号：Android MAC 查公共库，其次沿用用户设备记录。
func lookupReadingDeviceType(ctx context.Context, tx *sql.Tx, q *sqlcgen.Queries, uid uuid.UUID, platform, externalID string) string {
	if platform == validate.PlatformAndroid {
		if pd, err := q.GetPublicDevice(ctx, strings.ToLower(externalID)); err == nil {
			return pd.DeviceType
		}
	}
	var deviceType sql.NullString
	err := tx.QueryRowContext(ctx, `
		SELECT device_type FROM user_devices
		WHERE user_id = $1 AND platform = $2 AND external_id = $3 AND deleted_at IS NULL`,
		uid, platform, externalID).Scan(&deviceType)
	if err == nil && deviceType.Valid {
		return strings.TrimSpace(deviceType.String)
	}
	return ""
}

func insertReading(ctx context.Context, tx *sql.Tx, uid uuid.UUID, id uuid.UUID, r model.PushReading, deviceType string) (bool, error) {
	res, err := tx.ExecContext(ctx, `
		INSERT INTO readings (id, user_id, platform, external_id, metric, value, unit, recorded_at, device_type)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (id) DO NOTHING`,
		id, uid, r.Platform, r.ExternalID, r.Metric, r.Value, r.Unit, r.RecordedAt,
		nullableString(deviceType))
	if err != nil {
		return false, fmt.Errorf("写入读数失败: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("读取写入结果失败: %w", err)
	}
	return affected > 0, nil
}

func selectReading(ctx context.Context, tx *sql.Tx, id uuid.UUID, uid uuid.UUID) (*readingRow, error) {
	var row readingRow
	err := tx.QueryRowContext(ctx, `
		SELECT id, platform, external_id, metric, value, unit, recorded_at, device_type
		FROM readings WHERE id = $1 AND user_id = $2`, id, uid).
		Scan(&row.ID, &row.Platform, &row.ExternalID, &row.Metric, &row.Value, &row.Unit, &row.RecordedAt, &row.DeviceType)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("查询读数失败: %w", err)
	}
	row.RecordedAt = row.RecordedAt.UTC()
	return &row, nil
}

// sameReadingContent 判断重复 id 的读数内容是否一致。
// recordedAt 已截断到微秒，避免客户端纳秒精度与数据库精度差异造成误判。
// deviceType 是服务端解析后的最终型号；客户端未显式上报时忽略历史推导值的差异。
func sameReadingContent(existing *readingRow, r model.PushReading, deviceType string) bool {
	var storedDeviceType string
	if existing.DeviceType.Valid {
		storedDeviceType = existing.DeviceType.String
	}
	if r.DeviceType != "" && storedDeviceType != r.DeviceType {
		return false
	}
	return existing.Platform == r.Platform &&
		existing.ExternalID == r.ExternalID &&
		existing.Metric == r.Metric &&
		existing.Value == r.Value &&
		existing.Unit == r.Unit &&
		existing.RecordedAt.Equal(r.RecordedAt) &&
		(deviceType == "" || storedDeviceType == deviceType)
}

// ListReadings 实现 store.Store。
func (s *Store) ListReadings(ctx context.Context, userID string, platform string, externalID string, q model.ReadingsQuery) (model.ReadingsPage, error) {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return model.ReadingsPage{}, fmt.Errorf("非法 user id: %w", err)
	}
	if err := s.ensureUserDevice(ctx, uid, platform, externalID); err != nil {
		return model.ReadingsPage{}, err
	}

	limit := q.Limit
	if limit <= 0 {
		limit = 100
	}

	var metric any
	if q.Metric != "" {
		metric = q.Metric
	}
	var from, to sql.NullTime
	if q.From != nil {
		from = sql.NullTime{Time: *q.From, Valid: true}
	}
	if q.To != nil {
		to = sql.NullTime{Time: *q.To, Valid: true}
	}
	var afterAt sql.NullTime
	afterID := uuid.Nil
	if q.Cursor != "" {
		at, id, err := decodeReadingsCursor(q.Cursor)
		if err != nil {
			return model.ReadingsPage{}, err
		}
		afterAt = sql.NullTime{Time: at, Valid: true}
		afterID = id
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, metric, value, unit, recorded_at
		FROM readings
		WHERE user_id = $1 AND platform = $2 AND external_id = $3
		  AND ($4::timestamptz IS NULL OR recorded_at >= $4)
		  AND ($5::timestamptz IS NULL OR recorded_at <= $5)
		  AND ($6::text IS NULL OR metric = $6)
		  AND ($7::timestamptz IS NULL OR (recorded_at, id) < ($7, $8))
		ORDER BY recorded_at DESC, id DESC
		LIMIT $9`,
		uid, platform, externalID, from, to, metric, afterAt, afterID, limit+1)
	if err != nil {
		return model.ReadingsPage{}, fmt.Errorf("查询读数失败: %w", err)
	}
	defer rows.Close()

	page := model.ReadingsPage{Items: make([]model.Reading, 0, limit)}
	for rows.Next() {
		var item model.Reading
		var id uuid.UUID
		if err := rows.Scan(&id, &item.Metric, &item.Value, &item.Unit, &item.RecordedAt); err != nil {
			return model.ReadingsPage{}, fmt.Errorf("解析读数失败: %w", err)
		}
		item.ID = id.String()
		item.RecordedAt = item.RecordedAt.UTC()
		page.Items = append(page.Items, item)
	}
	if err := rows.Err(); err != nil {
		return model.ReadingsPage{}, fmt.Errorf("遍历读数失败: %w", err)
	}

	if len(page.Items) > limit {
		page.HasMore = true
		page.Items = page.Items[:limit]
		last := page.Items[len(page.Items)-1]
		page.NextCursor = encodeReadingsCursor(last.RecordedAt, last.ID)
	}
	return page, nil
}

// ReadingsSummary 实现 store.Store。
func (s *Store) ReadingsSummary(ctx context.Context, userID string, platform string, externalID string, metric string, from time.Time, to time.Time) (model.ReadingsSummary, error) {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return model.ReadingsSummary{}, fmt.Errorf("非法 user id: %w", err)
	}
	if err := s.ensureUserDevice(ctx, uid, platform, externalID); err != nil {
		return model.ReadingsSummary{}, err
	}

	summary := model.ReadingsSummary{Metric: metric, WindowStart: from.UTC(), WindowEnd: to.UTC()}
	var minV, maxV, avgV sql.NullFloat64
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*), MIN(value), MAX(value), AVG(value)
		FROM readings
		WHERE user_id = $1 AND platform = $2 AND external_id = $3 AND metric = $4
		  AND recorded_at >= $5 AND recorded_at <= $6`,
		uid, platform, externalID, metric, from, to).Scan(&summary.Count, &minV, &maxV, &avgV); err != nil {
		return model.ReadingsSummary{}, fmt.Errorf("聚合读数失败: %w", err)
	}
	if summary.Count == 0 {
		return summary, nil
	}

	summary.Min = floatPtr(minV.Float64)
	summary.Max = floatPtr(maxV.Float64)
	summary.Avg = floatPtr(avgV.Float64)

	var last float64
	var unit string
	if err := s.db.QueryRowContext(ctx, `
		SELECT value, unit FROM readings
		WHERE user_id = $1 AND platform = $2 AND external_id = $3 AND metric = $4
		  AND recorded_at >= $5 AND recorded_at <= $6
		ORDER BY recorded_at DESC, id DESC LIMIT 1`,
		uid, platform, externalID, metric, from, to).Scan(&last, &unit); err != nil {
		return model.ReadingsSummary{}, fmt.Errorf("查询最新读数失败: %w", err)
	}
	summary.Last = floatPtr(last)
	summary.Unit = unit
	return summary, nil
}

// CleanupReadings 实现 store.Store。
func (s *Store) CleanupReadings(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM readings WHERE recorded_at < $1`, before.UTC())
	if err != nil {
		return 0, fmt.Errorf("清理读数失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("读取清理结果失败: %w", err)
	}
	return n, nil
}

// ensureUserDevice 校验设备属于当前用户且未删除；否则返回 ErrNotFound（含越权）。
func (s *Store) ensureUserDevice(ctx context.Context, uid uuid.UUID, platform, externalID string) error {
	var exists bool
	if err := s.db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM user_devices
			WHERE user_id = $1 AND platform = $2 AND external_id = $3 AND deleted_at IS NULL
		)`, uid, platform, externalID).Scan(&exists); err != nil {
		return fmt.Errorf("查询用户设备失败: %w", err)
	}
	if !exists {
		return store.ErrNotFound
	}
	return nil
}

func floatPtr(v float64) *float64 { return &v }

// encodeReadingsCursor 把 (recordedAt, id) 编码为不透明游标。
func encodeReadingsCursor(at time.Time, id string) string {
	raw := at.UTC().Format(time.RFC3339Nano) + "|" + id
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// decodeReadingsCursor 解析游标；非法游标返回 ErrBadCursor 供 HTTP 层映射为 400。
func decodeReadingsCursor(cursor string) (time.Time, uuid.UUID, error) {
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
