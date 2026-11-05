package fixture

import (
	"context"
	"encoding/base64"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/platojobs/PJGoIoT/internal/model"
	"github.com/platojobs/PJGoIoT/internal/store"
	"github.com/platojobs/PJGoIoT/internal/validate"
)

// pushReadingsLocked 在已持有写锁的情况下按 id 幂等写入读数。
func (s *Store) pushReadingsLocked(data *userData, readings []model.PushReading, now time.Time, result *model.PushResult) error {
	if data.readings == nil {
		data.readings = map[string]*readingRecord{}
	}
	for i := range readings {
		r, err := normalizeFixtureReading(readings[i], now)
		if err != nil {
			return fmt.Errorf("读数 %d 非法: %w", i, err)
		}

		if existing, ok := data.readings[r.ID]; ok {
			result.SkippedReadings++
			if !sameFixtureReading(existing, r) {
				result.Conflicts = append(result.Conflicts, model.Conflict{
					ID: r.ID, Platform: r.Platform, ExternalID: r.ExternalID,
					Entity: "reading", Reason: "duplicate_id",
					Reading: &model.Reading{
						ID: r.ID, Metric: r.Metric, Value: r.Value, Unit: r.Unit, RecordedAt: r.RecordedAt,
					},
				})
			}
			continue
		}

		deviceType := r.DeviceType
		if deviceType == "" {
			deviceType = s.resolveReadingDeviceTypeLocked(data, r.Platform, r.ExternalID)
		}
		data.readings[r.ID] = &readingRecord{
			platform: r.Platform, externalID: r.ExternalID, metric: r.Metric, value: r.Value,
			unit: r.Unit, recordedAt: r.RecordedAt, deviceType: deviceType,
		}
		result.AppliedReadings++
	}
	return nil
}

func normalizeFixtureReading(in model.PushReading, now time.Time) (model.PushReading, error) {
	out := in
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

// resolveReadingDeviceTypeLocked 为隐式设备推导型号，与 Postgres 实现保持一致。
func (s *Store) resolveReadingDeviceTypeLocked(data *userData, platform, externalID string) string {
	if platform == validate.PlatformAndroid {
		if pd, ok := s.publicDevices[strings.ToLower(externalID)]; ok {
			return pd.DeviceType
		}
	}
	if d, ok := data.byKey[deviceKey(platform, externalID)]; ok && d.DeletedAt == nil && d.DeviceType != nil {
		return *d.DeviceType
	}
	return ""
}

func sameFixtureReading(existing *readingRecord, r model.PushReading) bool {
	// 客户端未显式提供 deviceType 时不对比服务端推导型号。
	if r.DeviceType != "" && existing.deviceType != r.DeviceType {
		return false
	}
	return existing.platform == r.Platform &&
		existing.externalID == r.ExternalID &&
		existing.metric == r.Metric &&
		existing.value == r.Value &&
		existing.unit == r.Unit &&
		existing.recordedAt.Equal(r.RecordedAt)
}

// ListReadings 实现 store.Store。
func (s *Store) ListReadings(ctx context.Context, userID string, platform string, externalID string, q model.ReadingsQuery) (model.ReadingsPage, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	data, ok := s.users[userID]
	if !ok {
		return model.ReadingsPage{}, store.ErrNotFound
	}
	d, ok := data.byKey[deviceKey(platform, externalID)]
	if !ok || d.DeletedAt != nil {
		return model.ReadingsPage{}, store.ErrNotFound
	}

	limit := q.Limit
	if limit <= 0 {
		limit = 100
	}
	afterAt, afterID, err := decodeFixtureCursor(q.Cursor)
	if err != nil {
		return model.ReadingsPage{}, err
	}

	items := make([]model.Reading, 0, len(data.readings))
	for id, rec := range data.readings {
		if rec.platform != platform || rec.externalID != externalID {
			continue
		}
		if q.Metric != "" && rec.metric != q.Metric {
			continue
		}
		if q.From != nil && rec.recordedAt.Before(*q.From) {
			continue
		}
		if q.To != nil && rec.recordedAt.After(*q.To) {
			continue
		}
		if q.Cursor != "" && !beforeCursor(rec.recordedAt, id, afterAt, afterID) {
			continue
		}
		items = append(items, model.Reading{
			ID: id, Metric: rec.metric, Value: rec.value, Unit: rec.unit, RecordedAt: rec.recordedAt,
		})
	}
	sort.SliceStable(items, func(i, j int) bool {
		if !items[i].RecordedAt.Equal(items[j].RecordedAt) {
			return items[i].RecordedAt.After(items[j].RecordedAt)
		}
		return items[i].ID > items[j].ID
	})

	page := model.ReadingsPage{Items: items}
	if len(page.Items) > limit {
		page.HasMore = true
		page.Items = page.Items[:limit]
		last := page.Items[len(page.Items)-1]
		page.NextCursor = encodeFixtureCursor(last.RecordedAt, last.ID)
	}
	return page, nil
}

// ReadingsSummary 实现 store.Store。
func (s *Store) ReadingsSummary(ctx context.Context, userID string, platform string, externalID string, metric string, from time.Time, to time.Time) (model.ReadingsSummary, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	data, ok := s.users[userID]
	if !ok {
		return model.ReadingsSummary{}, store.ErrNotFound
	}
	d, ok := data.byKey[deviceKey(platform, externalID)]
	if !ok || d.DeletedAt != nil {
		return model.ReadingsSummary{}, store.ErrNotFound
	}

	summary := model.ReadingsSummary{Metric: metric, WindowStart: from.UTC(), WindowEnd: to.UTC()}
	var sum float64
	var latest *readingRecord
	for _, rec := range data.readings {
		if rec.platform != platform || rec.externalID != externalID || rec.metric != metric {
			continue
		}
		if rec.recordedAt.Before(from) || rec.recordedAt.After(to) {
			continue
		}
		summary.Count++
		sum += rec.value
		if summary.Min == nil || rec.value < *summary.Min {
			v := rec.value
			summary.Min = &v
		}
		if summary.Max == nil || rec.value > *summary.Max {
			v := rec.value
			summary.Max = &v
		}
		if latest == nil || !rec.recordedAt.Before(latest.recordedAt) {
			latest = rec
		}
	}
	if summary.Count == 0 {
		return summary, nil
	}
	avg := sum / float64(summary.Count)
	summary.Avg = &avg
	last := latest.value
	summary.Last = &last
	summary.Unit = latest.unit
	return summary, nil
}

// CleanupReadings 实现 store.Store。
func (s *Store) CleanupReadings(ctx context.Context, before time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var deleted int64
	for _, data := range s.users {
		for id, rec := range data.readings {
			if rec.recordedAt.Before(before) {
				delete(data.readings, id)
				deleted++
			}
		}
	}
	return deleted, nil
}

// beforeCursor 判断 (at, id) 是否严格早于游标位置，用于倒序分页。
func beforeCursor(at time.Time, id string, cursorAt time.Time, cursorID uuid.UUID) bool {
	if !at.Equal(cursorAt) {
		return at.Before(cursorAt)
	}
	return id < cursorID.String()
}

func encodeFixtureCursor(at time.Time, id string) string {
	raw := at.UTC().Format(time.RFC3339Nano) + "|" + id
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeFixtureCursor(cursor string) (time.Time, uuid.UUID, error) {
	if cursor == "" {
		return time.Time{}, uuid.Nil, nil
	}
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
