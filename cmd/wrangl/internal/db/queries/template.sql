-- name: CreateTemplate :one
INSERT INTO runner_template (name, description, mise_toml, setup_script, agent_json,
                             min_vcpu, min_ram_mib, min_disk_gib, arch, instance_types, ami,
                             max_runners, idle_timeout_s, max_sessions)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING *;

-- name: GetTemplateByName :one
SELECT * FROM runner_template WHERE name = ?;

-- name: ListTemplates :many
SELECT * FROM runner_template ORDER BY name;

-- name: DeleteTemplate :execrows
DELETE FROM runner_template WHERE name = ?;

-- name: UpdateTemplate :one
-- Replace a template's definition, bump its version and clear any recorded failure.
UPDATE runner_template
SET description = sqlc.arg(description), mise_toml = sqlc.arg(mise_toml),
    setup_script = sqlc.arg(setup_script), agent_json = sqlc.arg(agent_json),
    min_vcpu = sqlc.arg(min_vcpu), min_ram_mib = sqlc.arg(min_ram_mib),
    min_disk_gib = sqlc.arg(min_disk_gib), arch = sqlc.arg(arch),
    instance_types = sqlc.arg(instance_types), ami = sqlc.arg(ami),
    max_runners = sqlc.arg(max_runners), idle_timeout_s = sqlc.arg(idle_timeout_s),
    max_sessions = sqlc.arg(max_sessions),
    version = version + 1, failed_version = 0,
    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE name = sqlc.arg(name)
RETURNING *;

-- name: MarkTemplateFailed :exec
UPDATE runner_template SET failed_version = sqlc.arg(version)
WHERE id = sqlc.arg(id) AND version = sqlc.arg(version);
