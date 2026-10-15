-- name: CreateSession :one
INSERT INTO sessions (
    id, name, image, state, fork_id, fork_path, created_at, updated_at, egress_policy, gateway, run_app, volume_id, env_vars
) VALUES (
    ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
)
RETURNING id, name, image, state, fork_id, fork_path, created_at, updated_at,
    last_used_at, egress_policy, gateway, run_app, prev_fork_id, prev_fork_path,
    volume_id, env_vars, execution_kind, host_user, host_uid, host_home, working_dir;

-- name: GetSessionByRef :one
SELECT id, name, image, state, fork_id, fork_path, created_at, updated_at,
    last_used_at, egress_policy, gateway, run_app, prev_fork_id, prev_fork_path,
    volume_id, env_vars, execution_kind, host_user, host_uid, host_home, working_dir
FROM sessions
WHERE id = ?1 OR name = ?1
ORDER BY CASE WHEN id = ?1 THEN 0 ELSE 1 END
LIMIT 1;

-- name: ListSessions :many
SELECT id, name, image, state, fork_id, fork_path, created_at, updated_at,
    last_used_at, egress_policy, gateway, run_app, prev_fork_id, prev_fork_path,
    volume_id, env_vars, execution_kind, host_user, host_uid, host_home, working_dir
FROM sessions
ORDER BY created_at DESC;

-- Legacy policy columns retain valid placeholders; host responses expose no VM policy.
-- name: CreateHostSession :one
INSERT INTO sessions (
    id, name, image, state, fork_id, fork_path, created_at, updated_at,
    egress_policy, gateway, run_app, env_vars,
    execution_kind, host_user, host_uid, host_home, working_dir
) VALUES (
    sqlc.arg(id), sqlc.arg(name), '', 'running', '', '', sqlc.arg(now), sqlc.arg(now),
    'none', 'off', 0, '[]',
    'host', sqlc.arg(host_user), sqlc.arg(host_uid), sqlc.arg(host_home), sqlc.arg(working_dir)
)
RETURNING id, name, image, state, fork_id, fork_path, created_at, updated_at,
    last_used_at, egress_policy, gateway, run_app, prev_fork_id, prev_fork_path,
    volume_id, env_vars, execution_kind, host_user, host_uid, host_home, working_dir;

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

-- name: UpdateSessionFork :exec
UPDATE sessions
SET fork_id = ?, fork_path = ?, updated_at = ?
WHERE id = ?;

-- name: UpdateSessionForks :exec
UPDATE sessions
SET fork_id = ?, fork_path = ?, prev_fork_id = ?, prev_fork_path = ?, image = ?, updated_at = ?
WHERE id = ?;

-- name: UpdateSessionConfiguration :exec
UPDATE sessions
SET egress_policy = ?, gateway = ?, env_vars = ?, updated_at = ?
WHERE id = ?;
