// Package scene 提供场景触发条件与动作的规范化与校验。
//
// 校验与存储分离：本包只做不依赖数据库的结构/范围校验，返回 store.ErrScene* 哨兵；
// 设备是否存在、设备类型与指标是否被目录支持由存储层在事务内查询，避免校验与写入之间漂移。
package scene

import (
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/platojobs/PJGoIoT/internal/model"
	"github.com/platojobs/PJGoIoT/internal/store"
	"github.com/platojobs/PJGoIoT/internal/validate"
)

// 场景取值边界。与迁移 000017 的 CHECK 约束保持一致。
const (
	MinCooldownSeconds = 30
	MaxCooldownSeconds = 86400
	DefaultCooldown    = 300

	MinOfflineSeconds = 60
	MaxOfflineSeconds = 86400
	DefaultOffline    = 300

	MinRSSI = -100
	MaxRSSI = 0

	MinConsecutive = 1
	MaxConsecutive = 30

	MaxNameRunes = 60
	MaxActions   = 4

	MaxNotifyTitleRunes = 80
	MaxNotifyBodyRunes  = 500
	MaxLogMessageRunes  = 500
	MaxMarkRunes        = 32
)

// 触发条件与动作种类。
const (
	TriggerDeviceOffline   = "device_offline"
	TriggerRSSIBelow       = "rssi_below"
	TriggerMetricThreshold = "metric_threshold"
	TriggerSchedule        = "schedule"

	ActionNotify     = "notify"
	ActionLog        = "log"
	ActionMarkDevice = "mark_device"
)

var (
	comparators = map[string]struct{}{"lt": {}, "lte": {}, "gt": {}, "gte": {}}
	logLevels   = map[string]struct{}{"info": {}, "warn": {}}
)

// NormalizeInput 校验并规范化 POST /scenes 的请求体。
func NormalizeInput(in model.SceneInput) (model.SceneInput, error) {
	name, err := NormalizeName(in.Name)
	if err != nil {
		return in, err
	}
	in.Name = name

	if in.Enabled == nil {
		v := true
		in.Enabled = &v
	}
	if in.CooldownSeconds == nil {
		v := DefaultCooldown
		in.CooldownSeconds = &v
	}
	if *in.CooldownSeconds < MinCooldownSeconds || *in.CooldownSeconds > MaxCooldownSeconds {
		return in, fmt.Errorf("%w: 必须在 %d..%d 秒之间", store.ErrSceneCooldownOutOfRange, MinCooldownSeconds, MaxCooldownSeconds)
	}

	trigger, err := NormalizeTrigger(in.Trigger)
	if err != nil {
		return in, err
	}
	in.Trigger = trigger

	actions, err := NormalizeActions(in.Actions)
	if err != nil {
		return in, err
	}
	in.Actions = actions
	return in, nil
}

// NormalizePatch 校验并规范化 PUT /scenes/{id} 的字段级补丁。
// 每个出现的字段必须自带非零 updatedAt；没有任何字段时返回错误（避免无意义写入）。
func NormalizePatch(p model.ScenePatch) (model.ScenePatch, error) {
	if p.Name == nil && p.Enabled == nil && p.CooldownSeconds == nil && p.Trigger == nil && p.Actions == nil {
		return p, fmt.Errorf("至少提供一个字段")
	}

	checkTS := func(field string, ts time.Time) error {
		if ts.IsZero() {
			return fmt.Errorf("%s.updatedAt 不能为空", field)
		}
		return nil
	}

	if p.Name != nil {
		if err := checkTS("name", p.Name.UpdatedAt); err != nil {
			return p, err
		}
		name, err := NormalizeName(p.Name.Value)
		if err != nil {
			return p, err
		}
		p.Name = &model.SceneField[string]{Value: name, UpdatedAt: p.Name.UpdatedAt}
	}
	if p.Enabled != nil {
		if err := checkTS("enabled", p.Enabled.UpdatedAt); err != nil {
			return p, err
		}
	}
	if p.CooldownSeconds != nil {
		if err := checkTS("cooldownSeconds", p.CooldownSeconds.UpdatedAt); err != nil {
			return p, err
		}
		if p.CooldownSeconds.Value < MinCooldownSeconds || p.CooldownSeconds.Value > MaxCooldownSeconds {
			return p, fmt.Errorf("%w: 必须在 %d..%d 秒之间", store.ErrSceneCooldownOutOfRange, MinCooldownSeconds, MaxCooldownSeconds)
		}
	}
	if p.Trigger != nil {
		if err := checkTS("trigger", p.Trigger.UpdatedAt); err != nil {
			return p, err
		}
		trigger, err := NormalizeTrigger(p.Trigger.Value)
		if err != nil {
			return p, err
		}
		p.Trigger = &model.SceneField[model.SceneTrigger]{Value: trigger, UpdatedAt: p.Trigger.UpdatedAt}
	}
	if p.Actions != nil {
		if err := checkTS("actions", p.Actions.UpdatedAt); err != nil {
			return p, err
		}
		actions, err := NormalizeActions(p.Actions.Value)
		if err != nil {
			return p, err
		}
		p.Actions = &model.SceneField[[]model.SceneAction]{Value: actions, UpdatedAt: p.Actions.UpdatedAt}
	}
	return p, nil
}

