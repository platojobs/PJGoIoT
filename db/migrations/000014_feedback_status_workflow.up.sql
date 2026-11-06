-- 反馈状态流转：000013 已包含 updated_at 与 status CHECK 约束，本迁移补运营侧所需的结构。
--
-- 取舍说明：
--   * 每个状态跳转写一条 feedback_status_events（谁、从什么状态到什么状态、备注、时间），
--     这是审计的权威来源；同状态重复 PATCH 视为幂等 no-op，不写审计。
--   * feedback.internal_note 保存「最新一条」内部备注，方便运营列表直接展示；
--     历史备注仍以审计表为准，不丢失。
--   * 单条审计记录用独立表而不是 ARRAY/JSONB 列，避免行膨胀与并发写热点，也便于按 feedback_id 查询。

ALTER TABLE feedback ADD COLUMN internal_note text CHECK (char_length(internal_note) <= 2000);

CREATE TABLE feedback_status_events (
    id uuid PRIMARY KEY,
    feedback_id uuid NOT NULL REFERENCES feedback(id) ON DELETE CASCADE,
    -- actor_type=user 时 actor_id 为 user_id；actor_type=admin 时为固定标识（当前只有共享 admin token）。
    actor_type text NOT NULL CHECK (actor_type IN ('user', 'admin')),
    actor_id text NOT NULL,
    from_status text NOT NULL,
    to_status text NOT NULL,
    note text CHECK (char_length(note) <= 2000),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX feedback_status_events_feedback_idx ON feedback_status_events (feedback_id, created_at, id);

-- 运营侧按状态 + 时间倒序分页。
CREATE INDEX feedback_status_created_idx ON feedback (status, created_at DESC, id);
