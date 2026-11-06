package fixture

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/platojobs/PJGoIoT/internal/model"
	"github.com/platojobs/PJGoIoT/internal/store"
)

const maxPairingAttempts = 5

// deletionTokenRecord 是内存版一次性删除令牌；userID 不随账号删除而消失（墓碑）。
type deletionTokenRecord struct {
	userID    string
	expiresAt time.Time
	usedAt    *time.Time
	createdAt time.Time
}

// exportTaskRecord 是内存版异步导出任务。
type exportTaskRecord struct {
	id                string
	userID            string
	status            string
	artifact          *model.PersonalDataExport
	failureReason     string
	downloadTokenHash string
	downloadExpiresAt time.Time
	downloadUsedAt    *time.Time
	createdAt         time.Time
	completedAt       *time.Time
	expiresAt         time.Time
}

// GetUser 实现 store.Store。
func (s *Store) GetUser(ctx context.Context, userID string) (model.User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	data, ok := s.users[userID]
	if !ok {
		return model.User{}, store.ErrUnauthorized
	}
	return model.User{ID: userID, Kind: data.kind, CreatedAt: data.createdAt}, nil
}

// CreateDeletionToken 实现 store.Store：同一账号的新令牌使旧令牌失效。
func (s *Store) CreateDeletionToken(ctx context.Context, userID string, tokenHash string, expiresAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.users[userID]; !ok {
		return store.ErrUnauthorized
	}
	for hash, rec := range s.deletionTokens {
		if rec.userID == userID && rec.usedAt == nil {
			delete(s.deletionTokens, hash)
		}
	}
	s.deletionTokens[tokenHash] = &deletionTokenRecord{userID: userID, expiresAt: expiresAt.UTC(), createdAt: time.Now().UTC()}
	return nil
}

// DeleteAccount 实现 store.Store。
func (s *Store) DeleteAccount(ctx context.Context, tokenHash string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	rec, ok := s.deletionTokens[tokenHash]
	if !ok {
		return store.ErrDeletionTokenInvalid
	}
	if rec.usedAt != nil {
		return store.ErrNotFound
	}
	if !rec.expiresAt.After(now) {
		return store.ErrDeletionTokenExpired
	}
	userID := rec.userID
	if _, ok := s.users[userID]; !ok {
		return store.ErrNotFound
	}

	// owner 的家庭还有其他成员时必须先转让或移除成员，否则会留下孤儿家庭。
	for _, h := range s.households {
		if h.OwnerUserID != userID {
			continue
		}
		if len(h.members) > 1 {
			return store.ErrAccountDeletionBlocked
		}
	}

	delete(s.users, userID)
	for hash, t := range s.tokens {
		if t.userID == userID {
			delete(s.tokens, hash)
		}
	}
	for key := range s.idempotency {
		if strings.HasPrefix(key, userID+"|") {
			delete(s.idempotency, key)
		}
	}
	for hash, code := range s.pairingCodes {
		if code.userID == userID {
			delete(s.pairingCodes, hash)
		}
	}
	kept := s.changes[:0]
	for _, c := range s.changes {
		if c.userID != userID {
			kept = append(kept, c)
		}
	}
	s.changes = kept

	keptFeedback := s.feedback[:0]
	removedFeedbackIDs := map[string]struct{}{}
	for _, f := range s.feedback {
		if f.userID != userID {
			keptFeedback = append(keptFeedback, f)
			continue
		}
		delete(s.feedbackIdem, userID+"|"+f.idemKey)
		removedFeedbackIDs[f.id] = struct{}{}
	}
	s.feedback = keptFeedback
	if len(removedFeedbackIDs) > 0 {
		keptEvents := s.feedbackEvents[:0]
		for _, ev := range s.feedbackEvents {
			if _, ok := removedFeedbackIDs[ev.feedbackID]; !ok {
				keptEvents = append(keptEvents, ev)
			}
		}
		s.feedbackEvents = keptEvents
	}

	// 家庭：自己解散的家庭整条删除；其他人家庭只移除自己的成员关系。
	owned := map[string]struct{}{}
	for id, h := range s.households {
		if h.OwnerUserID == userID {
			owned[id] = struct{}{}
			delete(s.households, id)
		}
	}
	for id, h := range s.households {
		if _, ok := h.members[userID]; ok {
			delete(h.members, userID)
		}
		if len(h.members) == 0 {
			delete(s.households, id)
		}
	}
	for id, room := range s.rooms {
		if _, ok := owned[room.HouseholdID]; ok {
			delete(s.rooms, id)
		}
	}
	for hash, inv := range s.invitations {
		if inv.invitedBy == userID {
			delete(s.invitations, hash)
			continue
		}
		if _, ok := owned[inv.householdID]; ok {
			delete(s.invitations, hash)
		}
	}
	delete(s.catalogAccesses, userID)
	for id, task := range s.exportTasks {
		if task.userID == userID {
			delete(s.exportTasks, id)
		}
	}

	// 令牌墓碑保留，标记已使用：重放删除请求返回 404。
	used := now.UTC()
	rec.usedAt = &used
	return nil
}

