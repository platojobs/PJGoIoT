-- 000004 回滚：移除新增列与索引。
-- 注意：device_type 的旧数据回填会随列删除一起丢弃，但原始 mac 仍保留，可重新推导。
DROP INDEX IF EXISTS user_devices_user_device_type_idx;

ALTER TABLE user_devices DROP COLUMN IF EXISTS device_type;

ALTER TABLE accessories DROP COLUMN IF EXISTS compatibility;
ALTER TABLE accessories DROP COLUMN IF EXISTS vendor;

ALTER TABLE firmwares DROP COLUMN IF EXISTS compatibility;
ALTER TABLE firmwares DROP COLUMN IF EXISTS vendor;

ALTER TABLE device_types DROP COLUMN IF EXISTS compatibility;
