-- name: CreateSession :one
INSERT INTO sessions (
    id, name, image, state, fork_id, fork_path, created_at, updated_at, egress_policy, gateway
) VALUES (
    ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
)
RETURNING *;

-- name: GetSessionByRef :one
SELECT * FROM sessions
WHERE id = sqlc.arg(ref) OR name = sqlc.arg(ref)
LIMIT 1;

-- name: ListSessions :many
SELECT * FROM sessions
ORDER BY created_at DESC;

-- name: CountSessions :one
SELECT COUNT(*) FROM sessions;

-- name: UpdateSessionState :exec
UPDATE sessions
SET state = ?, updated_at = ?
WHERE id = ?;

-- name: TouchSession :exec
UPDATE sessions
SET last_used_at = ?, updated_at = ?
WHERE id = ?;

-- name: DeleteSession :execrows
DELETE FROM sessions WHERE id = ?;