// CreateExportTask 实现 store.Store。
func (s *Store) CreateExportTask(ctx context.Context, userID string, expiresAt time.Time) (model.ExportTaskStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.users[userID]; !ok {
		return model.ExportTaskStatus{}, store.ErrUnauthorized
	}
	id := uuid.NewString()
	now := time.Now().UTC()
	s.exportTasks[id] = &exportTaskRecord{
		id:        id,
		userID:    userID,
		status:    "pending",
		createdAt: now,
		expiresAt: expiresAt.UTC(),
	}
	return model.ExportTaskStatus{ID: id, Status: "pending", CreatedAt: now, ExpiresAt: expiresAt.UTC()}, nil
}

// BuildPersonalDataExport 实现 store.Store。
func (s *Store) BuildPersonalDataExport(ctx context.Context, userID string) (model.PersonalDataExport, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	data, ok := s.users[userID]
	if !ok {
		return model.PersonalDataExport{}, store.ErrUnauthorized
	}

	devices := make([]model.UserDevice, 0, len(data.byID))
	for _, d := range data.byID {
		devices = append(devices, s.enrichUserDevice(*d))
	}
	sort.SliceStable(devices, func(i, j int) bool {
		if !devices[i].UpdatedAt.Equal(devices[j].UpdatedAt) {
			return devices[i].UpdatedAt.After(devices[j].UpdatedAt)
		}
		return devices[i].ID < devices[j].ID
	})

	type aggKey struct{ platform, externalID, metric, unit string }
	aggs := map[aggKey]*model.ExportReadingAggregate{}
	order := make([]aggKey, 0)
	for _, rec := range data.readings {
		k := aggKey{rec.platform, rec.externalID, rec.metric, rec.unit}
		item, ok := aggs[k]
		if !ok {
			item = &model.ExportReadingAggregate{
				Platform: rec.platform, ExternalID: rec.externalID,
				Metric: rec.metric, Unit: rec.unit,
				Min: rec.value, Max: rec.value,
				FirstAt: rec.recordedAt.UTC(), LastAt: rec.recordedAt.UTC(),
			}
			aggs[k] = item
			order = append(order, k)
		}
		item.Count++
		item.Avg += rec.value
		if rec.value < item.Min {
			item.Min = rec.value
		}
		if rec.value > item.Max {
			item.Max = rec.value
		}
		if rec.recordedAt.Before(item.FirstAt) {
			item.FirstAt = rec.recordedAt.UTC()
		}
		if rec.recordedAt.After(item.LastAt) {
			item.LastAt = rec.recordedAt.UTC()
		}
	}
	sort.SliceStable(order, func(i, j int) bool {
		a, b := order[i], order[j]
		if a.platform != b.platform {
			return a.platform < b.platform
		}
		if a.externalID != b.externalID {
			return a.externalID < b.externalID
		}
		if a.metric != b.metric {
			return a.metric < b.metric
		}
		return a.unit < b.unit
	})
	readings := make([]model.ExportReadingAggregate, 0, len(order))
	for _, k := range order {
		item := *aggs[k]
		item.Avg /= float64(item.Count)
		readings = append(readings, item)
	}

	households := make([]model.ExportHousehold, 0)
	for _, h := range s.households {
		role, ok := h.members[userID]
		if !ok {
			continue
		}
		item := model.ExportHousehold{
			ID: h.ID, Name: h.Name, Role: role, Personal: h.Personal,
			CreatedAt: h.CreatedAt.UTC(), Rooms: []model.ExportRoom{},
		}
		rooms := make([]model.ExportRoom, 0)
		for _, room := range s.rooms {
			if room.HouseholdID == h.ID {
				rooms = append(rooms, model.ExportRoom{
					ID: room.ID, Name: room.Name, SortOrder: room.SortOrder, CreatedAt: room.CreatedAt.UTC(),
				})
			}
		}
		sort.SliceStable(rooms, func(i, j int) bool {
			if rooms[i].SortOrder != rooms[j].SortOrder {
				return rooms[i].SortOrder < rooms[j].SortOrder
			}
			return rooms[i].CreatedAt.Before(rooms[j].CreatedAt)
		})
		item.Rooms = rooms
		households = append(households, item)
	}
	sort.SliceStable(households, func(i, j int) bool {
		if !households[i].CreatedAt.Equal(households[j].CreatedAt) {
			return households[i].CreatedAt.Before(households[j].CreatedAt)
		}
		return households[i].ID < households[j].ID
	})

	feedback := make([]model.ExportFeedback, 0)
	for _, f := range s.feedback {
		if f.userID != userID {
			continue
		}
		feedback = append(feedback, model.ExportFeedback{
			FeedbackID: f.id, Kind: f.kind, Body: f.body, Contact: f.contact,
			Platform: f.platform, AppVersion: f.appVersion, BuildType: f.buildType,
			OSVersion: f.osVersion, DeviceModel: f.deviceModel, Diagnostics: f.diagnostics,
			Status: f.status, CreatedAt: f.createdAt.UTC(), UpdatedAt: f.updatedAt.UTC(),
		})
	}
	sort.SliceStable(feedback, func(i, j int) bool {
		if !feedback[i].CreatedAt.Equal(feedback[j].CreatedAt) {
			return feedback[i].CreatedAt.Before(feedback[j].CreatedAt)
		}
		return feedback[i].FeedbackID < feedback[j].FeedbackID
	})

	return model.PersonalDataExport{
		User:        model.User{ID: userID, Kind: data.kind, CreatedAt: data.createdAt.UTC()},
		Devices:     devices,
		Readings:    readings,
		Households:  households,
		Feedback:    feedback,
		GeneratedAt: time.Now().UTC(),
	}, nil
}

