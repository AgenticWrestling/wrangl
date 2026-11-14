-- name: ListActiveRuns :many
SELECT r.id, r.state, r.kind, r.status_text, r.queued_at, r.started_at,
       t.title AS task_title, p.slug AS project_slug, rn.name AS runner_name
FROM run r
JOIN task t ON t.id = r.task_id
JOIN project p ON p.id = t.project_id
LEFT JOIN runner rn ON rn.id = r.runner_id
WHERE p.archived_at IS NULL AND r.state NOT IN ('succeeded', 'failed', 'blocked', 'cancelled')
ORDER BY p.priority DESC, r.queued_at;

-- name: CreateRun :one
INSERT INTO run (task_id, kind, attempt) VALUES (?, ?, ?)
RETURNING *;

-- name: QueueRun :one
INSERT INTO run (task_id, kind, attempt, template_id, repo_id, branch, session_uuid)
VALUES (sqlc.arg(task_id), sqlc.arg(kind), sqlc.arg(attempt), sqlc.arg(template_id), sqlc.arg(repo_id),
        sqlc.arg(branch), sqlc.arg(session_uuid))
RETURNING *;

-- name: CountRunsForTask :one
SELECT CAST(COUNT(*) AS INTEGER) FROM run WHERE task_id = ?;

-- name: GetRun :one
SELECT * FROM run WHERE id = ?;

-- name: ListRuns :many
-- Filter params are CAST so sqlc types them (quirk 7).
SELECT sqlc.embed(r), CAST(COALESCE(t.sqid, '') AS TEXT) AS task_sqid, CAST(t.title AS TEXT) AS task_title,
       CAST(p.slug AS TEXT) AS project_slug, CAST(COALESCE(rn.name, '') AS TEXT) AS runner_name,
       CAST(COALESCE(ct.sqid, '') AS TEXT) AS current_sqid
FROM run r
JOIN task t ON t.id = r.task_id
JOIN project p ON p.id = t.project_id
LEFT JOIN runner rn ON rn.id = r.runner_id
LEFT JOIN task ct ON ct.id = r.current_task_id
WHERE p.archived_at IS NULL
  AND (CAST(sqlc.arg(state) AS TEXT) = '' OR r.state = CAST(sqlc.arg(state) AS TEXT)
       OR (CAST(sqlc.arg(state) AS TEXT) = 'active' AND r.state NOT IN ('succeeded', 'failed', 'blocked', 'cancelled')))
  AND (CAST(sqlc.arg(runner) AS TEXT) = '' OR rn.name = CAST(sqlc.arg(runner) AS TEXT))
  AND (CAST(sqlc.arg(project) AS TEXT) = '' OR p.slug = CAST(sqlc.arg(project) AS TEXT))
ORDER BY r.id DESC
LIMIT sqlc.arg(lim);

-- name: QueuedRunsForAssignment :many
-- Smart Merge (resolve) runs are short and unblock a PR: they are offered a slot ahead of everything else.
SELECT r.id, r.task_id, r.template_id, r.repo_id, r.branch, r.session_uuid, r.attempt, r.kind, r.continue_branch,
       CAST(COALESCE(t.sqid, '') AS TEXT) AS task_sqid
FROM run r JOIN task t ON t.id = r.task_id JOIN project p ON p.id = t.project_id
WHERE r.state = 'queued' AND p.archived_at IS NULL
ORDER BY (r.kind = 'resolve') DESC, p.priority DESC, (r.queue_rank IS NULL), r.queue_rank, t.rank, r.queued_at;

-- name: ClaimIdleRunner :one
-- A runner on the template with a free session slot, at the template's current version and not
-- cordoned, draining or upgrading. Busy runners with room are packed first (fewer instances, so
-- less cost), then the longest-idle one. The slot is not counted until AssignRun marks the run
-- assigned, which the scheduler does right after (it assigns one run at a time).
UPDATE runner SET state = 'busy', idle_since = NULL
WHERE id = (
    SELECT n.id FROM runner n JOIN runner_template t ON t.id = n.template_id
    WHERE n.template_id = sqlc.arg(template_id) AND n.state IN ('idle', 'busy') AND n.cordoned = 0
      AND n.drain_requested = 0
      AND n.template_version = t.version AND n.upgrade_sha = ''
      AND NOT EXISTS (SELECT 1 FROM runner_capacity g WHERE g.runner_id = n.id AND g.closed_by_guard = 1)
      AND (SELECT COUNT(*) FROM run r WHERE r.runner_id = n.id
           AND r.state IN ('assigned', 'preparing', 'running', 'needs_input', 'pushing', 'cancelling')) < t.max_sessions
    ORDER BY (n.state = 'busy') DESC, n.idle_since
    LIMIT 1)
