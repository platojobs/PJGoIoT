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
	Type          string          `json:"type"`
	Name          string          `json:"name"`
	Vendor        string          `json:"vendor"`
	Description   string          `json:"description"`
	GATTServices  []GATTService   `json:"gattServices"`
	Compatibility json.RawMessage `json:"compatibility"`
}

// CatalogMetric 是设备类型目录中一个可解析指标。
// CharacteristicUUID 是该指标来源的 GATT 特征 UUID，客户端据此定位订阅目标。
type CatalogMetric struct {
	Key                string `json:"key"`
	Unit               string `json:"unit"`
	CharacteristicUUID string `json:"characteristicUuid"`
}

// CatalogDeviceType 是 /catalog/device-types 中的一项。
// 与旧 /device-types 的 DeviceType 分开：目录项带 metrics、limitations 与 updatedAt，
// 且 displayName 替代旧 name 字段，避免改变既有契约。
type CatalogDeviceType struct {
	Type          string          `json:"type"`
	DisplayName   string          `json:"displayName"`
	Description   string          `json:"description"`
	GATTServices  []GATTService   `json:"gattServices"`
	Metrics       []CatalogMetric `json:"metrics"`
	Limitations   []string        `json:"limitations"`
	Compatibility json.RawMessage `json:"compatibility"`
	UpdatedAt     time.Time       `json:"updatedAt"`
}

// DeviceTypeCatalog 是 GET /catalog/device-types 的响应体。
// Version 是目录整体版本号（每次管理端写入自增），GeneratedAt 是最近一次目录变更时间。
type DeviceTypeCatalog struct {
	Version     int64               `json:"version"`
	GeneratedAt time.Time           `json:"generatedAt"`
	Items       []CatalogDeviceType `json:"items"`
}

// CatalogDeviceTypeInput 是运营侧 PUT /admin/catalog/device-types/{type} 的请求体。
// 采用整项替换语义：字段必须全部提供，未提供的集合视为空数组。
type CatalogDeviceTypeInput struct {
	DisplayName   string          `json:"displayName"`
	Description   string          `json:"description"`
	GATTServices  []GATTService   `json:"gattServices"`
	Metrics       []CatalogMetric `json:"metrics"`
	Limitations   []string        `json:"limitations"`
	Compatibility json.RawMessage `json:"compatibility"`
}

// Firmware 是某一设备类型的固件版本记录。
// vendor 标识固件发布方，compatibility 描述平台与协议支持，供市场模块筛选。
type Firmware struct {
	ID            string          `json:"id"`
	DeviceType    string          `json:"deviceType"`
	Version       string          `json:"version"`
	Vendor        string          `json:"vendor"`
	ReleaseNotes  string          `json:"releaseNotes"`
	ReleasedAt    string          `json:"releasedAt"`
	Compatibility json.RawMessage `json:"compatibility"`
}

// Accessory 是某一设备类型的官方配件记录。
type Accessory struct {
	ID            string          `json:"id"`
	DeviceType    string          `json:"deviceType"`
	Name          string          `json:"name"`
	SKU           string          `json:"sku"`
	Vendor        string          `json:"vendor"`
	Description   string          `json:"description"`
	Compatibility json.RawMessage `json:"compatibility"`
}

// DeviceCatalog 汇总某设备类型的元数据、固件与配件。
// 供无法获取 MAC 的客户端（如 iOS）一次性读取型号详情、产品与市场信息。
type DeviceCatalog struct {
	DeviceType  DeviceType  `json:"deviceType"`
	Firmwares   []Firmware  `json:"firmwares"`
	Accessories []Accessory `json:"accessories"`
}

