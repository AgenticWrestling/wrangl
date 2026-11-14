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
UPDATE question SET state = 'cancelled' WHERE run_id = ? AND state = 'open' AND kind IN ('agent', 'system', 'tool_error', 'unplaceable', 'needs_repo');

-- name: UpdateOpenQuestion :exec
-- The reason an open item (unplaceable) is about changed: reword it in place.
UPDATE question SET prompt = sqlc.arg(prompt), detail_md = sqlc.arg(detail_md), payload_json = sqlc.arg(payload_json)
WHERE id = sqlc.arg(id) AND state = 'open';

-- name: DeleteQuestionOptions :exec
DELETE FROM question_option WHERE question_id = ?;

-- name: ResolveQuestion :execrows
UPDATE question SET state = 'cancelled', answer_text = sqlc.arg(text) WHERE id = sqlc.arg(id) AND state = 'open';

-- name: ListOpenUnplaceable :many
-- Open unplaceable items with the state of their run (NULL: the run is gone).
SELECT q.id, q.run_id, r.state AS run_state FROM question q LEFT JOIN run r ON r.id = q.run_id
WHERE q.kind = 'unplaceable' AND q.state = 'open';

-- name: SetQuestionPR :exec
INSERT OR REPLACE INTO question_pr (pr_id, question_id) VALUES (?, ?);

-- name: QuestionPRID :one
SELECT pr_id FROM question_pr WHERE question_id = ?;

-- name: CancelPRBlockingQuestions :execrows
-- A restarted resolve run's questions are stale: close them.
UPDATE question SET state = 'cancelled' WHERE state = 'open' AND id IN (SELECT question_id FROM question_pr WHERE pr_id = ?);

-- name: ListOpenNeedsRepo :many
-- Open needs_repo items with the state and repo of their run (NULL: the run is gone).
SELECT q.id, q.run_id, r.state AS run_state, r.repo_id AS run_repo_id FROM question q LEFT JOIN run r ON r.id = q.run_id
WHERE q.kind = 'needs_repo' AND q.state = 'open';
