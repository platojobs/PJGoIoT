package model

import "time"

// PushRegistration 是一条已注册的设备推送令牌。同一用户同一 token 只有一条记录，
// 令牌失效时只把 Status 置为 inactive，不硬删除（保留历史投递引用与排查线索）。
// 注意与 model.PushDevice（客户端同步上来的 IoT 设备）区分：后者是设备实体，本类型是推送凭据。
type PushRegistration struct {
	ID          string    `json:"id"`
	Platform    string    `json:"platform"`
	Token       string    `json:"token"`
	AppVersion  string    `json:"appVersion"`
	DeviceModel string    `json:"deviceModel"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// PushRegistrationInput 是 POST /push/devices 的请求体。
type PushRegistrationInput struct {
	Platform    string `json:"platform"`
	Token       string `json:"token"`
	AppVersion  string `json:"appVersion,omitempty"`
	DeviceModel string `json:"deviceModel,omitempty"`
}

// Alert 是服务端产生的一条告警（当前由运营侧 POST /admin/alerts 写入）。
// Payload 是平台无关的字符串键值，最终作为推送 data 载荷的一部分。
type Alert struct {
	ID        string            `json:"id"`
	UserID    string            `json:"userId"`
	Kind      string            `json:"kind"`
	Title     string            `json:"title"`
	Body      string            `json:"body"`
	Severity  string            `json:"severity"`
	Payload   map[string]string `json:"payload"`
	CreatedAt time.Time         `json:"createdAt"`
}

// AlertInput 是 POST /admin/alerts 的请求体。
type AlertInput struct {
	UserID   string            `json:"userId"`
	Kind     string            `json:"kind"`
	Title    string            `json:"title"`
	Body     string            `json:"body,omitempty"`
	Severity string            `json:"severity,omitempty"`
	Payload  map[string]string `json:"payload,omitempty"`
}

// PushDelivery 是投递队列中的一条记录。
// 对客户端而言一次告警只对应一条推送，不需要感知设备级投递状态。
type PushDelivery struct {
	ID            string
	UserID        string
	AlertID       string
	PushDeviceID  string
	Platform      string
	Token         string
	Title         string
	Body          string
	Data          map[string]string
	Attempts      int
	NextAttemptAt time.Time
	CreatedAt     time.Time
}

// ScenePage 是 GET /scenes 的分页结果。
// Limit<=0 时 NextCursor 为空、HasMore=false，Items 为全量（与分页前行为一致）。
type ScenePage struct {
	Items      []Scene
	NextCursor string
	HasMore    bool
}
