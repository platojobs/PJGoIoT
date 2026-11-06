package postgres

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/platojobs/PJGoIoT/internal/model"
	"github.com/platojobs/PJGoIoT/internal/store"
)

// feedbackTimeout 是反馈读写的时间预算。
const feedbackTimeout = 5 * time.Second

// feedbackColumns 是用户侧列表字段顺序。
const feedbackColumns = `id, kind, body, platform, app_version, build_type, os_version, device_model, status, created_at`

// feedbackDetailColumns 是用户侧单条详情字段顺序。
const feedbackDetailColumns = feedbackColumns + `, updated_at`

// adminFeedbackColumns 是运营侧字段顺序（含个人信息与内部备注）。
const adminFeedbackColumns = `id, user_id, kind, body, contact, platform, app_version, build_type, os_version, device_model, diagnostics, status, internal_note, created_at, updated_at`

// CreateFeedback 实现 store.Store。
//
// 幂等靠 (user_id, idem_key) 唯一索引：先尝试 INSERT ... ON CONFLICT DO NOTHING，
// 命中冲突时回读既有记录并比对请求哈希；哈希不同说明同一幂等键被复用于不同内容，
// 返回 ErrIdempotencyConflict（与 /sync/push 的语义一致）。
func (s *Store) CreateFeedback(ctx context.Context, userID string, sub model.FeedbackSubmission, opts store.FeedbackOptions) (model.Feedback, bool, error) {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return model.Feedback{}, false, fmt.Errorf("非法 user id: %w", err)
	}
	if opts.IdempotencyKey == "" {
		return model.Feedback{}, false, fmt.Errorf("幂等键不能为空")
	}
	ctx, cancel := context.WithTimeout(ctx, feedbackTimeout)
	defer cancel()

	item := model.Feedback{
		FeedbackID:  uuid.NewString(),
		Kind:        sub.Kind,
		Body:        sub.Body,
		Platform:    sub.Platform,
		AppVersion:  sub.AppVersion,
		BuildType:   sub.BuildType,
		OSVersion:   sub.OSVersion,
		DeviceModel: sub.DeviceModel,
		Status:      "submitted",
	}

	var createdAt time.Time
	err = s.db.QueryRowContext(ctx, `
INSERT INTO feedback (id, user_id, kind, body, contact, platform, app_version, build_type, os_version, device_model, diagnostics, status, idem_key, request_hash)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, 'submitted', $12, $13)
ON CONFLICT (user_id, idem_key) DO NOTHING
RETURNING created_at`,
		item.FeedbackID, uid, sub.Kind, sub.Body, sub.Contact, sub.Platform, sub.AppVersion,
		sub.BuildType, sub.OSVersion, sub.DeviceModel, sub.Diagnostics, opts.IdempotencyKey, opts.RequestHash,
	).Scan(&createdAt)
	if err == nil {
		item.CreatedAt = createdAt.UTC()
		return item, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return model.Feedback{}, false, fmt.Errorf("写入反馈失败: %w", err)
	}

	// 冲突：回读已存记录，保证重复提交返回同一 feedbackId 且不产生第二条记录。
	var storedHash string
	existing := item
	err = s.db.QueryRowContext(ctx, `
SELECT `+feedbackColumns+`, request_hash
FROM feedback WHERE user_id = $1 AND idem_key = $2`, uid, opts.IdempotencyKey).
		Scan(&existing.FeedbackID, &existing.Kind, &existing.Body, &existing.Platform,
			&existing.AppVersion, &existing.BuildType, &existing.OSVersion, &existing.DeviceModel,
			&existing.Status, &existing.CreatedAt, &storedHash)
	if errors.Is(err, sql.ErrNoRows) {
		// 极端竞态：冲突行在回读前被删除（如账号注销）。按普通错误处理，客户端可重试。
		return model.Feedback{}, false, fmt.Errorf("幂等记录回读为空")
	}
	if err != nil {
		return model.Feedback{}, false, fmt.Errorf("回读幂等反馈失败: %w", err)
	}
	if storedHash != opts.RequestHash {
		return model.Feedback{}, false, store.ErrIdempotencyConflict
	}
	existing.CreatedAt = existing.CreatedAt.UTC()
	return existing, true, nil
}

