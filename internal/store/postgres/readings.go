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
// 插入使用一条多行 INSERT，仅在主键冲突时才逐条读取比较。
func pushReadingsTx(ctx context.Context, tx *sql.Tx, uid uuid.UUID, readings []model.PushReading, now time.Time, result *model.PushResult, fallback *sqlcgen.Queries) error {
	// 同一批读数通常来自少量设备，缓存型号推导结果避免逐条查询（N+1）。
	typeCache := map[string]string{}
	canonical := make([]model.PushReading, 0, len(readings))
	deviceTypes := make([]string, 0, len(readings))
	for i := range readings {
		r := &readings[i]
		c, err := normalizePushReading(r, now)
		if err != nil {
			return fmt.Errorf("读数 %d 非法: %w", i, err)
		}

		deviceType := c.DeviceType
		if deviceType == "" {
			key := c.Platform + "|" + c.ExternalID
			cached, ok := typeCache[key]
			if !ok {
				cached = lookupReadingDeviceType(ctx, tx, fallback, uid, c.Platform, c.ExternalID)
				typeCache[key] = cached
			}
			deviceType = cached
		}
		canonical = append(canonical, c)
		deviceTypes = append(deviceTypes, deviceType)
	}

	inserted, err := insertReadingsBatch(ctx, tx, uid, canonical, deviceTypes)
	if err != nil {
		return err
	}
	// 同批出现重复 id 时，INSERT ... RETURNING 只返回一行（或同一 id 命中一次），
	// 但 inserted 是按 id 的集合，直接按条目计数会把重复条目重复计为 applied。
	// 用 credited 保证一个 id 最多计一次 applied，其余严格归入 skipped。
	credited := make(map[string]bool, len(inserted))
	for i := range canonical {
		c := &canonical[i]
		readingID, _ := uuid.Parse(c.ID)
		if inserted[c.ID] && !credited[c.ID] {
			credited[c.ID] = true
			result.AppliedReadings++
			continue
		}

		// 主键冲突：读取已存在记录比较内容。
		existing, err := selectReading(ctx, tx, readingID, uid)
		if err != nil {
			return err
		}
		result.SkippedReadings++
		if existing == nil || !sameReadingContent(existing, *c, deviceTypes[i]) {
			result.Conflicts = append(result.Conflicts, model.Conflict{
				ID: c.ID, Platform: c.Platform, ExternalID: c.ExternalID,
				Entity: "reading", Reason: "duplicate_id",
				Reading: &model.Reading{
					ID: c.ID, Metric: c.Metric, Value: c.Value,
					Unit: c.Unit, RecordedAt: c.RecordedAt,
				},
			})
		}
	}
	return nil
}

