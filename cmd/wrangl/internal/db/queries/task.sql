-- NOTE: always qualify the rank column (task.rank, t.rank). sqlc's sqlite grammar treats a
-- bare `rank` as the RANK() window-function keyword and fails to parse; SQLite itself is fine.
-- Other sqlc workarounds in this file are documented in the M1 plan ("Verified up front").

-- name: CreateTask :one
INSERT INTO task (project_id, parent_id, rank, repo_id, template_id, title, prompt, created_by)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
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
WHERE r.state NOT IN ('succeeded', 'failed', 'cancelled');

-- name: AddPrecursor :exec
INSERT INTO task_dependency (task_id, depends_on_id, created_by) VALUES (?, ?, ?);

-- name: CompleteTask :exec
-- Agent marks a leaf complete: in_review until its PR merges.
UPDATE task SET state = 'in_review', completion_summary = ?, completed_run_id = ?, completed_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now'),
       updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE id = ?;

-- name: MarkRunTasksDone :exec
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
