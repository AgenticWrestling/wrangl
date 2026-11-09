-- NEVER mix bare ? with sqlc.arg()/sqlc.narg() in one query (see EnrollRunner).
-- Timestamps are passed in (RFC 3339 UTC) rather than taken from SQLite's clock so the
-- reaper and offline sweep can be tested with a fake clock.

-- name: CreateLaunchedRunner :one
INSERT INTO runner (name, template_id, instance_id, instance_type, az, arch, private_ip,
                    state, launched, launched_at)
VALUES (?, ?, ?, ?, ?, ?, ?, 'launching', 1, ?)
RETURNING *;

-- name: CreateAdoptedRunner :one
INSERT INTO runner (name, template_id, state, launched) VALUES (?, ?, 'enrolling', 0)
RETURNING *;

-- name: GetRunner :one
SELECT * FROM runner WHERE id = ?;

-- name: GetRunnerByName :one
SELECT * FROM runner WHERE name = ?;

-- name: ListRunners :many
SELECT sqlc.embed(r), CAST(t.name AS TEXT) AS template_name
FROM runner r JOIN runner_template t ON t.id = r.template_id
WHERE r.state <> 'terminated' OR sqlc.arg(include_terminated) = 1
ORDER BY r.name;

-- name: EnrollRunner :exec
-- All parameters are named: mixing bare ? with sqlc.arg/narg makes sqlc emit ?N for the
-- named ones while SQLite keeps counting the bare ones, so arguments shift (quirk 5).
UPDATE runner
SET nkey_public = sqlc.arg(nkey_public), instance_id = COALESCE(instance_id, sqlc.narg(instance_id)),
    arch = sqlc.arg(arch), vcpu = sqlc.arg(vcpu), ram_mib = sqlc.arg(ram_mib),
    disk_gib = sqlc.arg(disk_gib), private_ip = sqlc.arg(private_ip),
    state = 'enrolling', enrolled_at = sqlc.arg(enrolled_at)
WHERE id = sqlc.arg(id);

-- name: RecordHeartbeat :one
-- First heartbeat after enrolment (or after being offline) makes the runner idle.
-- Busy/cordoned/draining runners keep their state; one returning from offline with live
-- sessions comes back busy, not idle.
-- Returning from offline keeps the existing idle_since: a network blip must not restart
-- the idle-shutdown clock (found by the M2 e2e test). A runner that went offline while
-- busy has idle_since NULL (cleared when work is assigned, M4) and starts afresh.
UPDATE runner
SET last_heartbeat_at = sqlc.arg(now), last_metrics_json = sqlc.arg(metrics),
    offline_since = NULL,
    idle_since = CASE WHEN state IN ('enrolling', 'provisioning') THEN sqlc.arg(now)
                      WHEN state = 'offline' THEN CASE WHEN EXISTS (SELECT 1 FROM run r WHERE r.runner_id = runner.id
                                                    AND r.state IN ('assigned', 'preparing', 'running', 'needs_input', 'pushing', 'cancelling'))
                                         THEN NULL ELSE COALESCE(idle_since, sqlc.arg(now)) END
                      ELSE idle_since END,
    state = CASE WHEN state = 'offline' AND EXISTS (SELECT 1 FROM run r WHERE r.runner_id = runner.id
                                                    AND r.state IN ('assigned', 'preparing', 'running', 'needs_input', 'pushing', 'cancelling'))
                      THEN 'busy' -- sessions survived the blip: not idle until every one ends
                 WHEN state IN ('enrolling', 'provisioning', 'offline') THEN 'idle'
                 ELSE state END
WHERE id = sqlc.arg(id) AND state <> 'terminated'
RETURNING state;

-- name: MarkOffline :many
-- Runners whose last heartbeat is older than the cutoff.
UPDATE runner
SET state = 'offline', offline_since = sqlc.arg(now)
WHERE state IN ('idle', 'busy', 'cordoned', 'draining', 'enrolling', 'provisioning')
  AND last_heartbeat_at IS NOT NULL AND last_heartbeat_at < sqlc.arg(cutoff)
