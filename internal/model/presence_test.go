package model

import (
	"testing"
	"time"
)

func TestDerivePresence(t *testing.T) {
	now := time.Date(2026, 11, 7, 1, 0, 0, 0, time.UTC)
	seen := now.Add(-time.Minute)

	cases := []struct {
		name string
		last *time.Time
		want string
	}{
		{"从未上报", nil, PresenceUnknown},
		{"窗口内", &seen, PresenceOnline},
		{"刚好等于窗口", ptrTime(now.Add(-PresenceWindow)), PresenceOnline},
		{"刚超出窗口 1 微秒", ptrTime(now.Add(-PresenceWindow - time.Microsecond)), PresenceOffline},
		{"未来时间（时钟回拨）", ptrTime(now.Add(time.Minute)), PresenceOnline},
		{"心跳只到秒级", ptrTime(now.Add(-14*time.Minute - 59*time.Second)), PresenceOnline},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DerivePresence(now, tc.last); got != tc.want {
				t.Fatalf("presence=%q want=%q", got, tc.want)
			}
		})
	}
}

func TestApplyPresenceOnlySetsDerivedField(t *testing.T) {
	now := time.Date(2026, 11, 7, 1, 0, 0, 0, time.UTC)
	older := now.Add(-2 * PresenceWindow)
	d := UserDevice{ID: "d1", Alias: "灯", LastSeenAt: &older, Presence: PresenceOnline}

	got := ApplyPresence(d, now)
	if got.Presence != PresenceOffline {
		t.Fatalf("presence=%q want=%q", got.Presence, PresenceOffline)
	}
	if got.ID != d.ID || got.Alias != d.Alias || got.LastSeenAt == nil || !got.LastSeenAt.Equal(older) {
		t.Fatalf("ApplyPresence 改动了非推导字段: %+v", got)
	}
	if !got.LastSeenAt.Equal(older) {
		t.Fatalf("lastSeenAt 被改写: %s", got.LastSeenAt)
	}
}
