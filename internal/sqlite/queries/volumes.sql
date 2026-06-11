-- name: CreateVolume :one
INSERT INTO volumes (
    id, name, path, size_bytes, created_at, updated_at
) VALUES (
    ?, ?, ?, ?, ?, ?
)
RETURNING *;

-- name: GetVolumeByRef :one
SELECT * FROM volumes
WHERE id = sqlc.arg(ref) OR name = sqlc.arg(ref)
LIMIT 1;

-- name: ListVolumes :many
SELECT * FROM volumes
ORDER BY created_at DESC;

-- name: DeleteVolume :execrows
DELETE FROM volumes WHERE id = ?;

-- name: ListSessionsByVolume :many
SELECT * FROM sessions
WHERE volume_id = ?;
