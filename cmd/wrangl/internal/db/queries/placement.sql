-- name: GetPlacementConfig :one
SELECT need_vcpu, need_ram_mib, need_disk_gib FROM placement_config WHERE id = 1;

-- name: SetPlacementNeeds :exec
UPDATE placement_config SET need_vcpu = ?, need_ram_mib = ?, need_disk_gib = ? WHERE id = 1;

-- name: GetProjectNeed :one
SELECT need_vcpu, need_ram_mib, need_disk_gib FROM project_need WHERE project_id = ?;

-- name: SetProjectNeed :exec
INSERT INTO project_need (project_id, need_vcpu, need_ram_mib, need_disk_gib) VALUES (?, ?, ?, ?)
ON CONFLICT (project_id) DO UPDATE SET need_vcpu = excluded.need_vcpu, need_ram_mib = excluded.need_ram_mib, need_disk_gib = excluded.need_disk_gib;

-- name: GetTaskNeed :one
SELECT need_vcpu, need_ram_mib, need_disk_gib FROM task_need WHERE task_id = ?;

-- name: SetTaskNeed :exec
INSERT INTO task_need (task_id, need_vcpu, need_ram_mib, need_disk_gib) VALUES (?, ?, ?, ?)
ON CONFLICT (task_id) DO UPDATE SET need_vcpu = excluded.need_vcpu, need_ram_mib = excluded.need_ram_mib, need_disk_gib = excluded.need_disk_gib;

-- name: InitRunnerCapacity :exec
-- At enrolment: remember the template's session ceiling; an existing row (explicit capacity) stays.
INSERT INTO runner_capacity (runner_id, max_sessions) VALUES (?, ?) ON CONFLICT (runner_id) DO NOTHING;

-- name: SetRunnerCapacity :exec
INSERT INTO runner_capacity (runner_id, cap_vcpu, cap_ram_mib, cap_disk_gib) VALUES (?, ?, ?, ?)
ON CONFLICT (runner_id) DO UPDATE SET cap_vcpu = excluded.cap_vcpu, cap_ram_mib = excluded.cap_ram_mib, cap_disk_gib = excluded.cap_disk_gib;

-- name: GetRunnerCapacity :one
SELECT cap_vcpu, cap_ram_mib, cap_disk_gib, max_sessions FROM runner_capacity WHERE runner_id = ?;

-- name: SumActiveNeeds :one
-- What the runner's active runs have reserved.
SELECT CAST(COALESCE(SUM(need_vcpu), 0) AS INTEGER) AS vcpu, CAST(COALESCE(SUM(need_ram_mib), 0) AS INTEGER) AS ram_mib,
       CAST(COALESCE(SUM(need_disk_gib), 0) AS INTEGER) AS disk_gib
FROM run WHERE runner_id = ? AND state IN ('assigned', 'preparing', 'running', 'needs_input', 'pushing', 'cancelling');

-- name: ClaimableRunners :many
-- Runners of the template a run could be placed on, before the resource and session checks (those are
-- the placer's and ClaimRunner's): idle or busy, not cordoned or draining, at the template's current
-- version, no upgrade pending.
SELECT n.id, n.name, n.state, n.idle_since, n.launched, n.vcpu, n.ram_mib, n.disk_gib, t.max_sessions
FROM runner n JOIN runner_template t ON t.id = n.template_id
WHERE n.template_id = ? AND n.state IN ('idle', 'busy') AND n.cordoned = 0 AND n.drain_requested = 0
  AND n.template_version = t.version AND n.upgrade_sha = ''
  AND NOT EXISTS (SELECT 1 FROM runner_capacity g WHERE g.runner_id = n.id AND g.closed_by_guard = 1);

