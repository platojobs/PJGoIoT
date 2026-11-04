-- 000001_init：初始化公共元数据、账号、用户设备与同步变更表。

CREATE TABLE users (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    kind text NOT NULL DEFAULT 'anonymous' CHECK (kind IN ('anonymous', 'account')),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE tokens (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    token_hash text NOT NULL UNIQUE,
    created_at timestamptz NOT NULL DEFAULT now(),
    revoked_at timestamptz
);

CREATE INDEX tokens_user_id_idx ON tokens (user_id);

CREATE TABLE device_types (
    type text PRIMARY KEY,
    name text NOT NULL,
    vendor text NOT NULL,
    description text NOT NULL DEFAULT ''
);

CREATE TABLE gatt_services (
    device_type text NOT NULL REFERENCES device_types (type) ON DELETE CASCADE,
    uuid text NOT NULL,
    name text NOT NULL,
    position integer NOT NULL DEFAULT 0,
    PRIMARY KEY (device_type, uuid)
);

CREATE TABLE public_devices (
    mac text PRIMARY KEY,
    vendor text NOT NULL,
    model text NOT NULL DEFAULT '',
    device_type text NOT NULL REFERENCES device_types (type),
    firmware_version text NOT NULL DEFAULT '',
    compatibility jsonb NOT NULL DEFAULT '{}'::jsonb,
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE firmwares (
    id text PRIMARY KEY,
    device_type text NOT NULL REFERENCES device_types (type) ON DELETE CASCADE,
    version text NOT NULL,
    release_notes text NOT NULL DEFAULT '',
    released_at text NOT NULL DEFAULT ''
);

CREATE INDEX firmwares_device_type_idx ON firmwares (device_type);

CREATE TABLE accessories (
    id text PRIMARY KEY,
    device_type text NOT NULL REFERENCES device_types (type) ON DELETE CASCADE,
    name text NOT NULL,
    sku text NOT NULL DEFAULT '',
    description text NOT NULL DEFAULT ''
);

CREATE INDEX accessories_device_type_idx ON accessories (device_type);

-- 用户私有设备。id 为客户端 UUID，(user_id, mac) 唯一，所有查询必须带 user_id。
CREATE TABLE user_devices (
    user_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    id uuid NOT NULL,
    client_id text NOT NULL DEFAULT '',
    mac text NOT NULL,
    alias text NOT NULL DEFAULT '',
    "group" text NOT NULL DEFAULT '',
    rules jsonb NOT NULL DEFAULT '{}'::jsonb,
    scan_records jsonb NOT NULL DEFAULT '[]'::jsonb,
    updated_at timestamptz NOT NULL,
    deleted_at timestamptz,
    server_seq bigint NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, id),
    UNIQUE (user_id, mac)
);

CREATE INDEX user_devices_user_updated_idx ON user_devices (user_id, updated_at DESC);

-- 增量同步日志，seq 单调递增；客户端按 user_id + seq 拉取。
CREATE TABLE change_log (
    seq bigserial PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    entity text NOT NULL,
    entity_id text NOT NULL,
    op text NOT NULL CHECK (op IN ('upsert', 'delete')),
    payload jsonb NOT NULL,
    server_ts timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX change_log_user_seq_idx ON change_log (user_id, seq);

-- 幂等键：同一用户同一 key 只保存一份响应。
CREATE TABLE idempotency_keys (
    user_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    key text NOT NULL,
    request_hash text NOT NULL,
    response jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, key)
);