// UserDevice 是用户私有设备记录。id 由客户端生成，serverSeq 由服务端同步序列填充。
// 唯一键为 (user_id, platform, externalId)：Android 的 externalId 为大写 MAC，
// iOS 的 externalId 为 CBPeripheral.identifier。mac 仅作为可选元数据保留。
//
// deviceType / vendor / firmwareVersion / compatibility 由公共元数据关联得到：
// deviceType 优先取用户记录中的值，缺失时按 mac 匹配 public_devices；
// 其余字段按 mac（public_devices）或 deviceType（device_types）解析。
// 关联缺失时对应字段为 JSON null，不影响用户记录本身返回。
type UserDevice struct {
	ID              string          `json:"id"`
	ClientID        string          `json:"clientId"`
	Platform        string          `json:"platform"`
	ExternalID      string          `json:"externalId"`
	MAC             string          `json:"mac,omitempty"`
	Alias           string          `json:"alias"`
	AdvertisedName  string          `json:"advertisedName"`
	Group           string          `json:"group"`
	Rules           json.RawMessage `json:"rules"`
	ScanRecords     json.RawMessage `json:"scanRecords"`
	UpdatedAt       time.Time       `json:"updatedAt"`
	DeletedAt       *time.Time      `json:"deletedAt,omitempty"`
	ServerSeq       int64           `json:"serverSeq"`
	DeviceType      *string         `json:"deviceType"`
	Vendor          *string         `json:"vendor"`
	FirmwareVersion *string         `json:"firmwareVersion"`
	Compatibility   json.RawMessage `json:"compatibility"`
	HouseholdID     *string         `json:"householdId"`
	RoomID          *string         `json:"roomId"`
}

// Household 是家庭视图：role 为当前用户在家庭中的角色，counts 由服务端聚合。
type Household struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	OwnerUserID string    `json:"ownerUserId"`
	Role        string    `json:"role"`
	Personal    bool      `json:"personal"`
	MemberCount int       `json:"memberCount"`
	DeviceCount int       `json:"deviceCount"`
	RoomCount   int       `json:"roomCount"`
	CreatedAt   time.Time `json:"createdAt"`
}

// HouseholdMember 是家庭成员视图。
// isSelf 标记该行是否为当前调用账号本人，客户端据此渲染「我」，无需自行比对 userId。
type HouseholdMember struct {
	UserID   string    `json:"userId"`
	Role     string    `json:"role"`
	Kind     string    `json:"kind"`
	IsSelf   bool      `json:"isSelf"`
	JoinedAt time.Time `json:"joinedAt"`
}

// HouseholdRoom 是家庭内的房间。
type HouseholdRoom struct {
	ID          string    `json:"id"`
	HouseholdID string    `json:"householdId"`
	Name        string    `json:"name"`
	SortOrder   int       `json:"sortOrder"`
	CreatedAt   time.Time `json:"createdAt"`
}

// HouseholdInvitation 是一次性家庭邀请码的签发结果（只返回明文一次）。
type HouseholdInvitation struct {
	HouseholdID string    `json:"householdId"`
	ExpiresAt   time.Time `json:"expiresAt"`
}

// HouseholdInvitationInfo 是邀请码管理视图：只暴露哈希前缀，绝不返回明文。
// Status 取值 active / expired / locked。
type HouseholdInvitationInfo struct {
	ID             string    `json:"id"`
	CodeHashPrefix string    `json:"codeHashPrefix"`
	Status         string    `json:"status"`
	Attempts       int       `json:"attempts"`
	ExpiresAt      time.Time `json:"expiresAt"`
	CreatedAt      time.Time `json:"createdAt"`
}

// PushDevice 是客户端在一次 push 中提交的单台设备。
//
// 可缺省字段使用指针区分「未上报」与「显式置空」：
//   - nil：未上报，服务端保持原值；
//   - 非 nil 的 ""：显式清除（group 为空串即清除分组）；
//   - 非 nil 的其他值：覆盖原值。
//
// alias 是用户设置的别名，只应在用户操作时上报；advertisedName 是设备广播名，
// 客户端扫描时可覆盖上报，二者互不影响。
type PushDevice struct {
	ID             string          `json:"id"`
	Platform       string          `json:"platform"`
	ExternalID     string          `json:"externalId"`
	MAC            string          `json:"mac,omitempty"`
	DeviceType     string          `json:"deviceType,omitempty"`
	Alias          *string         `json:"alias,omitempty"`
	AdvertisedName *string         `json:"advertisedName,omitempty"`
	Group          *string         `json:"group,omitempty"`
	Rules          json.RawMessage `json:"rules"`
	ScanRecords    json.RawMessage `json:"scanRecords"`
	UpdatedAt      time.Time       `json:"updatedAt"`
	Deleted        bool            `json:"deleted"`
}