// NormalizeName 去除首尾空白并校验长度。
func NormalizeName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" || utf8.RuneCountInString(name) > MaxNameRunes {
		return "", fmt.Errorf("%w: 必须为 1..%d 个字符", store.ErrSceneInvalidName, MaxNameRunes)
	}
	return name, nil
}

// NormalizeTrigger 按 kind 校验字段组合与取值范围，并返回规范化结果。
func NormalizeTrigger(t model.SceneTrigger) (model.SceneTrigger, error) {
	t.Kind = strings.ToLower(strings.TrimSpace(t.Kind))

	switch t.Kind {
	case TriggerDeviceOffline:
		if t.Device == nil {
			return t, fmt.Errorf("%w: device_offline 必须绑定设备", store.ErrSceneInvalidTrigger)
		}
		if err := rejectExtra(t, "metric", "deviceType", "rssiThreshold", "timeOfDay", "timezone", "daysOfWeek"); err != nil {
			return t, err
		}
		dev, err := normalizeDevice(*t.Device)
		if err != nil {
			return t, err
		}
		t.Device = &dev
		offline := DefaultOffline
		if t.OfflineSeconds != nil {
			offline = *t.OfflineSeconds
		}
		if offline < MinOfflineSeconds || offline > MaxOfflineSeconds {
			return t, fmt.Errorf("%w: offlineSeconds 必须在 %d..%d 秒之间", store.ErrSceneInvalidTrigger, MinOfflineSeconds, MaxOfflineSeconds)
		}
		t.OfflineSeconds = &offline
		return t, nil

	case TriggerRSSIBelow:
		if t.Device == nil || t.RSSIThreshold == nil {
			return t, fmt.Errorf("%w: rssi_below 必须提供 device 与 rssiThreshold", store.ErrSceneInvalidTrigger)
		}
		if err := rejectExtra(t, "metric", "deviceType", "offlineSeconds", "timeOfDay", "timezone", "daysOfWeek"); err != nil {
			return t, err
		}
		dev, err := normalizeDevice(*t.Device)
		if err != nil {
			return t, err
		}
		t.Device = &dev
		if *t.RSSIThreshold < MinRSSI || *t.RSSIThreshold > MaxRSSI {
			return t, fmt.Errorf("%w: rssiThreshold 必须在 %d..%d dBm 之间", store.ErrSceneInvalidTrigger, MinRSSI, MaxRSSI)
		}
		return t, nil

	case TriggerMetricThreshold:
		if t.Device == nil || t.DeviceType == "" || t.Metric == nil {
			return t, fmt.Errorf("%w: metric_threshold 必须提供 device、deviceType 与 metric", store.ErrSceneInvalidTrigger)
		}
		if err := rejectExtra(t, "offlineSeconds", "rssiThreshold", "timeOfDay", "timezone", "daysOfWeek"); err != nil {
			return t, err
		}
		dev, err := normalizeDevice(*t.Device)
		if err != nil {
			return t, err
		}
		t.Device = &dev
		deviceType := strings.ToLower(strings.TrimSpace(t.DeviceType))
		if !deviceTypePattern.MatchString(deviceType) {
			return t, fmt.Errorf("%w: deviceType 必须是小写字母/数字/下划线", store.ErrSceneInvalidTrigger)
		}
		t.DeviceType = deviceType
		metric := strings.ToLower(strings.TrimSpace(t.Metric.Metric))
		if _, err := validate.NormalizeMetric(metric); err != nil {
			return t, fmt.Errorf("%w: %v", store.ErrSceneInvalidTrigger, err)
		}
		comparator := strings.ToLower(strings.TrimSpace(t.Metric.Comparator))
		if _, ok := comparators[comparator]; !ok {
			return t, fmt.Errorf("%w: comparator 只能是 lt/lte/gt/gte", store.ErrSceneInvalidTrigger)
		}
		if err := validate.CheckFiniteValue(t.Metric.Threshold); err != nil {
			return t, fmt.Errorf("%w: threshold 必须是有限数值", store.ErrSceneInvalidTrigger)
		}
		if t.Metric.Threshold < -1e9 || t.Metric.Threshold > 1e9 {
			return t, fmt.Errorf("%w: threshold 绝对值不能超过 1e9", store.ErrSceneInvalidTrigger)
		}
		if t.Metric.Consecutive < MinConsecutive || t.Metric.Consecutive > MaxConsecutive {
			return t, fmt.Errorf("%w: consecutive 必须在 %d..%d 之间", store.ErrSceneInvalidTrigger, MinConsecutive, MaxConsecutive)
		}
		t.Metric = &model.SceneMetricCondition{
			Metric: metric, Comparator: comparator,
			Threshold: t.Metric.Threshold, Consecutive: t.Metric.Consecutive,
		}
		return t, nil

	case TriggerSchedule:
		if err := rejectExtra(t, "device", "offlineSeconds", "rssiThreshold", "deviceType", "metric"); err != nil {
			return t, err
		}
		timeOfDay, err := normalizeTimeOfDay(t.TimeOfDay)
		if err != nil {
			return t, err
		}
		t.TimeOfDay = timeOfDay
		timezone := strings.TrimSpace(t.Timezone)
		if timezone == "" {
			timezone = "UTC"
		}
		if _, err := time.LoadLocation(timezone); err != nil {
			return t, fmt.Errorf("%w: timezone 必须是合法的 IANA 时区", store.ErrSceneInvalidTrigger)
		}
		t.Timezone = timezone
		if len(t.DaysOfWeek) > 0 {
			seen := map[int]struct{}{}
			out := make([]int, 0, len(t.DaysOfWeek))
			for _, d := range t.DaysOfWeek {
				if d < 1 || d > 7 {
					return t, fmt.Errorf("%w: daysOfWeek 取值必须在 1..7（1=周一）", store.ErrSceneInvalidTrigger)
				}
				if _, ok := seen[d]; ok {
					return t, fmt.Errorf("%w: daysOfWeek 不能重复", store.ErrSceneInvalidTrigger)
				}
				seen[d] = struct{}{}
				out = append(out, d)
			}
			sort.Ints(out)
			t.DaysOfWeek = out
		} else {
			t.DaysOfWeek = nil
		}
		return t, nil

	default:
		return t, fmt.Errorf("%w: kind 必须是 %s/%s/%s/%s", store.ErrSceneInvalidTrigger,
			TriggerDeviceOffline, TriggerRSSIBelow, TriggerMetricThreshold, TriggerSchedule)
	}
}

