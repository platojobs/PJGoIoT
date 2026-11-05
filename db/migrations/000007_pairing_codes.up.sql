-- 000007：新增一次性配对码表（v0.6.0），用于跨设备接管同一账号。
--
-- 设计要点：
--   - 只保存配对码的 SHA-256 哈希（code_hash），日志与响应之外不落明文；
--   - user_id 为 NULL 的行表示「错误尝试记录」：任何人提交不存在的码都会被计数，
--     同一码累计错 5 次即删除作废，避免暴力猜测；
--   - 同一用户生成新码时先删除其旧码（新码使旧码失效）；
--   - 码 TTL 10 分钟，启动时与 cmd/cleanup 按 expires_at 清理。

CREATE TABLE pairing_codes (
    code_hash  text PRIMARY KEY,
    user_id    uuid REFERENCES users (id) ON DELETE CASCADE,
    attempts   smallint NOT NULL DEFAULT 0,
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX pairing_codes_user_id_idx ON pairing_codes (user_id);
CREATE INDEX pairing_codes_expires_at_idx ON pairing_codes (expires_at);

COMMENT ON TABLE pairing_codes IS '一次性配对码；同一用户新码使旧码失效，码错 5 次作废';
COMMENT ON COLUMN pairing_codes.code_hash IS '配对码 SHA-256 十六进制哈希，不保存明文';
COMMENT ON COLUMN pairing_codes.user_id IS '码所属用户；NULL 表示错误尝试计数行';
COMMENT ON COLUMN pairing_codes.attempts IS '失败尝试次数，达到 5 次删除该行';
COMMENT ON COLUMN pairing_codes.expires_at IS '过期时间，默认签发后 10 分钟';
