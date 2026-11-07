-- 设备在线状态（契约 0.21.0）。
-- last_seen_at：服务端最后一次观察到该设备读数上报的「接收时间」（UTC，微秒）。
--   NULL 表示该设备自注册以来从未上报过读数，对应 presence=unknown，
--   必须与「曾在线、现已超出窗口」的 presence=offline 可区分。
-- 该列只由服务端在 /sync/push 接受读数的同一事务内维护，客户端只读、不可写。
-- presence 本身不落库：每次响应按 now() - last_seen_at 与固定 15 分钟窗口实时推导。
ALTER TABLE user_devices ADD COLUMN last_seen_at timestamptz;

COMMENT ON COLUMN user_devices.last_seen_at IS '服务端最后观察到设备读数上报的接收时间（UTC，微秒）；NULL=从未上报。仅服务端维护，客户端只读。';
