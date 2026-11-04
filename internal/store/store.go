// Package store 定义存储抽象。
// fixture 与 postgres 两种实现共享同一套接口，HTTP 层不感知底层存储。
package store

import (
	"context"
	"errors"

	"github.com/platojobs/PJGoIoT/internal/model"
)

var (
	// ErrNotFound 用于公共元数据或用户私有资源不存在，HTTP 层统一映射为 404。
	ErrNotFound = errors.New("资源不存在")
	// ErrUnauthorized 表示 token 缺失、无效或已被吊销。
	ErrUnauthorized = errors.New("未认证")
	// ErrIdempotencyConflict 表示同一 Idempotency-Key 被用于不同的请求体。
	ErrIdempotencyConflict = errors.New("Idempotency-Key 对应的请求体不一致")
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
	// DeviceTypeExists 判断设备类型是否存在。
	DeviceTypeExists(ctx context.Context, deviceType string) (bool, error)
	// ListFirmwares 返回指定设备类型的固件列表。
	ListFirmwares(ctx context.Context, deviceType string) ([]model.Firmware, error)
	// ListAccessories 返回指定设备类型的配件列表。
	ListAccessories(ctx context.Context, deviceType string) ([]model.Accessory, error)

	// CreateUserWithToken 创建匿名用户并写入 token 哈希，返回用户 id。
	CreateUserWithToken(ctx context.Context, tokenHash string) (userID string, err error)
	// UserIDByTokenHash 解析 token 哈希；已吊销视为未认证。
	UserIDByTokenHash(ctx context.Context, tokenHash string) (userID string, err error)
	// RevokeToken 吊销 token，重复吊销不报错。
	RevokeToken(ctx context.Context, tokenHash string) error

	// ListUserDevices 返回用户未删除的设备，按 updated_at 倒序。
	ListUserDevices(ctx context.Context, userID string) ([]model.UserDevice, error)
	// GetUserDevice 按 (user_id, mac) 查询，越权或已删除均返回 ErrNotFound。
	GetUserDevice(ctx context.Context, userID string, mac string) (model.UserDevice, error)
	// Push 执行一次同步写入；replayed 表示命中幂等缓存并直接返回历史响应。
	Push(ctx context.Context, userID string, req model.PushRequest, opts PushOptions) (result model.PushResult, replayed bool, err error)
	// Changes 返回用户自 cursor 之后的增量变更，limit 为本次上限。
	Changes(ctx context.Context, userID string, cursor int64, limit int) (model.ChangesPage, error)
}
