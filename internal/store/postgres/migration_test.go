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

// TestMigrationBackfillsLegacyDevices 验证 000003、000004 与 000005 迁移：
// 旧 (user_id, mac) 记录在迁移后变为 platform=android、external_id=大写 MAC，
// 仍可通过 MAC 读取；旧 change_log 也会补 platform/externalId 与新 entity_id；
// 000004 按 mac 回填 user_devices.device_type，并为固件/配件补厂商信息；
// 000005 新增 advertised_name，旧记录的别名保持不变，广播名默认空串。
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
	// 旧 token 没有 expires_at，000008 需要按 created_at + 180 天回填。
	legacyCreatedAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := db.Exec(`INSERT INTO tokens (user_id, token_hash, created_at) VALUES ($1, 'legacy-token', $2)`,
		userID, legacyCreatedAt); err != nil {
		t.Fatalf("写入旧 token 失败: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO user_devices (user_id, id, client_id, mac, alias, "group", rules, scan_records, updated_at, server_seq)
		VALUES ($1, $2, 'legacy-client', $3, '旧设备', '默认', '{}'::jsonb, '[]'::jsonb, $4, 7)`,
		userID, deviceID, legacyMAC, time.Now().UTC()); err != nil {
		t.Fatalf("写入旧设备失败: %v", err)
	}
	// 000001 时代客户端可能写入大写 MAC；000004 的精确比较会漏掉它，000009 才补齐型号。
	upperDeviceID := uuid.New()
	if _, err := db.Exec(`
		INSERT INTO user_devices (user_id, id, client_id, mac, alias, "group", rules, scan_records, updated_at, server_seq)
		VALUES ($1, $2, 'legacy-client-2', 'AA:BB:CC:00:00:02', '大写 MAC 设备', '', '{}'::jsonb, '[]'::jsonb, $3, 8)`,
		userID, upperDeviceID, time.Now().UTC()); err != nil {
		t.Fatalf("写入大写 MAC 旧设备失败: %v", err)
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

	// 000009：大写 MAC 的历史记录也要按 lower(mac) 补齐型号。
	var upperType sql.NullString
	if err := db.QueryRow(`SELECT device_type FROM user_devices WHERE id = $1`, upperDeviceID).
		Scan(&upperType); err != nil {
		t.Fatalf("读取大写 MAC 设备型号失败: %v", err)
	}
	if !upperType.Valid || upperType.String != "environmental_sensor" {
		t.Fatalf("000009 未补齐大写 MAC 型号: valid=%v value=%q", upperType.Valid, upperType.String)
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

	// 000005：新增 advertised_name，旧记录的 alias 视为用户别名，广播名默认空串。
	var advertisedName string
	if err := db.QueryRow(`SELECT advertised_name FROM user_devices WHERE user_id = $1`, userID).
		Scan(&advertisedName); err != nil {
		t.Fatalf("读取 advertised_name 失败: %v", err)
	}
	if advertisedName != "" {
		t.Fatalf("旧记录 advertised_name 应为空串: %q", advertisedName)
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

	// 000006：readings 表存在；down 可回滚且不影响设备表，随后可再次 up。
	var hasReadings bool
	if err := db.QueryRow(`SELECT EXISTS (
		SELECT 1 FROM information_schema.tables WHERE table_name = 'readings'
	)`).Scan(&hasReadings); err != nil {
		t.Fatalf("查询 readings 表失败: %v", err)
	}
	if !hasReadings {
		t.Fatalf("迁移到最新版本后缺少 readings 表")
	}
	if err := m.Migrate(5); err != nil {
		t.Fatalf("回滚 000006 失败: %v", err)
	}
	if err := db.QueryRow(`SELECT EXISTS (
		SELECT 1 FROM information_schema.tables WHERE table_name = 'readings'
	)`).Scan(&hasReadings); err != nil {
		t.Fatalf("回滚后查询 readings 表失败: %v", err)
	}
	if hasReadings {
		t.Fatalf("回滚 000006 后 readings 表仍存在")
	}
	var deviceCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM user_devices WHERE user_id = $1`, userID).Scan(&deviceCount); err != nil {
		t.Fatalf("回滚后查询设备失败: %v", err)
	}
	if deviceCount != 2 {
		t.Fatalf("回滚 000006 破坏了设备数据: count=%d", deviceCount)
	}
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("回滚后重新迁移失败: %v", err)
	}

	// 000007：pairing_codes 表存在，down 可回滚且不影响用户数据，随后可再次 up。
	var hasPairing bool
	if err := db.QueryRow(`SELECT EXISTS (
		SELECT 1 FROM information_schema.tables WHERE table_name = 'pairing_codes'
	)`).Scan(&hasPairing); err != nil {
		t.Fatalf("查询 pairing_codes 表失败: %v", err)
	}
	if !hasPairing {
		t.Fatalf("迁移到最新版本后缺少 pairing_codes 表")
	}
	if _, err := db.Exec(`INSERT INTO pairing_codes (code_hash, user_id, expires_at)
		VALUES ('legacy-hash', $1, now() + interval '10 minutes')`, userID); err != nil {
		t.Fatalf("写入配对码失败: %v", err)
	}
	if err := m.Migrate(6); err != nil {
		t.Fatalf("回滚 000007 失败: %v", err)
	}
	if err := db.QueryRow(`SELECT EXISTS (
		SELECT 1 FROM information_schema.tables WHERE table_name = 'pairing_codes'
	)`).Scan(&hasPairing); err != nil {
		t.Fatalf("回滚后查询 pairing_codes 表失败: %v", err)
	}
	if hasPairing {
		t.Fatalf("回滚 000007 后 pairing_codes 表仍存在")
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM users WHERE id = $1`, userID).Scan(&deviceCount); err != nil {
		t.Fatalf("回滚 000007 后查询用户失败: %v", err)
	}
	if deviceCount != 1 {
		t.Fatalf("回滚 000007 破坏了用户数据: count=%d", deviceCount)
	}
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("回滚 000007 后重新迁移失败: %v", err)
	}

	// 000008：旧 token 回填 expires_at = created_at + 180 天，索引存在；down 删除列不影响用户数据。
	var expiresAt time.Time
	if err := db.QueryRow(`SELECT expires_at FROM tokens WHERE token_hash = 'legacy-token'`).
		Scan(&expiresAt); err != nil {
		t.Fatalf("读取 token 过期时间失败: %v", err)
	}
	if want := legacyCreatedAt.Add(180 * 24 * time.Hour); !expiresAt.Equal(want) {
		t.Fatalf("token expires_at 回填异常: got=%s want=%s", expiresAt, want)
	}
	for _, idx := range []string{"tokens_expires_at_idx", "change_log_server_ts_idx", "idempotency_keys_created_at_idx"} {
		var exists bool
		if err := db.QueryRow(`SELECT EXISTS (
			SELECT 1 FROM pg_indexes WHERE schemaname = 'public' AND indexname = $1
		)`, idx).Scan(&exists); err != nil {
			t.Fatalf("查询索引 %s 失败: %v", idx, err)
		}
		if !exists {
			t.Fatalf("000008 迁移后缺少索引 %s", idx)
		}
	}
	if err := m.Migrate(7); err != nil {
		t.Fatalf("回滚 000008 失败: %v", err)
	}
	var hasExpiresAt bool
	if err := db.QueryRow(`SELECT EXISTS (
		SELECT 1 FROM information_schema.columns
		WHERE table_name = 'tokens' AND column_name = 'expires_at'
	)`).Scan(&hasExpiresAt); err != nil {
		t.Fatalf("回滚后查询 tokens.expires_at 失败: %v", err)
	}
	if hasExpiresAt {
		t.Fatalf("回滚 000008 后 tokens.expires_at 仍存在")
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM users WHERE id = $1`, userID).Scan(&deviceCount); err != nil {
		t.Fatalf("回滚 000008 后查询用户失败: %v", err)
	}
	if deviceCount != 1 {
		t.Fatalf("回滚 000008 破坏了用户数据: count=%d", deviceCount)
	}
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("回滚 000008 后重新迁移失败: %v", err)
	}

	// 000013：feedback 表与索引存在；down 删除表后数据不受影响，随后可再次 up。
	var hasFeedback bool
	feedbackTableExists := func() bool {
		if err := db.QueryRow(`SELECT EXISTS (
			SELECT 1 FROM information_schema.tables WHERE table_name = 'feedback'
		)`).Scan(&hasFeedback); err != nil {
			t.Fatalf("查询 feedback 表失败: %v", err)
		}
		return hasFeedback
	}
	if !feedbackTableExists() {
		t.Fatalf("迁移到最新版本后缺少 feedback 表")
	}
	for _, idx := range []string{"feedback_user_created_idx", "feedback_user_idem_idx"} {
		var exists bool
		if err := db.QueryRow(`SELECT EXISTS (
			SELECT 1 FROM pg_indexes WHERE schemaname = 'public' AND indexname = $1
		)`, idx).Scan(&exists); err != nil {
			t.Fatalf("查询索引 %s 失败: %v", idx, err)
		}
		if !exists {
			t.Fatalf("000013 迁移后缺少索引 %s", idx)
		}
	}
	if err := m.Migrate(12); err != nil {
		t.Fatalf("回滚 000013 失败: %v", err)
	}
	if feedbackTableExists() {
		t.Fatalf("回滚 000013 后 feedback 表仍存在")
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM users WHERE id = $1`, userID).Scan(&deviceCount); err != nil {
		t.Fatalf("回滚 000013 后查询用户失败: %v", err)
	}
	if deviceCount != 1 {
		t.Fatalf("回滚 000013 破坏了用户数据: count=%d", deviceCount)
	}
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("回滚 000013 后重新迁移失败: %v", err)
	}
	if !feedbackTableExists() {
		t.Fatalf("重新迁移后缺少 feedback 表")
	}

	// 000014：补 internal_note、审计表与 (status, created_at) 索引；down 需精确回滚。
	tableExists := func(name string) bool {
		var exists bool
		if err := db.QueryRow(`SELECT EXISTS (
			SELECT 1 FROM information_schema.tables WHERE table_name = $1
		)`, name).Scan(&exists); err != nil {
			t.Fatalf("查询表 %s 失败: %v", name, err)
		}
		return exists
	}
	columnExists := func(table, column string) bool {
		var exists bool
		if err := db.QueryRow(`SELECT EXISTS (
			SELECT 1 FROM information_schema.columns WHERE table_name = $1 AND column_name = $2
		)`, table, column).Scan(&exists); err != nil {
			t.Fatalf("查询列 %s.%s 失败: %v", table, column, err)
		}
		return exists
	}
	indexExists := func(name string) bool {
		var exists bool
		if err := db.QueryRow(`SELECT EXISTS (
			SELECT 1 FROM pg_indexes WHERE schemaname = 'public' AND indexname = $1
		)`, name).Scan(&exists); err != nil {
			t.Fatalf("查询索引 %s 失败: %v", name, err)
		}
		return exists
	}
	if !tableExists("feedback_status_events") {
		t.Fatalf("000014 迁移后缺少 feedback_status_events 表")
	}
	if !columnExists("feedback", "internal_note") {
		t.Fatalf("000014 迁移后缺少 feedback.internal_note")
	}
	for _, idx := range []string{"feedback_status_created_idx", "feedback_status_events_feedback_idx"} {
		if !indexExists(idx) {
			t.Fatalf("000014 迁移后缺少索引 %s", idx)
		}
	}
	if err := m.Migrate(13); err != nil {
		t.Fatalf("回滚 000014 失败: %v", err)
	}
	if tableExists("feedback_status_events") {
		t.Fatalf("回滚 000014 后审计表仍存在")
	}
	if columnExists("feedback", "internal_note") {
		t.Fatalf("回滚 000014 后 internal_note 仍存在")
	}
	if indexExists("feedback_status_created_idx") {
		t.Fatalf("回滚 000014 后状态索引仍存在")
	}
	if !feedbackTableExists() {
		t.Fatalf("回滚 000014 不应删除 feedback 表")
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM users WHERE id = $1`, userID).Scan(&deviceCount); err != nil {
		t.Fatalf("回滚 000014 后查询用户失败: %v", err)
	}
	if deviceCount != 1 {
		t.Fatalf("回滚 000014 破坏了用户数据: count=%d", deviceCount)
	}
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("回滚 000014 后重新迁移失败: %v", err)
	}
	if !tableExists("feedback_status_events") || !columnExists("feedback", "internal_note") {
		t.Fatalf("重新迁移 000014 后结构未恢复")
	}

	// 000017：scenes 表与两个索引存在；down 删除表但不影响用户数据；随后可再次 up。
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("迁移到 000017 失败: %v", err)
	}
	if !tableExists("scenes") {
		t.Fatalf("000017 迁移后缺少 scenes 表")
	}
	for _, col := range []string{"trigger", "actions", "name_updated_at", "enabled_updated_at",
		"cooldown_updated_at", "trigger_updated_at", "actions_updated_at", "version", "deleted_at"} {
		if !columnExists("scenes", col) {
			t.Fatalf("000017 迁移后缺少 scenes.%s", col)
		}
	}
	for _, idx := range []string{"scenes_user_updated_idx", "scenes_user_deleted_idx"} {
		if !indexExists(idx) {
			t.Fatalf("000017 迁移后缺少索引 %s", idx)
		}
	}
	if err := m.Migrate(16); err != nil {
		t.Fatalf("回滚 000017 失败: %v", err)
	}
	if tableExists("scenes") {
		t.Fatalf("回滚 000017 后 scenes 表仍存在")
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM users WHERE id = $1`, userID).Scan(&deviceCount); err != nil {
		t.Fatalf("回滚 000017 后查询用户失败: %v", err)
	}
	if deviceCount != 1 {
		t.Fatalf("回滚 000017 破坏了用户数据: count=%d", deviceCount)
	}
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("回滚 000017 后重新迁移失败: %v", err)
	}
	if !tableExists("scenes") {
		t.Fatalf("重新迁移 000017 后 scenes 表未恢复")
	}
}
