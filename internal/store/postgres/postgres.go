// Package postgres 实现基于 PostgreSQL 的存储与同步写入。
package postgres

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/platojobs/PJGoIoT/internal/lww"
	"github.com/platojobs/PJGoIoT/internal/model"
	"github.com/platojobs/PJGoIoT/internal/store"
	"github.com/platojobs/PJGoIoT/internal/store/postgres/sqlcgen"
	"github.com/platojobs/PJGoIoT/internal/validate"
)

// Store 是 PostgreSQL 实现。查询由 sqlc 生成，Push 在单事务内完成。
type Store struct {
	db *sql.DB
	q  *sqlcgen.Queries
}

// New 打开连接并验证可用性。
func New(ctx context.Context, dsn string) (*Store, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(10)
	db.SetConnMaxLifetime(30 * time.Minute)

	pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		db.Close()
		return nil, fmt.Errorf("连接数据库失败: %w", err)
	}
	return &Store{db: db, q: sqlcgen.New(db)}, nil
}

// Backend 实现 store.Store。
func (s *Store) Backend() string { return "postgres" }

// Ping 实现 store.Store。
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// Close 实现 store.Store。
func (s *Store) Close() error { return s.db.Close() }

// GetPublicDevice 实现 store.Store。
func (s *Store) GetPublicDevice(ctx context.Context, mac string) (model.PublicDevice, error) {
	row, err := s.q.GetPublicDevice(ctx, mac)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.PublicDevice{}, store.ErrNotFound
		}
		return model.PublicDevice{}, fmt.Errorf("查询公共设备失败: %w", err)
	}
	services, err := s.q.ListGATTServicesByDeviceType(ctx, row.DeviceType)
	if err != nil {
		return model.PublicDevice{}, fmt.Errorf("查询 GATT 服务失败: %w", err)
	}
	return model.PublicDevice{
		MAC:             row.Mac,
		Vendor:          row.Vendor,
		Model:           row.Model,
		DeviceType:      row.DeviceType,
		FirmwareVersion: row.FirmwareVersion,
		GATTServices:    mapGATTServices(services),
		Compatibility:   row.Compatibility,
	}, nil
}

// ListDeviceTypes 实现 store.Store。
func (s *Store) ListDeviceTypes(ctx context.Context) ([]model.DeviceType, error) {
	rows, err := s.q.ListDeviceTypes(ctx)
	if err != nil {
		return nil, fmt.Errorf("查询设备类型失败: %w", err)
	}
	allServices, err := s.q.ListAllGATTServices(ctx)
	if err != nil {
		return nil, fmt.Errorf("查询 GATT 服务失败: %w", err)
	}
	grouped := map[string][]model.GATTService{}
	for _, svc := range allServices {
		grouped[svc.DeviceType] = append(grouped[svc.DeviceType], model.GATTService{
			UUID: svc.Uuid, Name: svc.Name,
		})
	}
	out := make([]model.DeviceType, 0, len(rows))
	for _, row := range rows {
		services := grouped[row.Type]
		if services == nil {
			services = []model.GATTService{}
		}
		out = append(out, model.DeviceType{
			Type: row.Type, Name: row.Name, Vendor: row.Vendor,
			Description: row.Description, GATTServices: services,
			Compatibility: row.Compatibility,
		})
	}
	return out, nil
}

// GetDeviceType 实现 store.Store。供无法获取 MAC 的 iOS 端按型号查询公共元数据。
func (s *Store) GetDeviceType(ctx context.Context, deviceType string) (model.DeviceType, error) {
	row, err := s.q.GetDeviceType(ctx, deviceType)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.DeviceType{}, store.ErrNotFound
		}
		return model.DeviceType{}, fmt.Errorf("查询设备类型失败: %w", err)
	}
	services, err := s.q.ListGATTServicesByDeviceType(ctx, deviceType)
	if err != nil {
		return model.DeviceType{}, fmt.Errorf("查询 GATT 服务失败: %w", err)
	}
	return model.DeviceType{
		Type: row.Type, Name: row.Name, Vendor: row.Vendor,
		Description: row.Description, GATTServices: mapGATTServices(services),
		Compatibility: row.Compatibility,
	}, nil
}

// DeviceTypeExists 实现 store.Store。
func (s *Store) DeviceTypeExists(ctx context.Context, deviceType string) (bool, error) {
	ok, err := s.q.DeviceTypeExists(ctx, deviceType)
	if err != nil {
		return false, fmt.Errorf("查询设备类型失败: %w", err)
	}
	return ok, nil
}

