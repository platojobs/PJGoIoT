-- user_devices 的全部列，保证 sqlc 生成统一的 UserDevice 结构。
-- name: GetUserDeviceByIDForUpdate :one
SELECT user_id, id, client_id, mac, alias, "group", rules, scan_records, updated_at, deleted_at, server_seq, created_at
FROM user_devices
WHERE user_id = $1 AND id = $2
FOR UPDATE;

-- name: GetUserDeviceByMACForUpdate :one
SELECT user_id, id, client_id, mac, alias, "group", rules, scan_records, updated_at, deleted_at, server_seq, created_at
FROM user_devices
WHERE user_id = $1 AND mac = $2
FOR UPDATE;

-- name: GetUserDeviceByMAC :one
SELECT user_id, id, client_id, mac, alias, "group", rules, scan_records, updated_at, deleted_at, server_seq, created_at
FROM user_devices
WHERE user_id = $1 AND mac = $2 AND deleted_at IS NULL;

-- name: ListUserDevices :many
SELECT user_id, id, client_id, mac, alias, "group", rules, scan_records, updated_at, deleted_at, server_seq, created_at
FROM user_devices
WHERE user_id = $1 AND deleted_at IS NULL
ORDER BY updated_at DESC, id;

-- name: InsertUserDevice :one
INSERT INTO user_devices (
    user_id, id, client_id, mac, alias, "group", rules, scan_records, updated_at, deleted_at, server_seq
) VALUES (
    sqlc.arg(user_id), sqlc.arg(id), sqlc.arg(client_id), sqlc.arg(mac), sqlc.arg(alias),
    sqlc.arg(group_name), sqlc.arg(rules), sqlc.arg(scan_records), sqlc.arg(updated_at),
    sqlc.arg(deleted_at), sqlc.arg(server_seq)
)
RETURNING user_id, id, client_id, mac, alias, "group", rules, scan_records, updated_at, deleted_at, server_seq, created_at;

-- name: UpdateUserDevice :one
UPDATE user_devices
SET id = sqlc.arg(new_id),
    client_id = sqlc.arg(client_id),
    mac = sqlc.arg(mac),
    alias = sqlc.arg(alias),
    "group" = sqlc.arg(group_name),
    rules = sqlc.arg(rules),
    scan_records = sqlc.arg(scan_records),
    updated_at = sqlc.arg(updated_at),
    deleted_at = sqlc.arg(deleted_at),
    server_seq = sqlc.arg(server_seq)
WHERE user_id = sqlc.arg(user_id) AND id = sqlc.arg(old_id)
RETURNING user_id, id, client_id, mac, alias, "group", rules, scan_records, updated_at, deleted_at, server_seq, created_at;

-- name: NextChangeSeq :one
SELECT nextval(pg_get_serial_sequence('change_log', 'seq'))::bigint AS seq;

-- name: InsertChangeLog :exec
INSERT INTO change_log (seq, user_id, entity, entity_id, op, payload, server_ts)
VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: GetMaxSeqForUser :one
SELECT COALESCE(MAX(seq), 0)::bigint AS seq
FROM change_log
WHERE user_id = $1;

-- name: ListChanges :many
SELECT seq, user_id, entity, entity_id, op, payload, server_ts
FROM change_log
WHERE user_id = $1 AND seq > $2
ORDER BY seq
LIMIT $3;

-- name: LockIdempotencyKey :exec
SELECT pg_advisory_xact_lock(hashtextextended($1::text, 0));

-- name: GetIdempotencyKey :one
SELECT request_hash, response
FROM idempotency_keys
WHERE user_id = $1 AND key = $2;

-- name: InsertIdempotencyKey :exec
INSERT INTO idempotency_keys (user_id, key, request_hash, response)
VALUES ($1, $2, $3, $4)
ON CONFLICT (user_id, key) DO NOTHING;
