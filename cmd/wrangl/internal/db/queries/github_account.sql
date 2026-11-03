-- name: ListGithubAccounts :many
SELECT * FROM github_account ORDER BY CASE kind WHEN 'user' THEN 0 ELSE 1 END, login;

-- name: GetGithubAccount :one
SELECT * FROM github_account WHERE login = ?;

-- name: UpsertGithubAccount :one
INSERT INTO github_account (login, kind) VALUES (?, ?)
ON CONFLICT (login) DO UPDATE SET kind = excluded.kind
RETURNING *;

-- name: SetGithubAccountApp :exec
UPDATE github_account SET app_id = ?, slug = ?, html_url = ?, key_file = ? WHERE login = ?;

-- name: DeleteGithubAccount :exec
DELETE FROM github_account WHERE login = ?;
