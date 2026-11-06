package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/platojobs/PJGoIoT/internal/model"
	"github.com/platojobs/PJGoIoT/internal/store"
)

// upsertPushDeviceSQL 命中 (user_id, token) 唯一约束时视为幂等更新：
// 令牌重新注册会把已失效状态恢复为 active，并清空 last_error。
const upsertPushDeviceSQL = `
INSERT INTO push_devices (id, user_id, platform, token, app_version, device_model, status, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, 'active', $7, $7)
ON CONFLICT (user_id, token) DO UPDATE SET
    platform = EXCLUDED.platform,
    app_version = EXCLUDED.app_version,
    device_model = EXCLUDED.device_model,
    status = 'active',
    last_error = NULL,
    updated_at = EXCLUDED.updated_at
RETURNING id, platform, token, app_version, device_model, status, created_at, updated_at`

// UpsertPushDevice 实现 store.Store。
func (s *Store) UpsertPushDevice(ctx context.Context, userID string, input model.PushRegistrationInput, now time.Time) (model.PushRegistration, error) {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return model.PushRegistration{}, store.ErrNotFound
	}
	platform := strings.TrimSpace(strings.ToLower(input.Platform))
	if platform != "android" && platform != "ios" {
		return model.PushRegistration{}, fmt.Errorf("%w: platform 只能是 android 或 ios", store.ErrPushDeviceInvalid)
	}
	token := strings.TrimSpace(input.Token)
	if token == "" || len(token) > 4096 {
		return model.PushRegistration{}, fmt.Errorf("%w: token 长度必须在 1..4096", store.ErrPushDeviceInvalid)
	}
	var d model.PushRegistration
	err = s.db.QueryRowContext(ctx, upsertPushDeviceSQL,
		uuid.NewString(), uid, platform, token, strings.TrimSpace(input.AppVersion), strings.TrimSpace(input.DeviceModel), now.UTC()).
		Scan(&d.ID, &d.Platform, &d.Token, &d.AppVersion, &d.DeviceModel, &d.Status, &d.CreatedAt, &d.UpdatedAt)
	if err != nil {
		if isForeignKeyViolation(err) {
			return model.PushRegistration{}, store.ErrNotFound
		}
		return model.PushRegistration{}, fmt.Errorf("注册推送设备失败: %w", err)
	}
	d.CreatedAt = d.CreatedAt.UTC()
	d.UpdatedAt = d.UpdatedAt.UTC()
	return d, nil
}

