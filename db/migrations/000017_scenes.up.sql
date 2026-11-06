-- 000017：把客户端本地场景（触发条件 + 动作）搬到服务端（契约 0.16.0）。
--
-- 取舍说明：
--   * 一个 scene 就是一个客户端可直接执行的规则包（触发条件 + 动作一起生效），
--     不拆成 scene_rules / scene_actions 子表：
--       - 子表会引入没有业务含义的子级 LWW 时间戳，字段级 LWW 反而更难表达；
--       - change_log 需要「完整可重建状态」，单行场景可一次 payload 重建，
--         子表要么拼多条变更，要么在 payload 里再嵌套一层；
--       - 客户端 Room/SQLite 本地也是「场景 → 规则/动作」整体读写，单行与客户端模型同构。
--     触发条件与动作分别存 jsonb，结构由 internal/scene 统一校验，写入前已是规范化形态。
--   * 字段级 LWW：name / enabled / cooldown_seconds / trigger / actions 各带一个独立
--     更新时间戳，与设备元数据同一套语义。并发 PUT 修改不同字段时互不覆盖；
--     修改同一字段时才按时间戳比较，服务端较新的值静默获胜（不回写冲突）。
--   * version 是场景整体版本号，仅在真正有字段被应用时自增，供客户端乐观展示与排查。
--   * 软删除：deleted_at 非空即 tombstone，GET/列表不再返回，但 change_log 保留 delete 记录。
--   * device_missing 不落库：它取决于设备是否仍存在，读取时实时计算，避免设备删除后
--     还要回写出入场景表（也避免删除设备的事务被迫级联更新场景）。

CREATE TABLE scenes (
    id uuid PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name text NOT NULL CHECK (char_length(name) BETWEEN 1 AND 60),
    enabled boolean NOT NULL DEFAULT true,
    cooldown_seconds integer NOT NULL DEFAULT 300
        CHECK (cooldown_seconds BETWEEN 30 AND 86400),
    -- 触发条件：{"kind":"device_offline|rssi_below|metric_threshold|schedule", ...}
    trigger jsonb NOT NULL CHECK (jsonb_typeof(trigger) = 'object'),
    -- 动作数组：[{"kind":"notify|log|mark_device", ...}]
    actions jsonb NOT NULL CHECK (jsonb_typeof(actions) = 'array'),
    -- 字段级 LWW 时间戳，分别对应上面五个可并发修改的字段。
    name_updated_at timestamptz NOT NULL,
    enabled_updated_at timestamptz NOT NULL,
    cooldown_updated_at timestamptz NOT NULL,
    trigger_updated_at timestamptz NOT NULL,
    actions_updated_at timestamptz NOT NULL,
    version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL,
    deleted_at timestamptz
);

-- 本人场景列表：按最近修改倒序；只索引未删除行，列表查询可直接命中。
CREATE INDEX scenes_user_updated_idx
    ON scenes (user_id, updated_at DESC, id)
    WHERE deleted_at IS NULL;

-- 导出与账号删除扫描使用；包含 tombstone，便于导出保留完整历史。
CREATE INDEX scenes_user_deleted_idx ON scenes (user_id, deleted_at);
