-- name: UpsertSetting :exec
INSERT INTO settings (key, value, updated_at)
VALUES (?, ?, ?)
ON CONFLICT (key) DO UPDATE SET
    value = excluded.value,
    updated_at = excluded.updated_at;

-- name: ListSettings :many
SELECT key, value, updated_at FROM settings ORDER BY key;

-- name: DeleteSetting :execrows
DELETE FROM settings WHERE key = ?;
