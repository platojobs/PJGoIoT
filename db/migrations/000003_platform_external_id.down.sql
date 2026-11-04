-- 000003 回滚：恢复 (user_id, mac) 唯一键。
-- 注意：iOS 记录的 external_id 不是 MAC，回滚会把 mac 回填为 external_id；
-- 该操作有损，仅用于本地回滚，生产环境回滚前需自行备份并评估数据。
DROP INDEX user_devices_user_mac_idx;

ALTER TABLE user_devices
    DROP CONSTRAINT user_devices_user_platform_external_key;

UPDATE user_devices SET mac = lower(external_id) WHERE mac IS NULL;

ALTER TABLE user_devices ALTER COLUMN mac SET NOT NULL;
ALTER TABLE user_devices ADD CONSTRAINT user_devices_user_id_mac_key UNIQUE (user_id, mac);

ALTER TABLE user_devices DROP COLUMN external_id;
ALTER TABLE user_devices DROP COLUMN platform;
