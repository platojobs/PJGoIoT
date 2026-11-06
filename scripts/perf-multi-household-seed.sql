-- 多家庭 /sync/changes 性能基线数据集（v0.24.0）。
--
-- 形状：1 个目标用户 U 加入 16 个家庭（1 个个人家庭 + 15 个共享家庭），
-- 每个共享家庭另有 2 名成员、3 个房间、20 台设备；change_log 约 108 万行，
-- 其中 U 的可见行（本人行 + 家庭行）集中在全局 seq 约 60 万之后，
-- 用来模拟「老库 + 新用户」以及「多家庭可见范围」两种真实场景。
--
-- 用法（先迁移到 000018）：
--   psql "$DB" -v ON_ERROR_STOP=1 -f scripts/perf-multi-household-seed.sql
--
-- 脚本可重复执行：先 TRUNCATE users CASCADE，重置 change_log 序列。
-- 目录类表（device_types 等）由迁移 000002 写入，不在这里清理。

\set ON_ERROR_STOP on

TRUNCATE users, households, household_members, household_rooms, user_devices,
         change_log, tokens, idempotency_keys, scenes, push_devices, alerts,
         push_deliveries, feedback, readings, pairing_codes, rate_limit_buckets
    RESTART IDENTITY CASCADE;

-- 目标用户 U 与 2,000 个填充用户（填充用户只用于制造全局 change_log 体积）。
INSERT INTO users (id, kind)
VALUES ('00000000-0000-0000-0000-000000000001', 'anonymous')
ON CONFLICT DO NOTHING;

INSERT INTO users (id, kind)
SELECT ('00000000-0000-0000-0001-' || lpad(g::text, 12, '0'))::uuid, 'anonymous'
FROM generate_series(1, 2000) g
ON CONFLICT DO NOTHING;

-- 共享家庭的另两名成员（每家庭 2 人，作为家庭 owner 与成员出现）。
INSERT INTO users (id, kind)
SELECT ('00000000-0000-0000-0002-' || lpad(g::text, 12, '0'))::uuid, 'anonymous'
FROM generate_series(2, 33) g
ON CONFLICT DO NOTHING;

-- 16 个家庭：第 1 个是 U 的个人家庭，2..16 是共享家庭。
INSERT INTO households (id, name, owner_user_id, is_personal, created_at)
SELECT ('10000000-0000-0000-0000-' || lpad(g::text, 12, '0'))::uuid,
       CASE WHEN g = 1 THEN '我的家' ELSE 'perf-household-' || g END,
       CASE WHEN g = 1 THEN '00000000-0000-0000-0000-000000000001'::uuid
            ELSE ('00000000-0000-0000-0002-' || lpad((g * 2)::text, 12, '0'))::uuid END,
       g = 1,
       now() - interval '200 days'
FROM generate_series(1, 16) g;

-- U 加入全部 16 个家庭；共享家庭另有 2 名成员（每家庭 3 人）。
INSERT INTO household_members (household_id, user_id, role, joined_at)
SELECT ('10000000-0000-0000-0000-' || lpad(g::text, 12, '0'))::uuid,
       '00000000-0000-0000-0000-000000000001'::uuid,
       CASE WHEN g = 1 THEN 'owner' ELSE 'member' END,
       now() - interval '180 days'
FROM generate_series(1, 16) g;

INSERT INTO household_members (household_id, user_id, role, joined_at)
SELECT ('10000000-0000-0000-0000-' || lpad(h::text, 12, '0'))::uuid,
       ('00000000-0000-0000-0002-' || lpad(k::text, 12, '0'))::uuid,
       'member',
       now() - interval '170 days'
FROM generate_series(2, 16) h
CROSS JOIN LATERAL (VALUES (h * 2), (h * 2 + 1)) AS v(k);

-- 每个家庭 3 个房间。
INSERT INTO household_rooms (id, household_id, name, sort_order, created_at)
SELECT ('20000000-0000-0000-0000-' || lpad((h * 10 + r)::text, 12, '0'))::uuid,
       ('10000000-0000-0000-0000-' || lpad(h::text, 12, '0'))::uuid,
       'room-' || r,
       r,
       now() - interval '150 days'
FROM generate_series(1, 16) h
CROSS JOIN generate_series(1, 3) r;

