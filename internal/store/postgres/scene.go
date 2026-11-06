package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/platojobs/PJGoIoT/internal/lww"
	"github.com/platojobs/PJGoIoT/internal/model"
	"github.com/platojobs/PJGoIoT/internal/scene"
	"github.com/platojobs/PJGoIoT/internal/store"
)

// sceneBaseColumns 不含 deviceMissing 子查询，供 UPDATE/DELETE 的 FOR UPDATE 使用，
// 避免 FOR UPDATE 把子查询中的 user_devices 行一起锁住。
const sceneBaseColumns = `s.id, s.name, s.enabled, s.cooldown_seconds, s.trigger, s.actions,
	s.version, s.created_at, s.updated_at, s.deleted_at,
	s.name_updated_at, s.enabled_updated_at, s.cooldown_updated_at,
	s.trigger_updated_at, s.actions_updated_at`

// sceneDeviceMissingExpr 实时判断触发条件绑定的设备是否仍然存在且属于场景所有者。
// scenes.user_id 即场景所有者；设备被删除后场景保留，但读取时 deviceMissing=true。
const sceneDeviceMissingExpr = `CASE WHEN s.trigger->'device' IS NOT NULL AND NOT EXISTS (
	SELECT 1 FROM user_devices d
	WHERE d.user_id = s.user_id AND d.deleted_at IS NULL
	  AND d.platform = s.trigger->'device'->>'platform'
	  AND d.external_id = s.trigger->'device'->>'externalId'
) THEN true ELSE false END`

// sceneState 是场景表一行的完整读取结果，包含字段级 LWW 所需的五个时间戳。
type sceneState struct {
	scene                 model.Scene
	nameTS, enabledTS     time.Time
	cooldownTS, triggerTS time.Time
	actionsTS             time.Time
}

// exportScenes 返回本人全部场景（含 tombstone），供个人数据导出使用。
func (s *Store) exportScenes(ctx context.Context, uid uuid.UUID) ([]model.Scene, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+sceneBaseColumns+`, `+sceneDeviceMissingExpr+`
		FROM scenes s
		WHERE s.user_id = $1
		ORDER BY s.updated_at DESC, s.id ASC`, uid)
	if err != nil {
		return nil, fmt.Errorf("查询导出场景失败: %w", err)
	}
	defer rows.Close()

	out := make([]model.Scene, 0)
	for rows.Next() {
		var st sceneState
		var missing bool
		var rawTrigger, rawActions []byte
		var deleted sql.NullTime
		if err := rows.Scan(&st.scene.ID, &st.scene.Name, &st.scene.Enabled, &st.scene.CooldownSeconds,
			&rawTrigger, &rawActions, &st.scene.Version, &st.scene.CreatedAt, &st.scene.UpdatedAt, &deleted,
			&st.nameTS, &st.enabledTS, &st.cooldownTS, &st.triggerTS, &st.actionsTS, &missing); err != nil {
			return nil, fmt.Errorf("读取导出场景失败: %w", err)
		}
		if deleted.Valid {
			t := deleted.Time.UTC()
			st.scene.DeletedAt = &t
		}
		if err := json.Unmarshal(rawTrigger, &st.scene.Trigger); err != nil {
			return nil, fmt.Errorf("解析场景触发条件失败: %w", err)
		}
		if err := json.Unmarshal(rawActions, &st.scene.Actions); err != nil {
			return nil, fmt.Errorf("解析场景动作失败: %w", err)
		}
		st.scene.DeviceMissing = missing
		out = append(out, st.scene)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历导出场景失败: %w", err)
	}
	return out, nil
}

