// Package fixture 提供只读 fixtures + 内存私有数据存储，用于无数据库环境启动和测试。
// 生产环境应使用 postgres 实现；本实现的私有数据在进程重启后丢失。
package fixture

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/platojobs/PJGoIoT/internal/lww"
	"github.com/platojobs/PJGoIoT/internal/model"
	"github.com/platojobs/PJGoIoT/internal/store"
	"github.com/platojobs/PJGoIoT/internal/validate"
)

type idempotencyRecord struct {
	RequestHash string
	Response    model.PushResult
	CreatedAt   time.Time
}

// tokenRecord 是内存版 token，与 tokens 表一一对应。
type tokenRecord struct {
	userID    string
	expiresAt time.Time
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
	byKey map[string]*model.UserDevice // key: platform|externalId

	readings map[string]*readingRecord // key: reading id

	kind      string
	createdAt time.Time
}

// pairingRecord 是内存版配对码，与 pairing_codes 表一一对应。
// userID 为空表示失败尝试计数行。
type pairingRecord struct {
	userID    string
	attempts  int
	expiresAt time.Time
}

// readingRecord 是内存版读数，字段与 readings 表一一对应。
type readingRecord struct {
	platform   string
	externalID string
	metric     string
	value      float64
	unit       string
	recordedAt time.Time
	deviceType string
}

// deviceKey 返回内存索引使用的复合键。
func deviceKey(platform, externalID string) string {
	return platform + "|" + externalID
}

// Store 是内存实现，读 fixtures 初始化公共元数据。
type Store struct {
	mu sync.RWMutex

	publicDevices map[string]model.PublicDevice
	deviceTypes   []model.DeviceType
	deviceTypeSet map[string]struct{}
	firmwares     []model.Firmware
	accessories   []model.Accessory

	tokens map[string]tokenRecord // tokenHash -> tokenRecord
	users  map[string]*userData

	changes     []changeRecord
	nextSeq     int64
	idempotency map[string]idempotencyRecord

	pairingCodes map[string]*pairingRecord // codeHash -> record
}

