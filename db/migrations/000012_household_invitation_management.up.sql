-- 邀请码管理：给邀请码增加稳定 id，供列表/撤销使用；created_at 已存在。
-- id 只用于管理接口，不参与领取（领取仍按 code_hash 主键）。

ALTER TABLE household_invitations
    ADD COLUMN id uuid NOT NULL DEFAULT gen_random_uuid();

CREATE UNIQUE INDEX household_invitations_id_idx ON household_invitations (id);
-- 管理列表只关心真实签到家庭的行；失败计数行 household_id 为 NULL，不展示。
CREATE INDEX household_invitations_active_idx
    ON household_invitations (household_id, created_at DESC)
    WHERE household_id IS NOT NULL;
