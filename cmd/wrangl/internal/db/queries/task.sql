-- NOTE: always qualify the rank column (task.rank, t.rank). sqlc's sqlite grammar treats a
-- bare `rank` as the RANK() window-function keyword and fails to parse; SQLite itself is fine.
-- Other sqlc workarounds in this file are documented in the M1 plan ("Verified up front").

-- name: CreateTask :one
INSERT INTO task (project_id, parent_id, rank, repo_id, template_id, title, prompt, created_by, task_type)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING id;

-- name: SetTaskSqid :exec
UPDATE task SET sqid = ? WHERE id = ?;

-- name: GetTaskBySqid :one
SELECT * FROM task WHERE sqid = ?;

-- name: SubtreeOrdered :many
-- Every task under (and including) root, depth-first in rank order.
WITH RECURSIVE sub (id, depth, path) AS (
    SELECT t.id, 0, t.rank FROM task t WHERE t.id = sqlc.arg(root_id)
    UNION ALL
    SELECT c.id, sub.depth + 1, sub.path || ' ' || c.rank
    FROM task c JOIN sub ON c.parent_id = sub.id
)
SELECT task.id, task.sqid, task.parent_id, task.title, task.state, task.rank,
       CAST(sub.depth AS INTEGER) AS depth, CAST(sub.path AS TEXT) AS path
FROM sub JOIN task ON task.id = sub.id
ORDER BY sub.path;

-- name: NextReadyLeaf :one
-- First open leaf under root (rank order, depth-first) whose precursors are satisfied:
-- merged (done), or completed earlier in this same run (in_review on the same branch).
WITH RECURSIVE sub (id, path) AS (
    SELECT t.id, t.rank FROM task t WHERE t.id = sqlc.arg(root_id)
    UNION ALL
    SELECT c.id, sub.path || ' ' || c.rank FROM task c JOIN sub ON c.parent_id = sub.id
)
SELECT task.id, task.sqid, task.title, task.prompt
FROM sub JOIN task ON task.id = sub.id
WHERE task.state IN ('open', 'in_progress')
  AND NOT EXISTS (SELECT 1 FROM task k WHERE k.parent_id = task.id AND k.state <> 'cancelled')
  AND NOT EXISTS (
      SELECT 1 FROM task_dependency d JOIN task p ON p.id = d.depends_on_id
      WHERE d.task_id = task.id
        AND NOT (p.state = 'done'
                 OR (p.state = 'in_review' AND p.completed_run_id = sqlc.arg(run_id))))
ORDER BY sub.path
LIMIT 1;

-- name: IsAncestorOrSelf :one
-- 1 if sqlc.arg(ancestor_id) is node_id or one of its ancestors.
WITH RECURSIVE up (id, parent_id) AS (
    SELECT t.id, t.parent_id FROM task t WHERE t.id = sqlc.arg(node_id)
    UNION ALL
    SELECT t.id, t.parent_id FROM task t JOIN up ON t.id = up.parent_id
)
SELECT CAST(COUNT(*) > 0 AS INTEGER) AS hit FROM up WHERE up.id = CAST(sqlc.arg(ancestor_id) AS INTEGER);

-- name: PrecursorReaches :one
-- 1 if from_id transitively depends on to_id (adding to_id -> from_id would cycle).
WITH RECURSIVE reach (id) AS (
    SELECT d.depends_on_id FROM task_dependency d WHERE d.task_id = sqlc.arg(from_id)
    UNION
    SELECT d.depends_on_id FROM task_dependency d JOIN reach ON d.task_id = reach.id
)
SELECT CAST(COUNT(*) > 0 AS INTEGER) AS hit FROM reach WHERE reach.id = CAST(sqlc.arg(to_id) AS INTEGER);

-- name: SubtreeLocked :one
-- 1 if an active run's root is an ancestor-or-self or a descendant of root_id.
WITH RECURSIVE up (id, parent_id) AS (
    SELECT t.id, t.parent_id FROM task t WHERE t.id = sqlc.arg(root_id)
    UNION ALL
    SELECT t.id, t.parent_id FROM task t JOIN up ON t.id = up.parent_id
), down (id) AS (
    SELECT t.id FROM task t WHERE t.parent_id = sqlc.arg(root_id)
    UNION ALL
    SELECT t.id FROM task t JOIN down ON t.parent_id = down.id
), near (id) AS (
    SELECT id FROM up UNION SELECT id FROM down
)
SELECT CAST(COUNT(*) > 0 AS INTEGER) AS locked
FROM run r JOIN near ON near.id = r.task_id
WHERE r.state NOT IN ('succeeded', 'failed', 'blocked', 'cancelled');

-- name: AddPrecursor :exec
INSERT INTO task_dependency (task_id, depends_on_id, created_by) VALUES (?, ?, ?);