// ResolvePresence 按「未上报保持原值、显式值覆盖」的规则，解析出本次写入应采用的值。
// existing 为 nil（新建）时未上报字段取空串。
func (d PushDevice) ResolvePresence(existing *UserDevice) (alias, advertisedName, group string) {
	if existing != nil {
		alias, advertisedName, group = existing.Alias, existing.AdvertisedName, existing.Group
	}
	if d.Alias != nil {
		alias = *d.Alias
	}
	if d.AdvertisedName != nil {
		advertisedName = *d.AdvertisedName
	}
	if d.Group != nil {
		group = *d.Group
	}
	return alias, advertisedName, group
}

// PushReading 是客户端在一次 push 中提交的单条测量读数。
// 读取到的设备可以不在 user_devices 中（「隐式设备」），服务端只按设备唯一键归档。
type PushReading struct {
	ID         string    `json:"id"`
	Platform   string    `json:"platform"`
	ExternalID string    `json:"externalId"`
	Metric     string    `json:"metric"`
	Value      float64   `json:"value"`
	Unit       string    `json:"unit"`
	RecordedAt time.Time `json:"recordedAt"`
	DeviceType string    `json:"deviceType,omitempty"`
}

// Reading 是读数查询接口返回的一条记录（只含客户端需要的字段）。
type Reading struct {
	ID         string    `json:"id"`
	Metric     string    `json:"metric"`
	Value      float64   `json:"value"`
	Unit       string    `json:"unit"`
	RecordedAt time.Time `json:"recordedAt"`
}

// ReadingsPage 是 GET /my/devices/{platform}/{externalId}/readings 的响应体。
// 按 recordedAt 倒序；nextCursor 为空串表示没有更多。
type ReadingsPage struct {
	Items      []Reading `json:"items"`
	NextCursor string    `json:"nextCursor"`
	HasMore    bool      `json:"hasMore"`
}

// ReadingsSummary 是读数聚合结果；无数据时 count 为 0，数值字段为 null。
type ReadingsSummary struct {
	Metric      string    `json:"metric"`
	Unit        string    `json:"unit"`
	Count       int64     `json:"count"`
	Min         *float64  `json:"min"`
	Max         *float64  `json:"max"`
	Avg         *float64  `json:"avg"`
	Last        *float64  `json:"last"`
	WindowStart time.Time `json:"windowStart"`
	WindowEnd   time.Time `json:"windowEnd"`
}

// ReadingsQuery 是读数列表的过滤与分页参数。
type ReadingsQuery struct {
	Metric string
	From   *time.Time
	To     *time.Time
	Limit  int
	Cursor string
}

// ReadingsBucket 是一个时间桶的聚合结果；Last 是桶内 recordedAt 最新的值。
type ReadingsBucket struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
	Count int64     `json:"count"`
	Min   *float64  `json:"min"`
	Max   *float64  `json:"max"`
	Avg   *float64  `json:"avg"`
	Last  *float64  `json:"last"`
}

// ReadingsSeries 是 GET .../readings/series 的响应体。
// 桶按 start 升序，且只包含 count>0 的桶；无数据时 Buckets 为空数组。
type ReadingsSeries struct {
	Metric  string           `json:"metric"`
	Unit    string           `json:"unit"`
	Bucket  string           `json:"bucket"`
	From    time.Time        `json:"from"`
	To      time.Time        `json:"to"`
	Buckets []ReadingsBucket `json:"buckets"`
}

