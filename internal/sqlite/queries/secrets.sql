-- name: UpsertSecret :exec
INSERT INTO secrets (name, ciphertext, created_at, updated_at)
VALUES (?, ?, ?, ?)
ON CONFLICT (name) DO UPDATE SET
    ciphertext = excluded.ciphertext,
    updated_at = excluded.updated_at;

-- name: GetSecret :one
SELECT ciphertext FROM secrets WHERE name = ?;

-- name: ListSecretMetadata :many
SELECT name, created_at, updated_at FROM secrets ORDER BY name;

-- name: DeleteSecret :execrows
DELETE FROM secrets WHERE name = ?;