// FinishExportTask 实现 store.Store。
func (s *Store) FinishExportTask(ctx context.Context, taskID string, artifact model.PersonalDataExport) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	task, ok := s.exportTasks[taskID]
	if !ok {
		return store.ErrNotFound
	}
	now := time.Now().UTC()
	task.status = "ready"
	task.artifact = &artifact
	task.completedAt = &now
	return nil
}

// FailExportTask 实现 store.Store。
func (s *Store) FailExportTask(ctx context.Context, taskID string, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	task, ok := s.exportTasks[taskID]
	if !ok {
		return store.ErrNotFound
	}
	now := time.Now().UTC()
	task.status = "failed"
	task.failureReason = reason
	task.completedAt = &now
	return nil
}

// GetExportTask 实现 store.Store。
func (s *Store) GetExportTask(ctx context.Context, userID string, taskID string) (model.ExportTaskStatus, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	task, ok := s.exportTasks[taskID]
	if !ok || task.userID != userID || !task.expiresAt.After(time.Now().UTC()) {
		return model.ExportTaskStatus{}, store.ErrNotFound
	}
	return exportTaskStatus(task), nil
}

// PrepareExportDownload 实现 store.Store：替换旧下载令牌。
func (s *Store) PrepareExportDownload(ctx context.Context, userID string, taskID string, tokenHash string, expiresAt time.Time) (model.ExportTaskStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	task, ok := s.exportTasks[taskID]
	if !ok || task.userID != userID || !task.expiresAt.After(time.Now().UTC()) {
		return model.ExportTaskStatus{}, store.ErrNotFound
	}
	if task.status == "pending" {
		return model.ExportTaskStatus{}, store.ErrExportTaskNotReady
	}
	if task.status != "ready" {
		return model.ExportTaskStatus{}, store.ErrNotFound
	}
	dl := expiresAt.UTC()
	task.downloadTokenHash = tokenHash
	task.downloadExpiresAt = dl
	task.downloadUsedAt = nil
	out := exportTaskStatus(task)
	out.DownloadExpiresAt = &dl
	return out, nil
}

// ConsumeExportDownload 实现 store.Store。
func (s *Store) ConsumeExportDownload(ctx context.Context, tokenHash string, now time.Time) (model.PersonalDataExport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, task := range s.exportTasks {
		if task.downloadTokenHash != tokenHash {
			continue
		}
		if task.status != "ready" || task.artifact == nil {
			return model.PersonalDataExport{}, store.ErrExportDownloadInvalid
		}
		if task.downloadUsedAt != nil {
			return model.PersonalDataExport{}, store.ErrExportDownloadExpired
		}
		if !task.downloadExpiresAt.After(now) {
			return model.PersonalDataExport{}, store.ErrExportDownloadExpired
		}
		used := now.UTC()
		task.downloadUsedAt = &used
		return *task.artifact, nil
	}
	return model.PersonalDataExport{}, store.ErrExportDownloadInvalid
}

// CleanupDeletionTokens 实现 store.Store。
func (s *Store) CleanupDeletionTokens(ctx context.Context, before time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int64
	for hash, rec := range s.deletionTokens {
		if rec.expiresAt.Before(before) {
			delete(s.deletionTokens, hash)
			n++
		}
	}
	return n, nil
}

