-- name: CreateQuestionOption :one
INSERT INTO question_option (question_id, position, label, value, description)
VALUES (?, ?, ?, ?, ?) RETURNING *;

-- name: CreateQuestion :one
INSERT INTO question (task_id, run_id, kind, prompt, detail_md, allow_free_text, payload_json)
VALUES (?, ?, ?, ?, ?, ?, ?)
RETURNING *;

-- name: ListOpenQuestions :many
SELECT * FROM question WHERE state = 'open' ORDER BY created_at;

-- name: GetQuestion :one
SELECT * FROM question WHERE id = ?;

-- name: ListQuestions :many
SELECT sqlc.embed(q), CAST(COALESCE(t.sqid, '') AS TEXT) AS task_sqid, CAST(t.title AS TEXT) AS task_title,
       CAST(p.slug AS TEXT) AS project_slug
FROM question q JOIN task t ON t.id = q.task_id JOIN project p ON p.id = t.project_id
WHERE p.archived_at IS NULL AND (CAST(sqlc.arg(open_only) AS INTEGER) = 0 OR q.state = 'open')
ORDER BY q.id DESC LIMIT 200;

-- name: ListQuestionOptions :many
SELECT * FROM question_option WHERE question_id = ? ORDER BY position;

-- name: AnswerQuestion :execrows
UPDATE question SET state = 'answered', answer_option_id = sqlc.narg(option_id), answer_text = sqlc.arg(text),
       answered_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND state = 'open';

-- name: CancelRunQuestions :exec
-- Only questions a waiting agent or runner needed; approvals and reviews outlive the run.
UPDATE question SET state = 'cancelled' WHERE run_id = ? AND state = 'open' AND kind IN ('agent', 'system');
