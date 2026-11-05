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
func (s *Store) CreateUserWithToken(ctx context.Context, tokenHash string, expiresAt time.Time) (string, error) {
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
	if err := q.CreateToken(ctx, sqlcgen.CreateTokenParams{UserID: userID, TokenHash: tokenHash, ExpiresAt: expiresAt.UTC()}); err != nil {
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

// dbQueryTimeout 是 API 查询的默认数据库超时预算。
// 超过后由 pgx 取消查询，HTTP 层映射为 503 TIMEOUT，避免请求无限挂起。
const dbQueryTimeout = 3 * time.Second

func queryTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, dbQueryTimeout)
}

// ListUserDevices 实现 store.Store。
// 用一条带 LEFT JOIN 的 SQL 同时取出设备与公共元数据，避免逐设备查询造成 N+1。
func (s *Store) ListUserDevices(ctx context.Context, userID string) ([]model.UserDevice, error) {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return nil, fmt.Errorf("非法 user id: %w", err)
	}
	ctx, cancel := queryTimeout(ctx)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, listUserDevicesEnrichedSQL, uid)
	if err != nil {
		return nil, fmt.Errorf("查询用户设备失败: %w", err)
	}
	defer rows.Close()

	out := []model.UserDevice{}
	for rows.Next() {
		var (
			d             model.UserDevice
			mac           sql.NullString
			deviceType    sql.NullString
			vendor        sql.NullString
			firmware      sql.NullString
			compatibility []byte
		)
		if err := rows.Scan(&d.ID, &d.ClientID, &d.Platform, &d.ExternalID, &mac, &d.Alias,
			&d.AdvertisedName, &d.Group, &d.Rules, &d.ScanRecords, &d.UpdatedAt, &d.ServerSeq,
			&deviceType, &vendor, &firmware, &compatibility); err != nil {
			return nil, fmt.Errorf("解析用户设备失败: %w", err)
		}
		if mac.Valid {
			d.MAC = mac.String
		}
		if deviceType.Valid && deviceType.String != "" {
			v := deviceType.String
			d.DeviceType = &v
		}
		if vendor.Valid && vendor.String != "" {
			v := vendor.String
			d.Vendor = &v
		}
		if firmware.Valid && firmware.String != "" {
			v := firmware.String
			d.FirmwareVersion = &v
		}
		if len(compatibility) > 0 && !emptyJSON(json.RawMessage(compatibility)) {
			d.Compatibility = json.RawMessage(compatibility)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历用户设备失败: %w", err)
	}
	return out, nil
}