// ListFirmwares 实现 store.Store。
func (s *Store) ListFirmwares(ctx context.Context, deviceType string) ([]model.Firmware, error) {
	rows, err := s.q.ListFirmwaresByDeviceType(ctx, deviceType)
	if err != nil {
		return nil, fmt.Errorf("查询固件列表失败: %w", err)
	}
	out := make([]model.Firmware, 0, len(rows))
	for _, row := range rows {
		out = append(out, model.Firmware{
			ID: row.ID, DeviceType: row.DeviceType, Version: row.Version,
			Vendor: row.Vendor, ReleaseNotes: row.ReleaseNotes, ReleasedAt: row.ReleasedAt,
			Compatibility: row.Compatibility,
		})
	}
	return out, nil
}

// ListAccessories 实现 store.Store。
func (s *Store) ListAccessories(ctx context.Context, deviceType string) ([]model.Accessory, error) {
	rows, err := s.q.ListAccessoriesByDeviceType(ctx, deviceType)
	if err != nil {
		return nil, fmt.Errorf("查询配件列表失败: %w", err)
	}
	out := make([]model.Accessory, 0, len(rows))
	for _, row := range rows {
		out = append(out, model.Accessory{
			ID: row.ID, DeviceType: row.DeviceType, Name: row.Name,
			SKU: row.Sku, Vendor: row.Vendor, Description: row.Description,
			Compatibility: row.Compatibility,
		})
	}
	return out, nil
}

// CreateUserWithToken 实现 store.Store。
func (s *Store) CreateUserWithToken(ctx context.Context, tokenHash string) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	q := s.q.WithTx(tx)
	userID, err := q.CreateUser(ctx)
	if err != nil {
		return "", fmt.Errorf("创建用户失败: %w", err)
	}
	if err := q.CreateToken(ctx, sqlcgen.CreateTokenParams{UserID: userID, TokenHash: tokenHash}); err != nil {
		return "", fmt.Errorf("写入 token 失败: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("提交事务失败: %w", err)
	}
	return userID.String(), nil
}

// UserIDByTokenHash 实现 store.Store。
func (s *Store) UserIDByTokenHash(ctx context.Context, tokenHash string) (string, error) {
	userID, err := s.q.GetUserIDByTokenHash(ctx, tokenHash)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", store.ErrUnauthorized
		}
		return "", fmt.Errorf("查询 token 失败: %w", err)
	}
	return userID.String(), nil
}

// RevokeToken 实现 store.Store。
func (s *Store) RevokeToken(ctx context.Context, tokenHash string) error {
	if _, err := s.q.RevokeToken(ctx, tokenHash); err != nil {
		return fmt.Errorf("吊销 token 失败: %w", err)
	}
	return nil
}

// ListUserDevices 实现 store.Store。
func (s *Store) ListUserDevices(ctx context.Context, userID string) ([]model.UserDevice, error) {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return nil, fmt.Errorf("非法 user id: %w", err)
	}
	rows, err := s.q.ListUserDevices(ctx, uid)
	if err != nil {
		return nil, fmt.Errorf("查询用户设备失败: %w", err)
	}
	out := make([]model.UserDevice, 0, len(rows))
	for _, row := range rows {
		out = append(out, s.enrichUserDevice(ctx, s.q, mapUserDevice(row)))
	}
	return out, nil
}

// GetUserDevice 实现 store.Store。
func (s *Store) GetUserDevice(ctx context.Context, userID string, mac string) (model.UserDevice, error) {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return model.UserDevice{}, fmt.Errorf("非法 user id: %w", err)
	}
	row, err := s.q.GetUserDeviceByMAC(ctx, sqlcgen.GetUserDeviceByMACParams{UserID: uid, Mac: nullableString(mac)})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.UserDevice{}, store.ErrNotFound
		}
		return model.UserDevice{}, fmt.Errorf("查询用户设备失败: %w", err)
	}
	return s.enrichUserDevice(ctx, s.q, mapUserDevice(row)), nil
}

// GetUserDeviceByKey 实现 store.Store。
func (s *Store) GetUserDeviceByKey(ctx context.Context, userID string, platform string, externalID string) (model.UserDevice, error) {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return model.UserDevice{}, fmt.Errorf("非法 user id: %w", err)
	}
	row, err := s.q.GetUserDeviceByKey(ctx, sqlcgen.GetUserDeviceByKeyParams{
		UserID: uid, Platform: platform, ExternalID: externalID,
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.UserDevice{}, store.ErrNotFound
		}
		return model.UserDevice{}, fmt.Errorf("查询用户设备失败: %w", err)
	}
	return s.enrichUserDevice(ctx, s.q, mapUserDevice(row)), nil
}