// New 从 fixtures 目录加载公共元数据。
func New(dir string) (*Store, error) {
	s := &Store{
		publicDevices: map[string]model.PublicDevice{},
		deviceTypeSet: map[string]struct{}{},
		tokens:        map[string]tokenRecord{},
		users:         map[string]*userData{},
		changes:       []changeRecord{},
		nextSeq:       1,
		idempotency:   map[string]idempotencyRecord{},
		pairingCodes:  map[string]*pairingRecord{},
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

// GetDeviceType 实现 store.Store。
func (s *Store) GetDeviceType(ctx context.Context, deviceType string) (model.DeviceType, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, dt := range s.deviceTypes {
		if dt.Type == deviceType {
			return dt, nil
		}
	}
	return model.DeviceType{}, store.ErrNotFound
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
func (s *Store) CreateUserWithToken(ctx context.Context, tokenHash string, expiresAt time.Time) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	userID := uuid.NewString()
	s.users[userID] = &userData{
		byID:      map[string]*model.UserDevice{},
		byKey:     map[string]*model.UserDevice{},
		readings:  map[string]*readingRecord{},
		kind:      "anonymous",
		createdAt: time.Now().UTC(),
	}
	s.tokens[tokenHash] = tokenRecord{userID: userID, expiresAt: expiresAt.UTC()}
	return userID, nil
}

// UserIDByTokenHash 实现 store.Store。
func (s *Store) UserIDByTokenHash(ctx context.Context, tokenHash string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.tokens[tokenHash]
	if !ok || !rec.expiresAt.After(time.Now().UTC()) {
		return "", store.ErrUnauthorized
	}
	return rec.userID, nil
}

// RevokeToken 实现 store.Store。
func (s *Store) RevokeToken(ctx context.Context, tokenHash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.tokens[tokenHash]
	if !ok {
		return store.ErrUnauthorized
	}
	userID := rec.userID
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

// GetUserDevice 实现 store.Store。
func (s *Store) GetUserDevice(ctx context.Context, userID string, mac string) (model.UserDevice, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	data, ok := s.users[userID]
	if !ok {
		return model.UserDevice{}, store.ErrNotFound
	}
	var found *model.UserDevice
	for _, d := range data.byID {
		if d.MAC != mac || d.DeletedAt != nil {
			continue
		}
		if found == nil || d.UpdatedAt.After(found.UpdatedAt) {
			found = d
		}
	}
	if found == nil {
		return model.UserDevice{}, store.ErrNotFound
	}
	return s.enrichUserDevice(*found), nil
}

// GetUserDeviceByKey 实现 store.Store。
func (s *Store) GetUserDeviceByKey(ctx context.Context, userID string, platform string, externalID string) (model.UserDevice, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	data, ok := s.users[userID]
	if !ok {
		return model.UserDevice{}, store.ErrNotFound
	}
	d, ok := data.byKey[deviceKey(platform, externalID)]
	if !ok || d.DeletedAt != nil {
		return model.UserDevice{}, store.ErrNotFound
	}
	return s.enrichUserDevice(*d), nil
}

// enrichUserDevice 用 fixtures 中的公共元数据补全型号、厂商、固件版本与兼容性。
// 关联缺失时字段为 nil（JSON null），不产生错误。
func (s *Store) enrichUserDevice(d model.UserDevice) model.UserDevice {
	resolved := ""
	if d.DeviceType != nil {
		resolved = *d.DeviceType
	}
	var public *model.PublicDevice
	if d.MAC != "" {
		if pd, ok := s.publicDevices[d.MAC]; ok {
			public = &pd
		}
	}
	if resolved == "" && public != nil {
		resolved = public.DeviceType
	}
	if resolved == "" {
		d.DeviceType = nil
	} else {
		dt := resolved
		d.DeviceType = &dt
	}

	var typeVendor string
	var typeCompatibility json.RawMessage
	for _, dt := range s.deviceTypes {
		if dt.Type == resolved {
			typeVendor = dt.Vendor
			typeCompatibility = dt.Compatibility
			break
		}
	}

	d.Vendor = nil
	d.FirmwareVersion = nil
	d.Compatibility = nil
	if public != nil {
		if public.Vendor != "" {
			v := public.Vendor
			d.Vendor = &v
		}
		if public.FirmwareVersion != "" {
			v := public.FirmwareVersion
			d.FirmwareVersion = &v
		}
		if !emptyJSON(public.Compatibility) {
			d.Compatibility = public.Compatibility
		}
	}
	if d.Vendor == nil && typeVendor != "" {
		v := typeVendor
		d.Vendor = &v
	}
	if d.Compatibility == nil && !emptyJSON(typeCompatibility) {
		d.Compatibility = typeCompatibility
	}
	return d
}

// emptyJSON 判断兼容性字段是否为空。
func emptyJSON(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) == 0 || bytes.Equal(trimmed, []byte("{}")) ||
		bytes.Equal(trimmed, []byte("[]")) || bytes.Equal(trimmed, []byte("null"))
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
		// 存储层再次规范化，避免直接调用 Store 时写入空唯一键。
		platform, externalID, mac, err := validate.ResolveDeviceKey(dev.Platform, dev.ExternalID, dev.MAC)
		if err != nil {
			return model.PushResult{}, false, fmt.Errorf("设备键非法: %w", err)
		}
		dev.Platform, dev.ExternalID, dev.MAC = platform, externalID, mac
		dev.DeviceType = strings.ToLower(strings.TrimSpace(dev.DeviceType))
		if dev.DeviceType == "" && dev.MAC != "" {
			if pd, ok := s.publicDevices[dev.MAC]; ok {
				dev.DeviceType = pd.DeviceType
			}
		}

		key := deviceKey(dev.Platform, dev.ExternalID)
		existingByID := data.byID[dev.ID]
		keyHolder := data.byKey[key]
		if existingByID != nil && keyHolder != nil && keyHolder.ID != existingByID.ID {
			server := s.enrichUserDevice(*keyHolder)
			result.Conflicts = append(result.Conflicts, model.Conflict{
				ID: dev.ID, Platform: dev.Platform, ExternalID: dev.ExternalID, MAC: dev.MAC,
				Reason: "key_taken", Server: &server, Client: &dev,
			})
			continue
		}

		existing := existingByID
		if existing == nil {
			existing = keyHolder
		}
		// 客户端未上报型号且公共库无关联时，沿用已存型号，避免更新把字段清空。
		if dev.DeviceType == "" && existing != nil && existing.DeviceType != nil {
			dev.DeviceType = *existing.DeviceType
		}
		// alias / advertisedName / group 缺省表示未上报，保持原值；空串表示显式清除。
		alias, advertisedName, group := dev.ResolvePresence(existing)

		decision := lww.Decide(now, dev, existing)
		if !decision.Apply && !decision.Noop {
			server := s.enrichUserDevice(*existing)
			result.Conflicts = append(result.Conflicts, model.Conflict{
				ID: dev.ID, Platform: dev.Platform, ExternalID: dev.ExternalID, MAC: dev.MAC,
				Reason: decision.Reason, Server: &server, Client: &dev,
			})
			continue
		}
		if decision.Noop {
			result.Applied = append(result.Applied, model.AppliedChange{
				ID: dev.ID, Platform: dev.Platform, ExternalID: dev.ExternalID, MAC: dev.MAC,
				Status: "unchanged", ServerSeq: existing.ServerSeq,
			})
			continue
		}

		seq := s.nextSeq
		s.nextSeq++
		record := &model.UserDevice{
			ID:             dev.ID,
			ClientID:       req.ClientID,
			Platform:       dev.Platform,
			ExternalID:     dev.ExternalID,
			MAC:            dev.MAC,
			Alias:          alias,
			AdvertisedName: advertisedName,
			Group:          group,
			Rules:          dev.Rules,
			ScanRecords:    dev.ScanRecords,
			UpdatedAt:      decision.EffectiveUpdatedAt,
			ServerSeq:      seq,
		}
		if dev.Deleted {
			deletedAt := decision.EffectiveUpdatedAt
			record.DeletedAt = &deletedAt
		}
		if dev.DeviceType != "" {
			dt := dev.DeviceType
			record.DeviceType = &dt
		}
		*record = s.enrichUserDevice(*record)

		if existing != nil {
			delete(data.byID, existing.ID)
			delete(data.byKey, deviceKey(existing.Platform, existing.ExternalID))
		}
		data.byID[record.ID] = record
		data.byKey[key] = record

		op := "upsert"
		if dev.Deleted {
			op = "delete"
		}
		entityID := userID + ":" + dev.Platform + ":" + dev.ExternalID
		payload, err := json.Marshal(record)
		if err != nil {
			return model.PushResult{}, false, fmt.Errorf("序列化变更记录失败: %w", err)
		}
		s.changes = append(s.changes, changeRecord{
			seq: seq, userID: userID, entity: "user_device", entityID: entityID,
			op: op, payload: payload, serverTS: now,
		})
		result.Applied = append(result.Applied, model.AppliedChange{
			ID: dev.ID, Platform: dev.Platform, ExternalID: dev.ExternalID, MAC: dev.MAC,
			Status: "applied", ServerSeq: seq,
		})
	}

	if len(req.Readings) > 0 {
		if err := s.pushReadingsLocked(data, req.Readings, now, &result); err != nil {
			return model.PushResult{}, false, err
		}
	}

	result.NextCursor = s.maxSeqLocked(userID)
	if opts.IdempotencyKey != "" {
		s.idempotency[userID+"|"+opts.IdempotencyKey] = idempotencyRecord{
			RequestHash: opts.RequestHash,
			Response:    cloneResult(result),
			CreatedAt:   now,
		}
	}
	return result, false, nil
}

// Changes 实现 store.Store。
func (s *Store) Changes(ctx context.Context, userID string, cursor int64, limit int) (model.ChangesPage, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	page := model.ChangesPage{Changes: []model.Change{}}
	var minSeq int64
	for _, rec := range s.changes {
		if rec.userID == userID && (minSeq == 0 || rec.seq < minSeq) {
			minSeq = rec.seq
		}
	}
	// 与 postgres 语义一致：cursor 早于最老保留 seq 减一表示中间存在被清理的变更。
	// cursor=0 表示客户端从未同步过，从头拉取剩余变更不构成数据缺口。
	if cursor > 0 && minSeq > 0 && cursor < minSeq-1 {
		return model.ChangesPage{}, store.ErrCursorExpired
	}
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