RETURNING id, name;

-- name: FreeRunnerSlot :one
-- 1 when ClaimIdleRunner would find a runner for the template (the same conditions).
SELECT CAST(EXISTS (
    SELECT 1 FROM runner n JOIN runner_template t ON t.id = n.template_id
    WHERE n.template_id = sqlc.arg(template_id) AND n.state IN ('idle', 'busy') AND n.cordoned = 0
      AND n.drain_requested = 0
      AND n.template_version = t.version AND n.upgrade_sha = ''
      AND NOT EXISTS (SELECT 1 FROM runner_capacity g WHERE g.runner_id = n.id AND g.closed_by_guard = 1)
      AND (SELECT COUNT(*) FROM run r WHERE r.runner_id = n.id
           AND r.state IN ('assigned', 'preparing', 'running', 'needs_input', 'pushing', 'cancelling')) < t.max_sessions
) AS INTEGER) AS free;

-- name: AssignRun :execrows
UPDATE run SET state = 'assigned', runner_id = sqlc.arg(runner_id), session_name = sqlc.arg(session_name),
    status_text = CASE WHEN blocked_reason <> '' AND status_text = blocked_reason THEN '' ELSE status_text END,
    blocked_since = NULL, blocked_reason = ''
WHERE id = sqlc.arg(id) AND state = 'queued';

-- name: UnassignRun :exec
UPDATE run SET state = 'queued', runner_id = NULL WHERE id = ? AND state = 'assigned';

-- name: ReleaseRunner :exec
-- Back to idle (with a fresh idle clock) unless terminated, and only when no other session is
-- still running on it.
UPDATE runner SET state = 'idle', idle_since = sqlc.arg(now)
WHERE runner.id = sqlc.arg(id) AND runner.state IN ('busy', 'offline')
  AND NOT EXISTS (SELECT 1 FROM run r WHERE r.runner_id = runner.id
                  AND r.state IN ('assigned', 'preparing', 'running', 'needs_input', 'pushing', 'cancelling'));

-- name: UpdateRunProgress :exec
UPDATE run
SET state = sqlc.arg(state),
    started_at = COALESCE(started_at, sqlc.narg(started_at)),  -- caller decides (quirk 6)
    base_sha = CASE WHEN CAST(sqlc.arg(base_sha) AS TEXT) <> '' THEN CAST(sqlc.arg(base_sha) AS TEXT) ELSE base_sha END,
    head_sha = CASE WHEN CAST(sqlc.arg(head_sha) AS TEXT) <> '' THEN CAST(sqlc.arg(head_sha) AS TEXT) ELSE head_sha END,
    status_text = sqlc.arg(status_text)
WHERE id = sqlc.arg(id) AND state NOT IN ('succeeded', 'failed', 'blocked', 'cancelled');

-- name: FinishRun :exec
UPDATE run
SET state = sqlc.arg(state), finished_at = sqlc.arg(now), exit_code = sqlc.narg(exit_code),
    error = sqlc.arg(error), status_text = sqlc.arg(status_text), pushed = sqlc.arg(pushed),
    head_sha = CASE WHEN CAST(sqlc.arg(head_sha) AS TEXT) <> '' THEN CAST(sqlc.arg(head_sha) AS TEXT) ELSE head_sha END,
    current_task_id = NULL
WHERE id = sqlc.arg(id) AND state NOT IN ('succeeded', 'failed', 'blocked', 'cancelled');