RETURNING id, name;

-- name: ReapCandidates :many
-- Controller-launched runners to terminate: idle or offline past the template's idle
-- timeout, or stuck launching/enrolling past the enrol deadline. Comparisons are on
-- RFC 3339 UTC strings, which sort chronologically.
SELECT r.id, r.name, r.instance_id, r.state, CAST(t.idle_timeout_s AS INTEGER) AS idle_timeout_s
FROM runner r JOIN runner_template t ON t.id = r.template_id
WHERE r.launched = 1 AND r.instance_id IS NOT NULL AND (
       (r.state = 'idle'    AND r.idle_since    <= strftime('%Y-%m-%dT%H:%M:%fZ', CAST(sqlc.arg(now) AS TEXT), '-' || t.idle_timeout_s || ' seconds'))
    OR (r.state = 'offline' AND r.offline_since <= strftime('%Y-%m-%dT%H:%M:%fZ', CAST(sqlc.arg(now) AS TEXT), '-' || t.idle_timeout_s || ' seconds'))
    OR (r.state IN ('launching', 'enrolling') AND r.launched_at <= sqlc.arg(enroll_deadline))
);

-- name: MarkTerminated :exec
UPDATE runner SET state = 'terminated', terminated_at = ? WHERE id = ?;

-- name: UpdateRunnerState :exec
UPDATE runner SET state = ? WHERE id = ?;

-- name: StartProvisioning :one
-- Claim a runner for a template apply. Succeeds only from a state where applying is
-- allowed, and for a runner already provisioning only after the resend grace period.
UPDATE runner
SET state = 'provisioning', provisioning_since = sqlc.arg(now)
WHERE id = sqlc.arg(id)
  AND (state IN ('enrolling', 'idle')
       OR (state = 'provisioning' AND (provisioning_since IS NULL OR provisioning_since <= sqlc.arg(resend_before))))
RETURNING id;

-- name: TemplateApplied :exec
-- Successful apply: record the version; a provisioning runner becomes idle with a
-- fresh idle clock.
UPDATE runner
SET template_version = sqlc.arg(version), template_log = sqlc.arg(log), last_error = '',
    provisioning_since = NULL,
    idle_since = CASE WHEN state = 'provisioning' THEN sqlc.arg(now) ELSE idle_since END,
    state = CASE WHEN state = 'provisioning' THEN 'idle' ELSE state END
WHERE id = sqlc.arg(id) AND state <> 'terminated';

-- name: TemplateFailed :exec
-- Failed apply: keep the box for debugging but cordon it; it is reaped after the idle
-- timeout like any idle runner.
UPDATE runner
SET last_error = sqlc.arg(error), template_log = sqlc.arg(log), cordoned = 1,
    provisioning_since = NULL,
    idle_since = CASE WHEN state = 'provisioning' THEN sqlc.arg(now) ELSE idle_since END,
    state = CASE WHEN state = 'provisioning' THEN 'idle' ELSE state END
WHERE id = sqlc.arg(id) AND state <> 'terminated';

-- name: SetCordon :execrows
UPDATE runner SET cordoned = sqlc.arg(cordoned),
       drain_requested = CASE WHEN sqlc.arg(cordoned) = 0 THEN 0 ELSE drain_requested END
WHERE name = sqlc.arg(name) AND state <> 'terminated';

-- name: RequestDrain :execrows
UPDATE runner SET cordoned = 1, drain_requested = 1 WHERE name = sqlc.arg(name) AND state <> 'terminated';

-- name: DrainCandidates :many
-- Drained, launched runners with nothing to do: terminate now.
SELECT id, name, instance_id FROM runner
WHERE drain_requested = 1 AND launched = 1 AND instance_id IS NOT NULL
  AND state IN ('idle', 'offline', 'enrolling', 'provisioning');