// ListUserFeedback 实现 store.Store：只查本人，可按状态过滤，按 created_at DESC, id DESC 游标分页。
func (s *Store) ListUserFeedback(ctx context.Context, userID string, status string, cursor string, limit int) (model.FeedbackPage, error) {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return model.FeedbackPage{}, fmt.Errorf("非法 user id: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, feedbackTimeout)
	defer cancel()

	where, args, err := feedbackWhere("user_id = $1", []any{uid}, status, cursor, limit)
	if err != nil {
		return model.FeedbackPage{}, err
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT `+feedbackColumns+`
FROM feedback WHERE `+where+`
ORDER BY created_at DESC, id DESC
LIMIT $`+fmt.Sprint(len(args)), args...)
	if err != nil {
		return model.FeedbackPage{}, fmt.Errorf("查询反馈列表失败: %w", err)
	}
	defer rows.Close()

	items := []model.Feedback{}
	for rows.Next() {
		var f model.Feedback
		if err := rows.Scan(&f.FeedbackID, &f.Kind, &f.Body, &f.Platform, &f.AppVersion,
			&f.BuildType, &f.OSVersion, &f.DeviceModel, &f.Status, &f.CreatedAt); err != nil {
			return model.FeedbackPage{}, fmt.Errorf("读取反馈失败: %w", err)
		}
		f.CreatedAt = f.CreatedAt.UTC()
		items = append(items, f)
	}
	if err := rows.Err(); err != nil {
		return model.FeedbackPage{}, fmt.Errorf("读取反馈失败: %w", err)
	}

	page := model.FeedbackPage{Items: items}
	if len(items) > limit {
		page.Items = items[:limit]
		last := page.Items[len(page.Items)-1]
		page.NextCursor = encodeFeedbackCursor(last.CreatedAt, last.FeedbackID)
		page.HasMore = true
	}
	return page, nil
}

// GetUserFeedback 返回本人单条详情；不存在或跨用户一律 ErrNotFound，不暴露存在性。
func (s *Store) GetUserFeedback(ctx context.Context, userID string, feedbackID string) (model.FeedbackDetail, error) {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return model.FeedbackDetail{}, store.ErrNotFound
	}
	fid, err := uuid.Parse(feedbackID)
	if err != nil {
		return model.FeedbackDetail{}, store.ErrNotFound
	}
	ctx, cancel := context.WithTimeout(ctx, feedbackTimeout)
	defer cancel()

	var d model.FeedbackDetail
	err = s.db.QueryRowContext(ctx, `
SELECT `+feedbackDetailColumns+`
FROM feedback WHERE id = $1 AND user_id = $2`, fid, uid).
		Scan(&d.FeedbackID, &d.Kind, &d.Body, &d.Platform, &d.AppVersion, &d.BuildType,
			&d.OSVersion, &d.DeviceModel, &d.Status, &d.CreatedAt, &d.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return model.FeedbackDetail{}, store.ErrNotFound
	}
	if err != nil {
		return model.FeedbackDetail{}, fmt.Errorf("查询反馈详情失败: %w", err)
	}
	d.CreatedAt = d.CreatedAt.UTC()
	d.UpdatedAt = d.UpdatedAt.UTC()
	d.StatusMessage = model.FeedbackStatusMessage(d.Status)
	return d, nil
}

// CloseUserFeedback 让本人关闭自己的反馈（submitted|received -> closed）。
func (s *Store) CloseUserFeedback(ctx context.Context, userID string, feedbackID string) (model.FeedbackDetail, error) {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return model.FeedbackDetail{}, store.ErrNotFound
	}
	fid, err := uuid.Parse(feedbackID)
	if err != nil {
		return model.FeedbackDetail{}, store.ErrNotFound
	}
	ctx, cancel := context.WithTimeout(ctx, feedbackTimeout)
	defer cancel()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.FeedbackDetail{}, fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	var d model.FeedbackDetail
	err = tx.QueryRowContext(ctx, `
SELECT `+feedbackDetailColumns+`
FROM feedback WHERE id = $1 AND user_id = $2 FOR UPDATE`, fid, uid).
		Scan(&d.FeedbackID, &d.Kind, &d.Body, &d.Platform, &d.AppVersion, &d.BuildType,
			&d.OSVersion, &d.DeviceModel, &d.Status, &d.CreatedAt, &d.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return model.FeedbackDetail{}, store.ErrNotFound
	}
	if err != nil {
		return model.FeedbackDetail{}, fmt.Errorf("查询反馈详情失败: %w", err)
	}
	// 用户侧只允许关闭；已关闭再改属于非法跳转，明确报错而不是静默成功。
	if d.Status == "closed" {
		return model.FeedbackDetail{}, store.ErrFeedbackInvalidTransition
	}
	var updatedAt time.Time
	if err := tx.QueryRowContext(ctx, `
UPDATE feedback SET status = 'closed', updated_at = now() WHERE id = $1
RETURNING updated_at`, fid).Scan(&updatedAt); err != nil {
		return model.FeedbackDetail{}, fmt.Errorf("更新反馈状态失败: %w", err)
	}
	if err := insertFeedbackEvent(ctx, tx, fid, "user", userID, d.Status, "closed", nil); err != nil {
		return model.FeedbackDetail{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.FeedbackDetail{}, fmt.Errorf("提交事务失败: %w", err)
	}

	d.Status = "closed"
	d.UpdatedAt = updatedAt.UTC()
	d.StatusMessage = model.FeedbackStatusMessage(d.Status)
	return d, nil
}

// ListAdminFeedback 跨用户返回反馈，可按 status 过滤，按 created_at DESC, id DESC 游标分页。
func (s *Store) ListAdminFeedback(ctx context.Context, status string, cursor string, limit int) (model.AdminFeedbackPage, error) {
	ctx, cancel := context.WithTimeout(ctx, feedbackTimeout)
	defer cancel()

	where, args, err := feedbackWhere("TRUE", nil, status, cursor, limit)
	if err != nil {
		return model.AdminFeedbackPage{}, err
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT `+adminFeedbackColumns+`
FROM feedback WHERE `+where+`
ORDER BY created_at DESC, id DESC
LIMIT $`+fmt.Sprint(len(args)), args...)
	if err != nil {
		return model.AdminFeedbackPage{}, fmt.Errorf("查询运营反馈列表失败: %w", err)
	}
	defer rows.Close()

	items := []model.AdminFeedback{}
	for rows.Next() {
		item, err := scanAdminFeedback(rows)
		if err != nil {
			return model.AdminFeedbackPage{}, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return model.AdminFeedbackPage{}, fmt.Errorf("读取运营反馈失败: %w", err)
	}

	page := model.AdminFeedbackPage{Items: items}
	if len(items) > limit {
		page.Items = items[:limit]
		last := page.Items[len(page.Items)-1]
		page.NextCursor = encodeFeedbackCursor(last.CreatedAt, last.FeedbackID)
		page.HasMore = true
	}
	return page, nil
}

// UpdateFeedbackStatus 由运营侧推进状态。
// 同状态为幂等 no-op（不写审计、忽略备注）；合法跳转写一条审计并更新 internal_note。
func (s *Store) UpdateFeedbackStatus(ctx context.Context, feedbackID string, targetStatus string, note *string) (model.AdminFeedback, error) {
	fid, err := uuid.Parse(feedbackID)
	if err != nil {
		return model.AdminFeedback{}, store.ErrNotFound
	}
	ctx, cancel := context.WithTimeout(ctx, feedbackTimeout)
	defer cancel()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.AdminFeedback{}, fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	item, err := scanAdminFeedback(tx.QueryRowContext(ctx, `
SELECT `+adminFeedbackColumns+`
FROM feedback WHERE id = $1 FOR UPDATE`, fid))
	if errors.Is(err, sql.ErrNoRows) {
		return model.AdminFeedback{}, store.ErrNotFound
	}
	if err != nil {
		return model.AdminFeedback{}, err
	}

	if item.Status == targetStatus {
		// 幂等 no-op：不改 updated_at、不追加审计、忽略本次备注。
		if err := tx.Commit(); err != nil {
			return model.AdminFeedback{}, fmt.Errorf("提交事务失败: %w", err)
		}
		return item, nil
	}
	if !model.FeedbackTransitionAllowed(item.Status, targetStatus) {
		return model.AdminFeedback{}, store.ErrFeedbackInvalidTransition
	}

	var updatedAt time.Time
	if err := tx.QueryRowContext(ctx, `
UPDATE feedback
SET status = $2, updated_at = now(),
    internal_note = COALESCE($3, internal_note)
WHERE id = $1
RETURNING updated_at`, fid, targetStatus, note).Scan(&updatedAt); err != nil {
		return model.AdminFeedback{}, fmt.Errorf("更新反馈状态失败: %w", err)
	}
	if err := insertFeedbackEvent(ctx, tx, fid, "admin", "admin", item.Status, targetStatus, note); err != nil {
		return model.AdminFeedback{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.AdminFeedback{}, fmt.Errorf("提交事务失败: %w", err)
	}

	item.Status = targetStatus
	item.UpdatedAt = updatedAt.UTC()
	if note != nil {
		item.InternalNote = note
	}
	return item, nil
}

// feedbackWhere 组装状态过滤与游标条件，供用户列表与运营列表复用。
func feedbackWhere(base string, args []any, status string, cursor string, limit int) (string, []any, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	where := base
	if status != "" {
		args = append(args, status)
		where += fmt.Sprintf(" AND status = $%d", len(args))
	}
	if cursor != "" {
		at, id, err := decodeFeedbackCursor(cursor)
		if err != nil {
			return "", nil, err
		}
		args = append(args, at, id)
		where += fmt.Sprintf(" AND (created_at, id) < ($%d, $%d)", len(args)-1, len(args))
	}
	args = append(args, limit+1)
	return where, args, nil
}

// insertFeedbackEvent 写一条状态审计；与状态更新在同一事务内，保证强一致。
func insertFeedbackEvent(ctx context.Context, tx *sql.Tx, feedbackID uuid.UUID, actorType, actorID, from, to string, note *string) error {
	if _, err := tx.ExecContext(ctx, `
INSERT INTO feedback_status_events (id, feedback_id, actor_type, actor_id, from_status, to_status, note)
VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		uuid.NewString(), feedbackID, actorType, actorID, from, to, note); err != nil {
		return fmt.Errorf("写入反馈审计失败: %w", err)
	}
	return nil
}

// rowScanner 抽象 sql.Row 与 sql.Rows，便于复用扫描逻辑。
type rowScanner interface {
	Scan(dest ...any) error
}

// scanAdminFeedback 扫描运营侧一行；NULL 字段用 sql.NullString 承接后转指针。
func scanAdminFeedback(row rowScanner) (model.AdminFeedback, error) {
	var f model.AdminFeedback
	var contact, diagnostics, note sql.NullString
	if err := row.Scan(&f.FeedbackID, &f.UserID, &f.Kind, &f.Body, &contact, &f.Platform,
		&f.AppVersion, &f.BuildType, &f.OSVersion, &f.DeviceModel, &diagnostics, &f.Status,
		&note, &f.CreatedAt, &f.UpdatedAt); err != nil {
		return model.AdminFeedback{}, err
	}
	f.Contact = nullStringPtr(contact)
	f.Diagnostics = nullStringPtr(diagnostics)
	f.InternalNote = nullStringPtr(note)
	f.CreatedAt = f.CreatedAt.UTC()
	f.UpdatedAt = f.UpdatedAt.UTC()
	return f, nil
}

func nullStringPtr(ns sql.NullString) *string {
	if !ns.Valid {
		return nil
	}
	v := ns.String
	return &v
}

// encodeFeedbackCursor 把 (createdAt, id) 编码为不透明游标。
func encodeFeedbackCursor(at time.Time, id string) string {
	raw := at.UTC().Format(time.RFC3339Nano) + "|" + id
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// decodeFeedbackCursor 解析游标；非法游标返回 ErrBadCursor 供 HTTP 层映射为 400。
func decodeFeedbackCursor(cursor string) (time.Time, uuid.UUID, error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return time.Time{}, uuid.Nil, store.ErrBadCursor
	}
	parts := strings.SplitN(string(raw), "|", 2)
	if len(parts) != 2 {
		return time.Time{}, uuid.Nil, store.ErrBadCursor
	}
	at, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return time.Time{}, uuid.Nil, store.ErrBadCursor
	}
	id, err := uuid.Parse(parts[1])
	if err != nil {
		return time.Time{}, uuid.Nil, store.ErrBadCursor
	}
	return at.UTC(), id, nil
}
