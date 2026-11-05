-- 回滚 000006：删除读数表（不影响 user_devices 与公共元数据）。
DROP TABLE IF EXISTS readings;
