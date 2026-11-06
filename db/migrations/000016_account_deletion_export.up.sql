-- 000016：账号删除确认令牌与个人数据异步导出任务（契约 0.15.0）。
--
-- 取舍说明：
--   * account_deletion_tokens 刻意不对 users 建外键：账号行删除后仍需保留「已用」墓碑，
--     才能把「令牌不存在(403) / 令牌已过期(410) / 已用于删除(404)」区分开，
--     避免重放删除请求被当成未知错误。
--   * 令牌只存 SHA-256 哈希，明文仅在 POST /me/deletion-request 响应中出现一次。
--   * 同一用户同时只允许一个未使用的删除令牌（部分唯一索引），再次申请即作废旧令牌，
--     用唯一索引而不是应用层先删后插，保证并发申请也不会留下两个可用令牌。
--   * personal_data_export_tasks 承载异步导出：artifact 为 jsonb，导出采用读数聚合，
--     体量有界；download_token_hash 同样只存哈希，download_used_at 保证一次性下载。
--   * expires_at 是产物保留边界，cmd/cleanup 按该列清理，配套普通索引避免全表扫描。

CREATE TABLE account_deletion_tokens (
    token_hash text PRIMARY KEY,
    -- 不对 users 建外键：账号删除后墓碑仍要保留。
    user_id uuid NOT NULL,
    expires_at timestamptz NOT NULL,
    used_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- TTL 扫描：cmd/cleanup 删除 expires_at < now-cutoff 的行。
CREATE INDEX account_deletion_tokens_expires_idx ON account_deletion_tokens (expires_at);
-- 每个用户最多一个未使用令牌。
CREATE UNIQUE INDEX account_deletion_tokens_user_active_idx
    ON account_deletion_tokens (user_id) WHERE used_at IS NULL;

CREATE TABLE personal_data_export_tasks (
    id uuid PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'ready', 'failed')),
    -- 导出内容；pending/failed 时为 NULL。
    artifact jsonb,
    -- 失败的对外可读原因（不含内部堆栈）。
    failure_reason text CHECK (char_length(failure_reason) <= 500),
    -- 下载链接一次性令牌：只存哈希，明文只在查询状态时的响应里出现。
    download_token_hash text,
    download_expires_at timestamptz,
    download_used_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    completed_at timestamptz,
    -- 产物保留到期时间；到期后由 cmd/cleanup 整行删除（artifact 一并清理）。
    expires_at timestamptz NOT NULL
);

-- 用户查询自己的任务，按创建时间倒序。
CREATE INDEX personal_data_export_tasks_user_created_idx
    ON personal_data_export_tasks (user_id, created_at DESC, id DESC);
-- 产物 TTL 清理。
CREATE INDEX personal_data_export_tasks_expires_idx ON personal_data_export_tasks (expires_at);
-- 下载令牌按哈希定位；一次性，同一哈希最多一个任务。
CREATE UNIQUE INDEX personal_data_export_tasks_download_idx
    ON personal_data_export_tasks (download_token_hash) WHERE download_token_hash IS NOT NULL;
