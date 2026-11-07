package model

import "time"

// 设备在线状态三态。unknown 与 offline 必须可区分：
//   - unknown：last_seen_at 为 NULL，该账号从未收到过这台设备的读数；
//   - online：最后一次读数接收时间在窗口内；
//   - offline：曾收到过读数，但最后一次已超出窗口。
const (
	PresenceUnknown = "unknown"
	PresenceOnline  = "online"
	PresenceOffline = "offline"
)

// PresenceWindow 是在线判定窗口（契约 0.21.0 固定为 15 分钟）。
// 判定规则：now - lastSeenAt <= PresenceWindow 为 online，否则 offline。
// 窗口由服务端统一执行，客户端不应自行实现另一套阈值。
const PresenceWindow = 15 * time.Minute

// DerivePresence 按服务端当前时间推导 presence。
// lastSeenAt 为 nil 时返回 unknown；刚好等于窗口边界仍算 online。
func DerivePresence(now time.Time, lastSeenAt *time.Time) string {
	if lastSeenAt == nil {
		return PresenceUnknown
	}
	if now.UTC().Sub(lastSeenAt.UTC()) <= PresenceWindow {
		return PresenceOnline
	}
	return PresenceOffline
}

// ApplyPresence 在响应生成前填充推导字段，避免遗漏任一读接口。
func ApplyPresence(d UserDevice, now time.Time) UserDevice {
	d.Presence = DerivePresence(now, d.LastSeenAt)
	return d
}
