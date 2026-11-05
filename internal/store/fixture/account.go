package fixture

import (
	"context"
	"encoding/base64"
	"sort"
	"strings"
	"time"

	"github.com/platojobs/PJGoIoT/internal/model"
	"github.com/platojobs/PJGoIoT/internal/store"
)

const maxPairingAttempts = 5

// GetUser 实现 store.Store。
func (s *Store) GetUser(ctx context.Context, userID string) (model.User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	data, ok := s.users[userID]
	if !ok {
		return model.User{}, store.ErrUnauthorized
	}
	return model.User{ID: userID, Kind: data.kind, CreatedAt: data.createdAt}, nil
}

// DeleteUser 实现 store.Store：硬删除该用户全部相关数据，重复注销返回 ErrUnauthorized。
func (s *Store) DeleteUser(ctx context.Context, userID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.users[userID]; !ok {
		return store.ErrUnauthorized
	}
	delete(s.users, userID)

	for hash, uid := range s.tokens {
		if uid == userID {
			delete(s.tokens, hash)
		}
	}
	for key := range s.idempotency {
		if strings.HasPrefix(key, userID+"|") {
			delete(s.idempotency, key)
		}
	}
	for hash, rec := range s.pairingCodes {
		if rec.userID == userID {
			delete(s.pairingCodes, hash)
		}
	}
	kept := s.changes[:0]
	for _, rec := range s.changes {
		if rec.userID != userID {
			kept = append(kept, rec)
		}
	}
	s.changes = kept
	return nil
}

// ListUserDevicesForExport 实现 store.Store：包含 tombstone，按 updatedAt 倒序。
func (s *Store) ListUserDevicesForExport(ctx context.Context, userID string) ([]model.UserDevice, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	data, ok := s.users[userID]
	if !ok {
		return []model.UserDevice{}, nil
	}
	out := make([]model.UserDevice, 0, len(data.byID))
	for _, d := range data.byID {
		out = append(out, s.enrichUserDevice(*d))
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].UpdatedAt.After(out[j].UpdatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// ExportReadings 实现 store.Store。
func (s *Store) ExportReadings(ctx context.Context, userID string, cursor string, limit int) ([]model.ExportReading, string, bool, error) {
	if limit <= 0 {
		limit = 5000
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	data, ok := s.users[userID]
	if !ok {
		return []model.ExportReading{}, "", false, nil
	}

	items := make([]model.ExportReading, 0, len(data.readings))
	for id, rec := range data.readings {
		items = append(items, model.ExportReading{
			ID:         id,
			Platform:   rec.platform,
			ExternalID: rec.externalID,
			Metric:     rec.metric,
			Value:      rec.value,
			Unit:       rec.unit,
			RecordedAt: rec.recordedAt.UTC(),
			DeviceType: rec.deviceType,
		})
	}
	sort.SliceStable(items, func(i, j int) bool {
		if !items[i].RecordedAt.Equal(items[j].RecordedAt) {
			return items[i].RecordedAt.Before(items[j].RecordedAt)
		}
		return items[i].ID < items[j].ID
	})

	start := 0
	if cursor != "" {
		at, id, err := decodeExportCursor(cursor)
		if err != nil {
			return nil, "", false, err
		}
		start = len(items)
		for i, item := range items {
			if item.RecordedAt.After(at) || (item.RecordedAt.Equal(at) && item.ID > id) {
				start = i
				break
			}
		}
	}
	if start > len(items) {
		start = len(items)
	}
	rest := items[start:]
	if len(rest) > limit {
		page := append([]model.ExportReading(nil), rest[:limit]...)
		last := page[len(page)-1]
		return page, encodeExportCursor(last.RecordedAt, last.ID), true, nil
	}
	return append([]model.ExportReading(nil), rest...), "", false, nil
}

func encodeExportCursor(at time.Time, id string) string {
	raw := at.UTC().Format(time.RFC3339Nano) + "|" + id
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeExportCursor(cursor string) (time.Time, string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return time.Time{}, "", store.ErrBadCursor
	}
	parts := strings.SplitN(string(raw), "|", 2)
	if len(parts) != 2 || parts[1] == "" {
		return time.Time{}, "", store.ErrBadCursor
	}
	at, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return time.Time{}, "", store.ErrBadCursor
	}
	return at.UTC(), parts[1], nil
}

// CreatePairingCode 实现 store.Store：同一用户的新码使旧码失效。
func (s *Store) CreatePairingCode(ctx context.Context, userID string, codeHash string, expiresAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.users[userID]; !ok {
		return store.ErrUnauthorized
	}
	now := time.Now().UTC()
	for hash, rec := range s.pairingCodes {
		if rec.userID == userID || !rec.expiresAt.After(now) {
			delete(s.pairingCodes, hash)
		}
	}
	s.pairingCodes[codeHash] = &pairingRecord{userID: userID, expiresAt: expiresAt.UTC()}
	return nil
}

// ClaimPairingCode 实现 store.Store。
func (s *Store) ClaimPairingCode(ctx context.Context, codeHash string, tokenHash string, now time.Time, ttl time.Duration) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rec, ok := s.pairingCodes[codeHash]
	if !ok {
		s.recordPairingFailureLocked(codeHash, now, ttl)
		return "", store.ErrNotFound
	}
	if rec.userID == "" || rec.attempts >= maxPairingAttempts || !rec.expiresAt.After(now) {
		s.recordPairingFailureLocked(codeHash, now, ttl)
		return "", store.ErrNotFound
	}
	if _, exists := s.users[rec.userID]; !exists {
		delete(s.pairingCodes, codeHash)
		return "", store.ErrNotFound
	}

	delete(s.pairingCodes, codeHash)
	s.tokens[tokenHash] = rec.userID
	return rec.userID, nil
}

func (s *Store) recordPairingFailureLocked(codeHash string, now time.Time, ttl time.Duration) {
	rec, ok := s.pairingCodes[codeHash]
	if !ok {
		s.pairingCodes[codeHash] = &pairingRecord{attempts: 1, expiresAt: now.Add(ttl).UTC()}
		return
	}
	rec.attempts++
	if !rec.expiresAt.After(now) {
		rec.expiresAt = now.Add(ttl).UTC()
	}
	if rec.attempts >= maxPairingAttempts {
		delete(s.pairingCodes, codeHash)
	}
}

// CleanupPairingCodes 实现 store.Store。
func (s *Store) CleanupPairingCodes(ctx context.Context, before time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int64
	for hash, rec := range s.pairingCodes {
		if rec.expiresAt.Before(before) {
			delete(s.pairingCodes, hash)
			n++
		}
	}
	return n, nil
}

// ApproxCounts 实现 store.Store：内存实现直接返回精确行数。
func (s *Store) ApproxCounts(ctx context.Context) (model.StoreCounts, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	counts := model.StoreCounts{Users: int64(len(s.users))}
	for _, data := range s.users {
		counts.Devices += int64(len(data.byID))
		counts.Readings += int64(len(data.readings))
	}
	return counts, nil
}
