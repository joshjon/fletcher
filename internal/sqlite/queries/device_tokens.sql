-- name: UpsertDeviceToken :exec
INSERT INTO device_tokens (token, created_at) VALUES (?, ?)
ON CONFLICT(token) DO NOTHING;

-- name: DeleteDeviceToken :execrows
DELETE FROM device_tokens WHERE token = ?;

-- name: ListDeviceTokens :many
SELECT token FROM device_tokens ORDER BY created_at;