-- 每个家庭 20 台设备，全部归 U（device_type 可为空，不影响同步查询）。
INSERT INTO user_devices (
    user_id, id, client_id, mac, alias, "group", rules, scan_records,
    updated_at, server_seq, platform, external_id, household_id, room_id, advertised_name
)
SELECT '00000000-0000-0000-0000-000000000001'::uuid,
       ('30000000-0000-0000-0000-' || lpad((h * 100 + d)::text, 12, '0'))::uuid,
       'perf-client-' || h || '-' || d,
       'AA:BB:' || lpad(h::text, 2, '0') || ':' || lpad(d::text, 2, '0') || ':00:01',
       'perf-device-' || h || '-' || d,
       'perf',
       '{}'::jsonb,
       '[]'::jsonb,
       now() - interval '100 days' + (h * 20 + d) * interval '1 hour',
       h * 20 + d,
       'android',
       'PERF-' || lpad(h::text, 2, '0') || '-' || lpad(d::text, 2, '0'),
       ('10000000-0000-0000-0000-' || lpad(h::text, 12, '0'))::uuid,
       ('20000000-0000-0000-0000-' || lpad((h * 10 + ((d - 1) % 3 + 1))::text, 12, '0'))::uuid,
       'Perf Device ' || h || '-' || d
FROM generate_series(1, 16) h
CROSS JOIN generate_series(1, 20) d;

-- 段 1：60 万行全局填充变更（U 在这里没有任何可见行）。
INSERT INTO change_log (user_id, household_id, entity, entity_id, op, payload, server_ts)
SELECT ('00000000-0000-0000-0001-' || lpad(((g % 2000) + 1)::text, 12, '0'))::uuid,
       NULL,
       'user_device',
       'filler:' || g,
       'upsert',
       jsonb_build_object('platform', 'android', 'externalId', 'FILL-' || g,
                          'alias', 'filler-' || g, 'group', 'filler'),
       now() - interval '80 days' + (g || ' seconds')::interval
FROM generate_series(1, 600000) g;

-- 段 2：U 的可见行。
-- 2a) 本人行：2 万条，落在个人家庭（user_id 与 household_id 同时命中）。
INSERT INTO change_log (user_id, household_id, entity, entity_id, op, payload, server_ts)
SELECT '00000000-0000-0000-0000-000000000001'::uuid,
       '10000000-0000-0000-0000-000000000001'::uuid,
       'user_device',
       'perf:personal:' || g,
       'upsert',
       jsonb_build_object('platform', 'android', 'externalId', 'PERF-P-' || g,
                          'alias', 'personal-' || g),
       now() - interval '60 days' + (g || ' seconds')::interval
FROM generate_series(1, 20000) g;

-- 2b) 家庭行：15 个共享家庭 × 4,000 条，写入者是同家庭的其他成员
--     （user_id <> U，只能通过 household_id 可见）。
INSERT INTO change_log (user_id, household_id, entity, entity_id, op, payload, server_ts)
SELECT ('00000000-0000-0000-0002-' || lpad((h * 2 + (g % 2))::text, 12, '0'))::uuid,
       ('10000000-0000-0000-0000-' || lpad(h::text, 12, '0'))::uuid,
       'user_device',
       'perf:household:' || h || ':' || g,
       'upsert',
       jsonb_build_object('platform', 'android', 'externalId', 'PERF-H-' || h || '-' || g,
                          'alias', 'household-' || h || '-' || g),
       now() - interval '50 days' + (g || ' seconds')::interval
FROM generate_series(2, 16) h
CROSS JOIN generate_series(1, 4000) g;

-- 段 3：再追加 40 万行全局填充变更，让 U 的可见窗口前后都有足量数据。
INSERT INTO change_log (user_id, household_id, entity, entity_id, op, payload, server_ts)
SELECT ('00000000-0000-0000-0001-' || lpad(((g % 2000) + 1)::text, 12, '0'))::uuid,
       NULL,
       'user_device',
       'filler-late:' || g,
       'upsert',
       jsonb_build_object('platform', 'android', 'externalId', 'LATE-' || g,
                          'alias', 'late-' || g, 'group', 'filler'),
       now() - interval '20 days' + (g || ' seconds')::interval
FROM generate_series(1, 400000) g;

ANALYZE users;
ANALYZE households;
ANALYZE household_members;
ANALYZE change_log;
ANALYZE user_devices;

SELECT 'users' AS table, count(*) FROM users
UNION ALL SELECT 'households', count(*) FROM households
UNION ALL SELECT 'household_members', count(*) FROM household_members
UNION ALL SELECT 'household_rooms', count(*) FROM household_rooms
UNION ALL SELECT 'user_devices', count(*) FROM user_devices
UNION ALL SELECT 'change_log', count(*) FROM change_log;