// ListScenes 实现 store.Store。
func (s *Store) ListScenes(ctx context.Context, userID string) ([]model.Scene, error) {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return nil, store.ErrNotFound
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+sceneBaseColumns+`, `+sceneDeviceMissingExpr+`
		FROM scenes s
		WHERE s.user_id = $1 AND s.deleted_at IS NULL
		ORDER BY s.updated_at DESC, s.id ASC`, uid)
	if err != nil {
		return nil, fmt.Errorf("查询场景列表失败: %w", err)
	}
	defer rows.Close()

	out := make([]model.Scene, 0)
	for rows.Next() {
		var st sceneState
		var missing bool
		var rawTrigger, rawActions []byte
		var deleted sql.NullTime
		if err := rows.Scan(&st.scene.ID, &st.scene.Name, &st.scene.Enabled, &st.scene.CooldownSeconds,
			&rawTrigger, &rawActions, &st.scene.Version, &st.scene.CreatedAt, &st.scene.UpdatedAt, &deleted,
			&st.nameTS, &st.enabledTS, &st.cooldownTS, &st.triggerTS, &st.actionsTS, &missing); err != nil {
			return nil, fmt.Errorf("读取场景失败: %w", err)
		}
		if err := json.Unmarshal(rawTrigger, &st.scene.Trigger); err != nil {
			return nil, fmt.Errorf("解析场景触发条件失败: %w", err)
		}
		if err := json.Unmarshal(rawActions, &st.scene.Actions); err != nil {
			return nil, fmt.Errorf("解析场景动作失败: %w", err)
		}
		st.scene.DeviceMissing = missing
		out = append(out, st.scene)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历场景列表失败: %w", err)
	}
	return out, nil
}

// GetScene 实现 store.Store。
func (s *Store) GetScene(ctx context.Context, userID string, sceneID string) (model.Scene, error) {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return model.Scene{}, store.ErrNotFound
	}
	sid, err := uuid.Parse(sceneID)
	if err != nil {
		return model.Scene{}, store.ErrNotFound
	}
	var st sceneState
	var missing bool
	var rawTrigger, rawActions []byte
	var deleted sql.NullTime
	err = s.db.QueryRowContext(ctx, `SELECT `+sceneBaseColumns+`, `+sceneDeviceMissingExpr+`
		FROM scenes s
		WHERE s.id = $1 AND s.user_id = $2 AND s.deleted_at IS NULL`, sid, uid).
		Scan(&st.scene.ID, &st.scene.Name, &st.scene.Enabled, &st.scene.CooldownSeconds,
			&rawTrigger, &rawActions, &st.scene.Version, &st.scene.CreatedAt, &st.scene.UpdatedAt, &deleted,
			&st.nameTS, &st.enabledTS, &st.cooldownTS, &st.triggerTS, &st.actionsTS, &missing)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Scene{}, store.ErrNotFound
	}
	if err != nil {
		return model.Scene{}, fmt.Errorf("查询场景失败: %w", err)
	}
	if err := json.Unmarshal(rawTrigger, &st.scene.Trigger); err != nil {
		return model.Scene{}, fmt.Errorf("解析场景触发条件失败: %w", err)
	}
	if err := json.Unmarshal(rawActions, &st.scene.Actions); err != nil {
		return model.Scene{}, fmt.Errorf("解析场景动作失败: %w", err)
	}
	st.scene.DeviceMissing = missing
	return st.scene, nil
}

