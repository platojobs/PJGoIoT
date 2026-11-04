package lww

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/platojobs/PJGoIoT/internal/model"
)

func serverAt(t time.Time) *model.UserDevice {
	return &model.UserDevice{
		ID: "server-id", MAC: "aa:bb:cc:00:00:01", Alias: "服务端", Group: "默认",
		Rules: json.RawMessage(`{"a":1}`), ScanRecords: json.RawMessage(`[]`),
		UpdatedAt: t, ServerSeq: 7,
	}
}

func clientAt(t time.Time, alias string) model.PushDevice {
	return model.PushDevice{
		ID: "client-id", MAC: "aa:bb:cc:00:00:01", Alias: alias, Group: "默认",
		Rules: json.RawMessage(`{"a": 1}`), ScanRecords: json.RawMessage(`[]`),
		UpdatedAt: t,
	}
}

func TestDecide(t *testing.T) {
	now := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)

	t.Run("服务端不存在时接受", func(t *testing.T) {
		d := Decide(now, clientAt(now.Add(-time.Hour), "新设备"), nil)
		if !d.Apply || d.Noop {
			t.Fatalf("decision=%+v", d)
		}
	})

	t.Run("客户端更新时接受", func(t *testing.T) {
		d := Decide(now, clientAt(now.Add(30*time.Second), "新名字"), serverAt(now.Add(10*time.Second)))
		if !d.Apply {
			t.Fatalf("decision=%+v", d)
		}
	})

	t.Run("时间相同且内容一致时幂等", func(t *testing.T) {
		d := Decide(now, clientAt(now, "服务端"), serverAt(now))
		if !d.Noop || d.Apply {
			t.Fatalf("decision=%+v", d)
		}
	})

	t.Run("客户端更旧时服务端获胜", func(t *testing.T) {
		d := Decide(now, clientAt(now.Add(-time.Minute), "旧名字"), serverAt(now))
		if d.Apply || d.Noop || d.Reason != ReasonServerNewer {
			t.Fatalf("decision=%+v", d)
		}
	})

	t.Run("客户端时间超前 2 分钟以上时用服务端时间兜底", func(t *testing.T) {
		d := Decide(now, clientAt(now.Add(10*time.Minute), "未来时间"), serverAt(now.Add(-time.Hour)))
		if !d.Apply || !d.ClockClamped {
			t.Fatalf("decision=%+v", d)
		}
		if !d.EffectiveUpdatedAt.Equal(now) {
			t.Fatalf("兜底时间=%v, want %v", d.EffectiveUpdatedAt, now)
		}
	})

	t.Run("超前不超过 2 分钟时保留客户端时间", func(t *testing.T) {
		clientTime := now.Add(90 * time.Second)
		d := Decide(now, clientAt(clientTime, "允许误差"), serverAt(now.Add(-time.Hour)))
		if !d.Apply || d.ClockClamped {
			t.Fatalf("decision=%+v", d)
		}
		if !d.EffectiveUpdatedAt.Equal(clientTime) {
			t.Fatalf("时间=%v, want %v", d.EffectiveUpdatedAt, clientTime)
		}
	})

	t.Run("JSON 键顺序不同但语义一致", func(t *testing.T) {
		server := serverAt(now)
		server.Rules = json.RawMessage(`{"a":1,"b":2}`)
		client := clientAt(now, "服务端")
		client.Rules = json.RawMessage(`{ "b": 2, "a": 1 }`)
		d := Decide(now, client, server)
		if !d.Noop {
			t.Fatalf("decision=%+v", d)
		}
	})
}

// TestDeviceTypeComparison 固化 deviceType 的比较规则：
// 客户端上报型号时参与内容比较；旧客户端不传该字段时不影响幂等判定。
func TestDeviceTypeComparison(t *testing.T) {
	now := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)

	server := serverAt(now)
	serverType := "nordic_uart"
	server.DeviceType = &serverType

	t.Run("客户端上报相同型号视为内容一致", func(t *testing.T) {
		client := clientAt(now, "服务端")
		client.DeviceType = "nordic_uart"
		if !SameContent(server, client) {
			t.Fatalf("相同型号被判为不同内容")
		}
	})

	t.Run("客户端上报不同型号判定为冲突", func(t *testing.T) {
		client := clientAt(now, "服务端")
		client.DeviceType = "smart_scale"
		if SameContent(server, client) {
			t.Fatalf("不同型号被判为内容一致")
		}
	})

	t.Run("旧客户端不上报型号不影响幂等", func(t *testing.T) {
		client := clientAt(now, "服务端")
		if !SameContent(server, client) {
			t.Fatalf("缺省 deviceType 不应造成冲突")
		}
	})
}
