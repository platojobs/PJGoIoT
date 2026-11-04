-- 000001_init 回滚：按依赖顺序删除。
DROP TABLE IF EXISTS idempotency_keys;
DROP TABLE IF EXISTS change_log;
DROP TABLE IF EXISTS user_devices;
DROP TABLE IF EXISTS accessories;
DROP TABLE IF EXISTS firmwares;
DROP TABLE IF EXISTS public_devices;
DROP TABLE IF EXISTS gatt_services;
DROP TABLE IF EXISTS device_types;
DROP TABLE IF EXISTS tokens;
DROP TABLE IF EXISTS users;
