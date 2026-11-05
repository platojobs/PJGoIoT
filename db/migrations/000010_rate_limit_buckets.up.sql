-- 跨实例共享的限流计数（令牌桶状态）。仅 RATE_LIMIT_BACKEND=postgres 时使用。
CREATE TABLE IF NOT EXISTS rate_limit_buckets (
    bucket     text PRIMARY KEY,
    tokens     double precision NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- 惰性清理按 updated_at 删除长期未访问的桶。
CREATE INDEX IF NOT EXISTS rate_limit_buckets_updated_at_idx
    ON rate_limit_buckets (updated_at);
