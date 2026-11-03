-- name: ListControllerAgents :many
SELECT * FROM controller_agent ORDER BY is_default DESC, name;

-- name: GetControllerAgent :one
SELECT * FROM controller_agent WHERE id = ?;

-- name: GetControllerAgentByName :one
SELECT * FROM controller_agent WHERE name = ?;

-- name: DefaultControllerAgent :one
SELECT * FROM controller_agent WHERE enabled = 1 ORDER BY is_default DESC, id LIMIT 1;

-- name: UpsertControllerAgent :one
INSERT INTO controller_agent (name, kind, command, model, auth, auth_secret, auth_env, max_turns, timeout_s, enabled)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (name) DO UPDATE SET kind = excluded.kind, command = excluded.command, model = excluded.model,
    auth = excluded.auth, auth_secret = excluded.auth_secret, auth_env = excluded.auth_env,
    max_turns = excluded.max_turns, timeout_s = excluded.timeout_s, enabled = excluded.enabled
RETURNING id;

-- name: ClearDefaultControllerAgent :exec
UPDATE controller_agent SET is_default = 0;

-- name: SetDefaultControllerAgent :exec
UPDATE controller_agent SET is_default = 1 WHERE name = ?;

-- name: DeleteControllerAgent :exec
DELETE FROM controller_agent WHERE name = ?;

-- name: AddThreadMessage :one
INSERT INTO task_thread (task_id, role, body, agent_id, status, session_id) VALUES (?, ?, ?, ?, ?, ?) RETURNING id;

-- name: FinishThreadMessage :exec
UPDATE task_thread SET body = ?, status = ?, session_id = ?, proposals = ? WHERE id = ?;

-- name: GetThreadMessage :one
SELECT * FROM task_thread WHERE id = ?;

-- name: SetThreadProposals :exec
UPDATE task_thread SET proposals = ? WHERE id = ?;

-- name: ListThread :many
SELECT * FROM task_thread WHERE task_id = ? ORDER BY id;

-- name: ThreadSession :one
SELECT session_id FROM task_thread WHERE task_id = ? AND agent_id = ? AND session_id <> '' ORDER BY id DESC LIMIT 1;

-- name: FailRunningThreadMessages :exec
UPDATE task_thread SET status = 'error', body = 'The controller restarted before this finished.' WHERE status = 'running';

-- name: SubtreeRev :one
WITH RECURSIVE sub (id) AS (
    SELECT t.id FROM task t WHERE t.id = sqlc.arg(root_id)
    UNION ALL
    SELECT c.id FROM task c JOIN sub ON c.parent_id = sub.id
)
SELECT CAST(COUNT(*) || ':' || COALESCE(MAX(task.updated_at), '') || ':' || COALESCE(SUM(task.parent_id), 0) AS TEXT) AS rev
FROM sub JOIN task ON task.id = sub.id;

-- name: SetThreadRev :exec
UPDATE task_thread SET rev = ? WHERE id = ?;
