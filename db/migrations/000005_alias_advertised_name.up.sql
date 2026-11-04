-- v0.3.3：区分用户别名与设备广播名。
-- alias 只由用户操作写入；advertised_name 由客户端扫描上报，二者互不覆盖。
-- 既有 group 列已是 NOT NULL DEFAULT ''，空串可直接表达「显式清除」，无需改表。
ALTER TABLE user_devices
    ADD COLUMN advertised_name text NOT NULL DEFAULT '';

COMMENT ON COLUMN user_devices.alias IS '用户设置的别名，只在用户操作时更新';
COMMENT ON COLUMN user_devices.advertised_name IS '设备广播名（BLE Local Name），客户端扫描时可覆盖，不影响 alias';
COMMENT ON COLUMN user_devices."group" IS '用户分组；空串表示未分组（显式清除）';
