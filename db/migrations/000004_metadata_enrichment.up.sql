-- 000004：为用户设备与市场数据补齐公共元数据字段。
--
-- 目标：iOS/Android 详情页直接展示型号、厂商、固件版本与兼容性；
-- 市场模块可按厂商与兼容性筛选固件/配件。
--
-- 兼容策略：
--   - 只新增列，均有默认值；不修改既有列语义。
--   - user_devices.device_type 允许为空：客户端未上报型号时，读取时按 mac 关联 public_devices；
--     旧记录按 mac 回填一次，未命中公共库的记录保持 NULL（返回 null，不报错）。
--   - 固件/配件的 vendor 与 compatibility 只回填可核实的公开型号数据，其余保持空值。

ALTER TABLE device_types ADD COLUMN compatibility jsonb NOT NULL DEFAULT '{}'::jsonb;

ALTER TABLE firmwares ADD COLUMN vendor text NOT NULL DEFAULT '';
ALTER TABLE firmwares ADD COLUMN compatibility jsonb NOT NULL DEFAULT '{}'::jsonb;

ALTER TABLE accessories ADD COLUMN vendor text NOT NULL DEFAULT '';
ALTER TABLE accessories ADD COLUMN compatibility jsonb NOT NULL DEFAULT '{}'::jsonb;

ALTER TABLE user_devices ADD COLUMN device_type text;

-- 旧用户设备按 mac 关联公共库回填型号；未注册型号保持 NULL。
UPDATE user_devices AS ud
SET device_type = pd.device_type
FROM public_devices AS pd
WHERE ud.mac = pd.mac
  AND ud.device_type IS NULL;

-- 型号级兼容性：仅回填有公开 GATT 依据的标准型号。
UPDATE device_types
SET compatibility = '{"platforms":["android","ios"],"minAppVersion":"1.0.0","protocol":"ble-gatt","features":["uart-transparent"]}'::jsonb
WHERE type = 'nordic_uart';

UPDATE device_types
SET compatibility = '{"platforms":["android","ios"],"minAppVersion":"1.0.0","protocol":"ble-gatt","features":["temperature","humidity","pressure"]}'::jsonb
WHERE type = 'environmental_sensor';

-- 固件发布方：依据厂商官方 SDK 版本信息。
UPDATE firmwares SET vendor = 'Nordic Semiconductor' WHERE id = 'fw-nrf-ncs-2.9.0';
UPDATE firmwares SET vendor = 'Espressif Systems'      WHERE id = 'fw-esp-idf-5.2.2';

-- 固件兼容性：沿用同型号已核实的 public_devices 兼容性声明。
UPDATE firmwares AS f
SET compatibility = pd.compatibility
FROM public_devices AS pd
WHERE pd.device_type = f.device_type
  AND f.compatibility = '{}'::jsonb;

-- 配件：可核实的厂商与适配型号（SKU 仍留空，以厂商渠道为准，不编造）。
UPDATE accessories
SET vendor = 'Polar Electro',
    compatibility = '{"deviceTypes":["heart_rate_monitor"]}'::jsonb
WHERE id = 'acc-hrm-polar-pro-strap';

UPDATE accessories
SET vendor = 'Espressif Systems',
    compatibility = '{"deviceTypes":["environmental_sensor"]}'::jsonb
WHERE id = 'acc-esp-usb-c-cable';

UPDATE accessories
SET compatibility = '{"deviceTypes":["smart_scale"]}'::jsonb
WHERE id = 'acc-scale-aa-battery';

CREATE INDEX user_devices_user_device_type_idx ON user_devices (user_id, device_type);
