-- 000018：服务端推送通道与告警投递队列（契约 0.18.0）。
--
-- 设计取舍：
--   * push_devices 一条记录对应「用户 + 平台 + 设备令牌」。同一用户重复注册同一 token
--     视为幂等更新（更新 app_version / device_model / 状态），不会生成多条记录；
--     唯一约束 (user_id, token) 保证这一点。令牌失效时只把 status 置为 inactive，
--     保留 last_error 便于排查，同时保留历史投递记录的引用。
--   * alerts 是服务端产生的一条告警。当前服务端没有告警产生者，先由运营侧
--     POST /admin/alerts 打通链路；prometheus/alerts.yml 的规则仍由 Alertmanager 侧处理，
--     两者在文档中分别说明。
--   * push_deliveries 是投递队列：告警与投递在同一事务写入（每个 active 令牌一条）。
--     status 为 pending/sending/sent/failed；worker 用 FOR UPDATE SKIP LOCKED 抢占，
--     多实例不会重复消费同一条。next_attempt_at 承载指数退避；claimed_at 用于
--     进程被 SIGKILL 后回收长时间停在 sending 的投递。
--   * token/title/body/data 是投递时的快照：令牌后续失效或告警正文改变，不影响
--     已经入队的投递。dry_run 记录「无凭据时本应发送」的事实，不当成功静默丢弃。

CREATE TABLE push_devices (
    id uuid PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    platform text NOT NULL CHECK (platform IN ('android', 'ios')),
    token text NOT NULL CHECK (char_length(token) BETWEEN 1 AND 4096),
    app_version text NOT NULL DEFAULT '',
    device_model text NOT NULL DEFAULT '',
    status text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'inactive')),
    last_error text,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    UNIQUE (user_id, token)
);

-- 告警产生时只查 active 令牌；部分索引避免把已失效令牌带进扫描。
CREATE INDEX push_devices_active_idx ON push_devices (user_id) WHERE status = 'active';
-- 过期清理按状态与更新时间扫描。
CREATE INDEX push_devices_status_updated_idx ON push_devices (status, updated_at);

CREATE TABLE alerts (
    id uuid PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    kind text NOT NULL CHECK (char_length(kind) BETWEEN 1 AND 64),
    title text NOT NULL CHECK (char_length(title) BETWEEN 1 AND 200),
    body text NOT NULL CHECK (char_length(body) <= 2000),
    severity text NOT NULL DEFAULT 'info' CHECK (severity IN ('info', 'warning', 'critical')),
    payload jsonb NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(payload) = 'object'),
    created_at timestamptz NOT NULL
);

CREATE INDEX alerts_user_created_idx ON alerts (user_id, created_at DESC, id);

CREATE TABLE push_deliveries (
    id uuid PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    alert_id uuid NOT NULL REFERENCES alerts (id) ON DELETE CASCADE,
    push_device_id uuid NOT NULL REFERENCES push_devices (id) ON DELETE CASCADE,
    platform text NOT NULL,
    token text NOT NULL,
    title text NOT NULL,
    body text NOT NULL,
    data jsonb NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(data) = 'object'),
    status text NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'sending', 'sent', 'failed')),
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at timestamptz NOT NULL,
    claimed_at timestamptz,
    last_error text,
    terminal_reason text,
    provider text,
    dry_run boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL,
    sent_at timestamptz
);

-- worker 抢占：只扫 pending，按退避时间与 id 稳定排序。
CREATE INDEX push_deliveries_pending_idx ON push_deliveries (next_attempt_at, id) WHERE status = 'pending';
-- 崩溃恢复：只扫 sending。
CREATE INDEX push_deliveries_sending_idx ON push_deliveries (claimed_at) WHERE status = 'sending';
-- 运营排查与清理。
CREATE INDEX push_deliveries_user_created_idx ON push_deliveries (user_id, created_at DESC);
CREATE INDEX push_deliveries_status_created_idx ON push_deliveries (status, created_at);