// CreateScene 实现 store.Store。
func (s *Store) CreateScene(ctx context.Context, userID string, input model.SceneInput) (model.Scene, error) {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return model.Scene{}, store.ErrNotFound
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Scene{}, fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()
	q := s.q.WithTx(tx)

	// 与 push 共用同一把用户锁：保证 change_log seq 的分配顺序与提交顺序一致，
	// 避免并发场景写入让客户端游标越过未提交的 seq。
	if err := q.LockIdempotencyKey(ctx, "push:"+uid.String()); err != nil {
		return model.Scene{}, fmt.Errorf("锁定用户变更失败: %w", err)
	}

	enabled := true
	if input.Enabled != nil {
		enabled = *input.Enabled
	}
	cooldown := scene.DefaultCooldown
	if input.CooldownSeconds != nil {
		cooldown = *input.CooldownSeconds
	}
	if err := validateSceneRefs(ctx, tx, uid, input.Trigger); err != nil {
		return model.Scene{}, err
	}

	now := time.Now().UTC()
	id := uuid.New()
	rawTrigger, err := json.Marshal(input.Trigger)
	if err != nil {
		return model.Scene{}, fmt.Errorf("序列化触发条件失败: %w", err)
	}
	rawActions, err := json.Marshal(input.Actions)
	if err != nil {
		return model.Scene{}, fmt.Errorf("序列化动作失败: %w", err)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO scenes (
			id, user_id, name, enabled, cooldown_seconds, trigger, actions,
			name_updated_at, enabled_updated_at, cooldown_updated_at, trigger_updated_at, actions_updated_at,
			version, created_at, updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$8,$8,$8,$8,$9,$8,$8)`,
		id, uid, input.Name, enabled, cooldown, rawTrigger, rawActions, now, int64(1))
	if err != nil {
		return model.Scene{}, fmt.Errorf("写入场景失败: %w", err)
	}

	created := model.Scene{
		ID:              id.String(),
		Name:            input.Name,
		Enabled:         enabled,
		CooldownSeconds: cooldown,
		Trigger:         input.Trigger,
		Actions:         input.Actions,
		Version:         1,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if err := writeSceneChange(ctx, tx, q, uid, id, "upsert", created, now); err != nil {
		return model.Scene{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.Scene{}, fmt.Errorf("提交事务失败: %w", err)
	}
	return created, nil
}

// UpdateScene 实现 store.Store，按字段级 LWW 应用补丁。
func (s *Store) UpdateScene(ctx context.Context, userID string, sceneID string, patch model.ScenePatch, now time.Time) (model.Scene, error) {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return model.Scene{}, store.ErrNotFound
	}
	sid, err := uuid.Parse(sceneID)
	if err != nil {
		return model.Scene{}, store.ErrNotFound
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Scene{}, fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()
	q := s.q.WithTx(tx)
	if err := q.LockIdempotencyKey(ctx, "push:"+uid.String()); err != nil {
		return model.Scene{}, fmt.Errorf("锁定用户变更失败: %w", err)
	}

	var st sceneState
	var rawTrigger, rawActions []byte
	var deleted sql.NullTime
	err = tx.QueryRowContext(ctx, `SELECT `+sceneBaseColumns+`
		FROM scenes s
		WHERE s.id = $1 AND s.user_id = $2 AND s.deleted_at IS NULL
		FOR UPDATE`, sid, uid).
		Scan(&st.scene.ID, &st.scene.Name, &st.scene.Enabled, &st.scene.CooldownSeconds,
			&rawTrigger, &rawActions, &st.scene.Version, &st.scene.CreatedAt, &st.scene.UpdatedAt, &deleted,
			&st.nameTS, &st.enabledTS, &st.cooldownTS, &st.triggerTS, &st.actionsTS)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Scene{}, store.ErrNotFound
	}
	if err != nil {
		return model.Scene{}, fmt.Errorf("查询场景失败: %w", err)
	}
	if err := json.Unmarshal(rawTrigger, &st.scene.Trigger); err != nil {
		return model.Scene{}, fmt.Errorf("解析场景触发条件失败: %w", err)
	}
	if err := json.Unmarshal(rawActions, &st.scene.Actions); err != nil {
		return model.Scene{}, fmt.Errorf("解析场景动作失败: %w", err)
	}

	// 即便某个补丁字段最终因 LWW 落败，也先校验其合法性：
	// 非法组合不应因为时间戳较旧就被静默接受。
	if patch.Trigger != nil {
		if err := validateSceneRefs(ctx, tx, uid, patch.Trigger.Value); err != nil {
			return model.Scene{}, err
		}
	}

	now = now.UTC()
	name, enabled, cooldown := st.scene.Name, st.scene.Enabled, st.scene.CooldownSeconds
	trigger, actions := st.scene.Trigger, st.scene.Actions
	nameTS, enabledTS, cooldownTS := st.nameTS, st.enabledTS, st.cooldownTS
	triggerTS, actionsTS := st.triggerTS, st.actionsTS
	applied := false

	if patch.Name != nil {
		d := lww.DecideField(now, patch.Name.UpdatedAt, st.nameTS, patch.Name.Value == st.scene.Name)
		if d.Apply {
			name, nameTS, applied = patch.Name.Value, d.EffectiveUpdatedAt, true
		}
	}
	if patch.Enabled != nil {
		d := lww.DecideField(now, patch.Enabled.UpdatedAt, st.enabledTS, patch.Enabled.Value == st.scene.Enabled)
		if d.Apply {
			enabled, enabledTS, applied = patch.Enabled.Value, d.EffectiveUpdatedAt, true
		}
	}
	if patch.CooldownSeconds != nil {
		d := lww.DecideField(now, patch.CooldownSeconds.UpdatedAt, st.cooldownTS, patch.CooldownSeconds.Value == st.scene.CooldownSeconds)
		if d.Apply {
			cooldown, cooldownTS, applied = patch.CooldownSeconds.Value, d.EffectiveUpdatedAt, true
		}
	}
	if patch.Trigger != nil {
		d := lww.DecideField(now, patch.Trigger.UpdatedAt, st.triggerTS, scene.SameTrigger(patch.Trigger.Value, st.scene.Trigger))
		if d.Apply {
			trigger, triggerTS, applied = patch.Trigger.Value, d.EffectiveUpdatedAt, true
		}
	}
	if patch.Actions != nil {
		d := lww.DecideField(now, patch.Actions.UpdatedAt, st.actionsTS, scene.SameActions(patch.Actions.Value, st.scene.Actions))
		if d.Apply {
			actions, actionsTS, applied = patch.Actions.Value, d.EffectiveUpdatedAt, true
		}
	}

	// 没有任何字段被应用（全部是幂等重复或服务端更新）时，不改 version、不写 change_log。
	if !applied {
		current := st.scene
		if err := fillSceneDeviceMissing(ctx, tx, uid, &current); err != nil {
			return model.Scene{}, err
		}
		if err := tx.Commit(); err != nil {
			return model.Scene{}, fmt.Errorf("提交事务失败: %w", err)
		}
		return current, nil
	}

	rawTrigger, err = json.Marshal(trigger)
	if err != nil {
		return model.Scene{}, fmt.Errorf("序列化触发条件失败: %w", err)
	}
	rawActions, err = json.Marshal(actions)
	if err != nil {
		return model.Scene{}, fmt.Errorf("序列化动作失败: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE scenes SET
			name = $2, enabled = $3, cooldown_seconds = $4, trigger = $5, actions = $6,
			name_updated_at = $7, enabled_updated_at = $8, cooldown_updated_at = $9,
			trigger_updated_at = $10, actions_updated_at = $11,
			version = version + 1, updated_at = $12
		WHERE id = $1`,
		sid, name, enabled, cooldown, rawTrigger, rawActions,
		nameTS, enabledTS, cooldownTS, triggerTS, actionsTS, now); err != nil {
		return model.Scene{}, fmt.Errorf("更新场景失败: %w", err)
	}

	updated := model.Scene{
		ID:              sid.String(),
		Name:            name,
		Enabled:         enabled,
		CooldownSeconds: cooldown,
		Trigger:         trigger,
		Actions:         actions,
		Version:         st.scene.Version + 1,
		CreatedAt:       st.scene.CreatedAt,
		UpdatedAt:       now,
	}
	if err := fillSceneDeviceMissing(ctx, tx, uid, &updated); err != nil {
		return model.Scene{}, err
	}
	if err := writeSceneChange(ctx, tx, q, uid, sid, "upsert", updated, now); err != nil {
		return model.Scene{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.Scene{}, fmt.Errorf("提交事务失败: %w", err)
	}
	return updated, nil
}

// DeleteScene 实现 store.Store：软删除并写 tombstone 变更。
func (s *Store) DeleteScene(ctx context.Context, userID string, sceneID string, now time.Time) error {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return store.ErrNotFound
	}
	sid, err := uuid.Parse(sceneID)
	if err != nil {
		return store.ErrNotFound
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()
	q := s.q.WithTx(tx)
	if err := q.LockIdempotencyKey(ctx, "push:"+uid.String()); err != nil {
		return fmt.Errorf("锁定用户变更失败: %w", err)
	}

	var st sceneState
	var rawTrigger, rawActions []byte
	var deleted sql.NullTime
	err = tx.QueryRowContext(ctx, `SELECT `+sceneBaseColumns+`
		FROM scenes s
		WHERE s.id = $1 AND s.user_id = $2 AND s.deleted_at IS NULL
		FOR UPDATE`, sid, uid).
		Scan(&st.scene.ID, &st.scene.Name, &st.scene.Enabled, &st.scene.CooldownSeconds,
			&rawTrigger, &rawActions, &st.scene.Version, &st.scene.CreatedAt, &st.scene.UpdatedAt, &deleted,
			&st.nameTS, &st.enabledTS, &st.cooldownTS, &st.triggerTS, &st.actionsTS)
	if errors.Is(err, sql.ErrNoRows) {
		return store.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("查询场景失败: %w", err)
	}
	if err := json.Unmarshal(rawTrigger, &st.scene.Trigger); err != nil {
		return fmt.Errorf("解析场景触发条件失败: %w", err)
	}
	if err := json.Unmarshal(rawActions, &st.scene.Actions); err != nil {
		return fmt.Errorf("解析场景动作失败: %w", err)
	}
	if err := fillSceneDeviceMissing(ctx, tx, uid, &st.scene); err != nil {
		return err
	}

	now = now.UTC()
	if _, err := tx.ExecContext(ctx, `UPDATE scenes
		SET deleted_at = $2, updated_at = $2, version = version + 1
		WHERE id = $1`, sid, now); err != nil {
		return fmt.Errorf("删除场景失败: %w", err)
	}

	deletedAt := now
	tombstone := st.scene
	tombstone.DeletedAt = &deletedAt
	tombstone.UpdatedAt = now
	tombstone.Version = st.scene.Version + 1
	if err := writeSceneChange(ctx, tx, q, uid, sid, "delete", tombstone, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交事务失败: %w", err)
	}
	return nil
}

// validateSceneRefs 在事务内校验场景引用的设备与指标：
// 设备必须属于本人且未删除；指标越界还要求设备类型存在于目录且该类型支持该指标。
func validateSceneRefs(ctx context.Context, db rowQueryer, uid uuid.UUID, trigger model.SceneTrigger) error {
	if trigger.Device != nil {
		var exists bool
		if err := db.QueryRowContext(ctx, `SELECT EXISTS (
			SELECT 1 FROM user_devices
			WHERE user_id = $1 AND platform = $2 AND external_id = $3 AND deleted_at IS NULL
		)`, uid, trigger.Device.Platform, trigger.Device.ExternalID).Scan(&exists); err != nil {
			return fmt.Errorf("校验场景设备失败: %w", err)
		}
		if !exists {
			return fmt.Errorf("%w: %s/%s", store.ErrSceneDeviceNotFound, trigger.Device.Platform, trigger.Device.ExternalID)
		}
	}

	if trigger.Kind == scene.TriggerMetricThreshold && trigger.Metric != nil {
		var rawMetrics []byte
		err := db.QueryRowContext(ctx, `SELECT metrics FROM device_type_catalog
			WHERE type = $1 AND deleted_at IS NULL`, trigger.DeviceType).Scan(&rawMetrics)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: 设备类型 %q 不存在", store.ErrSceneMetricNotSupported, trigger.DeviceType)
		}
		if err != nil {
			return fmt.Errorf("校验场景指标失败: %w", err)
		}
		var metrics []model.CatalogMetric
		if err := json.Unmarshal(rawMetrics, &metrics); err != nil {
			return fmt.Errorf("解析目录指标失败: %w", err)
		}
		for _, m := range metrics {
			if m.Key == trigger.Metric.Metric {
				return nil
			}
		}
		return fmt.Errorf("%w: 设备类型 %q 不支持指标 %q", store.ErrSceneMetricNotSupported, trigger.DeviceType, trigger.Metric.Metric)
	}
	return nil
}

