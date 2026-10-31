-- name: CreateEnrollmentToken :one
INSERT INTO enrollment_token (token_hash, template_id, instance_id, runner_id, runner_name, expires_at)
VALUES (?, ?, ?, ?, ?, ?)
RETURNING id;

-- name: BindTokenToRunner :exec
UPDATE enrollment_token SET instance_id = ?, runner_id = ? WHERE id = ?;

-- name: GetEnrollmentToken :one
SELECT * FROM enrollment_token WHERE token_hash = ?;

-- name: ConsumeEnrollmentToken :execrows
-- Single use: only succeeds if still unused.
UPDATE enrollment_token SET used_at = ?, runner_id = ? WHERE id = ? AND used_at IS NULL;

-- name: GetTemplate :one
SELECT * FROM runner_template WHERE id = ?;
