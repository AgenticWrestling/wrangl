-- name: CreateProject :one
INSERT INTO project (slug, name, description, default_template_id)
VALUES (?, ?, ?, ?)
RETURNING *;

-- name: ListProjects :many
SELECT * FROM project ORDER BY slug;

-- name: GetProjectBySlug :one
SELECT * FROM project WHERE slug = ?;