// ReadingMetric 是指标清单中的一项。
// Unit 取该指标最近一条读数的单位；FirstAt / LastAt 为该指标的记录时间范围。
type ReadingMetric struct {
	Metric  string    `json:"metric"`
	Unit    string    `json:"unit"`
	Count   int64     `json:"count"`
	FirstAt time.Time `json:"firstAt"`
	LastAt  time.Time `json:"lastAt"`
}

// ReadingMetrics 是 GET .../readings/metrics 的响应体，按 LastAt 倒序。
type ReadingMetrics struct {
	Items []ReadingMetric `json:"items"`
}

// PushRequest 是 POST /sync/push 的请求体。
// Readings 自 v0.4.0 起可选；缺失时行为与旧客户端完全一致。
type PushRequest struct {
	ClientID string        `json:"clientId"`
	Devices  []PushDevice  `json:"devices"`
	Readings []PushReading `json:"readings"`
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

// Conflict 表示一条未被写入的客户端数据。
//
// 设备冲突的 entity 字段省略（兼容旧客户端），server/client 为设备对象；
// 读数冲突的 entity 为 "reading"，reason 为 "duplicate_id"，reading 为客户端提交的读数。
type Conflict struct {
	ID         string      `json:"id"`
	Platform   string      `json:"platform"`
	ExternalID string      `json:"externalId"`
	MAC        string      `json:"mac,omitempty"`
	Entity     string      `json:"entity,omitempty"`
	Reason     string      `json:"reason"`
	Server     *UserDevice `json:"server,omitempty"`
	Client     *PushDevice `json:"client,omitempty"`
	Reading    *Reading    `json:"reading,omitempty"`
}

// PushResult 是 POST /sync/push 的响应体。
// appliedReadings / skippedReadings 自 v0.4.0 起新增；请求未带 readings 时恒为 0。
type PushResult struct {
	Applied         []AppliedChange `json:"applied"`
	Conflicts       []Conflict      `json:"conflicts"`
	NextCursor      int64           `json:"nextCursor"`
	AppliedReadings int             `json:"appliedReadings"`
	SkippedReadings int             `json:"skippedReadings"`
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

// HouseholdChangePayload 是 change_log 中 entity=household 的 payload（v0.15.0）。
// Role 是**该接收者本人**在家庭中的角色，客户端可直接据此刷新「我的家庭」列表。
// Deleted=true 表示该家庭已从当前账号的数据集中移除（解散、被移除、自己退出）。
type HouseholdChangePayload struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	OwnerUserID string `json:"ownerUserId"`
	Role        string `json:"role"`
	MemberCount int    `json:"memberCount"`
	Deleted     bool   `json:"deleted"`
}

// RoomChangePayload 是 change_log 中 entity=room 的 payload（v0.15.0）。
// Deleted=true 表示房间已删除，客户端应移除本地房间并按需清空设备 roomId。
type RoomChangePayload struct {
	ID          string `json:"id"`
	HouseholdID string `json:"householdId"`
	Name        string `json:"name"`
	SortOrder   int    `json:"sortOrder"`
	Deleted     bool   `json:"deleted"`
}

// ChangesPage 是 GET /sync/changes 的响应体。
type ChangesPage struct {
	Changes    []Change `json:"changes"`
	NextCursor int64    `json:"nextCursor"`
	HasMore    bool     `json:"hasMore"`
}

// FeedbackSubmission 是 POST /feedback 的规范化内容（服务端负责校验与去重）。
// contact / diagnostics 属于可选个人信息或大段文本，只在写入时使用，不进入任何列表响应。
type FeedbackSubmission struct {
	Kind        string  `json:"kind"`
	Body        string  `json:"body"`
	Contact     *string `json:"contact,omitempty"`
	Platform    string  `json:"platform"`
	AppVersion  string  `json:"appVersion"`
	BuildType   string  `json:"buildType"`
	OSVersion   string  `json:"osVersion"`
	DeviceModel string  `json:"deviceModel"`
	Diagnostics *string `json:"diagnostics,omitempty"`
}

// Feedback 是 GET /feedback/mine 中的一条反馈视图。
// 不包含 contact 与 diagnostics：前者是个人信息，后者体积大且仅用于排查。
type Feedback struct {
	FeedbackID  string    `json:"feedbackId"`
	Kind        string    `json:"kind"`
	Body        string    `json:"body"`
	Platform    string    `json:"platform"`
	AppVersion  string    `json:"appVersion"`
	BuildType   string    `json:"buildType"`
	OSVersion   string    `json:"osVersion"`
	DeviceModel string    `json:"deviceModel"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"createdAt"`
}

// FeedbackPage 是 GET /feedback/mine 的响应体，按 createdAt 倒序游标分页。
type FeedbackPage struct {
	Items      []Feedback `json:"items"`
	NextCursor string     `json:"nextCursor"`
	HasMore    bool       `json:"hasMore"`
}

// FeedbackDetail 是 GET /feedback/{feedbackId} 的单条视图（仅本人可见）。
// StatusMessage 是给用户看的状态说明；contact / diagnostics 不在此结构中。
type FeedbackDetail struct {
	FeedbackID    string    `json:"feedbackId"`
	Kind          string    `json:"kind"`
	Body          string    `json:"body"`
	Platform      string    `json:"platform"`
	AppVersion    string    `json:"appVersion"`
	BuildType     string    `json:"buildType"`
	OSVersion     string    `json:"osVersion"`
	DeviceModel   string    `json:"deviceModel"`
	Status        string    `json:"status"`
	StatusMessage string    `json:"statusMessage"`
	CreatedAt     time.Time `json:"createdAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

// AdminFeedback 是运营侧（GET /admin/feedback）的反馈视图：跨用户，含联系方式、
// 诊断文本与内部备注，仅通过 admin token 暴露，绝不复用到用户侧响应。
type AdminFeedback struct {
	FeedbackID   string    `json:"feedbackId"`
	UserID       string    `json:"userId"`
	Kind         string    `json:"kind"`
	Body         string    `json:"body"`
	Contact      *string   `json:"contact,omitempty"`
	Platform     string    `json:"platform"`
	AppVersion   string    `json:"appVersion"`
	BuildType    string    `json:"buildType"`
	OSVersion    string    `json:"osVersion"`
	DeviceModel  string    `json:"deviceModel"`
	Diagnostics  *string   `json:"diagnostics,omitempty"`
	Status       string    `json:"status"`
	InternalNote *string   `json:"internalNote,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

// AdminFeedbackPage 是 GET /admin/feedback 的响应体，按 createdAt 倒序游标分页。
type AdminFeedbackPage struct {
	Items      []AdminFeedback `json:"items"`
	NextCursor string          `json:"nextCursor"`
	HasMore    bool            `json:"hasMore"`
}

// FeedbackStatusMessage 把状态翻译成给用户看的中文说明。
func FeedbackStatusMessage(status string) string {
	switch status {
	case "received":
		return "已受理，工作人员正在处理"
	case "closed":
		return "已关闭"
	default:
		return "已提交，等待处理"
	}
}

// FeedbackTransitionAllowed 定义反馈状态机：submitted -> received -> closed，
// 允许直接关闭 submitted -> closed 与误受理回退 received -> submitted；closed 为终态。
func FeedbackTransitionAllowed(from, to string) bool {
	switch from {
	case "submitted":
		return to == "received" || to == "closed"
	case "received":
		return to == "submitted" || to == "closed"
	default:
		return false
	}
}

// User 是 GET /me/export 中返回的账号信息。
type User struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	CreatedAt time.Time `json:"createdAt"`
}

// DeletionRequest 是 POST /me/deletion-request 的响应体。
// Token 为一次性删除凭据，只在此处返回一次；服务端仅保存哈希，10 分钟后过期。
type DeletionRequest struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// ExportTask 是 POST /me/export 与 GET /me/export/{id} 的响应体（异步导出）。
// Status 取值 pending / ready / failed；仅 ready 时返回 downloadUrl 与 downloadExpiresAt。
type ExportTask struct {
	ID                string     `json:"id"`
	Status            string     `json:"status"`
	CreatedAt         time.Time  `json:"createdAt"`
	CompletedAt       *time.Time `json:"completedAt,omitempty"`
	DownloadURL       string     `json:"downloadUrl,omitempty"`
	DownloadExpiresAt *time.Time `json:"downloadExpiresAt,omitempty"`
	ExpiresAt         time.Time  `json:"expiresAt"`
}

// ExportTaskStatus 是存储层返回的导出任务内部视图（不直接序列化）。
type ExportTaskStatus struct {
	ID                string
	Status            string
	CreatedAt         time.Time
	CompletedAt       *time.Time
	ExpiresAt         time.Time
	DownloadExpiresAt *time.Time
}

// ExportReadingAggregate 是导出中的读数聚合项：按 (platform, externalId, metric, unit) 分组。
// 个人数据导出刻意采用聚合而非明细，避免大体量账号的产物与内存无界增长；
// 明细在账号删除前仍可通过 /my/devices/.../readings 获取。
type ExportReadingAggregate struct {
	Platform   string    `json:"platform"`
	ExternalID string    `json:"externalId"`
	Metric     string    `json:"metric"`
	Unit       string    `json:"unit"`
	Count      int64     `json:"count"`
	Min        float64   `json:"min"`
	Max        float64   `json:"max"`
	Avg        float64   `json:"avg"`
	FirstAt    time.Time `json:"firstAt"`
	LastAt     time.Time `json:"lastAt"`
}

// ExportRoom 是导出中的房间信息（仅结构，不含其他成员数据）。
type ExportRoom struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	SortOrder int       `json:"sortOrder"`
	CreatedAt time.Time `json:"createdAt"`
}