-- name: TemplateDemand :many
-- Per template: queued runs (demand), free session slots on uncordoned runners that are or will
-- be available (supply; a runner still coming up counts all its slots; an idle or busy runner
-- behind on the template version is not supply, ClaimIdleRunner refuses it), and live runners
-- (counted against max_runners).
SELECT t.id, t.name, t.version, t.failed_version, t.max_runners, t.max_sessions,
       CAST((SELECT COUNT(*) FROM run r JOIN task k ON k.id = r.task_id
             JOIN project p ON p.id = k.project_id
             WHERE r.state = 'queued' AND p.archived_at IS NULL
               AND (COALESCE(r.template_id, k.template_id, p.default_template_id) = t.id
                    OR COALESCE(r.template_id, k.template_id, p.default_template_id) IN
                       (SELECT c.template_id FROM template_connection c WHERE c.mode = 'remote' AND c.driver_template_id = t.id))) AS INTEGER) AS demand,
       CAST((SELECT COALESCE(SUM(MAX(t.max_sessions - (SELECT COUNT(*) FROM run r WHERE r.runner_id = n.id
                          AND r.state IN ('assigned', 'preparing', 'running', 'needs_input', 'pushing', 'cancelling')), 0)), 0)
             FROM runner n
             WHERE n.template_id = t.id AND n.cordoned = 0 AND n.drain_requested = 0 AND n.upgrade_sha = ''
               AND n.state IN ('launching', 'enrolling', 'provisioning', 'idle', 'busy')
               AND (n.state NOT IN ('idle', 'busy') OR n.template_version = t.version)) AS INTEGER) AS supply,
       CAST((SELECT COUNT(*) FROM runner n
             WHERE n.template_id = t.id AND n.state <> 'terminated') AS INTEGER) AS live
FROM runner_template t
ORDER BY t.name;

-- name: SetRunnerBinary :exec
UPDATE runner SET binary_sha = sqlc.arg(binary_sha) WHERE id = sqlc.arg(id) AND binary_sha <> sqlc.arg(binary_sha);

-- name: RequestUpgrade :exec
-- Marks the runner for an upgrade: ClaimIdleRunner skips it until the upgrade lands.
UPDATE runner SET upgrade_sha = sqlc.arg(upgrade_sha), upgrade_sent_at = NULL WHERE id = sqlc.arg(id);

-- name: MarkUpgradeSent :execrows
-- Claims the right to send cmd.upgrade: once, then again only after resend_before.
UPDATE runner SET upgrade_sent_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND state = 'idle' AND upgrade_sha <> ''
  AND (upgrade_sent_at IS NULL OR upgrade_sent_at < sqlc.arg(resend_before));

-- name: ClearUpgrade :exec
UPDATE runner SET upgrade_sha = '', upgrade_sent_at = NULL WHERE id = sqlc.arg(id);

-- name: CountFailedEnrolments :one
-- Controller-launched runners of a template that never enrolled and were terminated since a time.
SELECT CAST(COUNT(*) AS INTEGER) FROM runner
WHERE template_id = ? AND launched = 1 AND nkey_public IS NULL AND state = 'terminated' AND terminated_at >= ?;

-- name: SetRunnerLaunchedForProject :exec
UPDATE runner SET launched_for_project = sqlc.narg(project_id) WHERE id = sqlc.arg(id);

-- name: UpsertProjectTemplateLimit :exec
INSERT INTO project_template_limit (project_id, template_id, max_instances)
VALUES (sqlc.arg(project_id), sqlc.arg(template_id), sqlc.arg(max_instances))
ON CONFLICT (project_id, template_id) DO UPDATE SET max_instances = excluded.max_instances;

-- name: ListProjectTemplateLimits :many
-- Every template with the project's stored cap (NULL: default rule applies).
SELECT t.id AS template_id, t.name AS template_name, t.max_runners,
       l.max_instances AS stored, CAST(COALESCE(p.default_template_id = t.id, 0) AS INTEGER) AS is_default
FROM project p CROSS JOIN runner_template t
LEFT JOIN project_template_limit l ON l.project_id = p.id AND l.template_id = t.id
WHERE p.id = sqlc.arg(project_id)
ORDER BY t.name;

