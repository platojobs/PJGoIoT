package fixture

import (
	"context"
	"encoding/json"
	"time"

	"github.com/platojobs/PJGoIoT/internal/devicecatalog"
	"github.com/platojobs/PJGoIoT/internal/model"
	"github.com/platojobs/PJGoIoT/internal/store"
)

// catalogFromTruth 把唯一真值表转换为内存目录项，保证 fixture 与 Postgres seed 一致。
func catalogFromTruth() []model.CatalogDeviceType {
	types := devicecatalog.Types()
	out := make([]model.CatalogDeviceType, 0, len(types))
	now := time.Now().UTC()
	for _, t := range types {
		gatt := make([]model.GATTService, 0, len(t.GATTServices))
		for _, svc := range t.GATTServices {
			gatt = append(gatt, model.GATTService{UUID: svc.UUID, Name: svc.Name})
		}
		metrics := make([]model.CatalogMetric, 0, len(t.Metrics))
		for _, m := range t.Metrics {
			metrics = append(metrics, model.CatalogMetric{
				Key: m.Key, Unit: m.Unit, CharacteristicUUID: m.CharacteristicUUID,
			})
		}
		compat, _ := json.Marshal(t.Compatibility)
		out = append(out, model.CatalogDeviceType{
			Type:          t.Type,
			DisplayName:   t.DisplayName,
			Description:   t.Description,
			GATTServices:  gatt,
			Metrics:       metrics,
			Limitations:   append([]string(nil), t.Limitations...),
			Compatibility: compat,
			UpdatedAt:     now,
		})
	}
	return out
}

// GetDeviceTypeCatalog 实现 store.Store。
func (s *Store) GetDeviceTypeCatalog(ctx context.Context) (model.DeviceTypeCatalog, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	items := make([]model.CatalogDeviceType, len(s.catalog))
	copy(items, s.catalog)
	for i := range items {
		items[i].GATTServices = append([]model.GATTService(nil), s.catalog[i].GATTServices...)
		items[i].Metrics = append([]model.CatalogMetric(nil), s.catalog[i].Metrics...)
		items[i].Limitations = append([]string(nil), s.catalog[i].Limitations...)
		items[i].Compatibility = append(json.RawMessage(nil), s.catalog[i].Compatibility...)
	}
	return model.DeviceTypeCatalog{
		Version:     s.catalogVersion,
		GeneratedAt: s.catalogUpdatedAt,
		Items:       items,
	}, nil
}

// UpdateCatalogDeviceType 实现 store.Store。
func (s *Store) UpdateCatalogDeviceType(ctx context.Context, deviceType string, input model.CatalogDeviceTypeInput) (model.CatalogDeviceType, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	idx := -1
	for i := range s.catalog {
		if s.catalog[i].Type == deviceType {
			idx = i
			break
		}
	}
	if idx < 0 {
		return model.CatalogDeviceType{}, 0, store.ErrNotFound
	}
	gatt := input.GATTServices
	if gatt == nil {
		gatt = []model.GATTService{}
	}
	metrics := input.Metrics
	if metrics == nil {
		metrics = []model.CatalogMetric{}
	}
	limitations := input.Limitations
	if limitations == nil {
		limitations = []string{}
	}
	compat := input.Compatibility
	if len(compat) == 0 {
		compat = json.RawMessage(`{}`)
	}
	item := model.CatalogDeviceType{
		Type:          deviceType,
		DisplayName:   input.DisplayName,
		Description:   input.Description,
		GATTServices:  append([]model.GATTService(nil), gatt...),
		Metrics:       append([]model.CatalogMetric(nil), metrics...),
		Limitations:   append([]string(nil), limitations...),
		Compatibility: append(json.RawMessage(nil), compat...),
		UpdatedAt:     time.Now().UTC(),
	}
	s.catalog[idx] = item
	s.catalogVersion++
	s.catalogUpdatedAt = item.UpdatedAt
	return item, s.catalogVersion, nil
}

// DeleteCatalogDeviceType 实现 store.Store：内存实现直接移除，语义与 Postgres 软删一致。
func (s *Store) DeleteCatalogDeviceType(ctx context.Context, deviceType string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i := range s.catalog {
		if s.catalog[i].Type == deviceType {
			s.catalog = append(s.catalog[:i], s.catalog[i+1:]...)
			s.catalogVersion++
			s.catalogUpdatedAt = time.Now().UTC()
			return s.catalogVersion, nil
		}
	}
	return 0, store.ErrNotFound
}

// RecordCatalogAccess 实现 store.Store：同一用户同一版本只记一次。
func (s *Store) RecordCatalogAccess(ctx context.Context, userID string, catalogVersion int64) error {
	if userID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	versions, ok := s.catalogAccesses[userID]
	if !ok {
		versions = map[int64]struct{}{}
		s.catalogAccesses[userID] = versions
	}
	versions[catalogVersion] = struct{}{}
	return nil
}
