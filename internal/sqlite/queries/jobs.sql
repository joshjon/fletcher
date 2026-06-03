-- name: CreateJob :one
INSERT INTO jobs (
    id, status, trigger_kind, name, command, image, created_at, updated_at
) VALUES (
    ?, ?, ?, ?, ?, ?, ?, ?
)
RETURNING *;

-- name: GetJob :one
SELECT * FROM jobs WHERE id = ?;

-- name: ListJobs :many
SELECT * FROM jobs
ORDER BY created_at DESC
LIMIT ? OFFSET ?;

-- name: ListJobsByStatus :many
SELECT * FROM jobs
WHERE status = ?
ORDER BY created_at DESC
LIMIT ? OFFSET ?;

-- name: CountJobs :one
SELECT COUNT(*) FROM jobs;

-- name: CountJobsByStatus :one
SELECT COUNT(*) FROM jobs WHERE status = ?;

-- name: UpdateJobStatus :exec
UPDATE jobs
SET status = ?, updated_at = ?
WHERE id = ?;

-- name: MarkJobStarted :exec
UPDATE jobs
SET status = 'running', started_at = ?, updated_at = ?
WHERE id = ? AND status = 'queued';

-- name: MarkJobSucceeded :exec
UPDATE jobs
SET status = 'succeeded', exit_code = ?, completed_at = ?, updated_at = ?
WHERE id = ?;

-- name: MarkJobFailed :exec
UPDATE jobs
SET status = 'failed', exit_code = ?, error_message = ?, completed_at = ?, updated_at = ?
WHERE id = ?;

-- name: CancelJob :execrows
UPDATE jobs
SET status = 'cancelled', completed_at = ?, updated_at = ?
WHERE id = ? AND status IN ('queued', 'running');