// listUserDevicesEnrichedSQL 在设备行上直接关联公共设备与设备类型。
// 型号解析顺序与 enrichUserDevice 一致：用户记录显式型号优先，其次 public_devices。
const listUserDevicesEnrichedSQL = `
SELECT d.id, d.client_id, d.platform, d.external_id, d.mac, d.alias, d.advertised_name, d."group",
       d.rules, d.scan_records, d.updated_at, d.server_seq,
       COALESCE(NULLIF(d.device_type, ''), pd.device_type) AS resolved_type,
       COALESCE(NULLIF(pd.vendor, ''), dt.vendor) AS resolved_vendor,
       pd.firmware_version,
       CASE
         WHEN pd.compatibility IS NOT NULL AND pd.compatibility <> '{}'::jsonb THEN pd.compatibility
         WHEN dt.compatibility IS NOT NULL THEN dt.compatibility
       END AS resolved_compatibility
FROM user_devices d
LEFT JOIN public_devices pd ON pd.mac = d.mac
LEFT JOIN device_types dt ON dt.type = COALESCE(NULLIF(d.device_type, ''), pd.device_type)
WHERE d.user_id = $1 AND d.deleted_at IS NULL
ORDER BY d.updated_at DESC, d.id`

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

	// 第一遍：规范化设备键，并批量加载公共元数据与已存设备，避免逐设备查询。
	devices := make([]model.PushDevice, len(req.Devices))
	macSet := map[string]struct{}{}
	for i, dev := range req.Devices {
		platform, externalID, mac, err := validate.ResolveDeviceKey(dev.Platform, dev.ExternalID, dev.MAC)
		if err != nil {
			return model.PushResult{}, false, fmt.Errorf("设备键非法: %w", err)
		}
		dev.Platform, dev.ExternalID, dev.MAC = platform, externalID, mac
		dev.DeviceType = strings.ToLower(strings.TrimSpace(dev.DeviceType))
		devices[i] = dev
		if dev.MAC != "" {
			macSet[dev.MAC] = struct{}{}
		}
	}
	public, err := loadPublicDevices(ctx, tx, setToSlice(macSet))
	if err != nil {
		return model.PushResult{}, false, err
	}
	// 客户端未上报型号时按 mac 关联公共库推导，保证旧客户端也能拿到元数据。
	for i := range devices {
		if devices[i].DeviceType == "" && devices[i].MAC != "" {
			if pd, ok := public[devices[i].MAC]; ok {
				devices[i].DeviceType = pd.DeviceType
			}
		}
	}
	existingByID, existingByKey, err := loadExistingDevices(ctx, tx, uid, devices)
	if err != nil {
		return model.PushResult{}, false, err
	}

	pending := make([]pushPending, 0, len(devices))
	for _, dev := range devices {
		devID, err := uuid.Parse(dev.ID)
		if err != nil {
			return model.PushResult{}, false, fmt.Errorf("非法设备 id %q: %w", dev.ID, err)
		}
		var existing, keyHolder *model.UserDevice
		if rec, ok := existingByID[devID.String()]; ok {
			existing = rec
			if rec.Platform != dev.Platform || rec.ExternalID != dev.ExternalID {
				keyHolder = existingByKey[dev.Platform+"|"+dev.ExternalID]
			}
		} else {
			keyHolder = existingByKey[dev.Platform+"|"+dev.ExternalID]
		}
		// 复合键已存在但客户端 id 不同：视为同一设备的新 id（upsert），沿用已有记录。
		if existing == nil && keyHolder != nil {
			existing, keyHolder = keyHolder, nil
		}
		// 客户端未上报型号且公共库无关联时，沿用已存型号，避免更新把字段清空。
		if dev.DeviceType == "" && existing != nil && existing.DeviceType != nil {
			dev.DeviceType = *existing.DeviceType
		}
		alias, advertisedName, group := dev.ResolvePresence(existing)
		if existing != nil && keyHolder != nil && existing.ID != keyHolder.ID {
			result.Conflicts = append(result.Conflicts, model.Conflict{
				ID: dev.ID, Platform: dev.Platform, ExternalID: dev.ExternalID, MAC: dev.MAC,
				Reason: "key_taken", Server: keyHolder, Client: &dev,
			})
			continue
		}

		decision := lww.Decide(now, dev, existing)
		if !decision.Apply && !decision.Noop {
			result.Conflicts = append(result.Conflicts, model.Conflict{
				ID: dev.ID, Platform: dev.Platform, ExternalID: dev.ExternalID, MAC: dev.MAC,
				Reason: decision.Reason, Server: existing, Client: &dev,
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
		op := "upsert"
		if dev.Deleted {
			op = "delete"
		}
		result.Applied = append(result.Applied, model.AppliedChange{
			ID: dev.ID, Platform: dev.Platform, ExternalID: dev.ExternalID, MAC: dev.MAC,
			Status: "applied",
		})
		pending = append(pending, pushPending{
			dev:       dev,
			record:    record,
			existing:  existing,
			op:        op,
			entityID:  userID + ":" + dev.Platform + ":" + dev.ExternalID,
			deletedAt: deletedAt,
			resultIdx: len(result.Applied) - 1,
		})
	}

	enrich, err := loadPushEnrichment(ctx, tx, public, pending)
	if err != nil {
		return model.PushResult{}, false, err
	}
	seqs, err := allocateChangeSeqs(ctx, tx, len(pending))
	if err != nil {
		return model.PushResult{}, false, fmt.Errorf("分配变更序列失败: %w", err)
	}
	if err := writePendingDevices(ctx, tx, uid, req.ClientID, pending, seqs, enrich, &result); err != nil {
		return model.PushResult{}, false, err
	}

	if len(req.Readings) > 0 {
		if err := pushReadingsTx(ctx, tx, uid, req.Readings, now, &result, q); err != nil {
			return model.PushResult{}, false, err
		}
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

// pushPending 表示一条已确定要写入的变更，等待批量分配 seq 与写入。
type pushPending struct {
	dev       model.PushDevice
	record    model.UserDevice
	existing  *model.UserDevice
	op        string
	entityID  string
	deletedAt sql.NullTime
	resultIdx int
}

// pushEnrichment 是一次 push 内批量加载的公共元数据。
type pushEnrichment struct {
	public map[string]model.PublicDevice
	types  map[string]model.DeviceType
}

// queryer 抽象 *sql.DB 与 *sql.Tx 的批量查询能力。
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func setToSlice(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	return out
}

// buildIN 生成从 start 开始的 n 个占位符，返回占位符列表与下一个参数序号。
func buildIN(start, n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "$%d", start+i)
	}
	return b.String()
}

func loadPublicDevices(ctx context.Context, db queryer, macs []string) (map[string]model.PublicDevice, error) {
	out := map[string]model.PublicDevice{}
	if len(macs) == 0 {
		return out, nil
	}
	args := make([]any, len(macs))
	for i, m := range macs {
		args[i] = m
	}
	rows, err := db.QueryContext(ctx, `SELECT mac, vendor, model, device_type, firmware_version, compatibility
		FROM public_devices WHERE mac IN (`+buildIN(1, len(macs))+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("批量查询公共设备失败: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var d model.PublicDevice
		if err := rows.Scan(&d.MAC, &d.Vendor, &d.Model, &d.DeviceType, &d.FirmwareVersion, &d.Compatibility); err != nil {
			return nil, fmt.Errorf("解析公共设备失败: %w", err)
		}
		out[d.MAC] = d
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历公共设备失败: %w", err)
	}
	return out, nil
}

func loadDeviceTypes(ctx context.Context, db queryer, types []string) (map[string]model.DeviceType, error) {
	out := map[string]model.DeviceType{}
	if len(types) == 0 {
		return out, nil
	}
	args := make([]any, len(types))
	for i, t := range types {
		args[i] = t
	}
	rows, err := db.QueryContext(ctx, `SELECT type, vendor, compatibility
		FROM device_types WHERE type IN (`+buildIN(1, len(types))+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("批量查询设备类型失败: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var d model.DeviceType
		if err := rows.Scan(&d.Type, &d.Vendor, &d.Compatibility); err != nil {
			return nil, fmt.Errorf("解析设备类型失败: %w", err)
		}
		out[d.Type] = d
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历设备类型失败: %w", err)
	}
	return out, nil
}

// loadExistingDevices 一次查询本批设备可能命中的全部服务端记录（按 id 与唯一键）。
// 不改变既有 upsert 语义：id 命中优先，其次唯一键命中。
func loadExistingDevices(ctx context.Context, db queryer, uid uuid.UUID, devices []model.PushDevice) (map[string]*model.UserDevice, map[string]*model.UserDevice, error) {
	byID := map[string]*model.UserDevice{}
	byKey := map[string]*model.UserDevice{}
	if len(devices) == 0 {
		return byID, byKey, nil
	}
	idSet := map[string]struct{}{}
	extSet := map[string]struct{}{}
	for _, d := range devices {
		idSet[d.ID] = struct{}{}
		extSet[d.ExternalID] = struct{}{}
	}
	ids := setToSlice(idSet)
	exts := setToSlice(extSet)
	args := make([]any, 0, 1+len(ids)+len(exts))
	args = append(args, uid)
	for _, id := range ids {
		args = append(args, id)
	}
	for _, ext := range exts {
		args = append(args, ext)
	}
	query := `SELECT user_id, id, client_id, mac, alias, "group", rules, scan_records, updated_at,
		deleted_at, server_seq, created_at, platform, external_id, device_type, advertised_name
		FROM user_devices
		WHERE user_id = $1 AND (id IN (` + buildIN(2, len(ids)) + `) OR external_id IN (` +
		buildIN(2+len(ids), len(exts)) + `))`
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("批量查询已存设备失败: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var row sqlcgen.UserDevice
		if err := rows.Scan(&row.UserID, &row.ID, &row.ClientID, &row.Mac, &row.Alias, &row.Group,
			&row.Rules, &row.ScanRecords, &row.UpdatedAt, &row.DeletedAt, &row.ServerSeq,
			&row.CreatedAt, &row.Platform, &row.ExternalID, &row.DeviceType, &row.AdvertisedName); err != nil {
			return nil, nil, fmt.Errorf("解析已存设备失败: %w", err)
		}
		d := mapUserDevice(row)
		byID[d.ID] = &d
		byKey[d.Platform+"|"+d.ExternalID] = &d
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("遍历已存设备失败: %w", err)
	}
	return byID, byKey, nil
}

func loadPushEnrichment(ctx context.Context, db queryer, public map[string]model.PublicDevice, pending []pushPending) (pushEnrichment, error) {
	typeSet := map[string]struct{}{}
	for _, p := range pending {
		if p.record.DeviceType != nil && *p.record.DeviceType != "" {
			typeSet[*p.record.DeviceType] = struct{}{}
		}
	}
	for _, pd := range public {
		if pd.DeviceType != "" {
			typeSet[pd.DeviceType] = struct{}{}
		}
	}
	types, err := loadDeviceTypes(ctx, db, setToSlice(typeSet))
	if err != nil {
		return pushEnrichment{}, err
	}
	return pushEnrichment{public: public, types: types}, nil
}

// applyPushEnrichment 用已加载的元数据补全设备，不产生额外查询。
func applyPushEnrichment(d model.UserDevice, e pushEnrichment) model.UserDevice {
	resolved := ""
	if d.DeviceType != nil {
		resolved = *d.DeviceType
	}
	if resolved == "" {
		if pd, ok := e.public[d.MAC]; ok {
			resolved = pd.DeviceType
		}
	}
	if resolved == "" {
		d.DeviceType = nil
	} else {
		dt := resolved
		d.DeviceType = &dt
	}

	d.Vendor = nil
	d.FirmwareVersion = nil
	d.Compatibility = nil
	if public, ok := e.public[d.MAC]; ok {
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
	if dt, ok := e.types[resolved]; ok {
		if d.Vendor == nil && dt.Vendor != "" {
			v := dt.Vendor
			d.Vendor = &v
		}
		if d.Compatibility == nil && !emptyJSON(dt.Compatibility) {
			d.Compatibility = dt.Compatibility
		}
	}
	return d
}

// allocateChangeSeqs 一次分配 n 个变更序列。
func allocateChangeSeqs(ctx context.Context, db queryer, n int) ([]int64, error) {
	if n == 0 {
		return nil, nil
	}
	rows, err := db.QueryContext(ctx,
		`SELECT nextval(pg_get_serial_sequence('change_log', 'seq'))::bigint
		 FROM generate_series(1, $1::int)`, n)
	if err != nil {
		return nil, fmt.Errorf("分配变更序列失败: %w", err)
	}
	defer rows.Close()
	seqs := make([]int64, 0, n)
	for rows.Next() {
		var seq int64
		if err := rows.Scan(&seq); err != nil {
			return nil, fmt.Errorf("解析变更序列失败: %w", err)
		}
		seqs = append(seqs, seq)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历变更序列失败: %w", err)
	}
	return seqs, nil
}

// writePendingDevices 批量写入设备与变更日志，并回填结果中的 serverSeq。
func writePendingDevices(ctx context.Context, db *sql.Tx, uid uuid.UUID, clientID string,
	pending []pushPending, seqs []int64, enrich pushEnrichment, result *model.PushResult) error {
	if len(pending) == 0 {
		return nil
	}
	insertRows := make([][]any, 0, len(pending))
	updateRows := make([][]any, 0, len(pending))
	for i := range pending {
		p := &pending[i]
		p.record.ServerSeq = seqs[i]
		p.record = applyPushEnrichment(p.record, enrich)
		result.Applied[p.resultIdx].ServerSeq = seqs[i]
		devID, err := uuid.Parse(p.dev.ID)
		if err != nil {
			return fmt.Errorf("非法设备 id %q: %w", p.dev.ID, err)
		}
		if p.existing == nil {
			insertRows = append(insertRows, []any{
				uid, devID, clientID, nullableString(p.dev.MAC), p.record.Alias, p.record.Group,
				p.record.Rules, p.record.ScanRecords, p.record.UpdatedAt, p.deletedAt, seqs[i],
				p.dev.Platform, p.dev.ExternalID, nullableString(p.dev.DeviceType), p.record.AdvertisedName,
			})
			continue
		}
		if p.existing.ID == p.dev.ID {
			updateRows = append(updateRows, []any{
				devID, clientID, p.dev.Platform, p.dev.ExternalID, nullableString(p.dev.MAC),
				p.record.Alias, p.record.Group, p.record.Rules, p.record.ScanRecords,
				p.record.UpdatedAt, p.deletedAt, seqs[i], nullableString(p.dev.DeviceType), p.record.AdvertisedName,
			})
			continue
		}
		// 极少见：唯一键命中但客户端换用新 id，沿用逐行更新以改变主键。
		oldID, err := uuid.Parse(p.existing.ID)
		if err != nil {
			return fmt.Errorf("服务端设备 id 非法: %w", err)
		}
		if _, err := db.ExecContext(ctx, `UPDATE user_devices SET
			id = $1, client_id = $2, platform = $3, external_id = $4, mac = $5, alias = $6,
			"group" = $7, rules = $8, scan_records = $9, updated_at = $10, deleted_at = $11,
			server_seq = $12, device_type = $13, advertised_name = $14
			WHERE user_id = $15 AND id = $16`,
			devID, clientID, p.dev.Platform, p.dev.ExternalID, nullableString(p.dev.MAC),
			p.record.Alias, p.record.Group, p.record.Rules, p.record.ScanRecords,
			p.record.UpdatedAt, p.deletedAt, seqs[i], nullableString(p.dev.DeviceType),
			p.record.AdvertisedName, uid, oldID); err != nil {
			return fmt.Errorf("写入用户设备失败: %w", err)
		}
	}
	if err := insertDevicesBatch(ctx, db, insertRows); err != nil {
		return fmt.Errorf("写入用户设备失败: %w", err)
	}
	if err := updateDevicesBatch(ctx, db, uid, updateRows); err != nil {
		return fmt.Errorf("更新用户设备失败: %w", err)
	}

	// 变更日志同样使用一条多行 INSERT。
	logRows := make([][]any, 0, len(pending))
	for i := range pending {
		p := &pending[i]
		payload, err := json.Marshal(p.record)
		if err != nil {
			return fmt.Errorf("序列化变更记录失败: %w", err)
		}
		logRows = append(logRows, []any{seqs[i], uid, "user_device", p.entityID, p.op, payload, time.Now().UTC()})
	}
	if err := insertChangeLogBatch(ctx, db, logRows); err != nil {
		return fmt.Errorf("写入变更日志失败: %w", err)
	}
	return nil
}

const listUserDevicesInsertSQL = `INSERT INTO user_devices
	(user_id, id, client_id, mac, alias, "group", rules, scan_records, updated_at, deleted_at,
	 server_seq, platform, external_id, device_type, advertised_name)
VALUES %s ON CONFLICT (user_id, platform, external_id) DO NOTHING`

const listUserDevicesUpdateSQL = `UPDATE user_devices AS d SET
	platform = v.platform, external_id = v.external_id, mac = v.mac, alias = v.alias,
	"group" = v.group_name, rules = v.rules, scan_records = v.scan_records,
	updated_at = v.updated_at, deleted_at = v.deleted_at, server_seq = v.server_seq,
	device_type = v.device_type, advertised_name = v.advertised_name,
	client_id = v.client_id
FROM (VALUES %s) AS v(id, client_id, platform, external_id, mac, alias, group_name, rules,
	scan_records, updated_at, deleted_at, server_seq, device_type, advertised_name)
WHERE d.user_id = $1 AND d.id = v.id`

const changeLogInsertSQL = `INSERT INTO change_log (seq, user_id, entity, entity_id, op, payload, server_ts)
VALUES %s`

var insertDeviceCasts = []string{"uuid", "uuid", "text", "text", "text", "text", "jsonb", "jsonb", "timestamptz", "timestamptz", "bigint", "text", "text", "text", "text"}

var updateDeviceCasts = []string{"uuid", "text", "text", "text", "text", "text", "text", "jsonb", "jsonb", "timestamptz", "timestamptz", "bigint", "text", "text"}

var changeLogCasts = []string{"bigint", "uuid", "text", "text", "text", "jsonb", "timestamptz"}

func insertDevicesBatch(ctx context.Context, db *sql.Tx, rows [][]any) error {
	if len(rows) == 0 {
		return nil
	}
	values, args := buildBatchValues(1, rows, insertDeviceCasts)
	_, err := db.ExecContext(ctx, fmt.Sprintf(listUserDevicesInsertSQL, values), args...)
	return err
}

func updateDevicesBatch(ctx context.Context, db *sql.Tx, uid uuid.UUID, rows [][]any) error {
	if len(rows) == 0 {
		return nil
	}
	values, args := buildBatchValues(2, rows, updateDeviceCasts)
	args = append([]any{uid}, args...)
	_, err := db.ExecContext(ctx, fmt.Sprintf(listUserDevicesUpdateSQL, values), args...)
	return err
}

func insertChangeLogBatch(ctx context.Context, db *sql.Tx, rows [][]any) error {
	if len(rows) == 0 {
		return nil
	}
	values, args := buildBatchValues(1, rows, changeLogCasts)
	_, err := db.ExecContext(ctx, fmt.Sprintf(changeLogInsertSQL, values), args...)
	return err
}

// buildBatchValues 返回占位符文本与按行展开的参数；start 为参数起始编号，casts 逐列类型。
func buildBatchValues(start int, rows [][]any, casts []string) (string, []any) {
	cols := len(casts)
	parts := make([]string, 0, len(rows))
	args := make([]any, 0, len(rows)*cols)
	for i, row := range rows {
		ph := make([]string, cols)
		for c := 0; c < cols; c++ {
			ph[c] = fmt.Sprintf("$%d::%s", start+i*cols+c, casts[c])
		}
		parts = append(parts, "("+strings.Join(ph, ", ")+")")
		args = append(args, row...)
	}
	return strings.Join(parts, ", "), args
}

// Changes 实现 store.Store。
func (s *Store) Changes(ctx context.Context, userID string, cursor int64, limit int) (model.ChangesPage, error) {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return model.ChangesPage{}, fmt.Errorf("非法 user id: %w", err)
	}
	qctx, cancel := queryTimeout(ctx)
	defer cancel()
	// 保留边界检查：若该用户最早保留的 seq 比 cursor+1 还大，说明中间的行已被清理，
	// 静默返回空页会让客户端永久丢数据，因此返回 ErrCursorExpired。
	// cursor=0 表示客户端从未同步过，此时从头拉取剩余变更即可，不构成数据缺口。
	var minSeq sql.NullInt64
	if err := s.db.QueryRowContext(qctx, `SELECT MIN(seq) FROM change_log WHERE user_id = $1`, uid).Scan(&minSeq); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return model.ChangesPage{}, store.ErrTimeout
		}
		return model.ChangesPage{}, fmt.Errorf("查询变更保留边界失败: %w", err)
	}
	if cursor > 0 && minSeq.Valid && cursor < minSeq.Int64-1 {
		return model.ChangesPage{}, store.ErrCursorExpired
	}
	// 多取一条用于判断 hasMore。
	rows, err := s.q.ListChanges(qctx, sqlcgen.ListChangesParams{
		UserID: uid, Seq: cursor, Limit: int32(limit + 1),
	})
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return model.ChangesPage{}, store.ErrTimeout
		}
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