// Push 实现 store.Store。所有设备、变更日志与幂等记录在同一事务内写入。
func (s *Store) Push(ctx context.Context, userID string, req model.PushRequest, opts store.PushOptions) (model.PushResult, bool, error) {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return model.PushResult{}, false, fmt.Errorf("非法 user id: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.PushResult{}, false, fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()
	q := s.q.WithTx(tx)

	if opts.IdempotencyKey != "" {
		// 对同一 (user, key) 加事务级咨询锁，避免并发重复写入。
		if err := q.LockIdempotencyKey(ctx, userID+"|"+opts.IdempotencyKey); err != nil {
			return model.PushResult{}, false, fmt.Errorf("锁定幂等键失败: %w", err)
		}
		rec, err := q.GetIdempotencyKey(ctx, sqlcgen.GetIdempotencyKeyParams{UserID: uid, Key: opts.IdempotencyKey})
		if err == nil {
			if rec.RequestHash != opts.RequestHash {
				return model.PushResult{}, false, store.ErrIdempotencyConflict
			}
			var cached model.PushResult
			if err := json.Unmarshal(rec.Response, &cached); err != nil {
				return model.PushResult{}, false, fmt.Errorf("解析幂等响应失败: %w", err)
			}
			normalizeResult(&cached)
			if err := tx.Commit(); err != nil {
				return model.PushResult{}, false, fmt.Errorf("提交事务失败: %w", err)
			}
			return cached, true, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return model.PushResult{}, false, fmt.Errorf("查询幂等键失败: %w", err)
		}
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
		// 客户端未上报型号时，按 mac 关联公共库推导，保证旧客户端也能拿到元数据。
		if dev.DeviceType == "" && dev.MAC != "" {
			if pd, lookupErr := q.GetPublicDevice(ctx, dev.MAC); lookupErr == nil && pd.DeviceType != "" {
				dev.DeviceType = pd.DeviceType
			}
		}

		devID, err := uuid.Parse(dev.ID)
		if err != nil {
			return model.PushResult{}, false, fmt.Errorf("非法设备 id %q: %w", dev.ID, err)
		}
		existing, keyHolder, err := findExisting(ctx, q, uid, devID, dev.Platform, dev.ExternalID)
		if err != nil {
			return model.PushResult{}, false, err
		}
		// 复合键已存在但客户端 id 不同：视为同一设备的新 id（upsert），沿用已有记录。
		if existing == nil && keyHolder != nil {
			existing, keyHolder = keyHolder, nil
		}
		// 客户端未上报型号且公共库无关联时，沿用已存型号，避免更新把字段清空。
		if dev.DeviceType == "" && existing != nil && existing.DeviceType != nil {
			dev.DeviceType = *existing.DeviceType
		}
		// alias / advertisedName / group 缺省表示未上报，保持原值；空串表示显式清除。
		alias, advertisedName, group := dev.ResolvePresence(existing)
		if existing != nil && keyHolder != nil && existing.ID != keyHolder.ID {
			result.Conflicts = append(result.Conflicts, model.Conflict{
				ID: dev.ID, Platform: dev.Platform, ExternalID: dev.ExternalID, MAC: dev.MAC,
				Reason: "key_taken", Server: *keyHolder, Client: dev,
			})
			continue
		}

		decision := lww.Decide(now, dev, existing)
		if !decision.Apply && !decision.Noop {
			result.Conflicts = append(result.Conflicts, model.Conflict{
				ID: dev.ID, Platform: dev.Platform, ExternalID: dev.ExternalID, MAC: dev.MAC,
				Reason: decision.Reason, Server: *existing, Client: dev,
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

		seq, err := q.NextChangeSeq(ctx)
		if err != nil {
			return model.PushResult{}, false, fmt.Errorf("分配变更序列失败: %w", err)
		}
		record := model.UserDevice{
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
		var deletedAt sql.NullTime
		if dev.Deleted {
			record.DeletedAt = &decision.EffectiveUpdatedAt
			deletedAt = sql.NullTime{Time: decision.EffectiveUpdatedAt, Valid: true}
		}
		if dev.DeviceType != "" {
			dt := dev.DeviceType
			record.DeviceType = &dt
		}
		// 补全 vendor / firmwareVersion / compatibility；关联缺失时保持 null。
		record = s.enrichUserDevice(ctx, q, record)

		if existing == nil {
			_, err = q.InsertUserDevice(ctx, sqlcgen.InsertUserDeviceParams{
				UserID: uid, ID: devID, ClientID: req.ClientID,
				Platform: dev.Platform, ExternalID: dev.ExternalID, Mac: nullableString(dev.MAC),
				Alias: alias, AdvertisedName: advertisedName, GroupName: group, Rules: dev.Rules,
				ScanRecords: dev.ScanRecords, UpdatedAt: decision.EffectiveUpdatedAt,
				DeletedAt: deletedAt, ServerSeq: seq, DeviceType: nullableString(dev.DeviceType),
			})
		} else {
			oldID, parseErr := uuid.Parse(existing.ID)
			if parseErr != nil {
				return model.PushResult{}, false, fmt.Errorf("服务端设备 id 非法: %w", parseErr)
			}
			_, err = q.UpdateUserDevice(ctx, sqlcgen.UpdateUserDeviceParams{
				NewID: devID, ClientID: req.ClientID,
				Platform: dev.Platform, ExternalID: dev.ExternalID, Mac: nullableString(dev.MAC),
				Alias: alias, AdvertisedName: advertisedName, GroupName: group, Rules: dev.Rules,
				ScanRecords: dev.ScanRecords, UpdatedAt: decision.EffectiveUpdatedAt,
				DeletedAt: deletedAt, ServerSeq: seq, DeviceType: nullableString(dev.DeviceType),
				UserID: uid, OldID: oldID,
			})
		}
		if err != nil {
			return model.PushResult{}, false, fmt.Errorf("写入用户设备失败: %w", err)
		}

		payload, err := json.Marshal(record)
		if err != nil {
			return model.PushResult{}, false, fmt.Errorf("序列化变更记录失败: %w", err)
		}
		op := "upsert"
		if dev.Deleted {
			op = "delete"
		}
		entityID := userID + ":" + dev.Platform + ":" + dev.ExternalID
		if err := q.InsertChangeLog(ctx, sqlcgen.InsertChangeLogParams{
			Seq: seq, UserID: uid, Entity: "user_device", EntityID: entityID,
			Op: op, Payload: payload, ServerTs: now,
		}); err != nil {
			return model.PushResult{}, false, fmt.Errorf("写入变更日志失败: %w", err)
		}

		result.Applied = append(result.Applied, model.AppliedChange{
			ID: dev.ID, Platform: dev.Platform, ExternalID: dev.ExternalID, MAC: dev.MAC,
			Status: "applied", ServerSeq: seq,
		})
	}

	next, err := q.GetMaxSeqForUser(ctx, uid)
	if err != nil {
		return model.PushResult{}, false, fmt.Errorf("查询同步游标失败: %w", err)
	}
	result.NextCursor = next

	if opts.IdempotencyKey != "" {
		raw, err := json.Marshal(result)
		if err != nil {
			return model.PushResult{}, false, fmt.Errorf("序列化幂等响应失败: %w", err)
		}
		if err := q.InsertIdempotencyKey(ctx, sqlcgen.InsertIdempotencyKeyParams{
			UserID: uid, Key: opts.IdempotencyKey, RequestHash: opts.RequestHash, Response: raw,
		}); err != nil {
			return model.PushResult{}, false, fmt.Errorf("写入幂等键失败: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return model.PushResult{}, false, fmt.Errorf("提交事务失败: %w", err)
	}
	return result, false, nil
}

// Changes 实现 store.Store。
func (s *Store) Changes(ctx context.Context, userID string, cursor int64, limit int) (model.ChangesPage, error) {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return model.ChangesPage{}, fmt.Errorf("非法 user id: %w", err)
	}
	// 多取一条用于判断 hasMore。
	rows, err := s.q.ListChanges(ctx, sqlcgen.ListChangesParams{
		UserID: uid, Seq: cursor, Limit: int32(limit + 1),
	})
	if err != nil {
		return model.ChangesPage{}, fmt.Errorf("查询增量变更失败: %w", err)
	}
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	page := model.ChangesPage{Changes: make([]model.Change, 0, len(rows)), NextCursor: cursor, HasMore: hasMore}
	for _, row := range rows {
		page.Changes = append(page.Changes, model.Change{
			Seq: row.Seq, Entity: row.Entity, EntityID: row.EntityID,
			Op: row.Op, Payload: row.Payload, ServerTS: row.ServerTs,
		})
		page.NextCursor = row.Seq
	}
	return page, nil
}

// findExisting 按客户端 id 与复合键 (platform, externalId) 查找已有记录。
// 返回 (按 id 命中的记录, 按复合键命中的其他记录, error)；两个结果可能同时存在，
// 调用方据此判断是否发生 key_taken 冲突。
func findExisting(ctx context.Context, q *sqlcgen.Queries, uid uuid.UUID, devID uuid.UUID, platform, externalID string) (*model.UserDevice, *model.UserDevice, error) {
	row, err := q.GetUserDeviceByIDForUpdate(ctx, sqlcgen.GetUserDeviceByIDForUpdateParams{UserID: uid, ID: devID})
	if err == nil {
		existing := mapUserDevice(row)
		if row.Platform == platform && row.ExternalID == externalID {
			return &existing, nil, nil
		}
		// id 相同但唯一键变化：需要确认新键是否已被同用户的其他记录占用。
		keyRow, err := q.GetUserDeviceByKeyForUpdate(ctx, sqlcgen.GetUserDeviceByKeyForUpdateParams{
			UserID: uid, Platform: platform, ExternalID: externalID,
		})
		switch {
		case err == nil:
			holder := mapUserDevice(keyRow)
			return &existing, &holder, nil
		case errors.Is(err, sql.ErrNoRows):
			return &existing, nil, nil
		default:
			return nil, nil, fmt.Errorf("查询用户设备失败: %w", err)
		}
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, nil, fmt.Errorf("查询用户设备失败: %w", err)
	}

	keyRow, err := q.GetUserDeviceByKeyForUpdate(ctx, sqlcgen.GetUserDeviceByKeyForUpdateParams{
		UserID: uid, Platform: platform, ExternalID: externalID,
	})
	if err == nil {
		existing := mapUserDevice(keyRow)
		return nil, &existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, nil, fmt.Errorf("查询用户设备失败: %w", err)
	}
	return nil, nil, nil
}

func nullableString(v string) sql.NullString {
	if v == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: v, Valid: true}
}

func mapUserDevice(row sqlcgen.UserDevice) model.UserDevice {
	d := model.UserDevice{
		ID:             row.ID.String(),
		ClientID:       row.ClientID,
		Platform:       row.Platform,
		ExternalID:     row.ExternalID,
		MAC:            row.Mac.String,
		Alias:          row.Alias,
		AdvertisedName: row.AdvertisedName,
		Group:          row.Group,
		Rules:          row.Rules,
		ScanRecords:    row.ScanRecords,
		UpdatedAt:      row.UpdatedAt.UTC(),
		ServerSeq:      row.ServerSeq,
	}
	if row.DeletedAt.Valid {
		t := row.DeletedAt.Time.UTC()
		d.DeletedAt = &t
	}
	if row.DeviceType.Valid && strings.TrimSpace(row.DeviceType.String) != "" {
		dt := strings.TrimSpace(row.DeviceType.String)
		d.DeviceType = &dt
	}
	return d
}

// enrichUserDevice 用公共元数据补全型号、厂商、固件版本与兼容性。
// 关联缺失（未注册型号、无 mac、无公共记录）时字段保持 nil，JSON 输出 null，不返回错误。
func (s *Store) enrichUserDevice(ctx context.Context, q *sqlcgen.Queries, d model.UserDevice) model.UserDevice {
	resolved := ""
	if d.DeviceType != nil {
		resolved = *d.DeviceType
	}

	var public *model.PublicDevice
	if d.MAC != "" {
		if row, err := q.GetPublicDevice(ctx, d.MAC); err == nil {
			public = &model.PublicDevice{
				MAC: row.Mac, Vendor: row.Vendor, Model: row.Model, DeviceType: row.DeviceType,
				FirmwareVersion: row.FirmwareVersion, Compatibility: row.Compatibility,
			}
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
	if resolved != "" {
		if row, err := q.GetDeviceType(ctx, resolved); err == nil {
			typeVendor = row.Vendor
			typeCompatibility = row.Compatibility
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

// emptyJSON 判断兼容性字段是否为空（NULL、{}、[] 或 null 视为无数据）。
func emptyJSON(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) == 0 || bytes.Equal(trimmed, []byte("{}")) ||
		bytes.Equal(trimmed, []byte("[]")) || bytes.Equal(trimmed, []byte("null"))
}

func mapGATTServices(rows []sqlcgen.ListGATTServicesByDeviceTypeRow) []model.GATTService {
	out := make([]model.GATTService, 0, len(rows))
	for _, row := range rows {
		out = append(out, model.GATTService{UUID: row.Uuid, Name: row.Name})
	}
	return out
}

func normalizeResult(result *model.PushResult) {
	if result.Applied == nil {
		result.Applied = []model.AppliedChange{}
	}
	if result.Conflicts == nil {
		result.Conflicts = []model.Conflict{}
	}
}
