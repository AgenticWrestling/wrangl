-- name: CreateQuestionOption :one
INSERT INTO question_option (question_id, position, label, value, description)
VALUES (?, ?, ?, ?, ?) RETURNING *;

-- name: CreateQuestion :one
INSERT INTO question (task_id, run_id, kind, prompt, detail_md, allow_free_text, payload_json)
VALUES (?, ?, ?, ?, ?, ?, ?)
RETURNING *;

-- name: ListOpenQuestions :many
SELECT * FROM question WHERE state = 'open' ORDER BY created_at;
