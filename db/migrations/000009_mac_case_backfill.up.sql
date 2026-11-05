-- 000009：兼容历史数据中大小写不一致的 MAC，补齐 user_devices.device_type。
--
-- 000004 的回填使用 ud.mac = pd.mac 精确比较；000001 时代客户端可能写入大写 MAC，
-- 而 public_devices 的键为小写，导致这些记录永远拿不到型号（读路径 LEFT JOIN 同样落空）。
-- 这里按 lower(mac) 重新回填一次，幂等，仅影响仍为 NULL 的记录。

UPDATE user_devices AS ud
SET device_type = pd.device_type
FROM public_devices AS pd
WHERE ud.device_type IS NULL
  AND ud.mac IS NOT NULL
  AND lower(ud.mac) = pd.mac;
