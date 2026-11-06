package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/platojobs/PJGoIoT/internal/auth"
	"github.com/platojobs/PJGoIoT/internal/model"
	"github.com/platojobs/PJGoIoT/internal/store"
	"github.com/platojobs/PJGoIoT/internal/store/postgres/sqlcgen"
)

// GetUser 实现 store.Store。
func (s *Store) GetUser(ctx context.Context, userID string) (model.User, error) {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return model.User{}, fmt.Errorf("非法 user id: %w", err)
	}
	var user model.User
	var id uuid.UUID
	if err := s.db.QueryRowContext(ctx, `SELECT id, kind, created_at FROM users WHERE id = $1`, uid).
		Scan(&id, &user.Kind, &user.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.User{}, store.ErrUnauthorized
		}
		return model.User{}, fmt.Errorf("查询用户失败: %w", err)
	}
	user.ID = id.String()
	user.CreatedAt = user.CreatedAt.UTC()
	return user, nil
}

// CreateDeletionToken 实现 store.Store。
// 同一账号只保留一个未使用的删除令牌：新申请先删除旧令牌再写入，保证并发申请也不会留下两个可用令牌。
func (s *Store) CreateDeletionToken(ctx context.Context, userID string, tokenHash string, expiresAt time.Time) error {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return fmt.Errorf("非法 user id: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM account_deletion_tokens WHERE user_id = $1 AND used_at IS NULL`, uid); err != nil {
		return fmt.Errorf("回收旧删除令牌失败: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO account_deletion_tokens (token_hash, user_id, expires_at) VALUES ($1, $2, $3)`,
		tokenHash, uid, expiresAt.UTC()); err != nil {
		return fmt.Errorf("写入删除令牌失败: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交事务失败: %w", err)
	}
	return nil
}

// DeleteAccount 实现 store.Store。
//
// 所有删除在同一个事务内完成：先按令牌定位账号，再检查 owner 家庭是否还有其他成员，
// 然后按外键安全顺序逐表显式删除，最后删除 users 行并把令牌标记为已使用。
// 任一步失败都整体回滚，不会留下部分删除的账号。
func (s *Store) DeleteAccount(ctx context.Context, tokenHash string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	var uid uuid.UUID
	var expiresAt time.Time
	var usedAt sql.NullTime
	err = tx.QueryRowContext(ctx, `
		SELECT user_id, expires_at, used_at
		FROM account_deletion_tokens WHERE token_hash = $1 FOR UPDATE`, tokenHash).
		Scan(&uid, &expiresAt, &usedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return store.ErrDeletionTokenInvalid
	}
	if err != nil {
		return fmt.Errorf("查询删除令牌失败: %w", err)
	}
	if usedAt.Valid {
		// 令牌已用于一次删除：账号已不存在，重放按 404 处理，避免把重放当成未知错误。
		return store.ErrNotFound
	}
	if !expiresAt.After(now) {
		return store.ErrDeletionTokenExpired
	}

	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE id = $1)`, uid).Scan(&exists); err != nil {
		return fmt.Errorf("查询用户失败: %w", err)
	}
	if !exists {
		return store.ErrNotFound
	}

	// owner 的家庭还有其他成员时不能删除：直接删 users 会级联删掉家庭与成员关系，
	// 对其他成员造成孤儿状态。必须先转让所有权或移除成员。
	var blockedNames []string
	rows, err := tx.QueryContext(ctx, `
		SELECT h.name FROM households h
		WHERE h.owner_user_id = $1
		  AND EXISTS (SELECT 1 FROM household_members m
		              WHERE m.household_id = h.id AND m.user_id <> $1)
		ORDER BY h.created_at, h.id`, uid)
	if err != nil {
		return fmt.Errorf("检查家庭成员失败: %w", err)
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return fmt.Errorf("解析家庭名失败: %w", err)
		}
		blockedNames = append(blockedNames, name)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("遍历家庭失败: %w", err)
	}
	rows.Close()
	if len(blockedNames) > 0 {
		return store.ErrAccountDeletionBlocked
	}

	// 显式逐表删除：不依赖外键级联，删除范围在代码里可见，也便于集成测试逐表断言。
	// 顺序遵循外键依赖：先删引用方，再删被引用方。
	stmts := []struct {
		name string
		sql  string
	}{
		{"feedback_status_events", `DELETE FROM feedback_status_events WHERE feedback_id IN (SELECT id FROM feedback WHERE user_id = $1)`},
		{"feedback", `DELETE FROM feedback WHERE user_id = $1`},
		{"personal_data_export_tasks", `DELETE FROM personal_data_export_tasks WHERE user_id = $1`},
		{"device_type_catalog_accesses", `DELETE FROM device_type_catalog_accesses WHERE user_id = $1`},
		{"household_invitations", `DELETE FROM household_invitations
			WHERE invited_by_user_id = $1
			   OR household_id IN (SELECT id FROM households WHERE owner_user_id = $1)`},
		{"household_rooms", `DELETE FROM household_rooms
			WHERE household_id IN (SELECT id FROM households WHERE owner_user_id = $1)`},
		{"user_devices", `DELETE FROM user_devices WHERE user_id = $1`},
		{"readings", `DELETE FROM readings WHERE user_id = $1`},
		{"scenes", `DELETE FROM scenes WHERE user_id = $1`},
		{"change_log", `DELETE FROM change_log WHERE user_id = $1`},
		{"idempotency_keys", `DELETE FROM idempotency_keys WHERE user_id = $1`},
		{"pairing_codes", `DELETE FROM pairing_codes WHERE user_id = $1`},
		{"household_members", `DELETE FROM household_members
			WHERE user_id = $1
			   OR household_id IN (SELECT id FROM households WHERE owner_user_id = $1)`},
		{"households", `DELETE FROM households WHERE owner_user_id = $1`},
		{"tokens", `DELETE FROM tokens WHERE user_id = $1`},
		{"users", `DELETE FROM users WHERE id = $1`},
	}
	for _, stmt := range stmts {
		if _, err := tx.ExecContext(ctx, stmt.sql, uid); err != nil {
			return fmt.Errorf("删除 %s 失败: %w", stmt.name, err)
		}
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE account_deletion_tokens SET used_at = $2 WHERE token_hash = $1`, tokenHash, now.UTC()); err != nil {
		return fmt.Errorf("标记删除令牌失败: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交事务失败: %w", err)
	}
	return nil
}

