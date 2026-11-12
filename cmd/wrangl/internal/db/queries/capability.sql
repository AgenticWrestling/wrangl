-- name: DeleteTemplateCapsBySource :exec
DELETE FROM template_capability WHERE template_id = ? AND source = ?;

-- name: UpsertTemplateCap :exec
INSERT INTO template_capability (template_id, key, value, source) VALUES (?, ?, ?, ?)
ON CONFLICT (template_id, key) DO UPDATE SET value = excluded.value, source = excluded.source;

-- name: ListTemplateCaps :many
SELECT template_id, key, value, source FROM template_capability ORDER BY template_id, key;

-- name: DeleteRunnerCapsBySource :exec
DELETE FROM runner_capability WHERE runner_id = ? AND source = ?;

-- name: UpsertRunnerCap :exec
INSERT INTO runner_capability (runner_id, key, value, source) VALUES (?, ?, ?, ?)
ON CONFLICT (runner_id, key) DO UPDATE SET value = excluded.value, source = excluded.source;

-- name: AddRunnerCapIfAbsent :exec
INSERT OR IGNORE INTO runner_capability (runner_id, key, value, source) VALUES (?, ?, ?, ?);

-- name: ListRunnerCaps :many
SELECT runner_id, key, value, source FROM runner_capability ORDER BY runner_id, key;

-- name: DeleteRunnerCap :exec
DELETE FROM runner_capability WHERE runner_id = ? AND key = ?;

-- name: SetRequirement :exec
INSERT INTO requirement (owner_kind, owner_id, key, op, value) VALUES (?, ?, ?, ?, ?)
ON CONFLICT (owner_kind, owner_id, key) DO UPDATE SET op = excluded.op, value = excluded.value;

-- name: DeleteRequirement :exec
DELETE FROM requirement WHERE owner_kind = ? AND owner_id = ? AND key = ?;

-- name: DeleteRequirements :exec
DELETE FROM requirement WHERE owner_kind = ? AND owner_id = ?;

-- name: ListRequirements :many
SELECT key, op, value FROM requirement WHERE owner_kind = ? AND owner_id = ? ORDER BY key;

-- name: AddTemplateCapIfAbsent :exec
INSERT OR IGNORE INTO template_capability (template_id, key, value, source) VALUES (?, ?, ?, ?);
