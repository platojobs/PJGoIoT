package model

import (
	"encoding/json"
	"testing"
	"time"
)

// TestOptionalTimeJSON 校验 markedAt 的三态：
// 字段缺失=Set=false；null/空串=显式取消；RFC3339=标记并截断到微秒。
func TestOptionalTimeJSON(t *testing.T) {
	var absent struct {
		D PushDevice `json:"device"`
	}
	if err := json.Unmarshal([]byte(`{"device":{"updatedAt":"2026-11-05T00:00:00Z"}}`), &absent); err != nil {
		t.Fatalf("解码缺少 markedAt 的设备失败: %v", err)
	}
	if absent.D.MarkedAt.Set || absent.D.MarkedAt.Time != nil {
		t.Fatalf("缺失 markedAt 应为未上报: %+v", absent.D.MarkedAt)
	}

	cases := []struct {
		name string
		raw  string
		want *time.Time
		set  bool
	}{
		{name: "null 取消标记", raw: `null`, set: true},
		{name: "空串取消标记", raw: `""`, set: true},
		{name: "时间戳标记", raw: `"2026-11-05T08:00:00.123456789+08:00"`, set: true, want: ptrTime(time.Date(2026, 11, 5, 0, 0, 0, 123456000, time.UTC))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var o OptionalTime
			if err := json.Unmarshal([]byte(tc.raw), &o); err != nil {
				t.Fatalf("解码失败: %v", err)
			}
			if !o.Set {
				t.Fatalf("显式字段应标记 Set=true")
			}
			if tc.want == nil {
				if o.Time != nil {
					t.Fatalf("期望 null，得到 %v", o.Time)
				}
				return
			}
			if o.Time == nil || !o.Time.Equal(*tc.want) {
				t.Fatalf("时间未按微秒截断: got=%v want=%v", o.Time, tc.want)
			}
		})
	}

	t.Run("非法时间报错", func(t *testing.T) {
		var o OptionalTime
		if err := json.Unmarshal([]byte(`"not-a-time"`), &o); err == nil {
			t.Fatalf("期望报错")
		}
	})

	t.Run("输出 null 与时间戳", func(t *testing.T) {
		b, err := json.Marshal(OptionalTime{Set: true})
		if err != nil || string(b) != "null" {
			t.Fatalf("marshal=%s err=%v", b, err)
		}
		tm := time.Date(2026, 11, 5, 0, 0, 0, 123456789, time.UTC)
		b, err = json.Marshal(OptionalTime{Set: true, Time: &tm})
		if err != nil || string(b) != `"2026-11-05T00:00:00.123456Z"` {
			t.Fatalf("marshal=%s err=%v", b, err)
		}
	})
}

func ptrTime(t time.Time) *time.Time { return &t }
