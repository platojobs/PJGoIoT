-- 000006：新增设备测量读数表 readings（v0.4.0）。
--
-- 设计要点：
--   - 主键为客户端生成的 id（uuid），服务端按 id 幂等写入；
--   - 读数引用设备唯一键 (platform, external_id)，但**不要求 user_devices 中存在对应设备**：
--     未注册设备按「隐式设备」接受写入（查询仍以 /my/devices 的存在性判定 404）；
--   - device_type 记录写入时的型号（可空），便于后续按型号统计；
--   - 保留 90 天，超期数据由 cmd/cleanup 或运维定时任务删除。
--
-- value 使用 double precision；CHECK 约束拒绝 NaN 与正负无穷，避免污染聚合结果。

CREATE TABLE readings (
    id          uuid PRIMARY KEY,
    user_id     uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    platform    text NOT NULL,
    external_id text NOT NULL,
    metric      text NOT NULL,
    value       double precision NOT NULL,
    unit        text NOT NULL DEFAULT '',
    recorded_at timestamptz NOT NULL,
    device_type text,
    created_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT readings_value_finite CHECK (
        value <> 'NaN'::double precision
        AND value <> 'Infinity'::double precision
        AND value <> '-Infinity'::double precision
    )
);

COMMENT ON TABLE readings IS '设备测量读数；按 id 幂等写入，服务端保留 90 天';
COMMENT ON COLUMN readings.metric IS '指标名，小写 snake_case，1-32 字符';
COMMENT ON COLUMN readings.unit IS '单位，最长 16 字符，允许空串（无量纲）';
COMMENT ON COLUMN readings.device_type IS '写入时解析到的设备型号；无法关联时为 NULL';
COMMENT ON COLUMN readings.recorded_at IS '客户端测量时间（RFC3339）；超前服务端 5 分钟以上会被拒绝';

CREATE INDEX readings_user_device_metric_time_idx
    ON readings (user_id, platform, external_id, metric, recorded_at DESC);