// NormalizeActions 校验动作数组：1..4 个、种类不重复、字段组合与长度合法。
func NormalizeActions(actions []model.SceneAction) ([]model.SceneAction, error) {
	if len(actions) == 0 || len(actions) > MaxActions {
		return actions, fmt.Errorf("%w: 必须有 1..%d 个动作", store.ErrSceneInvalidAction, MaxActions)
	}
	seen := map[string]struct{}{}
	out := make([]model.SceneAction, 0, len(actions))
	for _, a := range actions {
		kind := strings.ToLower(strings.TrimSpace(a.Kind))
		if _, ok := seen[kind]; ok {
			return actions, fmt.Errorf("%w: 动作种类不能重复", store.ErrSceneInvalidAction)
		}
		seen[kind] = struct{}{}

		switch kind {
		case ActionNotify:
			title := strings.TrimSpace(a.Title)
			body := strings.TrimSpace(a.Body)
			if title == "" || utf8.RuneCountInString(title) > MaxNotifyTitleRunes {
				return actions, fmt.Errorf("%w: notify.title 必须为 1..%d 个字符", store.ErrSceneInvalidAction, MaxNotifyTitleRunes)
			}
			if body == "" || utf8.RuneCountInString(body) > MaxNotifyBodyRunes {
				return actions, fmt.Errorf("%w: notify.body 必须为 1..%d 个字符", store.ErrSceneInvalidAction, MaxNotifyBodyRunes)
			}
			if a.Message != "" || a.Level != "" || a.Mark != "" {
				return actions, fmt.Errorf("%w: notify 不接受 message/level/mark", store.ErrSceneInvalidAction)
			}
			out = append(out, model.SceneAction{Kind: kind, Title: title, Body: body})

		case ActionLog:
			message := strings.TrimSpace(a.Message)
			if message == "" || utf8.RuneCountInString(message) > MaxLogMessageRunes {
				return actions, fmt.Errorf("%w: log.message 必须为 1..%d 个字符", store.ErrSceneInvalidAction, MaxLogMessageRunes)
			}
			level := strings.ToLower(strings.TrimSpace(a.Level))
			if level == "" {
				level = "info"
			}
			if _, ok := logLevels[level]; !ok {
				return actions, fmt.Errorf("%w: log.level 只能是 info/warn", store.ErrSceneInvalidAction)
			}
			if a.Title != "" || a.Body != "" || a.Mark != "" {
				return actions, fmt.Errorf("%w: log 不接受 title/body/mark", store.ErrSceneInvalidAction)
			}
			out = append(out, model.SceneAction{Kind: kind, Message: message, Level: level})

		case ActionMarkDevice:
			mark := strings.TrimSpace(a.Mark)
			if mark == "" || utf8.RuneCountInString(mark) > MaxMarkRunes {
				return actions, fmt.Errorf("%w: mark_device.mark 必须为 1..%d 个字符", store.ErrSceneInvalidAction, MaxMarkRunes)
			}
			if a.Title != "" || a.Body != "" || a.Message != "" || a.Level != "" {
				return actions, fmt.Errorf("%w: mark_device 不接受 title/body/message/level", store.ErrSceneInvalidAction)
			}
			out = append(out, model.SceneAction{Kind: kind, Mark: mark})

		default:
			return actions, fmt.Errorf("%w: kind 必须是 %s/%s/%s", store.ErrSceneInvalidAction,
				ActionNotify, ActionLog, ActionMarkDevice)
		}
	}
	return out, nil
}

