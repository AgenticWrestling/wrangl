-- name: UpsertTaskCheckpoint :exec
INSERT INTO task_checkpoint (task_id, run_id, sha) VALUES (?, ?, ?)
ON CONFLICT (task_id, run_id) DO UPDATE SET sha = excluded.sha, at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now');

-- name: LatestTaskCheckpoint :one
-- The task's newest checkpoint with its repo, and the previous task's checkpoint in the same
-- run (the base for a compare link; '' for the first task of a run).
SELECT c.sha AS sha, c.run_id AS run_id, c.at AS at, r.owner AS owner, r.name AS repo, r.default_branch AS default_branch,
       CAST(COALESCE((SELECT p.sha FROM task_checkpoint p WHERE p.run_id = c.run_id AND p.id < c.id ORDER BY p.id DESC LIMIT 1), '') AS TEXT) AS prev_sha
FROM task_checkpoint c JOIN run ru ON ru.id = c.run_id JOIN repo r ON r.id = ru.repo_id
WHERE c.task_id = ? ORDER BY c.id DESC LIMIT 1;
