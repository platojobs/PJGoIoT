DROP INDEX IF EXISTS change_log_household_seq_idx;
ALTER TABLE change_log DROP COLUMN IF EXISTS household_id;

DROP INDEX IF EXISTS user_devices_household_idx;
ALTER TABLE user_devices
    DROP COLUMN IF EXISTS room_id,
    DROP COLUMN IF EXISTS household_id;

DROP TABLE IF EXISTS household_invitations;
DROP TABLE IF EXISTS household_rooms;
DROP TABLE IF EXISTS household_members;
DROP TABLE IF EXISTS households;
