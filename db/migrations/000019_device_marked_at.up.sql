-- 设备标记时间：NULL 表示未标记。
-- 标记值本身是客户端操作时间，作为字段级 LWW 的版本；取消标记写入 NULL。
ALTER TABLE user_devices ADD COLUMN marked_at timestamptz;