-- name: ActiveRunsOfProject :many
-- Runs of a project that are past the queue (assigned or running in any way); queued runs are
-- handled separately (CancelQueuedRunsOfProject).
SELECT r.id, r.state, r.kind, CAST(COALESCE(t.sqid, '') AS TEXT) AS task_sqid, CAST(t.title AS TEXT) AS task_title
FROM run r JOIN task t ON t.id = r.task_id
WHERE t.project_id = ? AND r.state IN ('assigned', 'preparing', 'running', 'needs_input', 'pushing', 'cancelling')
ORDER BY r.id;

-- name: CancelQueuedRunsOfProject :many
UPDATE run SET state = 'cancelled', finished_at = sqlc.arg(now), error = sqlc.arg(reason)
WHERE state = 'queued' AND task_id IN (SELECT t.id FROM task t WHERE t.project_id = sqlc.arg(project_id))
RETURNING id, task_id;

-- name: CancelQueuedRun :execrows
UPDATE run SET state = 'cancelled', finished_at = sqlc.arg(now), error = 'cancelled before assignment'
WHERE id = sqlc.arg(id) AND state = 'queued';

-- name: MarkRunCancelling :execrows
UPDATE run SET state = 'cancelling'
WHERE id = ? AND state IN ('assigned', 'preparing', 'running', 'needs_input', 'pushing');

-- name: SetRunCurrentTask :exec
UPDATE run SET current_task_id = sqlc.narg(task_id) WHERE id = sqlc.arg(id);

-- name: ActiveRunForRunner :one
SELECT * FROM run
WHERE runner_id = ? AND state IN ('assigned', 'preparing', 'running', 'needs_input', 'pushing', 'cancelling')
ORDER BY id DESC LIMIT 1;

-- name: CountActiveRunsForRunner :one
SELECT CAST(COUNT(*) AS INTEGER) FROM run
WHERE runner_id = ? AND state IN ('assigned', 'preparing', 'running', 'needs_input', 'pushing', 'cancelling');

-- name: RunsOnLostRunners :many
-- Active runs whose runner is terminated, or offline since before the cutoff.
SELECT r.id, r.runner_id, r.current_task_id FROM run r JOIN runner n ON n.id = r.runner_id
WHERE r.state IN ('assigned', 'preparing', 'running', 'needs_input', 'pushing', 'cancelling')
  AND (n.state = 'terminated' OR (n.state = 'offline' AND n.offline_since <= sqlc.arg(cutoff)));

-- name: RunsForTask :many
SELECT r.id, r.state, r.kind, r.attempt, r.branch, r.pushed, r.head_sha, r.queued_at, r.finished_at,
       CAST(COALESCE(n.name, '') AS TEXT) AS runner_name
FROM run r LEFT JOIN runner n ON n.id = r.runner_id
WHERE r.task_id = ? ORDER BY r.id DESC;

-- name: SetRunState :exec
-- running <-> needs_input while an agent waits for an answer.
UPDATE run SET state = sqlc.arg(state) WHERE id = sqlc.arg(id) AND state = sqlc.arg(from_state);

-- name: ReassertNeedsInput :exec
-- A progress event (JetStream) can arrive after the question that already parked the run
-- (core NATS) and would put it back to running: an open question means needs_input.
UPDATE run SET state = 'needs_input'
WHERE run.id = ? AND run.state IN ('assigned', 'preparing', 'running')
  AND EXISTS (SELECT 1 FROM question q WHERE q.run_id = run.id AND q.state = 'open');