-- name: ProjectTemplateDemand :many
-- Queued runs per project and template, the runners already launched for that project on the
-- template, and its stored cap.
SELECT p.id AS project_id, CAST(COALESCE(r.template_id, k.template_id, p.default_template_id) AS INTEGER) AS template_id,
       CAST(COUNT(*) AS INTEGER) AS demand,
       CAST((SELECT COUNT(*) FROM runner n WHERE n.launched_for_project = p.id
             AND n.template_id = COALESCE(r.template_id, k.template_id, p.default_template_id)
             AND n.state <> 'terminated') AS INTEGER) AS live,
       l.max_instances AS stored, CAST(COALESCE(p.default_template_id = COALESCE(r.template_id, k.template_id, p.default_template_id), 0) AS INTEGER) AS is_default
FROM run r JOIN task k ON k.id = r.task_id JOIN project p ON p.id = k.project_id
LEFT JOIN project_template_limit l ON l.project_id = p.id
     AND l.template_id = COALESCE(r.template_id, k.template_id, p.default_template_id)
WHERE r.state = 'queued' AND p.archived_at IS NULL AND COALESCE(r.template_id, k.template_id, p.default_template_id) IS NOT NULL
GROUP BY p.id, COALESCE(r.template_id, k.template_id, p.default_template_id)
ORDER BY p.priority DESC, p.id;

-- name: CountProjectRunners :one
-- Live (not terminated) runners launched for a project on a template.
SELECT CAST(COUNT(*) AS INTEGER) FROM runner
WHERE launched_for_project = sqlc.arg(project_id) AND template_id = sqlc.arg(template_id) AND state <> 'terminated';

-- name: ListAllowedTemplates :many
-- Templates (besides its own) the project may use, by name.
SELECT t.id, t.name FROM project_allowed_template a JOIN runner_template t ON t.id = a.template_id
WHERE a.project_id = ? ORDER BY t.name;

-- name: ListAllowedTemplatesForTask :many
SELECT a.template_id FROM project_allowed_template a JOIN task k ON k.project_id = a.project_id
WHERE k.id = ? ORDER BY a.template_id;

-- name: ClearAllowedTemplates :exec
DELETE FROM project_allowed_template WHERE project_id = ?;

-- name: AddAllowedTemplate :exec
INSERT OR IGNORE INTO project_allowed_template (project_id, template_id) VALUES (?, ?);

-- name: AllowedSupply :one
-- Free session slots (claimable runners, plus runners still coming up) on templates that projects
-- with queued runs on this template may also use. The autoscaler counts them as supply.
SELECT CAST(COALESCE(SUM(MAX(a.max_sessions - (SELECT COUNT(*) FROM run r WHERE r.runner_id = n.id
                  AND r.state IN ('assigned', 'preparing', 'running', 'needs_input', 'pushing', 'cancelling')), 0)), 0) AS INTEGER) AS supply
FROM runner n JOIN runner_template a ON a.id = n.template_id
WHERE a.id <> sqlc.arg(template_id)
  AND a.id IN (SELECT pa.template_id FROM project_allowed_template pa
               WHERE pa.project_id IN (SELECT k.project_id FROM run r2 JOIN task k ON k.id = r2.task_id
                                       WHERE r2.state = 'queued'
                                         AND COALESCE(r2.template_id, k.template_id, (SELECT p.default_template_id FROM project p WHERE p.id = k.project_id)) = sqlc.arg(template_id)))
  AND n.cordoned = 0 AND n.drain_requested = 0 AND n.upgrade_sha = ''
  AND n.state IN ('launching', 'enrolling', 'provisioning', 'idle', 'busy')
  AND (n.state NOT IN ('idle', 'busy') OR n.template_version = a.version);

-- name: CountLiveRunners :one
SELECT CAST(COUNT(*) AS INTEGER) FROM runner WHERE template_id = ? AND state <> 'terminated';
