-- name: CreateReport :one
INSERT INTO reports (
    id, source_type, source_id, source_name, title, summary, status, link, created_at
) VALUES (
    ?, ?, ?, ?, ?, ?, ?, ?, ?
)
RETURNING *;

-- name: GetReport :one
SELECT * FROM reports WHERE id = ?;

-- name: ListReports :many
SELECT * FROM reports
ORDER BY created_at DESC
LIMIT ? OFFSET ?;