// CreateExportTask 实现 store.Store：插入一个 pending 任务。
func (s *Store) CreateExportTask(ctx context.Context, userID string, expiresAt time.Time) (model.ExportTaskStatus, error) {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return model.ExportTaskStatus{}, fmt.Errorf("非法 user id: %w", err)
	}
	id := uuid.New()
	var createdAt time.Time
	if err := s.db.QueryRowContext(ctx, `
		INSERT INTO personal_data_export_tasks (id, user_id, status, expires_at)
		VALUES ($1, $2, 'pending', $3)
		RETURNING created_at`, id, uid, expiresAt.UTC()).Scan(&createdAt); err != nil {
		return model.ExportTaskStatus{}, fmt.Errorf("创建导出任务失败: %w", err)
	}
	return model.ExportTaskStatus{
		ID:        id.String(),
		Status:    "pending",
		CreatedAt: createdAt.UTC(),
		ExpiresAt: expiresAt.UTC(),
	}, nil
}

// BuildPersonalDataExport 实现 store.Store。
// 只读取调用账号自己的数据；家庭仅取本人角色与房间，反馈不含 internal_note。
func (s *Store) BuildPersonalDataExport(ctx context.Context, userID string) (model.PersonalDataExport, error) {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return model.PersonalDataExport{}, fmt.Errorf("非法 user id: %w", err)
	}
	user, err := s.GetUser(ctx, userID)
	if err != nil {
		return model.PersonalDataExport{}, err
	}
	devices, err := s.exportDevices(ctx, uid)
	if err != nil {
		return model.PersonalDataExport{}, err
	}
	readings, err := s.exportReadingAggregates(ctx, uid)
	if err != nil {
		return model.PersonalDataExport{}, err
	}
	households, err := s.exportHouseholds(ctx, uid)
	if err != nil {
		return model.PersonalDataExport{}, err
	}
	scenes, err := s.exportScenes(ctx, uid)
	if err != nil {
		return model.PersonalDataExport{}, err
	}
	feedback, err := s.exportFeedback(ctx, uid)
	if err != nil {
		return model.PersonalDataExport{}, err
	}
	return model.PersonalDataExport{
		User:        user,
		Devices:     devices,
		Readings:    readings,
		Households:  households,
		Scenes:      scenes,
		Feedback:    feedback,
		GeneratedAt: time.Now().UTC(),
	}, nil
}

