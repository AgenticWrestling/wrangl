-- name: CreatePR :one
INSERT INTO pull_request (task_id, repo_id, run_id, gh_number, url, head_branch, head_sha)
VALUES (?, ?, ?, ?, ?, ?, ?)
RETURNING *;

-- name: GetPR :one
SELECT * FROM pull_request WHERE id = ?;

-- name: OpenPRForBranch :one
SELECT * FROM pull_request WHERE repo_id = ? AND head_branch = ? AND state = 'open';

-- name: OpenPRForTask :one
SELECT * FROM pull_request WHERE task_id = ? AND state = 'open' ORDER BY id DESC LIMIT 1;

-- name: OpenPRsByHeadSHA :many
SELECT * FROM pull_request WHERE head_sha = ? AND state = 'open';

-- name: OpenPRs :many
SELECT * FROM pull_request WHERE state = 'open' ORDER BY id;

-- name: ListPRs :many
SELECT sqlc.embed(pr), CAST(COALESCE(t.sqid, '') AS TEXT) AS task_sqid, CAST(t.title AS TEXT) AS task_title,
       CAST(r.owner || '/' || r.name AS TEXT) AS repo
FROM pull_request pr JOIN task t ON t.id = pr.task_id JOIN repo r ON r.id = pr.repo_id
WHERE CAST(sqlc.arg(state) AS TEXT) = 'all' OR pr.state = CAST(sqlc.arg(state) AS TEXT)
ORDER BY pr.id DESC LIMIT 200;

-- name: UpdatePRHead :exec
UPDATE pull_request SET head_sha = ?, run_id = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE id = ?;

-- name: SetPRHeadSHA :exec
UPDATE pull_request SET head_sha = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE id = ?;

-- name: SetPRGates :exec
UPDATE pull_request SET gates_json = ?, checks = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE id = ?;

-- name: SetPRReview :exec
UPDATE pull_request SET risk = sqlc.narg(risk), review_summary_md = sqlc.arg(summary), reviewed_sha = sqlc.arg(reviewed_sha),
       updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE id = sqlc.arg(id);

-- name: ClosePRRow :exec
UPDATE pull_request SET state = sqlc.arg(state), decision = sqlc.narg(decision),
       updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE id = sqlc.arg(id) AND state = 'open';

-- name: SetPRDecision :exec
UPDATE pull_request SET decision = sqlc.narg(decision), updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE id = sqlc.arg(id);

-- name: RunsForPR :many
-- Work/revise runs whose commits are on the PR's branch.
SELECT id FROM run WHERE repo_id = ? AND branch = ? AND kind IN ('work', 'revise', 'resolve');

-- name: ActiveRunForPR :one
SELECT CAST(COUNT(*) AS INTEGER) FROM run
WHERE pr_id = ? AND kind = sqlc.arg(kind) AND state NOT IN ('succeeded', 'failed', 'blocked', 'cancelled');

-- name: OpenQuestionForPR :one
SELECT CAST(COUNT(*) AS INTEGER) FROM question
WHERE kind = 'pr_review' AND state = 'open' AND json_extract(payload_json, '$.pr_id') = CAST(sqlc.arg(pr_id) AS INTEGER);

-- name: CancelPRQuestions :exec
UPDATE question SET state = 'cancelled'
WHERE kind = 'pr_review' AND state = 'open' AND json_extract(payload_json, '$.pr_id') = CAST(sqlc.arg(pr_id) AS INTEGER);

-- name: QueueSpecialRun :one
INSERT INTO run (task_id, kind, attempt, template_id, repo_id, branch, session_uuid, prompt_override, pr_id, continue_branch)
VALUES (sqlc.arg(task_id), sqlc.arg(kind), sqlc.arg(attempt), sqlc.arg(template_id), sqlc.arg(repo_id), sqlc.arg(branch),
        sqlc.arg(session_uuid), sqlc.arg(prompt_override), sqlc.narg(pr_id), sqlc.arg(continue_branch))
RETURNING *;

-- name: UseOverride :execrows
UPDATE run SET override_used = 1 WHERE id = ? AND override_used = 0;

-- name: ReopenRunTasks :exec
-- PR closed without merging: its completed leaves go back to open.
UPDATE task SET state = 'open', completed_run_id = NULL, completed_at = NULL,
       updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE completed_run_id = ? AND state = 'in_review';

-- name: RecordDelivery :execrows
INSERT INTO gh_delivery (id, received_at) VALUES (?, ?) ON CONFLICT (id) DO NOTHING;

-- name: LatestBlockedRuns :many
-- The most recent run of each task root, where that run ended blocked.
SELECT r.* FROM run r
WHERE r.state = 'blocked' AND r.kind IN ('work', 'revise')
  AND r.id = (SELECT MAX(x.id) FROM run x WHERE x.task_id = r.task_id);

-- name: PRByNumber :one
SELECT pr.* FROM pull_request pr JOIN repo r ON r.id = pr.repo_id
WHERE r.owner = ? AND r.name = ? AND pr.gh_number = ?
ORDER BY pr.id DESC LIMIT 1;

-- name: ReposByName :many
SELECT * FROM repo WHERE owner = ? AND name = ?;

-- name: SetPRMergeable :exec
UPDATE pull_request SET mergeable = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE id = ?;
