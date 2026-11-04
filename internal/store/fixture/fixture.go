// Package fixture 提供只读 fixtures + 内存私有数据存储，用于无数据库环境启动和测试。
// 生产环境应使用 postgres 实现；本实现的私有数据在进程重启后丢失。
package fixture

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/platojobs/PJGoIoT/internal/lww"
	"github.com/platojobs/PJGoIoT/internal/model"
	"github.com/platojobs/PJGoIoT/internal/store"
)

type idempotencyRecord struct {
	RequestHash string
	Response    model.PushResult
}

type changeRecord struct {
	seq      int64
	userID   string
	entity   string
	entityID string
	op       string
	payload  json.RawMessage
	serverTS time.Time
}

type userData struct {
	byID  map[string]*model.UserDevice
	byMAC map[string]*model.UserDevice
}

// Store 是内存实现，读 fixtures 初始化公共元数据。
type Store struct {
	mu sync.RWMutex

	publicDevices map[string]model.PublicDevice
	deviceTypes   []model.DeviceType
	deviceTypeSet map[string]struct{}
	firmwares     []model.Firmware
	accessories   []model.Accessory

	tokens map[string]string // tokenHash -> userID
	users  map[string]*userData

	changes     []changeRecord
	nextSeq     int64
	idempotency map[string]idempotencyRecord
}

// New 从 fixtures 目录加载公共元数据。
func New(dir string) (*Store, error) {
	s := &Store{
		publicDevices: map[string]model.PublicDevice{},
		deviceTypeSet: map[string]struct{}{},
		tokens:        map[string]string{},
		users:         map[string]*userData{},
		nextSeq:       1,
		idempotency:   map[string]idempotencyRecord{},
	}

	deviceTypes, err := loadJSON[[]model.DeviceType](filepath.Join(dir, "device_types.json"))
	if err != nil {
		return nil, err
	}
	for _, dt := range deviceTypes {
		s.deviceTypes = append(s.deviceTypes, dt)
		s.deviceTypeSet[dt.Type] = struct{}{}
	}
	sort.SliceStable(s.deviceTypes, func(i, j int) bool { return s.deviceTypes[i].Type < s.deviceTypes[j].Type })

	devices, err := loadJSON[[]model.PublicDevice](filepath.Join(dir, "public_devices.json"))
	if err != nil {
		return nil, err
	}
	for _, d := range devices {
		s.publicDevices[d.MAC] = d
	}

	firmwares, err := loadJSON[[]model.Firmware](filepath.Join(dir, "firmwares.json"))
	if err != nil {
		return nil, err
	}
	s.firmwares = firmwares

	accessories, err := loadJSON[[]model.Accessory](filepath.Join(dir, "accessories.json"))
	if err != nil {
		return nil, err
	}
	s.accessories = accessories

	return s, nil
}

func loadJSON[T any](path string) (T, error) {
	var out T
	raw, err := os.ReadFile(path)
	if err != nil {
		return out, fmt.Errorf("读取 fixture %s 失败: %w", path, err)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, fmt.Errorf("解析 fixture %s 失败: %w", path, err)
	}
	return out, nil
}

// Backend 实现 store.Store。
func (s *Store) Backend() string { return "fixture" }

// Ping 实现 store.Store。
func (s *Store) Ping(ctx context.Context) error { return ctx.Err() }

// Close 实现 store.Store。
func (s *Store) Close() error { return nil }

// GetPublicDevice 实现 store.Store。
func (s *Store) GetPublicDevice(ctx context.Context, mac string) (model.PublicDevice, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	d, ok := s.publicDevices[mac]
	if !ok {
		return model.PublicDevice{}, store.ErrNotFound
	}
	return d, nil
}

// ListDeviceTypes 实现 store.Store。
func (s *Store) ListDeviceTypes(ctx context.Context) ([]model.DeviceType, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]model.DeviceType, len(s.deviceTypes))
	copy(out, s.deviceTypes)
	return out, nil
}

