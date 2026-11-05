-- name: InsertTool :one
INSERT INTO tool (name, kind, effect, description, kdl, decl_json)
VALUES (sqlc.arg(name), sqlc.arg(kind), sqlc.arg(effect), sqlc.arg(description), sqlc.arg(kdl), sqlc.arg(decl_json))
RETURNING *;

-- name: GetToolByName :one
SELECT * FROM tool WHERE name = ?;

-- name: ListTools :many
SELECT * FROM tool ORDER BY name;

-- name: UpdateTool :one
UPDATE tool SET kind = sqlc.arg(kind), effect = sqlc.arg(effect), description = sqlc.arg(description),
    kdl = sqlc.arg(kdl), decl_json = sqlc.arg(decl_json), version = version + 1,
    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE name = sqlc.arg(name)
RETURNING *;

-- name: DeleteTool :execrows
DELETE FROM tool WHERE name = ?;

-- name: EnableProjectTool :exec
INSERT OR IGNORE INTO project_tool (project_id, tool_id) VALUES (?, ?);

-- name: DisableProjectTool :execrows
DELETE FROM project_tool WHERE project_id = ? AND tool_id = ?;

-- name: ListProjectTools :many
SELECT t.* FROM tool t JOIN project_tool pt ON pt.tool_id = t.id WHERE pt.project_id = ? ORDER BY t.name;

-- name: ListToolProjects :many
SELECT p.slug FROM project p JOIN project_tool pt ON pt.project_id = p.id WHERE pt.tool_id = ? ORDER BY p.slug;

-- name: IsProjectToolEnabled :one
SELECT COUNT(*) FROM project_tool WHERE project_id = ? AND tool_id = ?;
