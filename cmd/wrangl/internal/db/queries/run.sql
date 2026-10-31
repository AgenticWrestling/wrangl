-- name: ListActiveRuns :many
SELECT r.id, r.state, r.kind, r.status_text, r.queued_at, r.started_at,
       t.title AS task_title, p.slug AS project_slug, rn.name AS runner_name
FROM run r
JOIN task t ON t.id = r.task_id
JOIN project p ON p.id = t.project_id
LEFT JOIN runner rn ON rn.id = r.runner_id
WHERE r.state NOT IN ('succeeded', 'failed', 'cancelled')
ORDER BY p.priority DESC, r.queued_at;

-- name: CreateRun :one
INSERT INTO run (task_id, kind, attempt) VALUES (?, ?, ?)
RETURNING *;
