package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/platojobs/PJGoIoT/internal/model"
	"github.com/platojobs/PJGoIoT/internal/validate"
)

const (
	maxPushDevices          = 100
	maxAliasLength          = 100
	maxAdvertisedNameLength = 248
	maxGroupLength          = 64
	maxClientIDLength       = 128
	maxIdempotencyKey       = 128
	// maxPushReadings 与 validate.MaxReadingsPerPush 保持一致。
	maxPushReadings = 500
	// defaultReadingsLimit 是读数列表的默认页大小。
	defaultReadingsLimit = 100
	// maxSummaryHours / defaultSummaryHours 限定聚合窗口。
	defaultSummaryHours = 24
	maxSummaryHours     = 2160 // 90 天，与服务端保留期一致
)

// macURIParam 用 ShouldBindUri 读取路径参数。
type macURIParam struct {
	MAC string `uri:"mac" binding:"required"`
}

// typeURIParam 用 ShouldBindUri 读取设备类型。
type typeURIParam struct {
	Type string `uri:"type" binding:"required"`
}

// changesQuery 用 ShouldBindQuery 读取分页参数。
type changesQuery struct {
	Cursor string `form:"cursor"`
	Limit  *int   `form:"limit" binding:"omitempty,min=1,max=500"`
}

// readingsQuery 是读数列表的查询参数。
type readingsQuery struct {
	Metric string `form:"metric"`
	From   string `form:"from"`
	To     string `form:"to"`
	Limit  *int   `form:"limit" binding:"omitempty,min=1,max=500"`
	Cursor string `form:"cursor"`
}

// readingsSummaryQuery 是读数聚合的查询参数。
type readingsSummaryQuery struct {
	Metric string `form:"metric"`
	Hours  *int   `form:"hours" binding:"omitempty,min=1,max=2160"`
}

// readingsSeriesQuery 是读数分桶序列的查询参数；必填项在 handler 内逐项校验。
type readingsSeriesQuery struct {
	Metric string `form:"metric"`
	From   string `form:"from"`
	To     string `form:"to"`
	Bucket string `form:"bucket"`
}

// pushPayload 是 POST /sync/push 的请求 DTO，利用 validator 标签做基础校验。
// devices 与 readings 至少有一个非空，两者都缺省或都为空时返回 400。
type pushPayload struct {
	ClientID string               `json:"clientId" binding:"max=128"`
	Devices  []pushDevicePayload  `json:"devices" binding:"max=100,dive"`
	Readings []pushReadingPayload `json:"readings" binding:"max=500,dive"`
}

// pushReadingPayload 是 readings 数组中单条读数的 DTO。
type pushReadingPayload struct {
	ID         string    `json:"id" binding:"required,uuid"`
	Platform   string    `json:"platform" binding:"omitempty,oneof=android ios"`
	ExternalID string    `json:"externalId" binding:"max=128"`
	Metric     string    `json:"metric" binding:"required,max=32"`
	Value      float64   `json:"value"`
	Unit       string    `json:"unit" binding:"max=16"`
	RecordedAt time.Time `json:"recordedAt" binding:"required"`
	DeviceType string    `json:"deviceType" binding:"omitempty,max=64"`
}

type pushDevicePayload struct {
	ID             string             `json:"id" binding:"required,uuid"`
	Platform       string             `json:"platform" binding:"omitempty,oneof=android ios"`
	ExternalID     string             `json:"externalId" binding:"max=128"`
	MAC            string             `json:"mac" binding:"max=64"`
	DeviceType     string             `json:"deviceType" binding:"omitempty,max=64"`
	Alias          *string            `json:"alias"`
	AdvertisedName *string            `json:"advertisedName"`
	Group          *string            `json:"group"`
	Rules          json.RawMessage    `json:"rules"`
	ScanRecords    json.RawMessage    `json:"scanRecords"`
	UpdatedAt      time.Time          `json:"updatedAt" binding:"required"`
	MarkedAt       model.OptionalTime `json:"markedAt"`
	Deleted        bool               `json:"deleted"`
}

// toModel 把 DTO 转成领域请求；字段 JSON 结构对外保持不变。
func (p pushPayload) toModel() model.PushRequest {
	req := model.PushRequest{ClientID: p.ClientID, Devices: make([]model.PushDevice, 0, len(p.Devices))}
	for _, d := range p.Devices {
		req.Devices = append(req.Devices, model.PushDevice{
			ID:             d.ID,
			Platform:       d.Platform,
			ExternalID:     d.ExternalID,
			MAC:            d.MAC,
			DeviceType:     d.DeviceType,
			Alias:          d.Alias,
			AdvertisedName: d.AdvertisedName,
			Group:          d.Group,
			Rules:          d.Rules,
			ScanRecords:    d.ScanRecords,
			UpdatedAt:      d.UpdatedAt,
			MarkedAt:       d.MarkedAt,
			Deleted:        d.Deleted,
		})
	}
	for _, r := range p.Readings {
		req.Readings = append(req.Readings, model.PushReading{
			ID:         r.ID,
			Platform:   r.Platform,
			ExternalID: r.ExternalID,
			Metric:     r.Metric,
			Value:      r.Value,
			Unit:       r.Unit,
			RecordedAt: r.RecordedAt,
			DeviceType: r.DeviceType,
		})
	}
	return req
}

