-- 000003：设备唯一键从 MAC 迁移为 (platform, external_id)。
--
-- 背景：iOS 无法获取设备 MAC，只能使用 CBPeripheral.identifier（每台手机各不相同）。
-- 若继续以 mac 为唯一键，同一台设备在 Android 与 iOS 上会变成两条无法对齐的记录。
--
-- 兼容策略：
--   - 新增 platform（android | ios）与 external_id；
--   - 旧记录全部视为 android，external_id 回填为规范化大写 MAC；
--   - mac 保留为可选元数据（iOS 可以为空），不再作为唯一键；
--   - change_log 中旧的 user_device 记录同步回填 platform/externalId 与新的 entity_id。

ALTER TABLE user_devices ADD COLUMN platform text;
ALTER TABLE user_devices ADD COLUMN external_id text;

UPDATE user_devices
SET platform = 'android',
    external_id = upper(mac);

ALTER TABLE user_devices ALTER COLUMN platform SET NOT NULL;
ALTER TABLE user_devices ALTER COLUMN external_id SET NOT NULL;
ALTER TABLE user_devices ALTER COLUMN mac DROP NOT NULL;

ALTER TABLE user_devices DROP CONSTRAINT user_devices_user_id_mac_key;
ALTER TABLE user_devices
    ADD CONSTRAINT user_devices_user_platform_external_key
    UNIQUE (user_id, platform, external_id);

-- mac 仅作为可选元数据，保留索引用于 /my/devices/{mac} 查询。
CREATE INDEX user_devices_user_mac_idx ON user_devices (user_id, mac);

-- 旧变更日志：补 platform / externalId，并把 entity_id 改为复合键派生形式。
UPDATE change_log
SET payload = payload
        || jsonb_build_object('platform', 'android', 'externalId', upper(payload ->> 'mac')),
    entity_id = user_id::text || ':android:' || upper(payload ->> 'mac')
WHERE entity = 'user_device' AND payload ? 'mac';