// fillSceneDeviceMissing 为已读取的场景实时计算 deviceMissing。
func fillSceneDeviceMissing(ctx context.Context, db rowQueryer, uid uuid.UUID, sc *model.Scene) error {
	if !scene.TriggerNeedsDevice(sc.Trigger) {
		sc.DeviceMissing = false
		return nil
	}
	var exists bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM user_devices
		WHERE user_id = $1 AND platform = $2 AND external_id = $3 AND deleted_at IS NULL
	)`, uid, sc.Trigger.Device.Platform, sc.Trigger.Device.ExternalID).Scan(&exists); err != nil {
		return fmt.Errorf("校验场景设备失败: %w", err)
	}
	sc.DeviceMissing = !exists
	return nil
}

// writeSceneChange 在同一事务内分配 change_log 序列并写入完整场景快照。
// 场景只对本人可见，household_id 恒为 NULL。
func writeSceneChange(ctx context.Context, tx *sql.Tx, q interface {
	NextChangeSeq(ctx context.Context) (int64, error)
}, uid uuid.UUID, sceneID uuid.UUID, op string, sc model.Scene, now time.Time) error {
	seq, err := q.NextChangeSeq(ctx)
	if err != nil {
		return fmt.Errorf("分配变更序列失败: %w", err)
	}
	payload, err := json.Marshal(sc)
	if err != nil {
		return fmt.Errorf("序列化场景变更失败: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO change_log
		(seq, user_id, household_id, entity, entity_id, op, payload, server_ts)
		VALUES ($1::bigint, $2::uuid, NULL, 'scene', $3::text, $4::text, $5::jsonb, $6::timestamptz)`,
		seq, uid, sceneID, op, payload, now); err != nil {
		return fmt.Errorf("写入场景变更失败: %w", err)
	}
	return nil
}