// exportDevices 返回全部设备（含 tombstone），按 updated_at 倒序。
func (s *Store) exportDevices(ctx context.Context, uid uuid.UUID) ([]model.UserDevice, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, client_id, platform, external_id, mac, alias, advertised_name, "group",
		       rules, scan_records, updated_at, deleted_at, server_seq, device_type, marked_at
		FROM user_devices
		WHERE user_id = $1
		ORDER BY updated_at DESC, id ASC`, uid)
	if err != nil {
		return nil, fmt.Errorf("导出用户设备失败: %w", err)
	}
	defer rows.Close()

	out := make([]model.UserDevice, 0)
	for rows.Next() {
		var (
			row      sqlcgen.UserDevice
			markedAt sql.NullTime
		)
		if err := rows.Scan(&row.ID, &row.ClientID, &row.Platform, &row.ExternalID, &row.Mac,
			&row.Alias, &row.AdvertisedName, &row.Group, &row.Rules, &row.ScanRecords,
			&row.UpdatedAt, &row.DeletedAt, &row.ServerSeq, &row.DeviceType, &markedAt); err != nil {
			return nil, fmt.Errorf("解析导出设备失败: %w", err)
		}
		d := mapUserDevice(row)
		if markedAt.Valid {
			v := markedAt.Time.UTC()
			d.MarkedAt = &v
		}
		out = append(out, s.enrichUserDevice(ctx, s.q, d))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历导出设备失败: %w", err)
	}
	return out, nil
}

// exportReadingAggregates 按 (platform, external_id, metric, unit) 聚合读数。
func (s *Store) exportReadingAggregates(ctx context.Context, uid uuid.UUID) ([]model.ExportReadingAggregate, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT platform, external_id, metric, unit,
		       count(*), min(value), max(value), avg(value),
		       min(recorded_at), max(recorded_at)
		FROM readings
		WHERE user_id = $1
		GROUP BY platform, external_id, metric, unit
		ORDER BY platform, external_id, metric, unit`, uid)
	if err != nil {
		return nil, fmt.Errorf("导出读数聚合失败: %w", err)
	}
	defer rows.Close()

	out := make([]model.ExportReadingAggregate, 0)
	for rows.Next() {
		var item model.ExportReadingAggregate
		if err := rows.Scan(&item.Platform, &item.ExternalID, &item.Metric, &item.Unit,
			&item.Count, &item.Min, &item.Max, &item.Avg, &item.FirstAt, &item.LastAt); err != nil {
			return nil, fmt.Errorf("解析读数聚合失败: %w", err)
		}
		item.FirstAt = item.FirstAt.UTC()
		item.LastAt = item.LastAt.UTC()
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历读数聚合失败: %w", err)
	}
	return out, nil
}

