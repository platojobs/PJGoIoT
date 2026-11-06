package fixture

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/platojobs/PJGoIoT/internal/lww"
	"github.com/platojobs/PJGoIoT/internal/model"
	"github.com/platojobs/PJGoIoT/internal/scene"
	"github.com/platojobs/PJGoIoT/internal/store"
)

// sceneRecord 是内存版场景，scene 内是完整状态，其余五个字段是字段级 LWW 时间戳。
type sceneRecord struct {
	scene                 model.Scene
	nameTS, enabledTS     time.Time
	cooldownTS, triggerTS time.Time
	actionsTS             time.Time
}

// ListScenes 实现 store.Store。
func (s *Store) ListScenes(ctx context.Context, userID string) ([]model.Scene, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	data, ok := s.users[userID]
	if !ok {
		return nil, store.ErrNotFound
	}
	out := make([]model.Scene, 0, len(data.scenes))
	for _, rec := range data.scenes {
		if rec.scene.DeletedAt != nil {
			continue
		}
		sc := copyScene(rec.scene)
		sc.DeviceMissing = sceneDeviceMissingLocked(data, sc.Trigger)
		out = append(out, sc)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	return out, nil
}

// GetScene 实现 store.Store。
func (s *Store) GetScene(ctx context.Context, userID string, sceneID string) (model.Scene, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	data, ok := s.users[userID]
	if !ok {
		return model.Scene{}, store.ErrNotFound
	}
	rec, ok := data.scenes[sceneID]
	if !ok || rec.scene.DeletedAt != nil {
		return model.Scene{}, store.ErrNotFound
	}
	sc := copyScene(rec.scene)
	sc.DeviceMissing = sceneDeviceMissingLocked(data, sc.Trigger)
	return sc, nil
}

// CreateScene 实现 store.Store。
func (s *Store) CreateScene(ctx context.Context, userID string, input model.SceneInput) (model.Scene, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, ok := s.users[userID]
	if !ok {
		return model.Scene{}, store.ErrNotFound
	}
	if err := validateSceneRefsLocked(s, data, input.Trigger); err != nil {
		return model.Scene{}, err
	}

	now := time.Now().UTC()
	enabled := true
	if input.Enabled != nil {
		enabled = *input.Enabled
	}
	cooldown := scene.DefaultCooldown
	if input.CooldownSeconds != nil {
		cooldown = *input.CooldownSeconds
	}

	sc := model.Scene{
		ID:              uuid.NewString(),
		Name:            input.Name,
		Enabled:         enabled,
		CooldownSeconds: cooldown,
		Trigger:         copyTrigger(input.Trigger),
		Actions:         copyActions(input.Actions),
		Version:         1,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	data.scenes[sc.ID] = &sceneRecord{
		scene:      sc,
		nameTS:     now,
		enabledTS:  now,
		cooldownTS: now,
		triggerTS:  now,
		actionsTS:  now,
	}
	s.appendSceneChangeLocked(userID, sc.ID, "upsert", sc, now)
	return copyScene(sc), nil
}

// UpdateScene 实现 store.Store，按字段级 LWW 应用补丁。
func (s *Store) UpdateScene(ctx context.Context, userID string, sceneID string, patch model.ScenePatch, now time.Time) (model.Scene, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, ok := s.users[userID]
	if !ok {
		return model.Scene{}, store.ErrNotFound
	}
	rec, ok := data.scenes[sceneID]
	if !ok || rec.scene.DeletedAt != nil {
		return model.Scene{}, store.ErrNotFound
	}
	if patch.Trigger != nil {
		if err := validateSceneRefsLocked(s, data, patch.Trigger.Value); err != nil {
			return model.Scene{}, err
		}
	}

	now = now.UTC()
	current := rec.scene
	applied := false

	if patch.Name != nil {
		d := lww.DecideField(now, patch.Name.UpdatedAt, rec.nameTS, patch.Name.Value == current.Name)
		if d.Apply {
			current.Name, rec.nameTS, applied = patch.Name.Value, d.EffectiveUpdatedAt, true
		}
	}
	if patch.Enabled != nil {
		d := lww.DecideField(now, patch.Enabled.UpdatedAt, rec.enabledTS, patch.Enabled.Value == current.Enabled)
		if d.Apply {
			current.Enabled, rec.enabledTS, applied = patch.Enabled.Value, d.EffectiveUpdatedAt, true
		}
	}
	if patch.CooldownSeconds != nil {
		d := lww.DecideField(now, patch.CooldownSeconds.UpdatedAt, rec.cooldownTS, patch.CooldownSeconds.Value == current.CooldownSeconds)
		if d.Apply {
			current.CooldownSeconds, rec.cooldownTS, applied = patch.CooldownSeconds.Value, d.EffectiveUpdatedAt, true
		}
	}
	if patch.Trigger != nil {
		d := lww.DecideField(now, patch.Trigger.UpdatedAt, rec.triggerTS, scene.SameTrigger(patch.Trigger.Value, current.Trigger))
		if d.Apply {
			current.Trigger, rec.triggerTS, applied = copyTrigger(patch.Trigger.Value), d.EffectiveUpdatedAt, true
		}
	}
	if patch.Actions != nil {
		d := lww.DecideField(now, patch.Actions.UpdatedAt, rec.actionsTS, scene.SameActions(patch.Actions.Value, current.Actions))
		if d.Apply {
			current.Actions, rec.actionsTS, applied = copyActions(patch.Actions.Value), d.EffectiveUpdatedAt, true
		}
	}
	if !applied {
		out := copyScene(current)
		out.DeviceMissing = sceneDeviceMissingLocked(data, out.Trigger)
		return out, nil
	}

	current.Version++
	current.UpdatedAt = now
	rec.scene = current
	s.appendSceneChangeLocked(userID, sceneID, "upsert", current, now)

	out := copyScene(current)
	out.DeviceMissing = sceneDeviceMissingLocked(data, out.Trigger)
	return out, nil
}

// DeleteScene 实现 store.Store。
func (s *Store) DeleteScene(ctx context.Context, userID string, sceneID string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, ok := s.users[userID]
	if !ok {
		return store.ErrNotFound
	}
	rec, ok := data.scenes[sceneID]
	if !ok || rec.scene.DeletedAt != nil {
		return store.ErrNotFound
	}

	now = now.UTC()
	deletedAt := now
	rec.scene.DeletedAt = &deletedAt
	rec.scene.UpdatedAt = now
	rec.scene.Version++
	s.appendSceneChangeLocked(userID, sceneID, "delete", rec.scene, now)
	return nil
}

func (s *Store) appendSceneChangeLocked(userID, sceneID, op string, sc model.Scene, now time.Time) {
	payload, _ := json.Marshal(sc)
	s.changes = append(s.changes, changeRecord{
		seq:         s.nextSeq,
		userID:      userID,
		householdID: "",
		entity:      "scene",
		entityID:    sceneID,
		op:          op,
		payload:     payload,
		serverTS:    now,
	})
	s.nextSeq++
}

// validateSceneRefsLocked 校验设备归属与目录指标，调用方必须已持有写锁。
func validateSceneRefsLocked(s *Store, data *userData, trigger model.SceneTrigger) error {
	if trigger.Device != nil {
		dev, ok := data.byKey[trigger.Device.Platform+"|"+trigger.Device.ExternalID]
		if !ok || dev.DeletedAt != nil {
			return fmt.Errorf("%w: %s/%s", store.ErrSceneDeviceNotFound, trigger.Device.Platform, trigger.Device.ExternalID)
		}
	}
	if trigger.Kind == scene.TriggerMetricThreshold && trigger.Metric != nil {
		for i := range s.catalog {
			if s.catalog[i].Type != trigger.DeviceType {
				continue
			}
			for _, m := range s.catalog[i].Metrics {
				if m.Key == trigger.Metric.Metric {
					return nil
				}
			}
			return fmt.Errorf("%w: 设备类型 %q 不支持指标 %q", store.ErrSceneMetricNotSupported, trigger.DeviceType, trigger.Metric.Metric)
		}
		return fmt.Errorf("%w: 设备类型 %q 不存在", store.ErrSceneMetricNotSupported, trigger.DeviceType)
	}
	return nil
}

// sceneDeviceMissingLocked 计算设备是否已缺失；调用方必须持有读锁或写锁。
func sceneDeviceMissingLocked(data *userData, trigger model.SceneTrigger) bool {
	if !scene.TriggerNeedsDevice(trigger) {
		return false
	}
	dev, ok := data.byKey[trigger.Device.Platform+"|"+trigger.Device.ExternalID]
	return !ok || dev.DeletedAt != nil
}

func copyScene(sc model.Scene) model.Scene {
	sc.Trigger = copyTrigger(sc.Trigger)
	sc.Actions = copyActions(sc.Actions)
	return sc
}

func copyTrigger(t model.SceneTrigger) model.SceneTrigger {
	if t.Device != nil {
		d := *t.Device
		t.Device = &d
	}
	if t.OfflineSeconds != nil {
		v := *t.OfflineSeconds
		t.OfflineSeconds = &v
	}
	if t.RSSIThreshold != nil {
		v := *t.RSSIThreshold
		t.RSSIThreshold = &v
	}
	if t.Metric != nil {
		m := *t.Metric
		t.Metric = &m
	}
	if t.DaysOfWeek != nil {
		t.DaysOfWeek = append([]int(nil), t.DaysOfWeek...)
	}
	return t
}

func copyActions(actions []model.SceneAction) []model.SceneAction {
	if actions == nil {
		return []model.SceneAction{}
	}
	return append([]model.SceneAction(nil), actions...)
}
