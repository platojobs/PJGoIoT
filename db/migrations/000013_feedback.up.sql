-- 意见反馈：用户主动提交的问题/建议，仅本人可见。
-- 现有表没有合适的落点（设备、家庭、读数都不承载自由文本反馈），因此新建独立表。

CREATE TABLE feedback (
    id uuid PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    kind text NOT NULL CHECK (kind IN ('bug', 'feature', 'experience', 'other')),
    -- 空白或超长的正文在应用层已被拒绝，这里作为最后防线，防止绕过 HTTP 层写入脏数据。
    body text NOT NULL CHECK (char_length(btrim(body)) > 0 AND char_length(body) <= 4000),
    -- contact 属于个人信息：只存库，不回显给他人，日志也不打印明文。
    contact text CHECK (char_length(contact) <= 200),
    platform text NOT NULL CHECK (platform IN ('android', 'ios')),
    app_version text NOT NULL,
    build_type text NOT NULL,
    os_version text NOT NULL,
    device_model text NOT NULL,
    -- 诊断文本体积大且仅用于排查，列表接口不返回。
    diagnostics text CHECK (char_length(diagnostics) <= 8000),
    status text NOT NULL DEFAULT 'submitted' CHECK (status IN ('submitted', 'received', 'closed')),
    -- 幂等键由 HTTP 层派生（客户端 header 或规范化请求体哈希），唯一约束保证同一请求只落一条。
    idem_key text NOT NULL,
    request_hash text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- 本人列表按创建时间倒序游标分页。
CREATE INDEX feedback_user_created_idx ON feedback (user_id, created_at DESC, id);
-- 同一用户的同一幂等键只允许一条记录。
CREATE UNIQUE INDEX feedback_user_idem_idx ON feedback (user_id, idem_key);