// insertReadingsBatch 批量插入读数，返回本次真正插入的 id 集合。
func insertReadingsBatch(ctx context.Context, tx *sql.Tx, uid uuid.UUID, readings []model.PushReading, deviceTypes []string) (map[string]bool, error) {
	inserted := map[string]bool{}
	const chunk = 500
	for start := 0; start < len(readings); start += chunk {
		end := start + chunk
		if end > len(readings) {
			end = len(readings)
		}
		rows := make([][]any, 0, end-start)
		for i := start; i < end; i++ {
			id, err := uuid.Parse(readings[i].ID)
			if err != nil {
				return nil, fmt.Errorf("解析读数 id 失败: %w", err)
			}
			rows = append(rows, []any{
				id, uid, readings[i].Platform, readings[i].ExternalID, readings[i].Metric,
				readings[i].Value, readings[i].Unit, readings[i].RecordedAt, nullableString(deviceTypes[i]),
			})
		}
		values, args := buildBatchValues(1, rows, []string{
			"uuid", "uuid", "text", "text", "text", "double precision", "text", "timestamptz", "text",
		})
		query := fmt.Sprintf(`INSERT INTO readings
			(id, user_id, platform, external_id, metric, value, unit, recorded_at, device_type)
			VALUES %s ON CONFLICT (id) DO NOTHING RETURNING id`, values)
		res, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, fmt.Errorf("批量写入读数失败: %w", err)
		}
		for res.Next() {
			var id uuid.UUID
			if err := res.Scan(&id); err != nil {
				res.Close()
				return nil, fmt.Errorf("解析读数插入结果失败: %w", err)
			}
			inserted[id.String()] = true
		}
		if err := res.Err(); err != nil {
			res.Close()
			return nil, fmt.Errorf("遍历读数插入结果失败: %w", err)
		}
		res.Close()
	}
	return inserted, nil
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
	ctx, cancel := queryTimeout(ctx)
	defer cancel()
	uid, err := uuid.Parse(userID)
	if err != nil {
		return model.ReadingsPage{}, fmt.Errorf("非法 user id: %w", err)
	}
	owner, err := s.resolveVisibleOwner(ctx, uid, platform, externalID)
	if err != nil {
		return model.ReadingsPage{}, err
	}
	uid = owner

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
	ctx, cancel := queryTimeout(ctx)
	defer cancel()
	uid, err := uuid.Parse(userID)
	if err != nil {
		return model.ReadingsSummary{}, fmt.Errorf("非法 user id: %w", err)
	}
	owner, err := s.resolveVisibleOwner(ctx, uid, platform, externalID)
	if err != nil {
		return model.ReadingsSummary{}, err
	}
	uid = owner

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

// bucketIntervals 把对外枚举映射为 PostgreSQL interval 字面量。
var bucketIntervals = map[string]string{
	"1m": "1 minute",
	"1h": "1 hour",
	"1d": "1 day",
}