// exportHouseholds 返回本人所属家庭与房间，只含本人角色，不含其他成员身份。
func (s *Store) exportHouseholds(ctx context.Context, uid uuid.UUID) ([]model.ExportHousehold, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT h.id, h.name, h.is_personal, h.created_at, m.role
		FROM households h
		JOIN household_members m ON m.household_id = h.id
		WHERE m.user_id = $1
		ORDER BY h.created_at ASC, h.id`, uid)
	if err != nil {
		return nil, fmt.Errorf("导出家庭失败: %w", err)
	}
	defer rows.Close()

	out := make([]model.ExportHousehold, 0)
	ids := make([]uuid.UUID, 0)
	for rows.Next() {
		var item model.ExportHousehold
		var id uuid.UUID
		if err := rows.Scan(&id, &item.Name, &item.Personal, &item.CreatedAt, &item.Role); err != nil {
			return nil, fmt.Errorf("解析家庭失败: %w", err)
		}
		item.ID = id.String()
		item.CreatedAt = item.CreatedAt.UTC()
		item.Rooms = []model.ExportRoom{}
		out = append(out, item)
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历家庭失败: %w", err)
	}

	for i, id := range ids {
		rooms, err := s.exportRooms(ctx, id)
		if err != nil {
			return nil, err
		}
		out[i].Rooms = rooms
	}
	return out, nil
}

func (s *Store) exportRooms(ctx context.Context, householdID uuid.UUID) ([]model.ExportRoom, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, sort_order, created_at
		FROM household_rooms WHERE household_id = $1
		ORDER BY sort_order, created_at, id`, householdID)
	if err != nil {
		return nil, fmt.Errorf("导出房间失败: %w", err)
	}
	defer rows.Close()

	out := make([]model.ExportRoom, 0)
	for rows.Next() {
		var item model.ExportRoom
		var id uuid.UUID
		if err := rows.Scan(&id, &item.Name, &item.SortOrder, &item.CreatedAt); err != nil {
			return nil, fmt.Errorf("解析房间失败: %w", err)
		}
		item.ID = id.String()
		item.CreatedAt = item.CreatedAt.UTC()
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历房间失败: %w", err)
	}
	return out, nil
}

// exportFeedback 返回本人反馈；包含本人联系方式与诊断文本，但绝不选择 internal_note。
func (s *Store) exportFeedback(ctx context.Context, uid uuid.UUID) ([]model.ExportFeedback, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, kind, body, contact, platform, app_version, build_type,
		       os_version, device_model, diagnostics, status, created_at, updated_at
		FROM feedback WHERE user_id = $1
		ORDER BY created_at ASC, id`, uid)
	if err != nil {
		return nil, fmt.Errorf("导出反馈失败: %w", err)
	}
	defer rows.Close()

	out := make([]model.ExportFeedback, 0)
	for rows.Next() {
		var item model.ExportFeedback
		var id uuid.UUID
		if err := rows.Scan(&id, &item.Kind, &item.Body, &item.Contact, &item.Platform,
			&item.AppVersion, &item.BuildType, &item.OSVersion, &item.DeviceModel,
			&item.Diagnostics, &item.Status, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, fmt.Errorf("解析反馈失败: %w", err)
		}
		item.FeedbackID = id.String()
		item.CreatedAt = item.CreatedAt.UTC()
		item.UpdatedAt = item.UpdatedAt.UTC()
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历反馈失败: %w", err)
	}
	return out, nil
}

// FinishExportTask 实现 store.Store：写入产物并置为 ready。
func (s *Store) FinishExportTask(ctx context.Context, taskID string, artifact model.PersonalDataExport) error {
	id, err := uuid.Parse(taskID)
	if err != nil {
		return fmt.Errorf("非法导出任务 id: %w", err)
	}
	raw, err := json.Marshal(artifact)
	if err != nil {
		return fmt.Errorf("序列化导出产物失败: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `
		UPDATE personal_data_export_tasks
		SET status = 'ready', artifact = $2, failure_reason = NULL, completed_at = now()
		WHERE id = $1 AND status = 'pending'`, id, raw); err != nil {
		return fmt.Errorf("写入导出产物失败: %w", err)
	}
	return nil
}

// FailExportTask 实现 store.Store：置为 failed 并记录对外可读原因。
func (s *Store) FailExportTask(ctx context.Context, taskID string, reason string) error {
	id, err := uuid.Parse(taskID)
	if err != nil {
		return fmt.Errorf("非法导出任务 id: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `
		UPDATE personal_data_export_tasks
		SET status = 'failed', artifact = NULL, failure_reason = $2, completed_at = now()
		WHERE id = $1 AND status = 'pending'`, id, reason); err != nil {
		return fmt.Errorf("标记导出任务失败: %w", err)
	}
	return nil
}

// ReconcileExportTasks 实现 store.Store。
//
// 选择「标记 failed」而不是「重新入队」：任务执行体是进程内 goroutine，没有持久队列与
// worker 领取协议；若在对账时重新入队，需要额外实现「谁领取、如何防重复、产物写回」，
// 其复杂度与风险远大于收益。导出本身可随时重新发起且产物有 24 小时 TTL，
// 直接把超时未完成的任务收敛为 failed 并给出可读原因，客户端据此提示重新申请即可。
//
// 幂等性由单条 UPDATE 的条件保证：只有 status='pending' 且创建时间早于阈值的行会被
// 第一次推进到 failed，之后的实例（包括并发启动的其他实例）更新 0 行；
// 条件同时保证不会触碰 ready/failed 任务。
func (s *Store) ReconcileExportTasks(ctx context.Context, olderThan time.Time, reason string) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE personal_data_export_tasks
		SET status = 'failed', artifact = NULL, failure_reason = $2, completed_at = now()
		WHERE status = 'pending' AND created_at < $1`, olderThan.UTC(), reason)
	if err != nil {
		return 0, fmt.Errorf("对账导出任务失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("读取对账结果失败: %w", err)
	}
	return n, nil
}

