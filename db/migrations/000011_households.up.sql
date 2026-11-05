-- 家庭（household）模型：一个家庭有 owner、成员、房间，设备可归属家庭与房间。
-- 每个用户迁移时自动获得一个 is_personal=true 的默认家庭「我的家」。

CREATE TABLE households (
    id uuid PRIMARY KEY,
    name text NOT NULL,
    owner_user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    is_personal boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX households_personal_owner_idx ON households (owner_user_id) WHERE is_personal;
CREATE INDEX households_owner_idx ON households (owner_user_id);

CREATE TABLE household_members (
    household_id uuid NOT NULL REFERENCES households(id) ON DELETE CASCADE,
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role text NOT NULL CHECK (role IN ('owner', 'member')),
    joined_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (household_id, user_id)
);
CREATE INDEX household_members_user_idx ON household_members (user_id);

CREATE TABLE household_rooms (
    id uuid PRIMARY KEY,
    household_id uuid NOT NULL REFERENCES households(id) ON DELETE CASCADE,
    name text NOT NULL,
    sort_order integer NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX household_rooms_household_idx ON household_rooms (household_id, sort_order, created_at);

-- household_id 为 NULL 的行只用于“猜码失败”的计数，与 pairing_codes 的 user_id NULL 同义。
CREATE TABLE household_invitations (
    code_hash text PRIMARY KEY,
    household_id uuid REFERENCES households(id) ON DELETE CASCADE,
    invited_by_user_id uuid REFERENCES users(id) ON DELETE CASCADE,
    attempts smallint NOT NULL DEFAULT 0,
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX household_invitations_household_idx ON household_invitations (household_id);
CREATE INDEX household_invitations_expires_idx ON household_invitations (expires_at);

ALTER TABLE user_devices
    ADD COLUMN household_id uuid REFERENCES households(id) ON DELETE SET NULL,
    ADD COLUMN room_id uuid REFERENCES household_rooms(id) ON DELETE SET NULL;
CREATE INDEX user_devices_household_idx ON user_devices (household_id, updated_at DESC) WHERE deleted_at IS NULL;

ALTER TABLE change_log
    ADD COLUMN household_id uuid REFERENCES households(id) ON DELETE SET NULL;
CREATE INDEX change_log_household_seq_idx ON change_log (household_id, seq);

-- 回填：每个既有用户一个默认家庭；设备与既有变更归入该家庭。
INSERT INTO households (id, name, owner_user_id, is_personal, created_at)
SELECT gen_random_uuid(), '我的家', u.id, true, u.created_at
FROM users u
ON CONFLICT DO NOTHING;

INSERT INTO household_members (household_id, user_id, role, joined_at)
SELECT h.id, h.owner_user_id, 'owner', h.created_at
FROM households h
WHERE h.is_personal
ON CONFLICT DO NOTHING;

UPDATE user_devices d
SET household_id = h.id
FROM households h
WHERE h.owner_user_id = d.user_id AND h.is_personal;

UPDATE change_log c
SET household_id = h.id
FROM households h
WHERE h.owner_user_id = c.user_id AND h.is_personal;
