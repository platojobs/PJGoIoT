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
		createdAt:   time.Now().UTC(),
	}
	s.feedback = append(s.feedback, rec)
	s.feedbackIdem[key] = rec
	return feedbackView(rec), false, nil
}

// ListUserFeedback 实现 store.Store：只返回本人数据，按 createdAt DESC, id DESC 游标分页。
func (s *Store) ListUserFeedback(_ context.Context, userID string, cursor string, limit int) (model.FeedbackPage, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}

	var after time.Time
	var afterID string
	if cursor != "" {
		var err error
		after, afterID, err = decodeFeedbackCursor(cursor)
		if err != nil {
			return model.FeedbackPage{}, err
		}
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	mine := make([]*feedbackRecord, 0, len(s.feedback))
	for _, rec := range s.feedback {
		if rec.userID == userID {
			mine = append(mine, rec)
		}
	}
	sort.Slice(mine, func(i, j int) bool {
		if mine[i].createdAt.Equal(mine[j].createdAt) {
			return mine[i].id > mine[j].id
		}
		return mine[i].createdAt.After(mine[j].createdAt)
	})

	items := []model.Feedback{}
	hasMore := false
	for _, rec := range mine {
		if cursor != "" {
			if !rec.createdAt.Before(after) && !(rec.createdAt.Equal(after) && rec.id < afterID) {
				continue
			}
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

// feedbackView 把内部记录转成对外视图：不返回 contact 与 diagnostics。
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
