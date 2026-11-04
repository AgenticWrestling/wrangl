-- name: InsertAgent :one
INSERT INTO agent (name, description, spec_json) VALUES (sqlc.arg(name), sqlc.arg(description), sqlc.arg(spec_json))
RETURNING *;

-- name: GetAgentByName :one
SELECT * FROM agent WHERE name = ?;

-- name: GetAgent :one
SELECT * FROM agent WHERE id = ?;

-- name: ListAgentDefs :many
SELECT * FROM agent ORDER BY name;

-- name: AgentBySpec :one
SELECT * FROM agent WHERE spec_json = ? ORDER BY id LIMIT 1;

-- name: UpdateAgentDef :one
UPDATE agent SET description = sqlc.arg(description), spec_json = sqlc.arg(spec_json), version = version + 1,
    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE name = sqlc.arg(name)
RETURNING *;

-- name: DeleteAgentDef :execrows
DELETE FROM agent WHERE name = ?;

-- name: ListTemplateAgents :many
SELECT a.* FROM agent a JOIN template_agent ta ON ta.agent_id = a.id WHERE ta.template_id = ? ORDER BY a.name;

-- name: ListAgentTemplates :many
SELECT t.name FROM runner_template t JOIN template_agent ta ON ta.template_id = t.id WHERE ta.agent_id = ? ORDER BY t.name;

-- name: AddTemplateAgent :exec
INSERT OR IGNORE INTO template_agent (template_id, agent_id) VALUES (?, ?);

-- name: ClearTemplateAgents :exec
DELETE FROM template_agent WHERE template_id = ?;

-- name: GetProjectDefaultAgent :one
SELECT default_agent_id FROM project WHERE id = ?;

-- name: SetProjectDefaultAgent :exec
UPDATE project SET default_agent_id = sqlc.narg(agent_id) WHERE id = sqlc.arg(id);

-- name: SetRunAgent :exec
UPDATE run SET agent_id = sqlc.narg(agent_id) WHERE id = sqlc.arg(id);

-- name: GetRunAgent :one
SELECT agent_id FROM run WHERE id = ?;

-- name: RemoveTemplateAgent :exec
DELETE FROM template_agent WHERE template_id = ? AND agent_id = ?;
