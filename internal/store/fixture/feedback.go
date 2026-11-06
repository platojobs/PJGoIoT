package fixture

import (
	"context"
	"encoding/base64"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/platojobs/PJGoIoT/internal/model"
	"github.com/platojobs/PJGoIoT/internal/store"
)

// CreateFeedback 实现 store.Store：内存版用 userID|idemKey 去重，与 Postgres 唯一索引语义一致。
func (s *Store) CreateFeedback(_ context.Context, userID string, sub model.FeedbackSubmission, opts store.FeedbackOptions) (model.Feedback, bool, error) {
	if opts.IdempotencyKey == "" {
		return model.Feedback{}, false, fmt.Errorf("幂等键不能为空")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	key := userID + "|" + opts.IdempotencyKey
	if rec, ok := s.feedbackIdem[key]; ok {
		if rec.requestHash != opts.RequestHash {
			return model.Feedback{}, false, store.ErrIdempotencyConflict
		}
		return feedbackView(rec), true, nil
	}

	now := time.Now().UTC()
	rec := &feedbackRecord{
		id:          uuid.NewString(),
		userID:      userID,
		kind:        sub.Kind,
		body:        sub.Body,
		contact:     sub.Contact,
		platform:    sub.Platform,
		appVersion:  sub.AppVersion,
		buildType:   sub.BuildType,
		osVersion:   sub.OSVersion,
		deviceModel: sub.DeviceModel,
		diagnostics: sub.Diagnostics,
		status:      "submitted",
		idemKey:     opts.IdempotencyKey,
		requestHash: opts.RequestHash,
		createdAt:   now,
		updatedAt:   now,
	}
	s.feedback = append(s.feedback, rec)
	s.feedbackIdem[key] = rec
	return feedbackView(rec), false, nil
}

// ListUserFeedback 实现 store.Store：只返回本人数据，可按状态过滤，
// 按 createdAt DESC, id DESC 游标分页。
func (s *Store) ListUserFeedback(_ context.Context, userID string, status string, cursor string, limit int) (model.FeedbackPage, error) {
	var after time.Time
	var afterID string
	var err error
	if cursor != "" {
		after, afterID, err = decodeFeedbackCursor(cursor)
		if err != nil {
			return model.FeedbackPage{}, err
		}
	}
	limit = clampFeedbackLimit(limit)

	s.mu.RLock()
	defer s.mu.RUnlock()

	mine := make([]*feedbackRecord, 0, len(s.feedback))
	for _, rec := range s.feedback {
		if rec.userID == userID && (status == "" || rec.status == status) {
			mine = append(mine, rec)
		}
	}
	sortFeedbackRecords(mine)

	items := []model.Feedback{}
	hasMore := false
	for _, rec := range mine {
		if !feedbackAfterCursor(rec, cursor, after, afterID) {
			continue
		}
		if len(items) == limit {
			hasMore = true
			break
		}
		items = append(items, feedbackView(rec))
	}
	page := model.FeedbackPage{Items: items, HasMore: hasMore}
	if hasMore {
		last := items[len(items)-1]
		page.NextCursor = encodeFeedbackCursor(last.CreatedAt, last.FeedbackID)
	}
	return page, nil
}

// GetUserFeedback 返回本人单条详情；不存在或跨用户一律 ErrNotFound。
func (s *Store) GetUserFeedback(_ context.Context, userID string, feedbackID string) (model.FeedbackDetail, error) {
	if _, err := uuid.Parse(feedbackID); err != nil {
		return model.FeedbackDetail{}, store.ErrNotFound
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, rec := range s.feedback {
		if rec.id == feedbackID && rec.userID == userID {
			return feedbackDetailView(rec), nil
		}
	}
	return model.FeedbackDetail{}, store.ErrNotFound
}

// CloseUserFeedback 让本人关闭自己的反馈（submitted|received -> closed）。
func (s *Store) CloseUserFeedback(_ context.Context, userID string, feedbackID string) (model.FeedbackDetail, error) {
	if _, err := uuid.Parse(feedbackID); err != nil {
		return model.FeedbackDetail{}, store.ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	var rec *feedbackRecord
	for _, r := range s.feedback {
		if r.id == feedbackID && r.userID == userID {
			rec = r
			break
		}
	}
	if rec == nil {
		return model.FeedbackDetail{}, store.ErrNotFound
	}
	if rec.status == "closed" {
		return model.FeedbackDetail{}, store.ErrFeedbackInvalidTransition
	}
	from := rec.status
	rec.status = "closed"
	rec.updatedAt = time.Now().UTC()
	s.appendFeedbackEvent(rec.id, "user", userID, from, "closed", nil)
	return feedbackDetailView(rec), nil
}

// ListAdminFeedback 跨用户返回反馈，可按状态过滤，按 createdAt DESC, id DESC 游标分页。
func (s *Store) ListAdminFeedback(_ context.Context, status string, cursor string, limit int) (model.AdminFeedbackPage, error) {
	var after time.Time
	var afterID string
	var err error
	if cursor != "" {
		after, afterID, err = decodeFeedbackCursor(cursor)
		if err != nil {
			return model.AdminFeedbackPage{}, err
		}
	}
	limit = clampFeedbackLimit(limit)

	s.mu.RLock()
	defer s.mu.RUnlock()

	all := make([]*feedbackRecord, 0, len(s.feedback))
	for _, rec := range s.feedback {
		if status == "" || rec.status == status {
			all = append(all, rec)
		}
	}
	sortFeedbackRecords(all)

	items := []model.AdminFeedback{}
	hasMore := false
	for _, rec := range all {
		if !feedbackAfterCursor(rec, cursor, after, afterID) {
			continue
		}
		if len(items) == limit {
			hasMore = true
			break
		}
		items = append(items, adminFeedbackView(rec))
	}
	page := model.AdminFeedbackPage{Items: items, HasMore: hasMore}
	if hasMore {
		last := items[len(items)-1]
		page.NextCursor = encodeFeedbackCursor(last.CreatedAt, last.FeedbackID)
	}
	return page, nil
}

// UpdateFeedbackStatus 由运营侧推进状态；同状态为幂等 no-op（不写审计、忽略备注）。
func (s *Store) UpdateFeedbackStatus(_ context.Context, feedbackID string, targetStatus string, note *string) (model.AdminFeedback, error) {
	if _, err := uuid.Parse(feedbackID); err != nil {
		return model.AdminFeedback{}, store.ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	var rec *feedbackRecord
	for _, r := range s.feedback {
		if r.id == feedbackID {
			rec = r
			break
		}
	}
	if rec == nil {
		return model.AdminFeedback{}, store.ErrNotFound
	}
	if rec.status == targetStatus {
		return adminFeedbackView(rec), nil
	}
	if !model.FeedbackTransitionAllowed(rec.status, targetStatus) {
		return model.AdminFeedback{}, store.ErrFeedbackInvalidTransition
	}
	from := rec.status
	rec.status = targetStatus
	rec.updatedAt = time.Now().UTC()
	if note != nil {
		rec.internalNote = note
	}
	s.appendFeedbackEvent(rec.id, "admin", "admin", from, targetStatus, note)
	return adminFeedbackView(rec), nil
}

func (s *Store) appendFeedbackEvent(feedbackID, actorType, actorID, from, to string, note *string) {
	s.feedbackEvents = append(s.feedbackEvents, &feedbackEventRecord{
		id:         uuid.NewString(),
		feedbackID: feedbackID,
		actorType:  actorType,
		actorID:    actorID,
		fromStatus: from,
		toStatus:   to,
		note:       note,
		createdAt:  time.Now().UTC(),
	})
}

func clampFeedbackLimit(limit int) int {
	if limit <= 0 {
		return 20
	}
	if limit > 100 {
		return 100
	}
	return limit
}

func sortFeedbackRecords(recs []*feedbackRecord) {
	sort.Slice(recs, func(i, j int) bool {
		if recs[i].createdAt.Equal(recs[j].createdAt) {
			return recs[i].id > recs[j].id
		}
		return recs[i].createdAt.After(recs[j].createdAt)
	})
}

// feedbackAfterCursor 判断记录是否严格位于游标之后（createdAt DESC, id DESC 顺序）。
func feedbackAfterCursor(rec *feedbackRecord, cursor string, after time.Time, afterID string) bool {
	if cursor == "" {
		return true
	}
	return rec.createdAt.Before(after) || (rec.createdAt.Equal(after) && rec.id < afterID)
}

// feedbackView 把内部记录转成用户侧列表视图：不返回 contact 与 diagnostics。
func feedbackView(rec *feedbackRecord) model.Feedback {
	return model.Feedback{
		FeedbackID:  rec.id,
		Kind:        rec.kind,
		Body:        rec.body,
		Platform:    rec.platform,
		AppVersion:  rec.appVersion,
		BuildType:   rec.buildType,
		OSVersion:   rec.osVersion,
		DeviceModel: rec.deviceModel,
		Status:      rec.status,
		CreatedAt:   rec.createdAt,
	}
}

// feedbackDetailView 是用户侧单条详情：含状态说明与 updatedAt，不含个人信息。
func feedbackDetailView(rec *feedbackRecord) model.FeedbackDetail {
	return model.FeedbackDetail{
		FeedbackID:    rec.id,
		Kind:          rec.kind,
		Body:          rec.body,
		Platform:      rec.platform,
		AppVersion:    rec.appVersion,
		BuildType:     rec.buildType,
		OSVersion:     rec.osVersion,
		DeviceModel:   rec.deviceModel,
		Status:        rec.status,
		StatusMessage: model.FeedbackStatusMessage(rec.status),
		CreatedAt:     rec.createdAt,
		UpdatedAt:     rec.updatedAt,
	}
}

// adminFeedbackView 是运营侧视图：含提交人、个人信息、诊断与内部备注。
func adminFeedbackView(rec *feedbackRecord) model.AdminFeedback {
	return model.AdminFeedback{
		FeedbackID:   rec.id,
		UserID:       rec.userID,
		Kind:         rec.kind,
		Body:         rec.body,
		Contact:      rec.contact,
		Platform:     rec.platform,
		AppVersion:   rec.appVersion,
		BuildType:    rec.buildType,
		OSVersion:    rec.osVersion,
		DeviceModel:  rec.deviceModel,
		Diagnostics:  rec.diagnostics,
		Status:       rec.status,
		InternalNote: rec.internalNote,
		CreatedAt:    rec.createdAt,
		UpdatedAt:    rec.updatedAt,
	}
}

func encodeFeedbackCursor(at time.Time, id string) string {
	raw := at.UTC().Format(time.RFC3339Nano) + "|" + id
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeFeedbackCursor(cursor string) (time.Time, string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return time.Time{}, "", store.ErrBadCursor
	}
	parts := strings.SplitN(string(raw), "|", 2)
	if len(parts) != 2 {
		return time.Time{}, "", store.ErrBadCursor
	}
	at, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return time.Time{}, "", store.ErrBadCursor
	}
	if _, err := uuid.Parse(parts[1]); err != nil {
		return time.Time{}, "", store.ErrBadCursor
	}
	return at.UTC(), parts[1], nil
}
