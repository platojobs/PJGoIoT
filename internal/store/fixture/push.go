package fixture

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/platojobs/PJGoIoT/internal/model"
	"github.com/platojobs/PJGoIoT/internal/store"
)

type pushDeviceRecord struct {
	userID string
	device model.PushRegistration
}

type pushDeliveryRecord struct {
	delivery    model.PushDelivery
	status      string
	claimedAt   *time.Time
	provider    string
	dryRun      bool
	sentAt      *time.Time
	terminal    string
	lastError   string
	alertID     string
	deviceID    string
	title, body string
}

// UpsertPushDevice 实现 store.Store。
func (s *Store) UpsertPushDevice(ctx context.Context, userID string, input model.PushRegistrationInput, now time.Time) (model.PushRegistration, error) {
	platform := strings.TrimSpace(strings.ToLower(input.Platform))
	if platform != "android" && platform != "ios" {
		return model.PushRegistration{}, fmt.Errorf("%w: platform 只能是 android 或 ios", store.ErrPushDeviceInvalid)
	}
	token := strings.TrimSpace(input.Token)
	if token == "" || len(token) > 4096 {
		return model.PushRegistration{}, fmt.Errorf("%w: token 长度必须在 1..4096", store.ErrPushDeviceInvalid)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.users[userID]; !ok {
		return model.PushRegistration{}, store.ErrNotFound
	}
	for _, rec := range s.pushDevices {
		if rec.userID != userID || rec.device.Token != token {
			continue
		}
		rec.device.Platform = platform
		rec.device.AppVersion = strings.TrimSpace(input.AppVersion)
		rec.device.DeviceModel = strings.TrimSpace(input.DeviceModel)
		rec.device.Status = "active"
		rec.device.UpdatedAt = now.UTC()
		return rec.device, nil
	}
	rec := &pushDeviceRecord{
		userID: userID,
		device: model.PushRegistration{
			ID:          uuid.NewString(),
			Platform:    platform,
			Token:       token,
			AppVersion:  strings.TrimSpace(input.AppVersion),
			DeviceModel: strings.TrimSpace(input.DeviceModel),
			Status:      "active",
			CreatedAt:   now.UTC(),
			UpdatedAt:   now.UTC(),
		},
	}
	s.pushDevices[rec.device.ID] = rec
	return rec.device, nil
}

// DeactivatePushDevice 实现 store.Store。
func (s *Store) DeactivatePushDevice(ctx context.Context, userID string, deviceID string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.pushDevices[deviceID]
	if !ok || rec.userID != userID {
		return store.ErrNotFound
	}
	rec.device.Status = "inactive"
	rec.device.UpdatedAt = now.UTC()
	return nil
}

// CreateAlertWithDeliveries 实现 store.Store。
func (s *Store) CreateAlertWithDeliveries(ctx context.Context, input model.AlertInput, now time.Time) (model.Alert, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.users[input.UserID]; !ok {
		return model.Alert{}, 0, store.ErrNotFound
	}
	payload := input.Payload
	if payload == nil {
		payload = map[string]string{}
	}
	alert := model.Alert{
		ID:        uuid.NewString(),
		UserID:    input.UserID,
		Kind:      input.Kind,
		Title:     input.Title,
		Body:      input.Body,
		Severity:  input.Severity,
		Payload:   payload,
		CreatedAt: now.UTC(),
	}
	s.pushAlerts = append(s.pushAlerts, alert)

	ids := make([]string, 0, len(s.pushDevices))
	for id, rec := range s.pushDevices {
		if rec.userID == alert.UserID && rec.device.Status == "active" {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		rec := s.pushDevices[id]
		data := map[string]string{"alertId": alert.ID, "kind": alert.Kind, "severity": alert.Severity}
		for k, v := range payload {
			data[k] = v
		}
		s.pushDeliveries = append(s.pushDeliveries, &pushDeliveryRecord{
			delivery: model.PushDelivery{
				ID:            uuid.NewString(),
				UserID:        alert.UserID,
				AlertID:       alert.ID,
				PushDeviceID:  id,
				Platform:      rec.device.Platform,
				Token:         rec.device.Token,
				Title:         alert.Title,
				Body:          alert.Body,
				Data:          data,
				NextAttemptAt: now.UTC(),
				CreatedAt:     now.UTC(),
			},
			status:   "pending",
			alertID:  alert.ID,
			deviceID: id,
			title:    alert.Title,
			body:     alert.Body,
		})
	}
	return alert, len(ids), nil
}

// ClaimPushDeliveries 实现 store.Store。
func (s *Store) ClaimPushDeliveries(ctx context.Context, now time.Time, limit int, sendingStaleBefore time.Time) ([]model.PushDelivery, error) {
	if limit <= 0 {
		limit = 50
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, rec := range s.pushDeliveries {
		if rec.status == "sending" && rec.claimedAt != nil && rec.claimedAt.Before(sendingStaleBefore) {
			rec.status = "pending"
			rec.claimedAt = nil
			rec.delivery.NextAttemptAt = now.UTC()
		}
	}

	pending := make([]*pushDeliveryRecord, 0, len(s.pushDeliveries))
	for _, rec := range s.pushDeliveries {
		if rec.status == "pending" && !rec.delivery.NextAttemptAt.After(now) {
			pending = append(pending, rec)
		}
	}
	sort.Slice(pending, func(i, j int) bool {
		if pending[i].delivery.NextAttemptAt.Equal(pending[j].delivery.NextAttemptAt) {
			return pending[i].delivery.ID < pending[j].delivery.ID
		}
		return pending[i].delivery.NextAttemptAt.Before(pending[j].delivery.NextAttemptAt)
	})
	if len(pending) > limit {
		pending = pending[:limit]
	}
	out := make([]model.PushDelivery, 0, len(pending))
	claimed := now.UTC()
	for _, rec := range pending {
		rec.status = "sending"
		rec.claimedAt = &claimed
		rec.delivery.Attempts++
		out = append(out, rec.delivery)
	}
	return out, nil
}

// CompletePushDelivery 实现 store.Store。
func (s *Store) CompletePushDelivery(ctx context.Context, deliveryID string, provider string, dryRun bool, sentAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.findDelivery(deliveryID)
	if rec == nil {
		return store.ErrNotFound
	}
	rec.status = "sent"
	rec.provider = provider
	rec.dryRun = dryRun
	t := sentAt.UTC()
	rec.sentAt = &t
	rec.claimedAt = nil
	rec.lastError = ""
	rec.terminal = ""
	return nil
}

// RetryPushDelivery 实现 store.Store。
func (s *Store) RetryPushDelivery(ctx context.Context, deliveryID string, nextAttemptAt time.Time, lastError string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.findDelivery(deliveryID)
	if rec == nil {
		return store.ErrNotFound
	}
	rec.status = "pending"
	rec.delivery.NextAttemptAt = nextAttemptAt.UTC()
	rec.lastError = lastError
	rec.claimedAt = nil
	return nil
}

// FailPushDelivery 实现 store.Store。
func (s *Store) FailPushDelivery(ctx context.Context, deliveryID string, reason string, deactivate bool, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.findDelivery(deliveryID)
	if rec == nil {
		return store.ErrNotFound
	}
	rec.status = "failed"
	rec.terminal = reason
	rec.lastError = reason
	rec.claimedAt = nil
	if deactivate {
		if dev, ok := s.pushDevices[rec.delivery.PushDeviceID]; ok {
			dev.device.Status = "inactive"
			dev.device.UpdatedAt = now.UTC()
		}
	}
	return nil
}

// CleanupPushDeliveries 实现 store.Store。
func (s *Store) CleanupPushDeliveries(ctx context.Context, before time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var kept []*pushDeliveryRecord
	var deleted int64
	for _, rec := range s.pushDeliveries {
		if (rec.status == "sent" || rec.status == "failed") && rec.delivery.CreatedAt.Before(before) {
			deleted++
			continue
		}
		kept = append(kept, rec)
	}
	s.pushDeliveries = kept
	return deleted, nil
}

// CleanupPushDevices 实现 store.Store。
func (s *Store) CleanupPushDevices(ctx context.Context, before time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var deleted int64
	for id, rec := range s.pushDevices {
		if rec.device.Status == "inactive" && rec.device.UpdatedAt.Before(before) {
			delete(s.pushDevices, id)
			deleted++
		}
	}
	return deleted, nil
}

func (s *Store) findDelivery(id string) *pushDeliveryRecord {
	for _, rec := range s.pushDeliveries {
		if rec.delivery.ID == id {
			return rec
		}
	}
	return nil
}
