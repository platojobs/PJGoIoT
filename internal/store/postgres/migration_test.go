package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	migratepgx "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/platojobs/PJGoIoT/db/migrations"
	"github.com/platojobs/PJGoIoT/internal/store/postgres"
)

// TestMigrationBackfillsLegacyDevices 验证 000003 与 000004 迁移：
// 旧 (user_id, mac) 记录在迁移后变为 platform=android、external_id=大写 MAC，
// 仍可通过 MAC 读取；旧 change_log 也会补 platform/externalId 与新 entity_id；
// 000004 按 mac 回填 user_devices.device_type，并为固件/配件补厂商信息。
//
// 使用临时数据库，避免破坏 TEST_DATABASE_URL 指向的测试库。
func TestMigrationBackfillsLegacyDevices(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("未设置 TEST_DATABASE_URL，跳过迁移集成测试")
	}
	u, err := url.Parse(dsn)
	if err != nil || u.Scheme == "" {
		t.Skipf("TEST_DATABASE_URL 不是 URL 形式，跳过迁移测试: %v", err)
	}

	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	defer admin.Close()

	dbName := "pjgoiot_mig_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:10]
	if _, err := admin.Exec(`CREATE DATABASE "` + dbName + `"`); err != nil {
		t.Skipf("当前数据库用户无法创建临时数据库，跳过迁移测试: %v", err)
	}
	t.Cleanup(func() {
		admin.Exec(`DROP DATABASE IF EXISTS "` + dbName + `" WITH (FORCE)`)
	})

	tempURL := *u
	tempURL.Path = "/" + dbName
	tempDSN := tempURL.String()

	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		t.Fatalf("加载迁移失败: %v", err)
	}
	db, err := sql.Open("pgx", tempDSN)
	if err != nil {
		t.Fatalf("打开临时库失败: %v", err)
	}
	defer db.Close()
	driver, err := migratepgx.WithInstance(db, &migratepgx.Config{})
	if err != nil {
		t.Fatalf("初始化迁移驱动失败: %v", err)
	}
	m, err := migrate.NewWithInstance("iofs", src, "pgx5", driver)
	if err != nil {
		t.Fatalf("初始化迁移失败: %v", err)
	}
	defer m.Close()

	// 先迁移到 000002（旧 schema），写入一条历史数据。
	if err := m.Migrate(2); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("迁移到 version=2 失败: %v", err)
	}

	userID := uuid.New()
	deviceID := uuid.New()
	legacyMAC := "aa:bb:cc:00:00:01"
	if _, err := db.Exec(`INSERT INTO users (id) VALUES ($1)`, userID); err != nil {
		t.Fatalf("写入旧用户失败: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO user_devices (user_id, id, client_id, mac, alias, "group", rules, scan_records, updated_at, server_seq)
		VALUES ($1, $2, 'legacy-client', $3, '旧设备', '默认', '{}'::jsonb, '[]'::jsonb, $4, 7)`,
		userID, deviceID, legacyMAC, time.Now().UTC()); err != nil {
		t.Fatalf("写入旧设备失败: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO change_log (seq, user_id, entity, entity_id, op, payload, server_ts)
		VALUES (nextval(pg_get_serial_sequence('change_log','seq')), $1, 'user_device', $2, 'upsert',
		        jsonb_build_object('id', $2::text, 'mac', $3::text), $4)`,
		userID, deviceID, legacyMAC, time.Now().UTC()); err != nil {
		t.Fatalf("写入旧变更失败: %v", err)
	}

	// 再迁移到 000003，触发回填。
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("迁移到最新版本失败: %v", err)
	}

	var platform, externalID, mac string
	if err := db.QueryRow(`SELECT platform, external_id, mac FROM user_devices WHERE user_id = $1`, userID).
		Scan(&platform, &externalID, &mac); err != nil {
		t.Fatalf("读取回填结果失败: %v", err)
	}
	if platform != "android" || externalID != strings.ToUpper(legacyMAC) || mac != legacyMAC {
		t.Fatalf("回填结果异常: platform=%q external_id=%q mac=%q", platform, externalID, mac)
	}

	var payloadPlatform, payloadExternal, entityID string
	if err := db.QueryRow(`
		SELECT payload->>'platform', payload->>'externalId', entity_id
		FROM change_log WHERE user_id = $1`, userID).
		Scan(&payloadPlatform, &payloadExternal, &entityID); err != nil {
		t.Fatalf("读取旧变更回填结果失败: %v", err)
	}
	wantEntity := userID.String() + ":android:" + strings.ToUpper(legacyMAC)
	if payloadPlatform != "android" || payloadExternal != strings.ToUpper(legacyMAC) || entityID != wantEntity {
		t.Fatalf("旧变更回填异常: platform=%q externalId=%q entityId=%q want=%q",
			payloadPlatform, payloadExternal, entityID, wantEntity)
	}

	// 000004：按 mac 关联公共库回填 device_type；未注册型号保持 NULL。
	var deviceType sql.NullString
	if err := db.QueryRow(`SELECT device_type FROM user_devices WHERE user_id = $1`, userID).
		Scan(&deviceType); err != nil {
		t.Fatalf("读取 device_type 回填失败: %v", err)
	}
	if !deviceType.Valid || deviceType.String != "nordic_uart" {
		t.Fatalf("device_type 回填异常: valid=%v value=%q", deviceType.Valid, deviceType.String)
	}

	// 000004：为可核实的固件/配件补齐厂商，未核实的保持空字符串。
	var fwVendor, accVendor string
	if err := db.QueryRow(`SELECT vendor FROM firmwares WHERE id = 'fw-nrf-ncs-2.9.0'`).Scan(&fwVendor); err != nil {
		t.Fatalf("读取固件厂商失败: %v", err)
	}
	if fwVendor != "Nordic Semiconductor" {
		t.Fatalf("固件厂商回填异常: %q", fwVendor)
	}
	if err := db.QueryRow(`SELECT vendor FROM accessories WHERE id = 'acc-scale-aa-battery'`).Scan(&accVendor); err != nil {
		t.Fatalf("读取配件厂商失败: %v", err)
	}
	if accVendor != "" {
		t.Fatalf("未核实配件厂商应为空: %q", accVendor)
	}

	// 旧数据通过 Store 仍可读。
	st, err := postgres.New(context.Background(), tempDSN)
	if err != nil {
		t.Fatalf("连接临时库 Store 失败: %v", err)
	}
	defer st.Close()

	got, err := st.GetUserDevice(context.Background(), userID.String(), legacyMAC)
	if err != nil {
		t.Fatalf("迁移后旧设备不可读: %v", err)
	}
	if got.Platform != "android" || got.ExternalID != strings.ToUpper(legacyMAC) || got.Alias != "旧设备" || got.ServerSeq != 7 {
		t.Fatalf("迁移后旧设备字段异常: %+v", got)
	}
	if got.DeviceType == nil || *got.DeviceType != "nordic_uart" {
		t.Fatalf("迁移后旧设备型号异常: %+v", got.DeviceType)
	}
}
