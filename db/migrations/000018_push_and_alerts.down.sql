-- 与 up 对称删除；push_deliveries 先删，避免外键依赖。
DROP TABLE IF EXISTS push_deliveries;
DROP TABLE IF EXISTS alerts;
DROP TABLE IF EXISTS push_devices;