-- name: CompleteTask :exec
-- Agent marks a leaf complete: in_review until its PR merges.
UPDATE task SET state = 'in_review', completion_summary = ?, completed_run_id = ?, completed_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now'),
       updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE id = ?;

-- name: MarkRunTasksDone :exec
-- (also used by M6 merges)
-- PR merged: everything this run completed becomes done.
UPDATE task SET state = 'done', updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE completed_run_id = ? AND state = 'in_review';

-- name: ListChildren :many
SELECT * FROM task WHERE task.parent_id = ? ORDER BY task.rank;

-- name: ListRootTasks :many
SELECT * FROM task WHERE task.project_id = ? AND task.parent_id IS NULL ORDER BY task.rank;

-- name: LastChildRank :one
SELECT CAST(COALESCE(MAX(task.rank), '') AS TEXT) AS last_rank FROM task WHERE task.parent_id = ?;

-- name: LastRootRank :one
SELECT CAST(COALESCE(MAX(task.rank), '') AS TEXT) AS last_rank FROM task WHERE task.project_id = ? AND task.parent_id IS NULL;

-- name: InsertTaskEvent :exec
INSERT INTO task_event (task_sqid, run_id, actor, op, diff_json) VALUES (?, ?, ?, ?, ?);

-- name: GetTask :one
SELECT * FROM task WHERE id = ?;

-- name: UpdateTaskText :one
UPDATE task SET title = sqlc.arg(title), prompt = sqlc.arg(prompt),
       updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE id = sqlc.arg(id) RETURNING *;

-- name: SetTaskType :exec
UPDATE task SET task_type = sqlc.arg(task_type), updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE id = sqlc.arg(id);

-- name: SetTaskTargetDate :exec
-- NULL clears it. Dates are YYYY-MM-DD.
UPDATE task SET target_date = sqlc.narg(target_date), updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE id = sqlc.arg(id);

-- name: MoveTask :exec
UPDATE task SET parent_id = sqlc.narg(parent_id), rank = sqlc.arg(rank),
       updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE id = sqlc.arg(id);

-- name: SetTaskState :exec
-- completed_at is computed by the caller: sqlc leaves sqlc.arg() unrewritten inside
-- CASE WHEN ... IN (...) (quirk 6), so no CASE on parameters here.
UPDATE task SET state = sqlc.arg(state), completion_summary = sqlc.arg(summary),
       completed_run_id = sqlc.narg(completed_run_id), completed_at = sqlc.narg(completed_at),
       updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE id = sqlc.arg(id);

-- name: SetContainerState :exec
UPDATE task SET state = sqlc.arg(state), updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE id = sqlc.arg(id) AND state <> sqlc.arg(state);

-- name: ChildStateCounts :one
SELECT CAST(COUNT(*) AS INTEGER) AS total,
       CAST(COALESCE(SUM(state = 'open'), 0) AS INTEGER) AS open,
       CAST(COALESCE(SUM(state = 'in_progress'), 0) AS INTEGER) AS in_progress,
       CAST(COALESCE(SUM(state = 'in_review'), 0) AS INTEGER) AS in_review,
       CAST(COALESCE(SUM(state = 'done'), 0) AS INTEGER) AS done,
       CAST(COALESCE(SUM(state = 'cancelled'), 0) AS INTEGER) AS cancelled
FROM task WHERE parent_id = ?;

-- name: DeleteTask :exec
DELETE FROM task WHERE id = ?;

-- name: RemovePrecursor :execrows
DELETE FROM task_dependency WHERE task_id = ? AND depends_on_id = ?;

-- name: ListPrecursors :many
SELECT CAST(COALESCE(p.sqid, '') AS TEXT) AS sqid, p.title, p.state FROM task_dependency d JOIN task p ON p.id = d.depends_on_id
WHERE d.task_id = ? ORDER BY p.id;

-- name: ListDependents :many
SELECT CAST(COALESCE(t.sqid, '') AS TEXT) AS sqid, t.title, t.state FROM task_dependency d JOIN task t ON t.id = d.task_id
WHERE d.depends_on_id = ? ORDER BY t.id;

-- name: ProjectTree :many
-- The whole project, depth-first in rank order, with unmet-precursor and child counts.
WITH RECURSIVE sub (id, depth, path) AS (
    SELECT t.id, 0, t.rank FROM task t WHERE t.project_id = sqlc.arg(project_id) AND t.parent_id IS NULL
    UNION ALL
    SELECT c.id, sub.depth + 1, sub.path || ' ' || c.rank FROM task c JOIN sub ON c.parent_id = sub.id
)
SELECT task.id, CAST(COALESCE(task.sqid, '') AS TEXT) AS sqid, task.parent_id, task.title, task.state, task.rank,
       task.repo_id, task.template_id, task.created_by, task.task_type, task.target_date,
       CAST(sub.depth AS INTEGER) AS depth,
       CAST((SELECT COUNT(*) FROM task_dependency d JOIN task p ON p.id = d.depends_on_id
             WHERE d.task_id = task.id AND p.state <> 'done') AS INTEGER) AS unmet,
       CAST((SELECT COUNT(*) FROM task k WHERE k.parent_id = task.id) AS INTEGER) AS children
