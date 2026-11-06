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
	group := "默认"
	return model.PushDevice{
		ID: "client-id", MAC: "aa:bb:cc:00:00:01", Alias: &alias, Group: &group,
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

// TestPresenceOnlyComparison 固化可缺省字段的比较规则：
// 缺失表示「未上报，保持原值」，不参与比较；空串是显式清除，需要参与比较。
func TestPresenceOnlyComparison(t *testing.T) {
	now := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)
	server := serverAt(now)
	server.AdvertisedName = "PJ-Sensor"

	t.Run("缺省 alias/group/advertisedName 时视为内容一致", func(t *testing.T) {
		client := model.PushDevice{
			ID: "client-id", MAC: "aa:bb:cc:00:00:01",
			Rules: json.RawMessage(`{"a": 1}`), ScanRecords: json.RawMessage(`[]`),
			UpdatedAt: now,
		}
		if !SameContent(server, client) {
			t.Fatalf("缺省字段不应造成内容差异")
		}
	})

	t.Run("group 空串是显式清除，与服务端旧值不同", func(t *testing.T) {
		client := model.PushDevice{
			ID: "client-id", MAC: "aa:bb:cc:00:00:01",
			Group: strPtr(""),
			Rules: json.RawMessage(`{"a": 1}`), ScanRecords: json.RawMessage(`[]`),
			UpdatedAt: now,
		}
		if SameContent(server, client) {
			t.Fatalf("显式清除分组应视为内容差异")
		}
	})

	t.Run("advertisedName 上报不同值视为内容差异", func(t *testing.T) {
		client := model.PushDevice{
			ID: "client-id", MAC: "aa:bb:cc:00:00:01",
			AdvertisedName: strPtr("PJ-Sensor-2"),
			Rules:          json.RawMessage(`{"a": 1}`), ScanRecords: json.RawMessage(`[]`),
			UpdatedAt: now,
		}
		if SameContent(server, client) {
			t.Fatalf("广播名不同应视为内容差异")
		}
	})
}

func strPtr(s string) *string { return &s }

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

// TestDecideField 固化场景字段级 LWW 的判定规则，重点是跨精度比较：
// 客户端时间带纳秒、服务端已按 Postgres 微秒精度落库时，同一时间戳不应被
// 误判为「严格更新」，否则同一个补丁会被反复应用、version 无谓增长。
func TestDecideField(t *testing.T) {
	now := time.Date(2026, 11, 6, 6, 0, 0, 0, time.UTC)
	server := now.Add(-time.Hour).Truncate(time.Microsecond)

	t.Run("纳秒差异不应造成再次应用", func(t *testing.T) {
		client := server.Add(500 * time.Nanosecond)
		if d := DecideField(now, client, server, true); d.Apply || !d.Noop {
			t.Fatalf("同一微秒时间戳 + 同内容应为 no-op，得到 %+v", d)
		}
	})

	t.Run("严格更新应用并保留微秒精度", func(t *testing.T) {
		client := server.Add(time.Second).Add(700 * time.Nanosecond)
		d := DecideField(now, client, server, false)
		if !d.Apply {
			t.Fatalf("较新时间戳应应用，得到 %+v", d)
		}
		if got := d.EffectiveUpdatedAt; got.Nanosecond()%1000 != 0 {
			t.Fatalf("生效时间应截断到微秒，得到 %v", got)
		}
	})

	t.Run("过期写入服务端获胜", func(t *testing.T) {
		client := server.Add(-time.Second)
		d := DecideField(now, client, server, true)
		if d.Apply || d.Noop || d.Reason != ReasonServerNewer {
			t.Fatalf("过期写入应服务端获胜，得到 %+v", d)
		}
	})

	t.Run("时钟超前用服务端时间兜底", func(t *testing.T) {
		client := now.Add(MaxClockSkew + time.Minute)
		d := DecideField(now, client, server, false)
		if !d.Apply || !d.ClockClamped {
			t.Fatalf("超前时钟应兜底后应用，得到 %+v", d)
		}
		if !d.EffectiveUpdatedAt.Equal(now) {
			t.Fatalf("兜底时间应为服务端当前时间，得到 %v", d.EffectiveUpdatedAt)
		}
		if d.Reason != "" {
			t.Fatalf("已应用不应带冲突原因，得到 %q", d.Reason)
		}
	})
}

// TestMarkedAtLWW 固化设备标记字段的三态与精度规则：
// 缺省=不参与比较；null=显式取消；时间戳按微秒截断后比较，
// 重复标记同一时间戳必须是 no-op，旧写入不得覆盖新标记。
func TestMarkedAtLWW(t *testing.T) {
	now := time.Date(2026, 11, 7, 6, 0, 0, 0, time.UTC)
	marked := now.Add(-2 * time.Minute).Truncate(time.Microsecond)
	server := serverAt(now.Add(-time.Minute))
	server.MarkedAt = &marked

	t.Run("未上报 markedAt 不影响幂等", func(t *testing.T) {
		client := clientAt(server.UpdatedAt, "服务端")
		if !SameContent(server, client) {
			t.Fatalf("缺省 markedAt 不应造成内容差异")
		}
	})

	t.Run("显式 null 与已标记不同", func(t *testing.T) {
		client := clientAt(server.UpdatedAt, "服务端")
		client.MarkedAt = model.OptionalTime{Set: true}
		if SameContent(server, client) {
			t.Fatalf("取消标记应视为内容差异")
		}
	})

	t.Run("重复标记同一时间戳且内容一致为 no-op", func(t *testing.T) {
		clientTime := server.UpdatedAt.Add(500 * time.Nanosecond)
		client := clientAt(clientTime, "服务端")
		nano := marked.Add(400 * time.Nanosecond)
		client.MarkedAt = model.OptionalTime{Set: true, Time: &nano}
		d := Decide(now, client, server)
		if !d.Noop || d.Apply {
			t.Fatalf("同一微秒的重复标记应为 no-op，得到 %+v", d)
		}
	})

	t.Run("更旧的标记写入服务端获胜", func(t *testing.T) {
		client := clientAt(server.UpdatedAt.Add(-time.Second), "服务端")
		old := marked.Add(-time.Second)
		client.MarkedAt = model.OptionalTime{Set: true, Time: &old}
		d := Decide(now, client, server)
		if d.Apply || d.Noop || d.Reason != ReasonServerNewer {
			t.Fatalf("旧标记不应覆盖新标记，得到 %+v", d)
		}
	})

	t.Run("较新的取消标记被接受", func(t *testing.T) {
		client := clientAt(server.UpdatedAt.Add(time.Second), "服务端")
		client.MarkedAt = model.OptionalTime{Set: true}
		if d := Decide(now, client, server); !d.Apply {
			t.Fatalf("较新的取消标记应应用，得到 %+v", d)
		}
	})
}
