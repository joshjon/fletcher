-- name: CreateProject :one
INSERT INTO projects (id, name, session_id, working_dir, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?)
RETURNING id, name, session_id, working_dir, created_at, updated_at;

-- name: GetProjectByRef :one
SELECT id, name, session_id, working_dir, created_at, updated_at
FROM projects
WHERE id = sqlc.arg(ref) OR name = sqlc.arg(ref)
ORDER BY CASE WHEN id = ?1 THEN 0 ELSE 1 END
LIMIT 1;

-- name: ListProjects :many
SELECT id, name, session_id, working_dir, created_at, updated_at
FROM projects
ORDER BY created_at DESC, id DESC;

-- name: UpdateProject :one
UPDATE projects
SET name = COALESCE(sqlc.narg(name), name),
    working_dir = COALESCE(sqlc.narg(working_dir), working_dir), updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(id)
RETURNING id, name, session_id, working_dir, created_at, updated_at;

-- name: DeleteProject :execrows
DELETE FROM projects WHERE id = ?;
