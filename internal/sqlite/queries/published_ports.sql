-- name: CreatePublishedPort :one
INSERT INTO published_ports (
    id, session_id, guest_port, name, tunnel_port, public, host, created_at
) VALUES (
    ?, ?, ?, ?, ?, ?, ?, ?
)
RETURNING *;

-- name: GetPublishedPortBySessionPort :one
SELECT * FROM published_ports
WHERE session_id = ? AND guest_port = ?
LIMIT 1;

-- name: GetPublishedPublicPortByHost :one
SELECT * FROM published_ports
WHERE public = 1 AND host = ?
LIMIT 1;

-- name: ListPublishedPortsBySession :many
SELECT * FROM published_ports
WHERE session_id = ?
ORDER BY guest_port;

-- name: ListPublishedPorts :many
SELECT * FROM published_ports
ORDER BY created_at;

-- name: DeletePublishedPort :execrows
DELETE FROM published_ports WHERE id = ?;

-- name: DeletePublishedPortsBySession :exec
DELETE FROM published_ports WHERE session_id = ?;