// DeviceTypeExists 实现 store.Store。
func (s *Store) DeviceTypeExists(ctx context.Context, deviceType string) (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.deviceTypeSet[deviceType]
	return ok, nil
}

// ListFirmwares 实现 store.Store。
func (s *Store) ListFirmwares(ctx context.Context, deviceType string) ([]model.Firmware, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]model.Firmware, 0, 4)
	for _, f := range s.firmwares {
		if f.DeviceType == deviceType {
			out = append(out, f)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].ReleasedAt != out[j].ReleasedAt {
			return out[i].ReleasedAt > out[j].ReleasedAt
		}
		return out[i].Version > out[j].Version
	})
	return out, nil
}

// ListAccessories 实现 store.Store。
func (s *Store) ListAccessories(ctx context.Context, deviceType string) ([]model.Accessory, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]model.Accessory, 0, 4)
	for _, a := range s.accessories {
		if a.DeviceType == deviceType {
			out = append(out, a)
		}
	}
	return out, nil
}

// CreateUserWithToken 实现 store.Store。
func (s *Store) CreateUserWithToken(ctx context.Context, tokenHash string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	userID := uuid.NewString()
	s.users[userID] = &userData{byID: map[string]*model.UserDevice{}, byMAC: map[string]*model.UserDevice{}}
	s.tokens[tokenHash] = userID
	return userID, nil
}

// UserIDByTokenHash 实现 store.Store。
func (s *Store) UserIDByTokenHash(ctx context.Context, tokenHash string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	userID, ok := s.tokens[tokenHash]
	if !ok {
		return "", store.ErrUnauthorized
	}
	return userID, nil
}

// RevokeToken 实现 store.Store。
func (s *Store) RevokeToken(ctx context.Context, tokenHash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	userID, ok := s.tokens[tokenHash]
	if !ok {
		return store.ErrUnauthorized
	}
	delete(s.tokens, tokenHash)
	delete(s.users, userID)
	for i := range s.changes {
		if s.changes[i].userID == userID {
			// 用户注销后其变更不再对外可见。
			s.changes[i].payload = json.RawMessage(`null`)
		}
	}
	// 变更日志按 user_id 过滤，删除用户后自然不可见，无需重写。
	return nil
}

