-- name: CreateTemplate :one
INSERT INTO runner_template (name, description, mise_toml, setup_script, agent_json,
                             min_vcpu, min_ram_mib, min_disk_gib, arch, instance_types, ami,
                             max_runners, idle_timeout_s)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING *;

-- name: GetTemplateByName :one
SELECT * FROM runner_template WHERE name = ?;

-- name: ListTemplates :many
SELECT * FROM runner_template ORDER BY name;

-- name: DeleteTemplate :execrows
DELETE FROM runner_template WHERE name = ?;
