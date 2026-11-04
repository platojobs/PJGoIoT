// Package lww 实现同步所需的字段级 LWW（Last-Write-Wins）判定。
//
// 客户端与服务端各自维护 updated_at。比较规则：
//   - 客户端时间比服务端时间超前 2 分钟以上时，视为不可信，用服务端当前时间兜底；
//   - 客户端时间严格更新则接受；
//   - 时间相同且内容一致则幂等无操作；
//   - 其余情况判定服务端获胜，客户端数据进入 conflicts 返回，绝不静默覆盖。
package lww

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"

	"github.com/platojobs/PJGoIoT/internal/model"
)

// MaxClockSkew 是允许的客户端时钟超前上限。
const MaxClockSkew = 2 * time.Minute

// 冲突原因。
const (
	ReasonServerNewer  = "server_newer"
	ReasonClockClamped = "clock_clamped"
)

// Decision 是对单台设备的判定结果。
type Decision struct {
	// EffectiveUpdatedAt 是最终写入服务端的 updated_at（可能被服务端时间兜底）。
	EffectiveUpdatedAt time.Time
	// Apply 为 true 表示应写入客户端数据。
	Apply bool
	// Noop 为 true 表示时间与内容均一致，无需写库。
	Noop bool
	// Reason 在冲突时说明服务端获胜的原因。
	Reason string
	// ClockClamped 表示客户端时间超前，已用服务端时间兜底。
	ClockClamped bool
}

// Decide 计算单台设备的同步判定。server 为 nil 表示服务端尚无该设备。
func Decide(now time.Time, client model.PushDevice, server *model.UserDevice) Decision {
	now = now.UTC()
	effective := client.UpdatedAt.UTC()
	clamped := false
	if effective.After(now.Add(MaxClockSkew)) {
		effective = now
		clamped = true
	}

	decision := Decision{EffectiveUpdatedAt: effective, ClockClamped: clamped}

	if server == nil {
		decision.Apply = true
		return decision
	}

	if effective.After(server.UpdatedAt.UTC()) {
		decision.Apply = true
		return decision
	}

	if effective.Equal(server.UpdatedAt.UTC()) && SameContent(server, client) {
		decision.Noop = true
		return decision
	}

	decision.Reason = ReasonServerNewer
	if clamped {
		decision.Reason = ReasonClockClamped
	}
	return decision
}

// SameContent 比较客户端与服务端记录的业务内容（忽略时间与序列号）。
func SameContent(server *model.UserDevice, client model.PushDevice) bool {
	if server.Platform != client.Platform || server.ExternalID != client.ExternalID {
		return false
	}
	if server.MAC != client.MAC {
		return false
	}
	// 可缺省字段仅在客户端上报时参与比较：
	// 缺失表示「未上报，保持原值」，不应与服务端已存值产生冲突；
	// 上报空串是显式清除语义，需与服务端值比较。
	if client.Alias != nil && server.Alias != *client.Alias {
		return false
	}
	if client.AdvertisedName != nil && server.AdvertisedName != *client.AdvertisedName {
		return false
	}
	if client.Group != nil && server.Group != *client.Group {
		return false
	}
	if (server.DeletedAt != nil) != client.Deleted {
		return false
	}
	// 仅当客户端上报了 deviceType 时才比较：旧客户端不传该字段，
	// 不应因其缺失而与服务端（含由 mac 推导的型号）产生冲突。
	if client.DeviceType != "" {
		serverType := ""
		if server.DeviceType != nil {
			serverType = *server.DeviceType
		}
		if !strings.EqualFold(serverType, client.DeviceType) {
			return false
		}
	}
	return jsonEqual(server.Rules, client.Rules) && jsonEqual(server.ScanRecords, client.ScanRecords)
}

func jsonEqual(a, b []byte) bool {
	if len(bytes.TrimSpace(a)) == 0 || len(bytes.TrimSpace(b)) == 0 {
		return len(bytes.TrimSpace(a)) == len(bytes.TrimSpace(b))
	}
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		return false
	}
	return deepEqual(av, bv)
}

func deepEqual(a, b any) bool {
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for k, v := range av {
			if !deepEqual(v, bv[k]) {
				return false
			}
		}
		return true
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if !deepEqual(av[i], bv[i]) {
				return false
			}
		}
		return true
	case json.Number:
		bv, ok := b.(json.Number)
		return ok && av.String() == bv.String()
	default:
		return a == b
	}
}
