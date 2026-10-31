-- name: CreateRunner :one
INSERT INTO runner (name, template_id, instance_id, instance_type, az, arch)
VALUES (?, ?, ?, ?, ?, ?)
RETURNING *;

-- name: ListRunners :many
SELECT * FROM runner WHERE state <> 'terminated' ORDER BY name;

-- name: UpdateRunnerState :exec
UPDATE runner SET state = ? WHERE id = ?;
