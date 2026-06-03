-- name: CreatePeer :one
INSERT INTO peers (id, name, public_key, allowed_ips, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?)
RETURNING *;

-- name: GetPeer :one
SELECT * FROM peers WHERE id = ?;

-- name: GetPeerByName :one
SELECT * FROM peers WHERE name = ?;

-- name: ListPeers :many
SELECT * FROM peers
ORDER BY created_at DESC
LIMIT ? OFFSET ?;

-- name: CountPeers :one
SELECT COUNT(*) FROM peers;

-- name: DeletePeer :execrows
DELETE FROM peers WHERE id = ?;