// GetExportTask 实现 store.Store。
func (s *Store) GetExportTask(ctx context.Context, userID string, taskID string) (model.ExportTaskStatus, error) {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return model.ExportTaskStatus{}, fmt.Errorf("非法 user id: %w", err)
	}
	tid, err := uuid.Parse(taskID)
	if err != nil {
		return model.ExportTaskStatus{}, store.ErrNotFound
	}
	var status model.ExportTaskStatus
	var id uuid.UUID
	var completedAt sql.NullTime
	var failureReason sql.NullString
	err = s.db.QueryRowContext(ctx, `
		SELECT id, status, created_at, completed_at, expires_at, failure_reason
		FROM personal_data_export_tasks
		WHERE id = $1 AND user_id = $2 AND expires_at > now()`, tid, uid).
		Scan(&id, &status.Status, &status.CreatedAt, &completedAt, &status.ExpiresAt, &failureReason)
	if errors.Is(err, sql.ErrNoRows) {
		return model.ExportTaskStatus{}, store.ErrNotFound
	}
	if err != nil {
		return model.ExportTaskStatus{}, fmt.Errorf("查询导出任务失败: %w", err)
	}
	status.ID = id.String()
	status.CreatedAt = status.CreatedAt.UTC()
	status.ExpiresAt = status.ExpiresAt.UTC()
	if failureReason.Valid {
		status.FailureReason = failureReason.String
	}
	if completedAt.Valid {
		t := completedAt.Time.UTC()
		status.CompletedAt = &t
	}
	return status, nil
}

