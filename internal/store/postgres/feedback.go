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

// feedbackColumns 是列表/回放共用的字段顺序。
const feedbackColumns = `id, kind, body, platform, app_version, build_type, os_version, device_model, status, created_at`

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

// ListUserFeedback 实现 store.Store：只查本人，按 created_at DESC, id DESC 游标分页。
func (s *Store) ListUserFeedback(ctx context.Context, userID string, cursor string, limit int) (model.FeedbackPage, error) {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return model.FeedbackPage{}, fmt.Errorf("非法 user id: %w", err)
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	ctx, cancel := context.WithTimeout(ctx, feedbackTimeout)
	defer cancel()

	where := "user_id = $1"
	args := []any{uid}
	if cursor != "" {
		at, id, err := decodeFeedbackCursor(cursor)
		if err != nil {
			return model.FeedbackPage{}, err
		}
		where += " AND (created_at, id) < ($2, $3)"
		args = append(args, at, id)
	}
	args = append(args, limit+1)

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
