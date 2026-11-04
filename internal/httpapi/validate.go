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
	maxPushDevices    = 100
	maxAliasLength    = 100
	maxGroupLength    = 64
	maxClientIDLength = 128
	maxIdempotencyKey = 128
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

// pushPayload 是 POST /sync/push 的请求 DTO，利用 validator 标签做基础校验。
type pushPayload struct {
	ClientID string              `json:"clientId" binding:"max=128"`
	Devices  []pushDevicePayload `json:"devices" binding:"required,min=1,max=100,dive"`
}

type pushDevicePayload struct {
	ID          string          `json:"id" binding:"required,uuid"`
	Platform    string          `json:"platform" binding:"omitempty,oneof=android ios"`
	ExternalID  string          `json:"externalId" binding:"max=128"`
	MAC         string          `json:"mac" binding:"max=64"`
	Alias       string          `json:"alias" binding:"max=100"`
	Group       string          `json:"group" binding:"max=64"`
	Rules       json.RawMessage `json:"rules"`
	ScanRecords json.RawMessage `json:"scanRecords"`
	UpdatedAt   time.Time       `json:"updatedAt" binding:"required"`
	Deleted     bool            `json:"deleted"`
}

// toModel 把 DTO 转成领域请求；字段 JSON 结构对外保持不变。
func (p pushPayload) toModel() model.PushRequest {
	req := model.PushRequest{ClientID: p.ClientID, Devices: make([]model.PushDevice, 0, len(p.Devices))}
	for _, d := range p.Devices {
		req.Devices = append(req.Devices, model.PushDevice{
			ID:          d.ID,
			Platform:    d.Platform,
			ExternalID:  d.ExternalID,
			MAC:         d.MAC,
			Alias:       d.Alias,
			Group:       d.Group,
			Rules:       d.Rules,
			ScanRecords: d.ScanRecords,
			UpdatedAt:   d.UpdatedAt,
			Deleted:     d.Deleted,
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
	if len(req.Devices) == 0 {
		return fmt.Errorf("devices 不能为空")
	}
	if len(req.Devices) > maxPushDevices {
		return fmt.Errorf("单次最多提交 %d 台设备", maxPushDevices)
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

		dev.Alias = strings.TrimSpace(dev.Alias)
		if len([]rune(dev.Alias)) > maxAliasLength {
			return fmt.Errorf("devices[%d].alias 长度不能超过 %d", i, maxAliasLength)
		}
		dev.Group = strings.TrimSpace(dev.Group)
		if len([]rune(dev.Group)) > maxGroupLength {
			return fmt.Errorf("devices[%d].group 长度不能超过 %d", i, maxGroupLength)
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
