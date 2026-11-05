-- name: CreateUser :one
INSERT INTO users (kind) VALUES ('anonymous') RETURNING id;

-- name: CreateToken :exec
INSERT INTO tokens (user_id, token_hash, expires_at) VALUES ($1, $2, $3);

-- name: GetUserIDByTokenHash :one
SELECT user_id
FROM tokens
WHERE token_hash = $1 AND revoked_at IS NULL AND expires_at > now();

-- name: RevokeToken :execrows
UPDATE tokens
SET revoked_at = now()
WHERE token_hash = $1 AND revoked_at IS NULL;