-- name: ClaimRunner :one
-- Atomically claim one runner for a run needing need_*: the claimable conditions, the session
-- ceiling (the template's max_sessions) and, for each resource the run needs (non-zero), enough free
-- capacity are all re-evaluated in the WHERE, so a concurrent change loses the race (no rows). Capacity
-- is the owner's explicit value or derived from the runner's facts (see placement.DeriveCapacity).
UPDATE runner SET state = 'busy', idle_since = NULL
WHERE runner.id = sqlc.arg(id) AND runner.state IN ('idle', 'busy') AND runner.cordoned = 0 AND runner.drain_requested = 0
  AND runner.upgrade_sha = ''
  AND NOT EXISTS (SELECT 1 FROM runner_capacity g WHERE g.runner_id = runner.id AND g.closed_by_guard = 1)
  AND runner.template_version = (SELECT t.version FROM runner_template t WHERE t.id = runner.template_id)
  AND (SELECT COUNT(*) FROM run r WHERE r.runner_id = runner.id
       AND r.state IN ('assigned', 'preparing', 'running', 'needs_input', 'pushing', 'cancelling'))
      < (SELECT t.max_sessions FROM runner_template t WHERE t.id = runner.template_id)
  AND (sqlc.arg(need_vcpu) = 0 OR sqlc.arg(need_vcpu) <=
       COALESCE((SELECT c.cap_vcpu FROM runner_capacity c WHERE c.runner_id = runner.id),
                CASE WHEN runner.launched = 1 THEN runner.vcpu ELSE MIN(6, runner.vcpu * 3 / 4) END)
       - (SELECT COALESCE(SUM(r.need_vcpu), 0) FROM run r WHERE r.runner_id = runner.id
          AND r.state IN ('assigned', 'preparing', 'running', 'needs_input', 'pushing', 'cancelling')))
  AND (sqlc.arg(need_ram_mib) = 0 OR sqlc.arg(need_ram_mib) <=
       COALESCE((SELECT c.cap_ram_mib FROM runner_capacity c WHERE c.runner_id = runner.id),
                CASE WHEN runner.launched = 1 THEN MAX(runner.ram_mib - 1024, 0) ELSE MIN(12288, runner.ram_mib * 3 / 4) END)
       - (SELECT COALESCE(SUM(r.need_ram_mib), 0) FROM run r WHERE r.runner_id = runner.id
          AND r.state IN ('assigned', 'preparing', 'running', 'needs_input', 'pushing', 'cancelling')))
  AND (sqlc.arg(need_disk_gib) = 0 OR sqlc.arg(need_disk_gib) <=
       COALESCE((SELECT c.cap_disk_gib FROM runner_capacity c WHERE c.runner_id = runner.id),
                CASE WHEN runner.launched = 1 THEN MAX(runner.disk_gib - 8, 0) ELSE MIN(20, runner.disk_gib * 7 / 10) END)
       - (SELECT COALESCE(SUM(r.need_disk_gib), 0) FROM run r WHERE r.runner_id = runner.id
          AND r.state IN ('assigned', 'preparing', 'running', 'needs_input', 'pushing', 'cancelling')))
RETURNING id, name;

-- name: QueuedRunsForPlacement :many
-- Queued runs in assignment order with the template their demand is counted on and their need snapshot.
SELECT r.id, k.project_id, CAST(COALESCE(r.template_id, k.template_id, p.default_template_id) AS INTEGER) AS template_id,
       r.need_vcpu, r.need_ram_mib, r.need_disk_gib
FROM run r JOIN task k ON k.id = r.task_id JOIN project p ON p.id = k.project_id
WHERE r.state = 'queued' AND p.archived_at IS NULL
ORDER BY p.priority DESC, (r.queue_rank IS NULL), r.queue_rank, k.rank, r.queued_at;

-- name: GetBudget :one
SELECT budget_hourly_usd, budget_warn_pct, budget_warned FROM placement_config WHERE id = 1;

-- name: SetBudget :exec
UPDATE placement_config SET budget_hourly_usd = ?, budget_warned = 0 WHERE id = 1;

-- name: SetBudgetWarned :exec
UPDATE placement_config SET budget_warned = ? WHERE id = 1;

-- name: SetRunnerPrice :exec
INSERT INTO runner_capacity (runner_id, hourly_usd) VALUES (?, ?)
ON CONFLICT (runner_id) DO UPDATE SET hourly_usd = excluded.hourly_usd;

-- name: CommittedHourlyUSD :one
-- What the runners that still bill cost per hour: every runner not terminated (launching, draining and
-- offline ones too).
SELECT CAST(COALESCE(SUM(c.hourly_usd), 0) AS REAL) FROM runner n JOIN runner_capacity c ON c.runner_id = n.id
WHERE n.state <> 'terminated';

-- name: ClearBudgetBlock :exec
-- Drop a budget waiting reason the budget no longer explains.
UPDATE run SET blocked_reason = '', status_text = CASE WHEN status_text = blocked_reason THEN '' ELSE status_text END
WHERE id = ? AND state = 'queued' AND blocked_reason LIKE 'waiting: hourly budget%';

-- name: GetRunnerGuard :one
SELECT guard_load1, guard_mem_pct, guard_disk_pct, closed_by_guard, guard_breaches, guard_oks, guard_reason
FROM runner_capacity WHERE runner_id = ?;

-- name: SetRunnerGuardThresholds :exec
INSERT INTO runner_capacity (runner_id, guard_load1, guard_mem_pct, guard_disk_pct) VALUES (?, ?, ?, ?)
ON CONFLICT (runner_id) DO UPDATE SET guard_load1 = excluded.guard_load1, guard_mem_pct = excluded.guard_mem_pct, guard_disk_pct = excluded.guard_disk_pct;

-- name: SetRunnerGuardState :exec
UPDATE runner_capacity SET closed_by_guard = ?, guard_breaches = ?, guard_oks = ?, guard_reason = ? WHERE runner_id = ?;
