-- token 过期时间：默认 180 天，可由服务端 TOKEN_TTL_DAYS 写入。
ALTER TABLE tokens ADD COLUMN expires_at timestamptz;
UPDATE tokens SET expires_at = created_at + interval '180 days' WHERE expires_at IS NULL;
ALTER TABLE tokens ALTER COLUMN expires_at SET NOT NULL;
ALTER TABLE tokens ALTER COLUMN expires_at SET DEFAULT (now() + interval '180 days');
CREATE INDEX tokens_expires_at_idx ON tokens (expires_at);

-- 留存清理按时间列删除；没有索引时每次清理都会全表扫描。
CREATE INDEX IF NOT EXISTS change_log_server_ts_idx ON change_log (server_ts);
CREATE INDEX IF NOT EXISTS idempotency_keys_created_at_idx ON idempotency_keys (created_at);

COMMENT ON COLUMN tokens.expires_at IS 'token 过期时间；过期后中间件返回 401，cleanup 删除过期行';
COMMENT ON COLUMN change_log.server_ts IS '变更写入时间；cleanup 按此列删除超过保留期的行';
