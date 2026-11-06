package postgres_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/platojobs/PJGoIoT/internal/config"
	"github.com/platojobs/PJGoIoT/internal/devicecatalog"
	"github.com/platojobs/PJGoIoT/internal/httpapi"
	"github.com/platojobs/PJGoIoT/internal/model"
	"github.com/platojobs/PJGoIoT/internal/store"
	"github.com/platojobs/PJGoIoT/internal/store/postgres"
)

// TestPostgresCatalogDeviceTypesIntegration 覆盖 v0.18.0 的目录落库、版本自增、软删除、
// 审计与访问记录，并把接口返回内容与 internal/devicecatalog 唯一真值表逐项比对。
// 这是本轮最重要的测试：任何手改迁移导致 metrics / 特征 UUID 与解析能力声明漂移都会失败。
func TestPostgresCatalogDeviceTypesIntegration(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("未设置 TEST_DATABASE_URL，跳过真实数据库集成测试")
	}
	runMigrations(t, dsn)

	ctx := context.Background()
	st, err := postgres.New(ctx, dsn)
	if err != nil {
		t.Fatalf("连接数据库失败: %v", err)
	}
	defer st.Close()

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("打开清理连接失败: %v", err)
	}
	resetDeviceCatalog(t, db)
	t.Cleanup(func() {
		resetDeviceCatalog(t, db)
		db.Close()
	})

	t.Run("目录读取与解析能力一致", func(t *testing.T) {
		catalog, err := st.GetDeviceTypeCatalog(ctx)
		if err != nil {
			t.Fatalf("读取目录失败: %v", err)
		}
		if catalog.Version < 1 {
			t.Fatalf("目录版本=%d", catalog.Version)
		}
		if len(catalog.Items) != len(devicecatalog.Types()) {
			t.Fatalf("目录项=%d，真值表=%d", len(catalog.Items), len(devicecatalog.Types()))
		}

		// 接口返回的 metrics / 特征 UUID / GATT 服务必须与解析能力声明逐项一致。
		for _, truth := range devicecatalog.Types() {
			var got *model.CatalogDeviceType
			for i := range catalog.Items {
				if catalog.Items[i].Type == truth.Type {
					got = &catalog.Items[i]
					break
				}
			}
			if got == nil {
				t.Fatalf("目录缺少类型 %s", truth.Type)
			}
			// CAS 断言在 metrics 集合上：数量、key、unit、来源特征 UUID 全一致。
			wantMetrics := make([]model.CatalogMetric, 0, len(truth.Metrics))
			for _, m := range truth.Metrics {
				wantMetrics = append(wantMetrics, model.CatalogMetric{Key: m.Key, Unit: m.Unit, CharacteristicUUID: m.CharacteristicUUID})
			}
			if len(got.Metrics) != len(wantMetrics) {
				t.Fatalf("%s metrics=%d，解析能力声明=%d", truth.Type, len(got.Metrics), len(wantMetrics))
			}
			sort.Slice(wantMetrics, func(i, j int) bool { return wantMetrics[i].Key < wantMetrics[j].Key })
			sort.Slice(got.Metrics, func(i, j int) bool { return got.Metrics[i].Key < got.Metrics[j].Key })
			if !reflect.DeepEqual(got.Metrics, wantMetrics) {
				t.Fatalf("%s metrics=%+v，解析能力声明=%+v", truth.Type, got.Metrics, wantMetrics)
			}
			wantServices := make([]model.GATTService, 0, len(truth.GATTServices))
			for _, s := range truth.GATTServices {
				wantServices = append(wantServices, model.GATTService{UUID: s.UUID, Name: s.Name})
			}
			if !reflect.DeepEqual(got.GATTServices, wantServices) {
				t.Fatalf("%s gattServices=%+v，解析能力声明=%+v", truth.Type, got.GATTServices, wantServices)
			}
			if got.DisplayName != truth.DisplayName || got.Description != truth.Description {
				t.Fatalf("%s 展示字段与真值表不一致", truth.Type)
			}
			if !reflect.DeepEqual(got.Limitations, truth.Limitations) {
				t.Fatalf("%s limitations=%+v，真值表=%+v", truth.Type, got.Limitations, truth.Limitations)
			}
			var compat devicecatalog.Compatibility
			if err := json.Unmarshal(got.Compatibility, &compat); err != nil {
				t.Fatalf("%s compatibility 非法: %v", truth.Type, err)
			}
			if !reflect.DeepEqual(compat, truth.Compatibility) {
				t.Fatalf("%s compatibility=%+v，真值表=%+v", truth.Type, compat, truth.Compatibility)
			}
			if got.UpdatedAt.IsZero() {
				t.Fatalf("%s updatedAt 为空", truth.Type)
			}
		}
	})

	t.Run("更新后版本变化并写审计", func(t *testing.T) {
		before, err := st.GetDeviceTypeCatalog(ctx)
		if err != nil {
			t.Fatalf("读取目录失败: %v", err)
		}
		truth, _ := devicecatalog.Find("environmental_sensor")
		input := model.CatalogDeviceTypeInput{
			DisplayName:  truth.DisplayName + "（集成更新）",
			Description:  truth.Description,
			GATTServices: toGATTServices(truth),
			Metrics:      toCatalogMetrics(truth)[:1],
			Limitations:  truth.Limitations,
		}
		input.Compatibility, _ = json.Marshal(truth.Compatibility)

		item, version, err := st.UpdateCatalogDeviceType(ctx, "environmental_sensor", input)
		if err != nil {
			t.Fatalf("更新目录失败: %v", err)
		}
		if version != before.Version+1 {
			t.Fatalf("版本=%d，期望 %d", version, before.Version+1)
		}
		if item.DisplayName != input.DisplayName || item.UpdatedAt.IsZero() {
			t.Fatalf("更新结果=%+v", item)
		}
		after, err := st.GetDeviceTypeCatalog(ctx)
		if err != nil {
			t.Fatalf("再次读取目录失败: %v", err)
		}
		if after.Version != version {
			t.Fatalf("读取版本=%d，写入版本=%d", after.Version, version)
		}
		var found bool
		for _, got := range after.Items {
			if got.Type == "environmental_sensor" {
				found = true
				if got.DisplayName != input.DisplayName {
					t.Fatalf("目录未反映更新: %q", got.DisplayName)
				}
				if len(got.Metrics) != 1 || got.Metrics[0].Key != "temperature_celsius" {
					t.Fatalf("指标未按请求整项替换: %+v", got.Metrics)
				}
			}
		}
		if !found {
			t.Fatal("更新后目录缺少 environmental_sensor")
		}

		// 审计：同一版本内 upsert 一条，actor=admin。
		var action, actor string
		if err := db.QueryRowContext(ctx,
			`SELECT action, actor FROM device_type_catalog_audits WHERE device_type = $1 AND catalog_version = $2`,
			"environmental_sensor", version,
		).Scan(&action, &actor); err != nil {
			t.Fatalf("读取审计失败: %v", err)
		}
		if action != "upsert" || actor != "admin" {
			t.Fatalf("审计 action=%q actor=%q", action, actor)
		}

		// 非法类型更新必须 404 语义（ErrNotFound），且不推进版本。
		beforeMissing, _ := st.GetDeviceTypeCatalog(ctx)
		_, _, err = st.UpdateCatalogDeviceType(ctx, "not_a_type", input)
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("非法类型更新 err=%v", err)
		}
		afterMissing, _ := st.GetDeviceTypeCatalog(ctx)
		if afterMissing.Version != beforeMissing.Version {
			t.Fatalf("非法更新推进了版本: %d -> %d", beforeMissing.Version, afterMissing.Version)
		}
	})

	t.Run("软删除与审计", func(t *testing.T) {
		before, err := st.GetDeviceTypeCatalog(ctx)
		if err != nil {
			t.Fatalf("读取目录失败: %v", err)
		}
		version, err := st.DeleteCatalogDeviceType(ctx, "environmental_sensor")
		if err != nil {
			t.Fatalf("软删除失败: %v", err)
		}
		if version != before.Version+1 {
			t.Fatalf("删除版本=%d，期望 %d", version, before.Version+1)
		}
		after, err := st.GetDeviceTypeCatalog(ctx)
		if err != nil {
			t.Fatalf("删除后读取目录失败: %v", err)
		}
		for _, item := range after.Items {
			if item.Type == "environmental_sensor" {
				t.Fatal("软删项仍出现在目录中")
			}
		}
		// 数据仍在库中（软删），deleted_at 已写入。
		var deletedAt sql.NullTime
		if err := db.QueryRowContext(ctx,
			`SELECT deleted_at FROM device_type_catalog WHERE type = 'environmental_sensor'`,
		).Scan(&deletedAt); err != nil {
			t.Fatalf("查询软删行失败: %v", err)
		}
		if !deletedAt.Valid {
			t.Fatal("deleted_at 未写入")
		}
		var action string
		if err := db.QueryRowContext(ctx,
			`SELECT action FROM device_type_catalog_audits WHERE device_type = 'environmental_sensor' AND catalog_version = $1`,
			version,
		).Scan(&action); err != nil {
			t.Fatalf("读取删除审计失败: %v", err)
		}
		if action != "delete" {
			t.Fatalf("删除审计 action=%q", action)
		}
		// 重复删除与更新已删除项均为 404 语义。
		if _, err := st.DeleteCatalogDeviceType(ctx, "environmental_sensor"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("重复删除 err=%v", err)
		}
		if _, _, err := st.UpdateCatalogDeviceType(ctx, "environmental_sensor", model.CatalogDeviceTypeInput{
			DisplayName: "恢复", GATTServices: []model.GATTService{}, Metrics: []model.CatalogMetric{},
			Limitations: []string{}, Compatibility: json.RawMessage(`{}`),
		}); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("更新已删除项 err=%v", err)
		}
	})

	t.Run("按用户记录访问", func(t *testing.T) {
		userID, err := st.CreateUserWithToken(ctx, hashOf("catalog-access-"+uuid.NewString()), time.Now().UTC().Add(180*24*time.Hour))
		if err != nil {
			t.Fatalf("创建用户失败: %v", err)
		}
		t.Cleanup(func() { db.Exec("DELETE FROM users WHERE id = $1", userID) })

		catalog, err := st.GetDeviceTypeCatalog(ctx)
		if err != nil {
			t.Fatalf("读取目录失败: %v", err)
		}
		for i := 0; i < 3; i++ {
			if err := st.RecordCatalogAccess(ctx, userID, catalog.Version); err != nil {
				t.Fatalf("记录访问失败: %v", err)
			}
		}
		var count int
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM device_type_catalog_accesses WHERE user_id = $1 AND catalog_version = $2`,
			userID, catalog.Version,
		).Scan(&count); err != nil {
			t.Fatalf("统计访问记录失败: %v", err)
		}
		if count != 1 {
			t.Fatalf("同一用户同一版本访问记录=%d，期望 1（去重）", count)
		}
	})

	t.Run("HTTP ETag 304 与变更感知", func(t *testing.T) {
		resetDeviceCatalog(t, db)
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		server := httpapi.New(config.Config{Version: "test", AdminToken: "ops-secret"}, st, logger, nil)

		get := func(headers map[string]string) *httptest.ResponseRecorder {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/catalog/device-types", nil)
			for k, v := range headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			server.ServeHTTP(rec, req)
			return rec
		}
		first := get(nil)
		if first.Code != http.StatusOK {
			t.Fatalf("匿名读取 status=%d body=%s", first.Code, first.Body.String())
		}
		etag := first.Header().Get("ETag")
		if etag == "" {
			t.Fatal("缺少 ETag")
		}
		if got := get(map[string]string{"If-None-Match": etag}); got.Code != http.StatusNotModified {
			t.Fatalf("ETag 命中 status=%d", got.Code)
		}

		truth, _ := devicecatalog.Find("environmental_sensor")
		input := model.CatalogDeviceTypeInput{
			DisplayName:  truth.DisplayName + "（HTTP 更新）",
			Description:  truth.Description,
			GATTServices: toGATTServices(truth),
			Metrics:      toCatalogMetrics(truth),
			Limitations:  truth.Limitations,
		}
		input.Compatibility, _ = json.Marshal(truth.Compatibility)
		raw, _ := json.Marshal(input)
		req := httptest.NewRequest(http.MethodPut, "/api/v1/admin/catalog/device-types/environmental_sensor", bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Admin-Token", "ops-secret")
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("管理端更新 status=%d body=%s", rec.Code, rec.Body.String())
		}
		newETag := rec.Header().Get("ETag")
		if newETag == "" || newETag == etag {
			t.Fatalf("更新后 ETag 未变化: old=%q new=%q", etag, newETag)
		}
		// 旧 ETag 不能再命中 304，客户端会拿到新目录。
		if got := get(map[string]string{"If-None-Match": etag}); got.Code != http.StatusOK {
			t.Fatalf("旧 ETag 仍命中 304，status=%d", got.Code)
		}
		if got := get(map[string]string{"If-None-Match": newETag}); got.Code != http.StatusNotModified {
			t.Fatalf("新 ETag 未命中 304，status=%d", got.Code)
		}
	})
}

func toGATTServices(t devicecatalog.Type) []model.GATTService {
	out := make([]model.GATTService, 0, len(t.GATTServices))
	for _, s := range t.GATTServices {
		out = append(out, model.GATTService{UUID: s.UUID, Name: s.Name})
	}
	return out
}

func toCatalogMetrics(t devicecatalog.Type) []model.CatalogMetric {
	out := make([]model.CatalogMetric, 0, len(t.Metrics))
	for _, m := range t.Metrics {
		out = append(out, model.CatalogMetric{Key: m.Key, Unit: m.Unit, CharacteristicUUID: m.CharacteristicUUID})
	}
	return out
}

// resetDeviceCatalog 把目录恢复为真值表的初始状态，避免上一次测试的更新/软删影响后续运行。
func resetDeviceCatalog(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`DELETE FROM device_type_catalog`); err != nil {
		t.Fatalf("清理目录失败: %v", err)
	}
	for _, truth := range devicecatalog.Types() {
		gatt, _ := json.Marshal(toGATTServices(truth))
		metrics, _ := json.Marshal(toCatalogMetrics(truth))
		limitations, _ := json.Marshal(truth.Limitations)
		compat, _ := json.Marshal(truth.Compatibility)
		if _, err := db.Exec(`
INSERT INTO device_type_catalog
    (type, display_name, description, gatt_services, metrics, limitations, compatibility, updated_at, deleted_at)
VALUES ($1, $2, $3, $4::jsonb, $5::jsonb, $6::jsonb, $7::jsonb, now(), NULL)`,
			truth.Type, truth.DisplayName, truth.Description, string(gatt), string(metrics),
			string(limitations), string(compat),
		); err != nil {
			t.Fatalf("恢复目录项 %s 失败: %v", truth.Type, err)
		}
	}
}
