package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/platojobs/PJGoIoT/internal/model"
	"github.com/platojobs/PJGoIoT/internal/store"
)

// catalogTimeout 是设备类型目录读写的时间预算。
const catalogTimeout = 5 * time.Second

// GetDeviceTypeCatalog 实现 store.Store。
// 目录版本与生成时间来自单行 device_type_catalog_meta，写入与版本自增在同一事务内，
// 因此管理端每次更新后 ETag 一定变化。
func (s *Store) GetDeviceTypeCatalog(ctx context.Context) (model.DeviceTypeCatalog, error) {
	ctx, cancel := context.WithTimeout(ctx, catalogTimeout)
	defer cancel()

	out := model.DeviceTypeCatalog{Items: []model.CatalogDeviceType{}}
	if err := s.db.QueryRowContext(ctx,
		`SELECT version, updated_at FROM device_type_catalog_meta WHERE id = 1`,
	).Scan(&out.Version, &out.GeneratedAt); err != nil {
		return model.DeviceTypeCatalog{}, fmt.Errorf("读取目录版本失败: %w", err)
	}

	rows, err := s.db.QueryContext(ctx, `
SELECT type, display_name, description, gatt_services, metrics, limitations, compatibility, updated_at
FROM device_type_catalog
WHERE deleted_at IS NULL
ORDER BY type`)
	if err != nil {
		return model.DeviceTypeCatalog{}, fmt.Errorf("查询设备类型目录失败: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			item                                  model.CatalogDeviceType
			gattRaw, metricsRaw, limitRaw, compat []byte
		)
		if err := rows.Scan(&item.Type, &item.DisplayName, &item.Description,
			&gattRaw, &metricsRaw, &limitRaw, &compat, &item.UpdatedAt); err != nil {
			return model.DeviceTypeCatalog{}, fmt.Errorf("读取设备类型目录行失败: %w", err)
		}
		if err := decodeCatalogJSON(gattRaw, metricsRaw, limitRaw, compat, &item.GATTServices, &item.Metrics, &item.Limitations, &item.Compatibility); err != nil {
			return model.DeviceTypeCatalog{}, err
		}
		item.UpdatedAt = item.UpdatedAt.UTC()
		out.Items = append(out.Items, item)
	}
	if err := rows.Err(); err != nil {
		return model.DeviceTypeCatalog{}, fmt.Errorf("遍历设备类型目录失败: %w", err)
	}
	out.GeneratedAt = out.GeneratedAt.UTC()
	return out, nil
}

