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
	// ErrDuplicateDevice 表示同一批 push 中出现重复设备 id 或重复 (platform, externalId)。
	// HTTP 层已提前校验；存储层再次校验用于直接调用 Store 的场景，映射为 400 BAD_REQUEST。
	ErrDuplicateDevice = errors.New("同一批设备存在重复 id 或唯一键")
	// ErrForbidden 表示已认证用户对同家庭内的资源没有操作权限（跨家庭仍返回 ErrNotFound）。
	ErrForbidden = errors.New("没有操作权限")
	// ErrHouseholdOwnerMustTransfer 表示 owner 在还有其他成员时不能退出/不能移除自己。
	// HTTP 层映射为 409，并给出可读提示。
	ErrHouseholdOwnerMustTransfer = errors.New("家庭还有其他成员，请先转让所有权或移除其他成员")
	// ErrHouseholdOwnerCannotRemoveSelf 表示 owner 不能把自己移出家庭。
	ErrHouseholdOwnerCannotRemoveSelf = errors.New("owner 不能移除自己")
	// ErrPersonalHouseholdImmutable 表示个人家庭不支持退出等操作。
	ErrPersonalHouseholdImmutable = errors.New("个人家庭不支持该操作")
	// ErrRoomOrderMismatch 表示整段房间排序与当前房间集合不一致（缺项/多项/重复）。
	ErrRoomOrderMismatch = errors.New("房间排序列表必须包含家庭内全部房间且不能重复")
	// ErrHouseholdTargetNotMember 表示转让所有权的目标用户不是该家庭成员。
	// HTTP 层映射为 404，与「跨家庭一律 404」保持一致，不暴露用户是否存在。
	ErrHouseholdTargetNotMember = errors.New("目标用户不是该家庭成员")
	// ErrHouseholdTargetAlreadyOwner 表示目标用户已经是该家庭 owner（转让给自己）。
	ErrHouseholdTargetAlreadyOwner = errors.New("目标用户已经是该家庭 owner")
	// ErrFeedbackInvalidTransition 表示反馈状态机不允许该跳转（含对已关闭反馈的任何推进）。
	// HTTP 层映射为 409 FEEDBACK_INVALID_TRANSITION。
	ErrFeedbackInvalidTransition = errors.New("反馈状态不允许该变更")
)

// PushOptions 携带同步写入的幂等参数。
type PushOptions struct {
	IdempotencyKey string
	RequestHash    string
}