// TriggerNeedsDevice 返回触发条件是否绑定了设备。
func TriggerNeedsDevice(t model.SceneTrigger) bool {
	return t.Device != nil && t.Kind != TriggerSchedule
}

// SameTrigger 比较两个已规范化触发条件的内容（含默认值补齐与顺序）。
func SameTrigger(a, b model.SceneTrigger) bool { return reflect.DeepEqual(a, b) }

// SameActions 比较两个已规范化动作数组的内容。
func SameActions(a, b []model.SceneAction) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return reflect.DeepEqual(a, b)
}

var deviceTypePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

func normalizeDevice(d model.SceneDeviceRef) (model.SceneDeviceRef, error) {
	platform, externalID, _, err := validate.ResolveDeviceKey(d.Platform, d.ExternalID, "")
	if err != nil {
		return d, fmt.Errorf("%w: %v", store.ErrSceneInvalidTrigger, err)
	}
	for _, r := range externalID {
		if r < 0x20 || r == 0x7f {
			return d, fmt.Errorf("%w: externalId 含不可见字符", store.ErrSceneInvalidTrigger)
		}
	}
	return model.SceneDeviceRef{Platform: platform, ExternalID: externalID}, nil
}

func normalizeTimeOfDay(raw string) (string, error) {
	v := strings.TrimSpace(raw)
	parts := strings.Split(v, ":")
	if len(parts) != 2 {
		return "", fmt.Errorf("%w: timeOfDay 必须是 HH:MM", store.ErrSceneInvalidTrigger)
	}
	hour, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil || hour < 0 || hour > 23 {
		return "", fmt.Errorf("%w: timeOfDay 小时必须在 00..23", store.ErrSceneInvalidTrigger)
	}
	minute, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil || minute < 0 || minute > 59 {
		return "", fmt.Errorf("%w: timeOfDay 分钟必须在 00..59", store.ErrSceneInvalidTrigger)
	}
	return fmt.Sprintf("%02d:%02d", hour, minute), nil
}

// rejectExtra 拒绝与当前 kind 无关的字段，避免同一语义出现两种写法。
// field 为 "device" 时检查指针，其余按零值判断。
func rejectExtra(t model.SceneTrigger, fields ...string) error {
	for _, field := range fields {
		set := false
		switch field {
		case "device":
			set = t.Device != nil
		case "offlineSeconds":
			set = t.OfflineSeconds != nil
		case "rssiThreshold":
			set = t.RSSIThreshold != nil
		case "deviceType":
			set = t.DeviceType != ""
		case "metric":
			set = t.Metric != nil
		case "timeOfDay":
			set = t.TimeOfDay != ""
		case "timezone":
			set = t.Timezone != ""
		case "daysOfWeek":
			set = len(t.DaysOfWeek) > 0
		}
		if set {
			return fmt.Errorf("%w: %s 与 kind=%s 不匹配", store.ErrSceneInvalidTrigger, field, t.Kind)
		}
	}
	return nil
}
