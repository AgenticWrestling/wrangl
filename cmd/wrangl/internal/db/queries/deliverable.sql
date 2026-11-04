-- name: InsertDeliverable :one
INSERT INTO deliverable (task_id, run_id, kind, title, url, file, mime)
VALUES (?, ?, ?, ?, ?, ?, ?)
RETURNING *;

-- name: GetDeliverable :one
SELECT * FROM deliverable WHERE id = ?;

-- name: ListDeliverablesForTask :many
SELECT * FROM deliverable WHERE task_id = ? ORDER BY id;

-- name: ListDeliverablesForRun :many
SELECT * FROM deliverable WHERE run_id = ? ORDER BY id;

-- name: CountDeliverablesForTasks :many
-- Deliverable counts for every task in a project, for the backlog badge.
SELECT d.task_id, CAST(COUNT(*) AS INTEGER) AS n
FROM deliverable d JOIN task t ON t.id = d.task_id
WHERE t.project_id = sqlc.arg(project_id)
GROUP BY d.task_id;

-- name: ExpiredDeliverables :many
SELECT * FROM deliverable WHERE created_at < sqlc.arg(cutoff) ORDER BY id LIMIT 500;

-- name: DeleteDeliverable :exec
DELETE FROM deliverable WHERE id = ?;

-- name: CountDeliverablesForRun :one
SELECT CAST(COUNT(*) AS INTEGER) FROM deliverable WHERE run_id = ?;

-- name: CountRunReviewTasks :one
-- Tasks this run completed that still wait for acceptance.
SELECT CAST(COUNT(*) AS INTEGER) FROM task WHERE completed_run_id = ? AND state = 'in_review';

-- name: OpenDeliverableQuestion :one
SELECT CAST(COUNT(*) AS INTEGER) FROM question
WHERE kind = 'deliverable_review' AND state = 'open' AND json_extract(payload_json, '$.run_id') = CAST(sqlc.arg(run_id) AS INTEGER);