// UpdateCatalogDeviceType 实现 store.Store：整项替换 + 版本自增 + 审计，单事务完成。
func (s *Store) UpdateCatalogDeviceType(ctx context.Context, deviceType string, input model.CatalogDeviceTypeInput) (model.CatalogDeviceType, int64, error) {
	ctx, cancel := context.WithTimeout(ctx, catalogTimeout)
	defer cancel()

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

	gattRaw, err := json.Marshal(gatt)
	if err != nil {
		return model.CatalogDeviceType{}, 0, fmt.Errorf("序列化 gattServices 失败: %w", err)
	}
	metricsRaw, err := json.Marshal(metrics)
	if err != nil {
		return model.CatalogDeviceType{}, 0, fmt.Errorf("序列化 metrics 失败: %w", err)
	}
	limitRaw, err := json.Marshal(limitations)
	if err != nil {
		return model.CatalogDeviceType{}, 0, fmt.Errorf("序列化 limitations 失败: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.CatalogDeviceType{}, 0, fmt.Errorf("开启目录更新事务失败: %w", err)
	}
	defer tx.Rollback()

	var exists int
	if err := tx.QueryRowContext(ctx,
		`SELECT 1 FROM device_type_catalog WHERE type = $1 AND deleted_at IS NULL`, deviceType,
	).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.CatalogDeviceType{}, 0, store.ErrNotFound
		}
		return model.CatalogDeviceType{}, 0, fmt.Errorf("查询目录项失败: %w", err)
	}

	item := model.CatalogDeviceType{
		Type:          deviceType,
		DisplayName:   input.DisplayName,
		Description:   input.Description,
		GATTServices:  gatt,
		Metrics:       metrics,
		Limitations:   limitations,
		Compatibility: compat,
	}
	if err := tx.QueryRowContext(ctx, `
UPDATE device_type_catalog
SET display_name = $2, description = $3, gatt_services = $4::jsonb, metrics = $5::jsonb,
    limitations = $6::jsonb, compatibility = $7::jsonb, updated_at = now()
WHERE type = $1 AND deleted_at IS NULL
RETURNING updated_at`,
		deviceType, input.DisplayName, input.Description, string(gattRaw), string(metricsRaw),
		string(limitRaw), string(compat),
	).Scan(&item.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.CatalogDeviceType{}, 0, store.ErrNotFound
		}
		return model.CatalogDeviceType{}, 0, fmt.Errorf("更新目录项失败: %w", err)
	}

	var version int64
	if err := tx.QueryRowContext(ctx,
		`UPDATE device_type_catalog_meta SET version = version + 1, updated_at = now() WHERE id = 1 RETURNING version`,
	).Scan(&version); err != nil {
		return model.CatalogDeviceType{}, 0, fmt.Errorf("递增目录版本失败: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO device_type_catalog_audits (device_type, action, actor, catalog_version) VALUES ($1, 'upsert', 'admin', $2)`,
		deviceType, version,
	); err != nil {
		return model.CatalogDeviceType{}, 0, fmt.Errorf("写入目录审计失败: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return model.CatalogDeviceType{}, 0, fmt.Errorf("提交目录更新失败: %w", err)
	}

	item.UpdatedAt = item.UpdatedAt.UTC()
	return item, version, nil
}

// DeleteCatalogDeviceType 实现 store.Store：软删除 + 版本自增 + 审计，单事务完成。
func (s *Store) DeleteCatalogDeviceType(ctx context.Context, deviceType string) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, catalogTimeout)
	defer cancel()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("开启目录删除事务失败: %w", err)
	}
	defer tx.Rollback()

	var exists int
	if err := tx.QueryRowContext(ctx,
		`SELECT 1 FROM device_type_catalog WHERE type = $1 AND deleted_at IS NULL`, deviceType,
	).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, store.ErrNotFound
		}
		return 0, fmt.Errorf("查询目录项失败: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE device_type_catalog SET deleted_at = now(), updated_at = now() WHERE type = $1`, deviceType,
	); err != nil {
		return 0, fmt.Errorf("软删除目录项失败: %w", err)
	}

	var version int64
	if err := tx.QueryRowContext(ctx,
		`UPDATE device_type_catalog_meta SET version = version + 1, updated_at = now() WHERE id = 1 RETURNING version`,
	).Scan(&version); err != nil {
		return 0, fmt.Errorf("递增目录版本失败: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO device_type_catalog_audits (device_type, action, actor, catalog_version) VALUES ($1, 'delete', 'admin', $2)`,
		deviceType, version,
	); err != nil {
		return 0, fmt.Errorf("写入目录审计失败: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("提交目录删除失败: %w", err)
	}
	return version, nil
}

// RecordCatalogAccess 实现 store.Store：同一用户同一目录版本只记一次。
func (s *Store) RecordCatalogAccess(ctx context.Context, userID string, catalogVersion int64) error {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return fmt.Errorf("非法 user id: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, catalogTimeout)
	defer cancel()
	if _, err := s.db.ExecContext(ctx, `
INSERT INTO device_type_catalog_accesses (user_id, catalog_version)
VALUES ($1, $2)
ON CONFLICT (user_id, catalog_version) DO NOTHING`, uid, catalogVersion); err != nil {
		return fmt.Errorf("记录目录访问失败: %w", err)
	}
	return nil
}

func decodeCatalogJSON(gattRaw, metricsRaw, limitRaw, compatRaw []byte, gatt *[]model.GATTService,
	metrics *[]model.CatalogMetric, limitations *[]string, compatibility *json.RawMessage) error {
	if len(gattRaw) > 0 {
		if err := json.Unmarshal(gattRaw, gatt); err != nil {
			return fmt.Errorf("解析 gattServices 失败: %w", err)
		}
	}
	if len(metricsRaw) > 0 {
		if err := json.Unmarshal(metricsRaw, metrics); err != nil {
			return fmt.Errorf("解析 metrics 失败: %w", err)
		}
	}
	if len(limitRaw) > 0 {
		if err := json.Unmarshal(limitRaw, limitations); err != nil {
			return fmt.Errorf("解析 limitations 失败: %w", err)
		}
	}
	if len(compatRaw) > 0 {
		*compatibility = json.RawMessage(append([]byte(nil), compatRaw...))
	} else {
		*compatibility = json.RawMessage(`{}`)
	}
	if *gatt == nil {
		*gatt = []model.GATTService{}
	}
	if *metrics == nil {
		*metrics = []model.CatalogMetric{}
	}
	if *limitations == nil {
		*limitations = []string{}
	}
	return nil
}