// CleanupExportTasks 实现 store.Store：整条删除过期任务（产物一并清理）。
func (s *Store) CleanupExportTasks(ctx context.Context, before time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int64
	for id, task := range s.exportTasks {
		if task.expiresAt.Before(before) {
			delete(s.exportTasks, id)
			n++
		}
	}
	return n, nil
}

func exportTaskStatus(task *exportTaskRecord) model.ExportTaskStatus {
	out := model.ExportTaskStatus{
		ID:        task.id,
		Status:    task.status,
		CreatedAt: task.createdAt.UTC(),
		ExpiresAt: task.expiresAt.UTC(),
	}
	if task.completedAt != nil {
		t := task.completedAt.UTC()
		out.CompletedAt = &t
	}
	return out
}

// CreatePairingCode 实现 store.Store：同一用户的新码使旧码失效。
func (s *Store) CreatePairingCode(ctx context.Context, userID string, codeHash string, expiresAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.users[userID]; !ok {
		return store.ErrUnauthorized
	}
	now := time.Now().UTC()
	for hash, rec := range s.pairingCodes {
		if rec.userID == userID || !rec.expiresAt.After(now) {
			delete(s.pairingCodes, hash)
		}
	}
	s.pairingCodes[codeHash] = &pairingRecord{userID: userID, expiresAt: expiresAt.UTC()}
	return nil
}

// ClaimPairingCode 实现 store.Store。
func (s *Store) ClaimPairingCode(ctx context.Context, codeHash string, tokenHash string, now time.Time, ttl time.Duration, tokenExpiresAt time.Time) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rec, ok := s.pairingCodes[codeHash]
	if !ok {
		s.recordPairingFailureLocked(codeHash, now, ttl)
		return "", store.ErrNotFound
	}
	if rec.userID == "" || rec.attempts >= maxPairingAttempts || !rec.expiresAt.After(now) {
		s.recordPairingFailureLocked(codeHash, now, ttl)
		return "", store.ErrNotFound
	}
	if _, exists := s.users[rec.userID]; !exists {
		delete(s.pairingCodes, codeHash)
		return "", store.ErrNotFound
	}

	delete(s.pairingCodes, codeHash)
	s.tokens[tokenHash] = tokenRecord{userID: rec.userID, expiresAt: tokenExpiresAt.UTC()}
	return rec.userID, nil
}

func (s *Store) recordPairingFailureLocked(codeHash string, now time.Time, ttl time.Duration) {
	rec, ok := s.pairingCodes[codeHash]
	if !ok {
		s.pairingCodes[codeHash] = &pairingRecord{attempts: 1, expiresAt: now.Add(ttl).UTC()}
		return
	}
	rec.attempts++
	if !rec.expiresAt.After(now) {
		rec.expiresAt = now.Add(ttl).UTC()
	}
	if rec.attempts >= maxPairingAttempts {
		delete(s.pairingCodes, codeHash)
	}
}

// CleanupPairingCodes 实现 store.Store。
func (s *Store) CleanupPairingCodes(ctx context.Context, before time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int64
	for hash, rec := range s.pairingCodes {
		if rec.expiresAt.Before(before) {
			delete(s.pairingCodes, hash)
			n++
		}
	}
	return n, nil
}

// CleanupChangeLog 实现 store.Store：删除 serverTS 早于 before 的变更。
func (s *Store) CleanupChangeLog(ctx context.Context, before time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.changes[:0]
	var n int64
	for _, rec := range s.changes {
		if rec.serverTS.Before(before) {
			n++
			continue
		}
		kept = append(kept, rec)
	}
	s.changes = kept
	return n, nil
}

// CleanupIdempotencyKeys 实现 store.Store：删除 createdAt 早于 before 的幂等键。
func (s *Store) CleanupIdempotencyKeys(ctx context.Context, before time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int64
	for key, rec := range s.idempotency {
		if rec.CreatedAt.Before(before) {
			delete(s.idempotency, key)
			n++
		}
	}
	return n, nil
}

// CleanupTokens 实现 store.Store：删除 expiresAt 早于 before 的 token。
func (s *Store) CleanupTokens(ctx context.Context, before time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int64
	for hash, rec := range s.tokens {
		if rec.expiresAt.Before(before) {
			delete(s.tokens, hash)
			n++
		}
	}
	return n, nil
}

// ApproxCounts 实现 store.Store：内存实现直接返回精确行数。
func (s *Store) ApproxCounts(ctx context.Context) (model.StoreCounts, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	counts := model.StoreCounts{Users: int64(len(s.users))}
	for _, data := range s.users {
		counts.Devices += int64(len(data.byID))
		counts.Readings += int64(len(data.readings))
	}
	return counts, nil
}
