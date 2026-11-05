DROP INDEX IF EXISTS household_invitations_active_idx;
DROP INDEX IF EXISTS household_invitations_id_idx;

ALTER TABLE household_invitations DROP COLUMN IF EXISTS id;