// ListUserDevices 实现 store.Store。
func (s *Store) ListUserDevices(ctx context.Context, userID string) ([]model.UserDevice, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	data, ok := s.users[userID]
	if !ok {
		return []model.UserDevice{}, nil
	}
	out := make([]model.UserDevice, 0, len(data.byID))
	for _, d := range data.byID {
		if d.DeletedAt != nil {
			continue
		}
		out = append(out, *d)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].UpdatedAt.After(out[j].UpdatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// GetUserDevice 实现 store.Store。
func (s *Store) GetUserDevice(ctx context.Context, userID string, mac string) (model.UserDevice, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	data, ok := s.users[userID]
	if !ok {
		return model.UserDevice{}, store.ErrNotFound
	}
	d, ok := data.byMAC[mac]
	if !ok || d.DeletedAt != nil {
		return model.UserDevice{}, store.ErrNotFound
	}
	return *d, nil
}

// Push 实现 store.Store。
func (s *Store) Push(ctx context.Context, userID string, req model.PushRequest, opts store.PushOptions) (model.PushResult, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if opts.IdempotencyKey != "" {
		key := userID + "|" + opts.IdempotencyKey
		if rec, ok := s.idempotency[key]; ok {
			if rec.RequestHash != opts.RequestHash {
				return model.PushResult{}, false, store.ErrIdempotencyConflict
			}
			return cloneResult(rec.Response), true, nil
		}
	}

	data, ok := s.users[userID]
	if !ok {
		return model.PushResult{}, false, store.ErrUnauthorized
	}

	now := time.Now().UTC()
	result := model.PushResult{Applied: []model.AppliedChange{}, Conflicts: []model.Conflict{}}

	for _, dev := range req.Devices {
		existingByID := data.byID[dev.ID]
		var macHolder *model.UserDevice
		if existingByID != nil && existingByID.MAC != dev.MAC {
			macHolder = data.byMAC[dev.MAC]
		}
		if existingByID != nil && macHolder != nil && macHolder.ID != existingByID.ID {
			result.Conflicts = append(result.Conflicts, model.Conflict{
				ID: dev.ID, MAC: dev.MAC, Reason: "mac_taken", Server: *macHolder, Client: dev,
			})
			continue
		}

		existing := existingByID
		if existing == nil {
			existing = data.byMAC[dev.MAC]
		}

		decision := lww.Decide(now, dev, existing)
		if !decision.Apply && !decision.Noop {
			result.Conflicts = append(result.Conflicts, model.Conflict{
				ID: dev.ID, MAC: dev.MAC, Reason: decision.Reason, Server: *existing, Client: dev,
			})
			continue
		}
		if decision.Noop {
			result.Applied = append(result.Applied, model.AppliedChange{
				ID: dev.ID, MAC: dev.MAC, Status: "unchanged", ServerSeq: existing.ServerSeq,
			})
			continue
		}

		seq := s.nextSeq
		s.nextSeq++
		record := &model.UserDevice{
			ID:          dev.ID,
			ClientID:    req.ClientID,
			MAC:         dev.MAC,
			Alias:       dev.Alias,
			Group:       dev.Group,
			Rules:       dev.Rules,
			ScanRecords: dev.ScanRecords,
			UpdatedAt:   decision.EffectiveUpdatedAt,
			ServerSeq:   seq,
		}
		if dev.Deleted {
			deletedAt := decision.EffectiveUpdatedAt
			record.DeletedAt = &deletedAt
		}

		if existing != nil {
			delete(data.byID, existing.ID)
			delete(data.byMAC, existing.MAC)
		}
		data.byID[record.ID] = record
		data.byMAC[record.MAC] = record

		op := "upsert"
		if dev.Deleted {
			op = "delete"
		}
		payload, err := json.Marshal(record)
		if err != nil {
			return model.PushResult{}, false, fmt.Errorf("序列化变更记录失败: %w", err)
		}
		s.changes = append(s.changes, changeRecord{
			seq: seq, userID: userID, entity: "user_device", entityID: record.ID,
			op: op, payload: payload, serverTS: now,
		})
		result.Applied = append(result.Applied, model.AppliedChange{
			ID: dev.ID, MAC: dev.MAC, Status: "applied", ServerSeq: seq,
		})
	}

	result.NextCursor = s.maxSeqLocked(userID)
	if opts.IdempotencyKey != "" {
		s.idempotency[userID+"|"+opts.IdempotencyKey] = idempotencyRecord{
			RequestHash: opts.RequestHash,
			Response:    cloneResult(result),
		}
	}
	return result, false, nil
}

// Changes 实现 store.Store。
func (s *Store) Changes(ctx context.Context, userID string, cursor int64, limit int) (model.ChangesPage, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	page := model.ChangesPage{Changes: []model.Change{}}
	count := 0
	lastSeq := cursor
	for _, rec := range s.changes {
		if rec.userID != userID || rec.seq <= cursor {
			continue
		}
		if count == limit {
			page.HasMore = true
			break
		}
		if rec.payload == nil || string(rec.payload) == "null" {
			continue
		}
		page.Changes = append(page.Changes, model.Change{
			Seq: rec.seq, Entity: rec.entity, EntityID: rec.entityID,
			Op: rec.op, Payload: rec.payload, ServerTS: rec.serverTS,
		})
		lastSeq = rec.seq
		count++
	}
	page.NextCursor = lastSeq
	return page, nil
}

func (s *Store) maxSeqLocked(userID string) int64 {
	var max int64
	for _, rec := range s.changes {
		if rec.userID == userID && rec.seq > max {
			max = rec.seq
		}
	}
	return max
}

func cloneResult(in model.PushResult) model.PushResult {
	raw, _ := json.Marshal(in)
	var out model.PushResult
	_ = json.Unmarshal(raw, &out)
	return out
}