FROM sub JOIN task ON task.id = sub.id
ORDER BY sub.path;

-- name: AncestorChain :many
-- node first, then its parent, up to the root.
WITH RECURSIVE up (id, parent_id, lvl) AS (
    SELECT t.id, t.parent_id, 0 FROM task t WHERE t.id = sqlc.arg(node_id)
    UNION ALL
    SELECT t.id, t.parent_id, up.lvl + 1 FROM task t JOIN up ON t.id = up.parent_id
)
SELECT task.* FROM up JOIN task ON task.id = up.id ORDER BY up.lvl;

-- name: SubtreeRepoIDs :many
-- Distinct explicit repo ids anywhere in the subtree.
WITH RECURSIVE sub (id) AS (
    SELECT t.id FROM task t WHERE t.id = sqlc.arg(root_id)
    UNION ALL
    SELECT c.id FROM task c JOIN sub ON c.parent_id = sub.id
)
SELECT DISTINCT CAST(task.repo_id AS INTEGER) AS repo_id FROM sub JOIN task ON task.id = sub.id
WHERE task.repo_id IS NOT NULL;

-- name: CountOpenLeaves :one
WITH RECURSIVE sub (id) AS (
    SELECT t.id FROM task t WHERE t.id = sqlc.arg(root_id)
    UNION ALL
    SELECT c.id FROM task c JOIN sub ON c.parent_id = sub.id
)
SELECT CAST(COUNT(*) AS INTEGER) FROM sub JOIN task ON task.id = sub.id
WHERE task.state IN ('open', 'in_progress')
  AND NOT EXISTS (SELECT 1 FROM task k WHERE k.parent_id = task.id AND k.state <> 'cancelled');

-- name: CountInReviewLeaves :one
WITH RECURSIVE sub (id) AS (
    SELECT t.id FROM task t WHERE t.id = sqlc.arg(root_id)
    UNION ALL
    SELECT c.id FROM task c JOIN sub ON c.parent_id = sub.id
)
SELECT CAST(COUNT(*) AS INTEGER) FROM sub JOIN task ON task.id = sub.id
WHERE task.state = 'in_review'
  AND NOT EXISTS (SELECT 1 FROM task k WHERE k.parent_id = task.id AND k.state <> 'cancelled');

-- name: RevertInProgress :exec
-- A run ended without finishing its current leaf: put it back.
UPDATE task SET state = 'open', updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE id = ? AND state = 'in_progress';

-- name: CompletedInRun :many
SELECT CAST(COALESCE(sqid, '') AS TEXT) AS sqid, title, completion_summary FROM task
WHERE completed_run_id = ? ORDER BY completed_at;

-- name: TaskEvents :many
SELECT * FROM task_event WHERE task_sqid = ? ORDER BY id;

-- name: ListProjectRepoCount :one
SELECT CAST(COUNT(*) AS INTEGER) AS n, CAST(COALESCE(MIN(id), 0) AS INTEGER) AS only_id FROM repo WHERE project_id = ?;

-- name: ListTasks :many
SELECT sqlc.embed(t), CAST(p.slug AS TEXT) AS project_slug FROM task t JOIN project p ON p.id = t.project_id
WHERE p.archived_at IS NULL
  AND (CAST(sqlc.arg(project) AS TEXT) = '' OR p.slug = CAST(sqlc.arg(project) AS TEXT))
  AND (CAST(sqlc.arg(state) AS TEXT) = '' OR t.state = CAST(sqlc.arg(state) AS TEXT))
  AND (CAST(sqlc.arg(parent_id) AS INTEGER) = 0 OR t.parent_id = CAST(sqlc.arg(parent_id) AS INTEGER))
ORDER BY p.slug, t.id
LIMIT 500;

-- name: SetTaskIssue :exec
UPDATE task SET issue_id = sqlc.narg(issue_id) WHERE id = sqlc.arg(id);

-- name: TaskProgress :one
-- Leaves under (and including) root, not counting cancelled ones, and how many are done.
WITH RECURSIVE sub (id) AS (
    SELECT CAST(sqlc.arg(root_id) AS INTEGER)
    UNION ALL
    SELECT c.id FROM task c JOIN sub ON c.parent_id = sub.id
)
SELECT CAST(COALESCE(SUM(t.state <> 'cancelled'), 0) AS INTEGER) AS total,
       CAST(COALESCE(SUM(t.state = 'done'), 0) AS INTEGER) AS done
FROM task t JOIN sub ON t.id = sub.id
WHERE NOT EXISTS (SELECT 1 FROM task k WHERE k.parent_id = t.id);
