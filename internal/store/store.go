// Package store 定义存储抽象。
// fixture 与 postgres 两种实现共享同一套接口，HTTP 层不感知底层存储。
package store

import (
	"context"
	"errors"
	"time"

	"github.com/platojobs/PJGoIoT/internal/model"
)

var (
	// ErrNotFound 用于公共元数据或用户私有资源不存在，HTTP 层统一映射为 404。
	ErrNotFound = errors.New("资源不存在")
	// ErrUnauthorized 表示 token 缺失、无效或已被吊销。
	ErrUnauthorized = errors.New("未认证")
	// ErrIdempotencyConflict 表示同一 Idempotency-Key 被用于不同的请求体。
	ErrIdempotencyConflict = errors.New("Idempotency-Key 对应的请求体不一致")
	// ErrBadCursor 表示分页游标无法解析，HTTP 层映射为 400。
	ErrBadCursor = errors.New("游标不合法")
	// ErrCursorExpired 表示同步游标早于 change_log 保留边界，HTTP 层映射为 410 RESYNC_REQUIRED。
	ErrCursorExpired = errors.New("同步游标已过期")
	// ErrTimeout 表示数据库查询超过服务端超时预算，HTTP 层映射为 503 TIMEOUT。
	ErrTimeout = errors.New("数据库查询超时")
)

// PushOptions 携带同步写入的幂等参数。
type PushOptions struct {
	IdempotencyKey string
	RequestHash    string
}

