DROP INDEX IF EXISTS feedback_status_created_idx;
DROP TABLE IF EXISTS feedback_status_events;
ALTER TABLE feedback DROP COLUMN IF EXISTS internal_note;