// FeedbackOptions 携带反馈写入的幂等参数。
// IdempotencyKey 由 HTTP 层统一派生（客户端提供或按规范化请求体哈希），存储层只负责唯一约束。
type FeedbackOptions struct {
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
	// 可见范围为「自己是成员的全部家庭」，不再只限本人写入。
	// 若 cursor 早于可见 change_log 的最老保留 seq（存在缺口），返回 ErrCursorExpired。
	Changes(ctx context.Context, userID string, cursor int64, limit int) (model.ChangesPage, error)

	// CreateHousehold 创建一个非默认家庭，创建者自动成为 owner。
	CreateHousehold(ctx context.Context, ownerUserID string, name string) (model.Household, error)
	// ListHouseholds 返回用户作为成员的全部家庭（含个人家庭）。
	ListHouseholds(ctx context.Context, userID string) ([]model.Household, error)
	// GetHousehold 仅家庭成员可见；非成员或不存在统一返回 ErrNotFound。
	GetHousehold(ctx context.Context, userID string, householdID string) (model.Household, error)
	// RenameHousehold 仅 owner 可改名。
	RenameHousehold(ctx context.Context, userID string, householdID string, name string) error
	// DeleteHousehold 仅 owner 可删除；个人家庭不可删除；设备回到各自 owner 的个人家庭。
	DeleteHousehold(ctx context.Context, userID string, householdID string) error
	// ListHouseholdMembers 返回成员列表，仅家庭成员可见。
	ListHouseholdMembers(ctx context.Context, userID string, householdID string) ([]model.HouseholdMember, error)
	// RemoveHouseholdMember 仅 owner 可移除成员；不能移除 owner 自己；被移除成员的设备回到其个人家庭。
	RemoveHouseholdMember(ctx context.Context, userID string, householdID string, targetUserID string) error
	// LeaveHousehold 让成员自己退出；personal 家庭不可退出；owner 还有其他成员时返回 ErrHouseholdOwnerMustTransfer；
	// owner 单独退出时家庭随之删除。
	LeaveHousehold(ctx context.Context, userID string, householdID string) error
	// TransferHouseholdOwnership 由 owner 把所有权转让给同家庭另一成员。
	// 事务内同时改 household_members.role 与 households.owner_user_id，保证任意时刻恰好一个 owner。
	// 个人家庭返回 ErrPersonalHouseholdImmutable；目标非成员返回 ErrHouseholdTargetNotMember；
	// 目标是自己（已是 owner）返回 ErrHouseholdTargetAlreadyOwner；非 owner 调用返回 ErrForbidden。
	TransferHouseholdOwnership(ctx context.Context, userID string, householdID string, targetUserID string) error
	// CreateHouseholdInvitation 由 owner 签发一次性邀请码（只存哈希）。
	CreateHouseholdInvitation(ctx context.Context, userID string, householdID string, codeHash string, expiresAt time.Time) (model.HouseholdInvitation, error)
	// ListHouseholdInvitations 由 owner 查看未使用的邀请码；只返回哈希前缀与状态。
	ListHouseholdInvitations(ctx context.Context, userID string, householdID string) ([]model.HouseholdInvitationInfo, error)
	// RevokeHouseholdInvitation 由 owner 撤销未使用的邀请码。
	RevokeHouseholdInvitation(ctx context.Context, userID string, householdID string, invitationID string) error
	// ClaimHouseholdInvitation 用邀请码加入家庭；失败按码计数，满 5 次作废。
	ClaimHouseholdInvitation(ctx context.Context, userID string, codeHash string, now time.Time, ttl time.Duration) (model.Household, error)
	// ListHouseholdRooms 返回房间列表，仅家庭成员可见。
	ListHouseholdRooms(ctx context.Context, userID string, householdID string) ([]model.HouseholdRoom, error)
	// CreateHouseholdRoom 仅 owner 可建房间。
	CreateHouseholdRoom(ctx context.Context, userID string, householdID string, name string, sortOrder int) (model.HouseholdRoom, error)
	// UpdateHouseholdRoom 仅 owner 可改房间；name/sortOrder 为 nil 表示不变。
	UpdateHouseholdRoom(ctx context.Context, userID string, householdID string, roomID string, name *string, sortOrder *int) (model.HouseholdRoom, error)
	// ReorderHouseholdRooms 仅 owner 可整段重排；roomIDs 必须恰好覆盖家庭全部房间，按数组顺序写入 0..n-1。
	ReorderHouseholdRooms(ctx context.Context, userID string, householdID string, roomIDs []string) ([]model.HouseholdRoom, error)
	// DeleteHouseholdRoom 仅 owner 可删房间；房间内设备的 room_id 置 NULL。
	DeleteHouseholdRoom(ctx context.Context, userID string, householdID string, roomID string) error
	// UpdateUserDeviceMeta 仅设备 owner 可改 alias/roomId/householdId。
	// alias/roomID/householdID 为 nil 表示不变，空串表示清除（householdId 清除=回到个人家庭）。
	UpdateUserDeviceMeta(ctx context.Context, userID string, platform string, externalID string, alias *string, roomID *string, householdID *string) (model.UserDevice, error)

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

	// CreateFeedback 写入一条反馈；同一 (userID, IdemKey) 重复提交返回已存记录且 replayed=true。
	// 同一幂等键对应不同请求体时返回 ErrIdempotencyConflict。
	CreateFeedback(ctx context.Context, userID string, sub model.FeedbackSubmission, opts FeedbackOptions) (item model.Feedback, replayed bool, err error)
	// ListUserFeedback 只返回本人反馈，可按 status 过滤（空串不过滤），
	// 按 createdAt 倒序游标分页；非法游标返回 ErrBadCursor。
	ListUserFeedback(ctx context.Context, userID string, status string, cursor string, limit int) (model.FeedbackPage, error)
	// GetUserFeedback 返回本人单条反馈详情；不存在或不属于该用户时返回 ErrNotFound。
	GetUserFeedback(ctx context.Context, userID string, feedbackID string) (model.FeedbackDetail, error)
	// CloseUserFeedback 让本人关闭自己的反馈（submitted|received -> closed）。
	// 不属于该用户返回 ErrNotFound；已关闭或状态不允许返回 ErrFeedbackInvalidTransition。
	CloseUserFeedback(ctx context.Context, userID string, feedbackID string) (model.FeedbackDetail, error)
	// ListAdminFeedback 跨用户返回反馈，可按 status 过滤（空串不过滤），按 createdAt 倒序游标分页。
	ListAdminFeedback(ctx context.Context, status string, cursor string, limit int) (model.AdminFeedbackPage, error)
	// UpdateFeedbackStatus 由运营侧推进状态；target 为空时不允许。
	// 允许 submitted->received、submitted->closed、received->submitted、received->closed；
	// 同状态视为幂等 no-op（不写审计）；其余跳转返回 ErrFeedbackInvalidTransition。
	UpdateFeedbackStatus(ctx context.Context, feedbackID string, targetStatus string, note *string) (model.AdminFeedback, error)

	// ListUserDevicesForExport 返回用户全部设备（含已删除的 tombstone），按 updated_at 倒序。
	ListUserDevicesForExport(ctx context.Context, userID string) ([]model.UserDevice, error)
	// ExportReadings 按 recorded_at 升序导出读数，cursor 为不透明游标。
	// 返回下一页游标与是否还有更多；非法游标返回 ErrBadCursor。
	ExportReadings(ctx context.Context, userID string, cursor string, limit int) ([]model.ExportReading, string, bool, error)
}
