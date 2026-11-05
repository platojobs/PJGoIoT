DROP INDEX IF EXISTS idempotency_keys_created_at_idx;
DROP INDEX IF EXISTS change_log_server_ts_idx;
DROP INDEX IF EXISTS tokens_expires_at_idx;
ALTER TABLE tokens DROP COLUMN IF EXISTS expires_at;
