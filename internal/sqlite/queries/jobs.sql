-- name: CreateJob :one
INSERT INTO jobs (
    id, status, trigger_kind, name, command, image, credentials, created_at, updated_at,
    schedule, next_run_at, parent_id
) VALUES (
    ?, ?, ?, ?, ?, ?, ?, ?, ?,
    ?, ?, ?
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
WHERE id = ? AND status IN ('queued', 'running', 'scheduled');

-- name: ListDueCronJobs :many
SELECT * FROM jobs
WHERE status = 'scheduled' AND trigger_kind = 'cron'
  AND next_run_at IS NOT NULL AND next_run_at <= ?
ORDER BY next_run_at ASC;

-- name: SetJobNextRun :exec
UPDATE jobs
SET next_run_at = ?, updated_at = ?
WHERE id = ?;