-- name: SetRunNeedsInput :exec
-- An agent asked a question. The run may still be preparing (the runner's state events travel
-- by a different route than the agent's question), so any live state moves to needs_input.
UPDATE run SET state = 'needs_input' WHERE id = ? AND state IN ('assigned', 'preparing', 'running');

-- name: SetRunStatus :exec
UPDATE run SET status_text = ? WHERE id = ?;

-- name: InsertHookEvent :exec
INSERT INTO hook_event (run_id, seq, ts, name, payload_json) VALUES (?, ?, ?, ?, ?)
ON CONFLICT (run_id, seq) DO NOTHING;

-- name: RunHookEvents :many
SELECT * FROM hook_event WHERE run_id = ? ORDER BY seq;

-- name: SetRunGuidance :exec
UPDATE run SET guidance = ? WHERE id = ?;

-- name: CountLostRunsForTask :one
-- Runs of a task that failed because their runner was lost.
SELECT CAST(COUNT(*) AS INTEGER) FROM run WHERE task_id = ? AND kind = 'work' AND state = 'failed' AND error = 'runner lost';

-- name: StuckRuns :many
-- Running runs with no log line, hook event or start since the cutoff, that the owner has not
-- been asked about since the cutoff either.
SELECT r.id, r.task_id, r.current_task_id FROM run r
WHERE r.state = 'running'
  AND MAX(
        COALESCE((SELECT MAX(ts) FROM log_line WHERE run_id = r.id), ''),
        COALESCE((SELECT MAX(ts) FROM hook_event WHERE run_id = r.id), ''),
        COALESCE(r.started_at, r.queued_at)) <= sqlc.arg(cutoff)
  AND NOT EXISTS (SELECT 1 FROM question q WHERE q.run_id = r.id AND q.kind = 'approval'
                  AND json_extract(q.payload_json, '$.op') = 'cancel_run' AND q.created_at > sqlc.arg(cutoff));

-- name: SetRunQueueRank :execrows
UPDATE run SET queue_rank = sqlc.arg(queue_rank) WHERE id = sqlc.arg(id) AND state = 'queued';

-- name: SetRunBlocked :execrows
-- Say why a queued run waits. Written only when the text changes (so a steady wait costs no
-- writes); blocked_since keeps the first time the run was seen blocked.
UPDATE run SET status_text = sqlc.arg(reason), blocked_reason = sqlc.arg(reason),
    blocked_since = COALESCE(blocked_since, sqlc.arg(now))
WHERE id = sqlc.arg(id) AND state = 'queued' AND (blocked_reason <> sqlc.arg(reason) OR status_text <> sqlc.arg(reason));

-- name: TemplateRunnerCounts :one
-- What the template's runners are doing, for the waiting reason: still coming up, usable but
-- full, and live but not claimable (cordoned, draining, behind on version or upgrading).
SELECT
  CAST(COALESCE(SUM(n.state IN ('launching', 'enrolling', 'provisioning')), 0) AS INTEGER) AS launching,
  CAST(COALESCE(SUM(n.state IN ('idle', 'busy') AND n.cordoned = 0 AND n.drain_requested = 0
      AND n.template_version = t.version AND n.upgrade_sha = ''), 0) AS INTEGER) AS usable,
  CAST(COALESCE(SUM(n.state IN ('idle', 'busy') AND NOT (n.cordoned = 0 AND n.drain_requested = 0
      AND n.template_version = t.version AND n.upgrade_sha = '')), 0) AS INTEGER) AS unavailable
FROM runner n JOIN runner_template t ON t.id = n.template_id
WHERE n.template_id = sqlc.arg(template_id);

-- name: SetTaskTemplate :exec
UPDATE task SET template_id = sqlc.narg(template_id) WHERE id = sqlc.arg(id);

-- name: SetQueuedRunTemplate :exec
UPDATE run SET template_id = sqlc.arg(template_id) WHERE id = sqlc.arg(id) AND state = 'queued';

-- name: SetRunNeed :exec
UPDATE run SET need_vcpu = sqlc.arg(need_vcpu), need_ram_mib = sqlc.arg(need_ram_mib), need_disk_gib = sqlc.arg(need_disk_gib),
    need_source = sqlc.arg(need_source) WHERE id = sqlc.arg(id);

-- name: SetRunRepo :execrows
-- Give a queued run that waits for a repo its repo, and drop the "needs a repo" reason.
UPDATE run SET repo_id = sqlc.arg(repo_id),
    status_text = CASE WHEN blocked_reason <> '' AND status_text = blocked_reason THEN '' ELSE status_text END,
    blocked_since = NULL, blocked_reason = ''
WHERE id = sqlc.arg(id) AND state = 'queued' AND repo_id IS NULL;

-- name: SetRunWarning :exec
INSERT INTO run_warning (run_id, text) VALUES (sqlc.arg(run_id), sqlc.arg(text))
ON CONFLICT (run_id) DO UPDATE SET text = excluded.text;

-- name: GetRunWarning :one
SELECT text FROM run_warning WHERE run_id = ?;