// ReadingsSeries 实现 store.Store。
//
// 使用单条 SQL + date_bin 做 UTC 对齐分桶，last 取桶内 recorded_at 最大的值。
// 现有索引 (user_id, platform, external_id, metric, recorded_at DESC) 的列顺序与过滤条件完全匹配，
// 无需新增覆盖索引。
func (s *Store) ReadingsSeries(ctx context.Context, userID string, platform string, externalID string, metric string, from time.Time, to time.Time, bucket string) (model.ReadingsSeries, error) {
	ctx, cancel := queryTimeout(ctx)
	defer cancel()
	uid, err := uuid.Parse(userID)
	if err != nil {
		return model.ReadingsSeries{}, fmt.Errorf("非法 user id: %w", err)
	}
	normMetric, err := validate.NormalizeMetric(metric)
	if err != nil {
		return model.ReadingsSeries{}, err
	}
	size, err := validate.NormalizeBucket(bucket)
	if err != nil {
		return model.ReadingsSeries{}, err
	}
	if _, err := validate.ValidateSeriesRange(from, to, size); err != nil {
		return model.ReadingsSeries{}, err
	}
	owner, err := s.resolveVisibleOwner(ctx, uid, platform, externalID)
	if err != nil {
		return model.ReadingsSeries{}, err
	}
	uid = owner

	series := model.ReadingsSeries{
		Metric: normMetric, Bucket: bucket, From: from.UTC(), To: to.UTC(),
		Buckets: make([]model.ReadingsBucket, 0),
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT
			date_bin($7::interval, recorded_at, $8::timestamptz) AS bucket_start,
			COUNT(*) AS count,
			MIN(value) AS min_value,
			MAX(value) AS max_value,
			AVG(value) AS avg_value,
			(ARRAY_AGG(value ORDER BY recorded_at DESC, id DESC))[1] AS last_value,
			(ARRAY_AGG(unit ORDER BY recorded_at DESC, id DESC))[1] AS last_unit
		FROM readings
		WHERE user_id = $1 AND platform = $2 AND external_id = $3 AND metric = $4
		  AND recorded_at >= $5 AND recorded_at <= $6
		GROUP BY bucket_start
		ORDER BY bucket_start ASC`,
		uid, platform, externalID, normMetric, from.UTC(), to.UTC(),
		bucketIntervals[bucket], "2000-01-01T00:00:00Z")
	if err != nil {
		return model.ReadingsSeries{}, fmt.Errorf("聚合读数序列失败: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var item model.ReadingsBucket
		var minV, maxV, avgV, lastV sql.NullFloat64
		var lastUnit string
		if err := rows.Scan(&item.Start, &item.Count, &minV, &maxV, &avgV, &lastV, &lastUnit); err != nil {
			return model.ReadingsSeries{}, fmt.Errorf("解析读数序列失败: %w", err)
		}
		item.Start = item.Start.UTC()
		item.End = item.Start.Add(size)
		item.Min = floatPtr(minV.Float64)
		item.Max = floatPtr(maxV.Float64)
		item.Avg = floatPtr(avgV.Float64)
		item.Last = floatPtr(lastV.Float64)
		series.Buckets = append(series.Buckets, item)
		series.Unit = lastUnit
	}
	if err := rows.Err(); err != nil {
		return model.ReadingsSeries{}, fmt.Errorf("遍历读数序列失败: %w", err)
	}
	return series, nil
}

// ReadingsMetrics 实现 store.Store。
//
// unit 取该指标最近一条读数的单位；排序按 lastAt 倒序，同一时间按 metric 升序保证稳定。
func (s *Store) ReadingsMetrics(ctx context.Context, userID string, platform string, externalID string) (model.ReadingMetrics, error) {
	ctx, cancel := queryTimeout(ctx)
	defer cancel()
	uid, err := uuid.Parse(userID)
	if err != nil {
		return model.ReadingMetrics{}, fmt.Errorf("非法 user id: %w", err)
	}
	owner, err := s.resolveVisibleOwner(ctx, uid, platform, externalID)
	if err != nil {
		return model.ReadingMetrics{}, err
	}
	uid = owner

	result := model.ReadingMetrics{Items: make([]model.ReadingMetric, 0)}
	rows, err := s.db.QueryContext(ctx, `
		SELECT metric,
			(ARRAY_AGG(unit ORDER BY recorded_at DESC, id DESC))[1] AS last_unit,
			COUNT(*) AS count,
			MIN(recorded_at) AS first_at,
			MAX(recorded_at) AS last_at
		FROM readings
		WHERE user_id = $1 AND platform = $2 AND external_id = $3
		GROUP BY metric
		ORDER BY last_at DESC, metric ASC`, uid, platform, externalID)
	if err != nil {
		return model.ReadingMetrics{}, fmt.Errorf("查询指标清单失败: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var item model.ReadingMetric
		if err := rows.Scan(&item.Metric, &item.Unit, &item.Count, &item.FirstAt, &item.LastAt); err != nil {
			return model.ReadingMetrics{}, fmt.Errorf("解析指标清单失败: %w", err)
		}
		item.FirstAt = item.FirstAt.UTC()
		item.LastAt = item.LastAt.UTC()
		result.Items = append(result.Items, item)
	}
	if err := rows.Err(); err != nil {
		return model.ReadingMetrics{}, fmt.Errorf("遍历指标清单失败: %w", err)
	}
	return result, nil
}

// resolveVisibleOwner 返回设备 owner 的 user_id；仅本人或家庭成员可见，否则 ErrNotFound。
// 读数仍按 owner 维度存储，共享成员读取时替换查询的 user_id 即可。
func (s *Store) resolveVisibleOwner(ctx context.Context, uid uuid.UUID, platform, externalID string) (uuid.UUID, error) {
	var owner uuid.UUID
	err := s.db.QueryRowContext(ctx, `
		SELECT d.user_id FROM user_devices d
		WHERE d.platform = $2 AND d.external_id = $3 AND d.deleted_at IS NULL
		  AND (d.user_id = $1 OR d.household_id IN (
		        SELECT hm.household_id FROM household_members hm WHERE hm.user_id = $1))
		ORDER BY (d.user_id = $1) DESC
		LIMIT 1`, uid, platform, externalID).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return uuid.Nil, store.ErrNotFound
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("查询用户设备失败: %w", err)
	}
	return owner, nil
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