// ExportHousehold 是导出中本人所属家庭的信息。
// 只包含本人角色与家庭结构，绝不包含其他成员的身份或联系方式。
type ExportHousehold struct {
	ID        string       `json:"id"`
	Name      string       `json:"name"`
	Role      string       `json:"role"`
	Personal  bool         `json:"personal"`
	CreatedAt time.Time    `json:"createdAt"`
	Rooms     []ExportRoom `json:"rooms"`
}

// ExportFeedback 是导出中的本人反馈。
// 包含本人填写的联系方式与诊断文本，但绝不包含运营侧内部备注（internalNote）。
type ExportFeedback struct {
	FeedbackID  string    `json:"feedbackId"`
	Kind        string    `json:"kind"`
	Body        string    `json:"body"`
	Contact     *string   `json:"contact,omitempty"`
	Platform    string    `json:"platform"`
	AppVersion  string    `json:"appVersion"`
	BuildType   string    `json:"buildType"`
	OSVersion   string    `json:"osVersion"`
	DeviceModel string    `json:"deviceModel"`
	Diagnostics *string   `json:"diagnostics,omitempty"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// PersonalDataExport 是导出产物的内容。
// 只包含调用账号自己的数据；家庭仅含本人角色与房间，不含其他成员信息。
type PersonalDataExport struct {
	User        User                     `json:"user"`
	Devices     []UserDevice             `json:"devices"`
	Readings    []ExportReadingAggregate `json:"readings"`
	Households  []ExportHousehold        `json:"households"`
	Feedback    []ExportFeedback         `json:"feedback"`
	GeneratedAt time.Time                `json:"generatedAt"`
}

// StoreCounts 是用于 /metrics 的近似行数，Postgres 通过 pg_class.reltuples 读取。
type StoreCounts struct {
	Users    int64
	Devices  int64
	Readings int64
}
