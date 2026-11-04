// Package model 定义服务端对外 JSON 契约中的领域对象。
// 该包不依赖数据库或 HTTP，便于 fixture 存储与 Postgres 存储复用。
package model

import (
	"encoding/json"
	"time"
)

// GATTService 表示一个蓝牙 GATT 服务（UUID 与可读名称）。
type GATTService struct {
	UUID string `json:"uuid"`
	Name string `json:"name"`
}

// PublicDevice 是全局共享的设备公共元数据，按规范化后的 MAC 索引。
// 不包含用户私有信息，因此无需鉴权。
type PublicDevice struct {
	MAC             string          `json:"mac"`
	Vendor          string          `json:"vendor"`
	Model           string          `json:"model"`
	DeviceType      string          `json:"deviceType"`
	FirmwareVersion string          `json:"firmwareVersion"`
	GATTServices    []GATTService   `json:"gattServices"`
	Compatibility   json.RawMessage `json:"compatibility"`
}

// DeviceType 是设备类型（如体重秤、心率带）的公共描述。
type DeviceType struct {
	Type         string        `json:"type"`
	Name         string        `json:"name"`
	Vendor       string        `json:"vendor"`
	Description  string        `json:"description"`
	GATTServices []GATTService `json:"gattServices"`
}

// Firmware 是某一设备类型的固件版本记录。
type Firmware struct {
	ID           string `json:"id"`
	DeviceType   string `json:"deviceType"`
	Version      string `json:"version"`
	ReleaseNotes string `json:"releaseNotes"`
	ReleasedAt   string `json:"releasedAt"`
}

// Accessory 是某一设备类型的官方配件记录。
type Accessory struct {
	ID          string `json:"id"`
	DeviceType  string `json:"deviceType"`
	Name        string `json:"name"`
	SKU         string `json:"sku"`
	Description string `json:"description"`
}

// UserDevice 是用户私有设备记录。id 由客户端生成，serverSeq 由服务端同步序列填充。
// 唯一键为 (user_id, platform, externalId)：Android 的 externalId 为大写 MAC，
// iOS 的 externalId 为 CBPeripheral.identifier。mac 仅作为可选元数据保留。
type UserDevice struct {
	ID          string          `json:"id"`
	ClientID    string          `json:"clientId"`
	Platform    string          `json:"platform"`
	ExternalID  string          `json:"externalId"`
	MAC         string          `json:"mac,omitempty"`
	Alias       string          `json:"alias"`
	Group       string          `json:"group"`
	Rules       json.RawMessage `json:"rules"`
	ScanRecords json.RawMessage `json:"scanRecords"`
	UpdatedAt   time.Time       `json:"updatedAt"`
	DeletedAt   *time.Time      `json:"deletedAt,omitempty"`
	ServerSeq   int64           `json:"serverSeq"`
}

// PushDevice 是客户端在一次 push 中提交的单台设备。
type PushDevice struct {
	ID          string          `json:"id"`
	Platform    string          `json:"platform"`
	ExternalID  string          `json:"externalId"`
	MAC         string          `json:"mac,omitempty"`
	Alias       string          `json:"alias"`
	Group       string          `json:"group"`
	Rules       json.RawMessage `json:"rules"`
	ScanRecords json.RawMessage `json:"scanRecords"`
	UpdatedAt   time.Time       `json:"updatedAt"`
	Deleted     bool            `json:"deleted"`
}

// PushRequest 是 POST /sync/push 的请求体。
type PushRequest struct {
	ClientID string       `json:"clientId"`
	Devices  []PushDevice `json:"devices"`
}

// AppliedChange 表示一条被服务端接受的变更。
type AppliedChange struct {
	ID         string `json:"id"`
	Platform   string `json:"platform"`
	ExternalID string `json:"externalId"`
	MAC        string `json:"mac,omitempty"`
	Status     string `json:"status"` // applied：已写入；unchanged：内容一致，无新变更
	ServerSeq  int64  `json:"serverSeq"`
}

// Conflict 表示 LWW 判定服务端获胜、客户端数据未被写入。
type Conflict struct {
	ID         string     `json:"id"`
	Platform   string     `json:"platform"`
	ExternalID string     `json:"externalId"`
	MAC        string     `json:"mac,omitempty"`
	Reason     string     `json:"reason"`
	Server     UserDevice `json:"server"`
	Client     PushDevice `json:"client"`
}

// PushResult 是 POST /sync/push 的响应体。
type PushResult struct {
	Applied    []AppliedChange `json:"applied"`
	Conflicts  []Conflict      `json:"conflicts"`
	NextCursor int64           `json:"nextCursor"`
}

// Change 是增量变更日志中的一条记录。
type Change struct {
	Seq      int64           `json:"seq"`
	Entity   string          `json:"entity"`
	EntityID string          `json:"entityId"`
	Op       string          `json:"op"`
	Payload  json.RawMessage `json:"payload"`
	ServerTS time.Time       `json:"serverTs"`
}

// ChangesPage 是 GET /sync/changes 的响应体。
type ChangesPage struct {
	Changes    []Change `json:"changes"`
	NextCursor int64    `json:"nextCursor"`
	HasMore    bool     `json:"hasMore"`
}