// DeactivatePushDevice 实现 store.Store。
func (s *Store) DeactivatePushDevice(ctx context.Context, userID string, deviceID string, now time.Time) error {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return store.ErrNotFound
	}
	did, err := uuid.Parse(deviceID)
	if err != nil {
		return store.ErrNotFound
	}
	res, err := s.db.ExecContext(ctx, `
UPDATE push_devices SET status = 'inactive', updated_at = $3
WHERE id = $1 AND user_id = $2`, did, uid, now.UTC())
	if err != nil {
		return fmt.Errorf("停用推送设备失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("读取停用结果失败: %w", err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// CreateAlertWithDeliveries 实现 store.Store：告警与投递在同一事务写入。
func (s *Store) CreateAlertWithDeliveries(ctx context.Context, input model.AlertInput, now time.Time) (model.Alert, int, error) {
	uid, err := uuid.Parse(input.UserID)
	if err != nil {
		return model.Alert{}, 0, store.ErrNotFound
	}
	payload := input.Payload
	if payload == nil {
		payload = map[string]string{}
	}
	data := map[string]string{
		"alertId":  "",
		"kind":     input.Kind,
		"severity": input.Severity,
	}
	for k, v := range payload {
		data[k] = v
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Alert{}, 0, fmt.Errorf("开启告警事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE id = $1)`, uid).Scan(&exists); err != nil {
		return model.Alert{}, 0, fmt.Errorf("检查用户失败: %w", err)
	}
	if !exists {
		return model.Alert{}, 0, store.ErrNotFound
	}

	alert := model.Alert{
		ID:        uuid.NewString(),
		UserID:    uid.String(),
		Kind:      input.Kind,
		Title:     input.Title,
		Body:      input.Body,
		Severity:  input.Severity,
		Payload:   payload,
		CreatedAt: now.UTC(),
	}
	data["alertId"] = alert.ID
	rawData, err := json.Marshal(data)
	if err != nil {
		return model.Alert{}, 0, fmt.Errorf("序列化告警载荷失败: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
INSERT INTO alerts (id, user_id, kind, title, body, severity, payload, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		alert.ID, uid, alert.Kind, alert.Title, alert.Body, alert.Severity, rawData, alert.CreatedAt); err != nil {
		return model.Alert{}, 0, fmt.Errorf("写入告警失败: %w", err)
	}

	rows, err := tx.QueryContext(ctx, `
SELECT id, platform, token FROM push_devices
WHERE user_id = $1 AND status = 'active'
ORDER BY id`, uid)
	if err != nil {
		return model.Alert{}, 0, fmt.Errorf("查询推送设备失败: %w", err)
	}
	defer rows.Close()

	type deviceSnapshot struct {
		id       uuid.UUID
		platform string
		token    string
	}
	devices := make([]deviceSnapshot, 0)
	for rows.Next() {
		var d deviceSnapshot
		if err := rows.Scan(&d.id, &d.platform, &d.token); err != nil {
			return model.Alert{}, 0, fmt.Errorf("读取推送设备失败: %w", err)
		}
		devices = append(devices, d)
	}
	if err := rows.Err(); err != nil {
		return model.Alert{}, 0, fmt.Errorf("遍历推送设备失败: %w", err)
	}

	for _, d := range devices {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO push_deliveries (id, user_id, alert_id, push_device_id, platform, token, title, body, data,
    status, attempts, next_attempt_at, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'pending', 0, $10, $10)`,
			uuid.NewString(), uid, alert.ID, d.id, d.platform, d.token, alert.Title, alert.Body, rawData, alert.CreatedAt); err != nil {
			return model.Alert{}, 0, fmt.Errorf("写入投递队列失败: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return model.Alert{}, 0, fmt.Errorf("提交告警事务失败: %w", err)
	}
	return alert, len(devices), nil
}

// ClaimPushDeliveries 实现 store.Store。
func (s *Store) ClaimPushDeliveries(ctx context.Context, now time.Time, limit int, sendingStaleBefore time.Time) ([]model.PushDelivery, error) {
	if limit <= 0 {
		limit = 50
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("开启投递抢占事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 崩溃恢复：进程被 SIGKILL 后 sending 永远不会再被推进，超过阈值后重新入队。
	if _, err := tx.ExecContext(ctx, `
UPDATE push_deliveries
SET status = 'pending', claimed_at = NULL, next_attempt_at = $1
WHERE status = 'sending' AND claimed_at < $2`, now.UTC(), sendingStaleBefore.UTC()); err != nil {
		return nil, fmt.Errorf("回收停滞投递失败: %w", err)
	}

	rows, err := tx.QueryContext(ctx, `
UPDATE push_deliveries d
SET status = 'sending', claimed_at = $1, attempts = d.attempts + 1
FROM (
    SELECT id FROM push_deliveries
    WHERE status = 'pending' AND next_attempt_at <= $1
    ORDER BY next_attempt_at, id
    LIMIT $2
    FOR UPDATE SKIP LOCKED
) c
WHERE d.id = c.id
RETURNING d.id, d.user_id, d.alert_id, d.push_device_id, d.platform, d.token, d.title, d.body,
    d.data, d.attempts, d.next_attempt_at, d.created_at`, now.UTC(), limit)
	if err != nil {
		return nil, fmt.Errorf("抢占投递失败: %w", err)
	}
	defer rows.Close()

	out := make([]model.PushDelivery, 0, limit)
	for rows.Next() {
		var d model.PushDelivery
		var rawData []byte
		if err := rows.Scan(&d.ID, &d.UserID, &d.AlertID, &d.PushDeviceID, &d.Platform, &d.Token,
			&d.Title, &d.Body, &rawData, &d.Attempts, &d.NextAttemptAt, &d.CreatedAt); err != nil {
			return nil, fmt.Errorf("读取投递失败: %w", err)
		}
		if err := json.Unmarshal(rawData, &d.Data); err != nil {
			return nil, fmt.Errorf("解析投递载荷失败: %w", err)
		}
		d.NextAttemptAt = d.NextAttemptAt.UTC()
		d.CreatedAt = d.CreatedAt.UTC()
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历投递失败: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("提交投递抢占事务失败: %w", err)
	}
	return out, nil
}

// CompletePushDelivery 实现 store.Store。
func (s *Store) CompletePushDelivery(ctx context.Context, deliveryID string, provider string, dryRun bool, sentAt time.Time) error {
	did, err := uuid.Parse(deliveryID)
	if err != nil {
		return store.ErrNotFound
	}
	res, err := s.db.ExecContext(ctx, `
UPDATE push_deliveries
SET status = 'sent', provider = $2, dry_run = $3, sent_at = $4,
    claimed_at = NULL, last_error = NULL, terminal_reason = NULL
WHERE id = $1`, did, provider, dryRun, sentAt.UTC())
	if err != nil {
		return fmt.Errorf("标记投递成功失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// RetryPushDelivery 实现 store.Store。
func (s *Store) RetryPushDelivery(ctx context.Context, deliveryID string, nextAttemptAt time.Time, lastError string) error {
	did, err := uuid.Parse(deliveryID)
	if err != nil {
		return store.ErrNotFound
	}
	res, err := s.db.ExecContext(ctx, `
UPDATE push_deliveries
SET status = 'pending', next_attempt_at = $2, last_error = $3, claimed_at = NULL
WHERE id = $1`, did, nextAttemptAt.UTC(), lastError)
	if err != nil {
		return fmt.Errorf("安排投递重试失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// FailPushDelivery 实现 store.Store；deactivate=true 时同一事务把令牌置 inactive。
func (s *Store) FailPushDelivery(ctx context.Context, deliveryID string, reason string, deactivate bool, now time.Time) error {
	did, err := uuid.Parse(deliveryID)
	if err != nil {
		return store.ErrNotFound
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启投递失败事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var deviceID uuid.UUID
	if err := tx.QueryRowContext(ctx, `SELECT push_device_id FROM push_deliveries WHERE id = $1`, did).Scan(&deviceID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return store.ErrNotFound
		}
		return fmt.Errorf("查询投递设备失败: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE push_deliveries
SET status = 'failed', terminal_reason = $2, last_error = $2, claimed_at = NULL
WHERE id = $1`, did, reason); err != nil {
		return fmt.Errorf("标记投递失败失败: %w", err)
	}
	if deactivate {
		if _, err := tx.ExecContext(ctx, `
UPDATE push_devices SET status = 'inactive', last_error = $2, updated_at = $3
WHERE id = $1`, deviceID, reason, now.UTC()); err != nil {
			return fmt.Errorf("停用失效令牌失败: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交投递失败事务失败: %w", err)
	}
	return nil
}

// CleanupPushDeliveries 实现 store.Store。
func (s *Store) CleanupPushDeliveries(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
DELETE FROM push_deliveries
WHERE status IN ('sent', 'failed') AND created_at < $1`, before.UTC())
	if err != nil {
		return 0, fmt.Errorf("清理推送投递失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("读取清理推送投递结果失败: %w", err)
	}
	return n, nil
}

// CleanupPushDevices 实现 store.Store。
func (s *Store) CleanupPushDevices(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
DELETE FROM push_devices
WHERE status = 'inactive' AND updated_at < $1`, before.UTC())
	if err != nil {
		return 0, fmt.Errorf("清理失效推送设备失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("读取清理失效设备结果失败: %w", err)
	}
	return n, nil
}

// isForeignKeyViolation 识别 PostgreSQL 外键冲突（23503），用于把「用户不存在」映射为 404。
func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}