// PrepareExportDownload 实现 store.Store：为 ready 任务签发新的下载令牌，替换旧令牌。
func (s *Store) PrepareExportDownload(ctx context.Context, userID string, taskID string, tokenHash string, expiresAt time.Time) (model.ExportTaskStatus, error) {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return model.ExportTaskStatus{}, fmt.Errorf("非法 user id: %w", err)
	}
	tid, err := uuid.Parse(taskID)
	if err != nil {
		return model.ExportTaskStatus{}, store.ErrNotFound
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.ExportTaskStatus{}, fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	var status model.ExportTaskStatus
	var id uuid.UUID
	var completedAt sql.NullTime
	err = tx.QueryRowContext(ctx, `
		SELECT id, status, created_at, completed_at, expires_at
		FROM personal_data_export_tasks
		WHERE id = $1 AND user_id = $2 FOR UPDATE`, tid, uid).
		Scan(&id, &status.Status, &status.CreatedAt, &completedAt, &status.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return model.ExportTaskStatus{}, store.ErrNotFound
	}
	if err != nil {
		return model.ExportTaskStatus{}, fmt.Errorf("查询导出任务失败: %w", err)
	}
	if !status.ExpiresAt.After(time.Now().UTC()) {
		return model.ExportTaskStatus{}, store.ErrNotFound
	}
	if status.Status != "ready" {
		if status.Status == "pending" {
			return model.ExportTaskStatus{}, store.ErrExportTaskNotReady
		}
		return model.ExportTaskStatus{}, store.ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE personal_data_export_tasks
		SET download_token_hash = $2, download_expires_at = $3, download_used_at = NULL
		WHERE id = $1`, tid, tokenHash, expiresAt.UTC()); err != nil {
		return model.ExportTaskStatus{}, fmt.Errorf("更新下载令牌失败: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return model.ExportTaskStatus{}, fmt.Errorf("提交事务失败: %w", err)
	}

	status.ID = id.String()
	status.CreatedAt = status.CreatedAt.UTC()
	status.ExpiresAt = status.ExpiresAt.UTC()
	if completedAt.Valid {
		t := completedAt.Time.UTC()
		status.CompletedAt = &t
	}
	dl := expiresAt.UTC()
	status.DownloadExpiresAt = &dl
	return status, nil
}

// ConsumeExportDownload 实现 store.Store：原子校验并消费一次性下载令牌。
func (s *Store) ConsumeExportDownload(ctx context.Context, tokenHash string, now time.Time) (model.PersonalDataExport, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.PersonalDataExport{}, fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	var artifact []byte
	var status string
	var expiresAt sql.NullTime
	var usedAt sql.NullTime
	err = tx.QueryRowContext(ctx, `
		SELECT artifact, status, download_expires_at, download_used_at
		FROM personal_data_export_tasks
		WHERE download_token_hash = $1 FOR UPDATE`, tokenHash).
		Scan(&artifact, &status, &expiresAt, &usedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return model.PersonalDataExport{}, store.ErrExportDownloadInvalid
	}
	if err != nil {
		return model.PersonalDataExport{}, fmt.Errorf("查询下载令牌失败: %w", err)
	}
	if status != "ready" || artifact == nil {
		return model.PersonalDataExport{}, store.ErrExportDownloadInvalid
	}
	if usedAt.Valid {
		return model.PersonalDataExport{}, store.ErrExportDownloadExpired
	}
	if !expiresAt.Valid || !expiresAt.Time.After(now) {
		return model.PersonalDataExport{}, store.ErrExportDownloadExpired
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE personal_data_export_tasks SET download_used_at = $2
		WHERE download_token_hash = $1`, tokenHash, now.UTC()); err != nil {
		return model.PersonalDataExport{}, fmt.Errorf("标记下载令牌失败: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return model.PersonalDataExport{}, fmt.Errorf("提交事务失败: %w", err)
	}

	var out model.PersonalDataExport
	if err := json.Unmarshal(artifact, &out); err != nil {
		return model.PersonalDataExport{}, fmt.Errorf("解析导出产物失败: %w", err)
	}
	return out, nil
}

// CleanupDeletionTokens 实现 store.Store。
func (s *Store) CleanupDeletionTokens(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM account_deletion_tokens WHERE expires_at < $1`, before.UTC())
	if err != nil {
		return 0, fmt.Errorf("清理删除令牌失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("读取清理结果失败: %w", err)
	}
	return n, nil
}

// CleanupExportTasks 实现 store.Store：整行删除过期任务，产物随之清理。
func (s *Store) CleanupExportTasks(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM personal_data_export_tasks WHERE expires_at < $1`, before.UTC())
	if err != nil {
		return 0, fmt.Errorf("清理导出任务失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("读取清理结果失败: %w", err)
	}
	return n, nil
}

// CreatePairingCode 实现 store.Store：同一用户的新码使旧码失效。
func (s *Store) CreatePairingCode(ctx context.Context, userID string, codeHash string, expiresAt time.Time) error {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return fmt.Errorf("非法 user id: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `DELETE FROM pairing_codes WHERE user_id = $1`, uid); err != nil {
		return fmt.Errorf("回收旧配对码失败: %w", err)
	}
	// 顺带清理过期行（含失败尝试记录），避免表无限增长。
	if _, err := tx.ExecContext(ctx, `DELETE FROM pairing_codes WHERE expires_at < now()`); err != nil {
		return fmt.Errorf("清理过期配对码失败: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO pairing_codes (code_hash, user_id, attempts, expires_at)
		VALUES ($1, $2, 0, $3)`, codeHash, uid, expiresAt.UTC()); err != nil {
		return fmt.Errorf("写入配对码失败: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交事务失败: %w", err)
	}
	return nil
}

// ClaimPairingCode 实现 store.Store。
//
// 失败（不存在、过期、已用过、哈希不一致）统一返回 ErrNotFound，不区分原因。
// 每次失败都对提交的码计数；同一码累计 5 次即删除作废。
func (s *Store) ClaimPairingCode(ctx context.Context, codeHash string, tokenHash string, now time.Time, ttl time.Duration, tokenExpiresAt time.Time) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	var storedHash string
	var owner uuid.NullUUID
	var attempts int
	var expiresAt time.Time
	err = tx.QueryRowContext(ctx, `
		SELECT code_hash, user_id, attempts, expires_at
		FROM pairing_codes WHERE code_hash = $1 FOR UPDATE`, codeHash).
		Scan(&storedHash, &owner, &attempts, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		if err := recordPairingFailure(ctx, tx, codeHash, now, ttl); err != nil {
			return "", err
		}
		if err := tx.Commit(); err != nil {
			return "", fmt.Errorf("提交事务失败: %w", err)
		}
		return "", store.ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("查询配对码失败: %w", err)
	}

	// 失败尝试记录行（user_id IS NULL）或已满 5 次：继续计数并最终删除。
	if !owner.Valid || attempts >= maxPairingAttempts {
		if err := incrementPairingFailure(ctx, tx, codeHash, attempts, now, ttl, true); err != nil {
			return "", err
		}
		if err := tx.Commit(); err != nil {
			return "", fmt.Errorf("提交事务失败: %w", err)
		}
		return "", store.ErrNotFound
	}

	// 常量时间比较，避免通过耗时差异推断哈希前缀。
	if !auth.EqualPairingCodeHash(storedHash, codeHash) {
		if err := incrementPairingFailure(ctx, tx, codeHash, attempts, now, ttl, false); err != nil {
			return "", err
		}
		if err := tx.Commit(); err != nil {
			return "", fmt.Errorf("提交事务失败: %w", err)
		}
		return "", store.ErrNotFound
	}

	expired := !expiresAt.After(now)
	if expired {
		// 过期码只计数，不延长过期时间，避免失败尝试把过期码“救活”。
		if err := incrementPairingFailure(ctx, tx, codeHash, attempts, now, ttl, false); err != nil {
			return "", err
		}
		if err := tx.Commit(); err != nil {
			return "", fmt.Errorf("提交事务失败: %w", err)
		}
		return "", store.ErrNotFound
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO tokens (user_id, token_hash, expires_at) VALUES ($1, $2, $3)`, owner.UUID, tokenHash, tokenExpiresAt.UTC()); err != nil {
		return "", fmt.Errorf("签发 token 失败: %w", err)
	}
	// 一次性：用掉即删除，重放命中「不存在」分支并返回 404。
	if _, err := tx.ExecContext(ctx, `DELETE FROM pairing_codes WHERE code_hash = $1`, codeHash); err != nil {
		return "", fmt.Errorf("删除已用配对码失败: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("提交事务失败: %w", err)
	}
	return owner.UUID.String(), nil
}

// maxPairingAttempts 是同一配对码允许的最大失败次数。
const maxPairingAttempts = 5

// recordPairingFailure 为不存在的码写入失败计数行；并发时递增已有计数。
func recordPairingFailure(ctx context.Context, tx *sql.Tx, codeHash string, now time.Time, ttl time.Duration) error {
	var attempts int
	err := tx.QueryRowContext(ctx, `
		INSERT INTO pairing_codes (code_hash, user_id, attempts, expires_at)
		VALUES ($1, NULL, 1, $2)
		ON CONFLICT (code_hash) DO UPDATE
		SET attempts = LEAST(pairing_codes.attempts + 1, $3),
		    expires_at = GREATEST(pairing_codes.expires_at, EXCLUDED.expires_at)
		RETURNING attempts`, codeHash, now.Add(ttl).UTC(), maxPairingAttempts).Scan(&attempts)
	if err != nil {
		return fmt.Errorf("记录配对码失败尝试失败: %w", err)
	}
	if attempts >= maxPairingAttempts {
		if _, err := tx.ExecContext(ctx, `DELETE FROM pairing_codes WHERE code_hash = $1`, codeHash); err != nil {
			return fmt.Errorf("作废配对码失败: %w", err)
		}
	}
	return nil
}

// incrementPairingFailure 对已有的码（含失败记录行）递增失败次数，满 5 次删除。
// extendExpiry 仅用于失败记录行（user_id IS NULL），让其 TTL 覆盖计数窗口；
// 对真实码（尤其过期码）绝不延长，否则失败尝试会把过期码重新“救活”。
func incrementPairingFailure(ctx context.Context, tx *sql.Tx, codeHash string, current int, now time.Time, ttl time.Duration, extendExpiry bool) error {
	next := current + 1
	if next >= maxPairingAttempts {
		if _, err := tx.ExecContext(ctx, `DELETE FROM pairing_codes WHERE code_hash = $1`, codeHash); err != nil {
			return fmt.Errorf("作废配对码失败: %w", err)
		}
		return nil
	}
	if extendExpiry {
		if _, err := tx.ExecContext(ctx, `
			UPDATE pairing_codes SET attempts = $2, expires_at = GREATEST(expires_at, $3)
			WHERE code_hash = $1`, codeHash, next, now.Add(ttl).UTC()); err != nil {
			return fmt.Errorf("更新配对码失败次数失败: %w", err)
		}
		return nil
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE pairing_codes SET attempts = $2 WHERE code_hash = $1`, codeHash, next); err != nil {
		return fmt.Errorf("更新配对码失败次数失败: %w", err)
	}
	return nil
}

// CleanupPairingCodes 实现 store.Store。
func (s *Store) CleanupPairingCodes(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM pairing_codes WHERE expires_at < $1`, before.UTC())
	if err != nil {
		return 0, fmt.Errorf("清理配对码失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("读取清理结果失败: %w", err)
	}
	return n, nil
}

// CleanupChangeLog 实现 store.Store。
func (s *Store) CleanupChangeLog(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM change_log WHERE server_ts < $1`, before.UTC())
	if err != nil {
		return 0, fmt.Errorf("清理变更日志失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("读取清理结果失败: %w", err)
	}
	return n, nil
}

// CleanupIdempotencyKeys 实现 store.Store。
func (s *Store) CleanupIdempotencyKeys(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM idempotency_keys WHERE created_at < $1`, before.UTC())
	if err != nil {
		return 0, fmt.Errorf("清理幂等键失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("读取清理结果失败: %w", err)
	}
	return n, nil
}

// CleanupTokens 实现 store.Store。
func (s *Store) CleanupTokens(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM tokens WHERE expires_at < $1`, before.UTC())
	if err != nil {
		return 0, fmt.Errorf("清理过期 token 失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("读取清理结果失败: %w", err)
	}
	return n, nil
}

// ApproxCounts 实现 store.Store：读取 pg_class.reltuples，不对大表执行 COUNT。
func (s *Store) ApproxCounts(ctx context.Context) (model.StoreCounts, error) {
	var counts model.StoreCounts
	err := s.db.QueryRowContext(ctx, `
		SELECT
			GREATEST(COALESCE((SELECT reltuples::bigint FROM pg_class WHERE oid = 'users'::regclass), 0), 0),
			GREATEST(COALESCE((SELECT reltuples::bigint FROM pg_class WHERE oid = 'user_devices'::regclass), 0), 0),
			GREATEST(COALESCE((SELECT reltuples::bigint FROM pg_class WHERE oid = 'readings'::regclass), 0), 0)`).
		Scan(&counts.Users, &counts.Devices, &counts.Readings)
	if err != nil {
		return model.StoreCounts{}, fmt.Errorf("查询近似行数失败: %w", err)
	}
	return counts, nil
}