// normalizePush 校验并规范化一次 push 请求。
// 返回的错误信息直接作为 400 响应的 message。
// 设备唯一键为 (platform, externalId)，mac 仅作为可选元数据。
func normalizePush(req *model.PushRequest) error {
	req.ClientID = strings.TrimSpace(req.ClientID)
	if len(req.ClientID) > maxClientIDLength {
		return fmt.Errorf("clientId 长度不能超过 %d", maxClientIDLength)
	}
	if len(req.Devices) == 0 && len(req.Readings) == 0 {
		return fmt.Errorf("devices 与 readings 不能同时为空")
	}
	if len(req.Devices) > maxPushDevices {
		return fmt.Errorf("单次最多提交 %d 台设备", maxPushDevices)
	}
	if len(req.Readings) > maxPushReadings {
		return fmt.Errorf("单次最多提交 %d 条读数", maxPushReadings)
	}

	seenID := make(map[string]struct{}, len(req.Devices))
	seenKey := make(map[string]struct{}, len(req.Devices))
	for i := range req.Devices {
		dev := &req.Devices[i]

		if _, err := uuid.Parse(dev.ID); err != nil {
			return fmt.Errorf("devices[%d].id 必须是 UUID", i)
		}
		if _, ok := seenID[dev.ID]; ok {
			return fmt.Errorf("devices[%d].id 重复", i)
		}
		seenID[dev.ID] = struct{}{}

		platform, externalID, mac, err := validate.ResolveDeviceKey(dev.Platform, dev.ExternalID, dev.MAC)
		if err != nil {
			return fmt.Errorf("devices[%d] 设备键非法: %s", i, err)
		}
		dev.Platform = platform
		dev.ExternalID = externalID
		dev.MAC = mac

		key := platform + "|" + externalID
		if _, ok := seenKey[key]; ok {
			return fmt.Errorf("devices[%d] 的 platform + externalId 重复", i)
		}
		seenKey[key] = struct{}{}

		if dev.Alias != nil {
			alias := strings.TrimSpace(*dev.Alias)
			dev.Alias = &alias
			if len([]rune(alias)) > maxAliasLength {
				return fmt.Errorf("devices[%d].alias 长度不能超过 %d", i, maxAliasLength)
			}
		}
		if dev.AdvertisedName != nil {
			name := strings.TrimSpace(*dev.AdvertisedName)
			dev.AdvertisedName = &name
			if len([]rune(name)) > maxAdvertisedNameLength {
				return fmt.Errorf("devices[%d].advertisedName 长度不能超过 %d", i, maxAdvertisedNameLength)
			}
		}
		dev.DeviceType = strings.ToLower(strings.TrimSpace(dev.DeviceType))
		if len([]rune(dev.DeviceType)) > 64 {
			return fmt.Errorf("devices[%d].deviceType 长度不能超过 64", i)
		}
		if dev.Group != nil {
			// 空串保留：表示显式清除分组，不能当作「未上报」。
			group := strings.TrimSpace(*dev.Group)
			dev.Group = &group
			if len([]rune(group)) > maxGroupLength {
				return fmt.Errorf("devices[%d].group 长度不能超过 %d", i, maxGroupLength)
			}
		}

		if dev.UpdatedAt.IsZero() {
			return fmt.Errorf("devices[%d].updatedAt 不能为空", i)
		}

		rules, err := normalizeObject(dev.Rules)
		if err != nil {
			return fmt.Errorf("devices[%d].rules 必须是 JSON 对象", i)
		}
		dev.Rules = rules

		records, err := normalizeArray(dev.ScanRecords)
		if err != nil {
			return fmt.Errorf("devices[%d].scanRecords 必须是 JSON 数组", i)
		}
		dev.ScanRecords = records
	}

	return normalizeReadings(req)
}

// normalizeReadings 校验并规范化 readings 数组。
//
// 重复 id 不在此报错：服务端按 id 幂等处理，重复的计入 skippedReadings，
// 内容不一致的额外计入 conflicts（entity=reading, reason=duplicate_id）。
func normalizeReadings(req *model.PushRequest) error {
	now := time.Now().UTC()
	for i := range req.Readings {
		r := &req.Readings[i]
		if _, err := uuid.Parse(r.ID); err != nil {
			return fmt.Errorf("readings[%d].id 必须是 UUID", i)
		}
		platform, externalID, _, err := validate.ResolveDeviceKey(r.Platform, r.ExternalID, "")
		if err != nil {
			return fmt.Errorf("readings[%d] 设备键非法: %s", i, err)
		}
		r.Platform, r.ExternalID = platform, externalID

		if r.Metric, err = validate.NormalizeMetric(r.Metric); err != nil {
			return fmt.Errorf("readings[%d].metric: %s", i, err)
		}
		if err := validate.CheckFiniteValue(r.Value); err != nil {
			return fmt.Errorf("readings[%d].value: %s", i, err)
		}
		if r.Unit, err = validate.NormalizeUnit(r.Unit); err != nil {
			return fmt.Errorf("readings[%d].unit: %s", i, err)
		}
		if err := validate.CheckRecordedAt(r.RecordedAt, now); err != nil {
			return fmt.Errorf("readings[%d].recordedAt: %s", i, err)
		}
		r.RecordedAt = r.RecordedAt.UTC().Truncate(time.Microsecond)
		r.DeviceType = strings.ToLower(strings.TrimSpace(r.DeviceType))
	}
	return nil
}

func normalizeObject(raw json.RawMessage) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return json.RawMessage(`{}`), nil
	}
	var obj map[string]any
	if err := json.Unmarshal(trimmed, &obj); err != nil || obj == nil {
		return nil, fmt.Errorf("不是对象")
	}
	return trimmed, nil
}

func normalizeArray(raw json.RawMessage) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return json.RawMessage(`[]`), nil
	}
	var arr []any
	if err := json.Unmarshal(trimmed, &arr); err != nil || arr == nil {
		return nil, fmt.Errorf("不是数组")
	}
	return trimmed, nil
}

func parseCursor(raw string) (int64, error) {
	if raw == "" {
		return 0, nil
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("cursor 必须是非负整数")
	}
	return v, nil
}
