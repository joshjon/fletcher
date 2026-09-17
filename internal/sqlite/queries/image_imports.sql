-- name: CreateImageImport :exec
INSERT INTO image_imports (id, ref, name, is_replacement, is_update, state, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, 'running', ?, ?);

-- name: GetImageImport :one
SELECT id, ref, name, is_replacement, is_update, state, error, created_at, updated_at FROM image_imports WHERE id = ?;

-- name: ListImageImports :many
SELECT id, ref, name, is_replacement, is_update, state, error, created_at, updated_at FROM image_imports
ORDER BY created_at DESC, id DESC LIMIT 50;

-- name: UpdateImageImport :exec
UPDATE image_imports SET state = ?, error = ?, updated_at = ? WHERE id = ?;

-- name: InterruptImageImports :exec
UPDATE image_imports SET state = 'failed', error = 'daemon restarted during import; inspect the image before retrying', updated_at = ?
WHERE state = 'running';

-- name: CountImageReferences :one
SELECT
    (SELECT COUNT(*) FROM sessions WHERE sessions.image = sqlc.arg(name)) +
    (SELECT COUNT(*) FROM jobs WHERE jobs.image = sqlc.arg(name) AND status IN ('queued', 'running', 'scheduled'));