// Store 是服务端依赖的全部存储能力。
type Store interface {
	// Backend 返回存储类型（fixture/postgres），用于健康检查与日志。
	Backend() string
	// Ping 检查存储可用性。
	Ping(ctx context.Context) error
	// Close 释放资源。
	Close() error

	// GetPublicDevice 按规范化 MAC 查询公共设备元数据。
	GetPublicDevice(ctx context.Context, mac string) (model.PublicDevice, error)
	// ListDeviceTypes 返回全部设备类型。
	ListDeviceTypes(ctx context.Context) ([]model.DeviceType, error)
	// GetDeviceType 按 type 查询设备类型，供无法获取 MAC 的 iOS 端按型号查询。
	GetDeviceType(ctx context.Context, deviceType string) (model.DeviceType, error)
	// DeviceTypeExists 判断设备类型是否存在。
	DeviceTypeExists(ctx context.Context, deviceType string) (bool, error)
	// ListFirmwares 返回指定设备类型的固件列表。
	ListFirmwares(ctx context.Context, deviceType string) ([]model.Firmware, error)
	// ListAccessories 返回指定设备类型的配件列表。
	ListAccessories(ctx context.Context, deviceType string) ([]model.Accessory, error)

	// CreateUserWithToken 创建匿名用户并写入 token 哈希与过期时间，返回用户 id。
	CreateUserWithToken(ctx context.Context, tokenHash string, expiresAt time.Time) (userID string, err error)
	// UserIDByTokenHash 解析 token 哈希；已吊销视为未认证。
	UserIDByTokenHash(ctx context.Context, tokenHash string) (userID string, err error)
	// RevokeToken 吊销 token，重复吊销不报错。
	RevokeToken(ctx context.Context, tokenHash string) error
	// GetUser 返回账号基础信息，供数据导出使用。
	GetUser(ctx context.Context, userID string) (model.User, error)
	// DeleteUser 硬删除用户及其设备、读数、变更日志、token 与配对码。
	// 用户不存在时返回 ErrUnauthorized，保证重复注销仍是 401。
	DeleteUser(ctx context.Context, userID string) error

	// CreatePairingCode 为当前用户签发一次性配对码；同一用户的新码使旧码失效。
	CreatePairingCode(ctx context.Context, userID string, codeHash string, expiresAt time.Time) error
	// ClaimPairingCode 校验配对码并在同一事务内签发新 token，返回配对用户 id。
	// 过期、已用过、不存在统一返回 ErrNotFound；失败尝试按码计数，满 5 次作废。
	ClaimPairingCode(ctx context.Context, codeHash string, tokenHash string, now time.Time, ttl time.Duration, tokenExpiresAt time.Time) (userID string, err error)
	// CleanupPairingCodes 删除 expiresAt 早于 before 的配对码（含失败尝试记录）。
	CleanupPairingCodes(ctx context.Context, before time.Time) (int64, error)

	// ApproxCounts 返回用户、设备、读数的近似行数，供 /metrics 使用。
	// Postgres 实现读取 pg_class.reltuples，不对大表做 COUNT。
	ApproxCounts(ctx context.Context) (model.StoreCounts, error)

	// ListUserDevices 返回用户未删除的设备，按 updated_at 倒序。
	ListUserDevices(ctx context.Context, userID string) ([]model.UserDevice, error)
	// GetUserDevice 按 (user_id, mac) 查询，越权或已删除均返回 ErrNotFound。
	// mac 仅为可选元数据；iOS 设备没有 MAC，应使用 GetUserDeviceByKey。
	GetUserDevice(ctx context.Context, userID string, mac string) (model.UserDevice, error)
	// GetUserDeviceByKey 按 (user_id, platform, external_id) 查询。
	// 越权、已删除或不存在均返回 ErrNotFound，供 iOS 按 CBPeripheral.identifier 定位单台设备。
	GetUserDeviceByKey(ctx context.Context, userID string, platform string, externalID string) (model.UserDevice, error)
	// Push 执行一次同步写入；replayed 表示命中幂等缓存并直接返回历史响应。
	Push(ctx context.Context, userID string, req model.PushRequest, opts PushOptions) (result model.PushResult, replayed bool, err error)
	// Changes 返回用户自 cursor 之后的增量变更，limit 为本次上限。
	// 若 cursor 早于 change_log 的最老保留 seq（存在缺口），返回 ErrCursorExpired。
	Changes(ctx context.Context, userID string, cursor int64, limit int) (model.ChangesPage, error)

	// ListReadings 返回某设备在时间窗口内的读数，按 recordedAt 倒序游标分页。
	// 设备不存在、已删除或不属于该用户时返回 ErrNotFound。
	ListReadings(ctx context.Context, userID string, platform string, externalID string, q model.ReadingsQuery) (model.ReadingsPage, error)
	// ReadingsSummary 聚合某设备在 [from, to] 内的读数；无数据时 count 为 0、数值为 nil。
	ReadingsSummary(ctx context.Context, userID string, platform string, externalID string, metric string, from time.Time, to time.Time) (model.ReadingsSummary, error)
	// ReadingsSeries 返回某指标在 [from, to] 内的 UTC 对齐分桶序列，按 start 升序，只含有数据的桶。
	ReadingsSeries(ctx context.Context, userID string, platform string, externalID string, metric string, from time.Time, to time.Time, bucket string) (model.ReadingsSeries, error)
	// ReadingsMetrics 返回设备的指标清单，按 lastAt 倒序；无数据时 items 为空数组。
	ReadingsMetrics(ctx context.Context, userID string, platform string, externalID string) (model.ReadingMetrics, error)
	// CleanupReadings 删除 recorded_at 早于 before 的读数，返回删除条数。
	CleanupReadings(ctx context.Context, before time.Time) (int64, error)
	// CleanupChangeLog 删除 server_ts 早于 before 的变更日志，返回删除条数。
	CleanupChangeLog(ctx context.Context, before time.Time) (int64, error)
	// CleanupIdempotencyKeys 删除 created_at 早于 before 的幂等键，返回删除条数。
	CleanupIdempotencyKeys(ctx context.Context, before time.Time) (int64, error)
	// CleanupTokens 删除 expires_at 早于 before 的 token，返回删除条数。
	CleanupTokens(ctx context.Context, before time.Time) (int64, error)

	// ListUserDevicesForExport 返回用户全部设备（含已删除的 tombstone），按 updated_at 倒序。
	ListUserDevicesForExport(ctx context.Context, userID string) ([]model.UserDevice, error)
	// ExportReadings 按 recorded_at 升序导出读数，cursor 为不透明游标。
	// 返回下一页游标与是否还有更多；非法游标返回 ErrBadCursor。
	ExportReadings(ctx context.Context, userID string, cursor string, limit int) ([]model.ExportReading, string, bool, error)
}
