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
-- Busy/cordoned/draining runners keep their state.
-- Returning from offline keeps the existing idle_since: a network blip must not restart
-- the idle-shutdown clock (found by the M2 e2e test). A runner that went offline while
-- busy has idle_since NULL (cleared when work is assigned, M4) and starts afresh.
UPDATE runner
SET last_heartbeat_at = sqlc.arg(now), last_metrics_json = sqlc.arg(metrics),
    offline_since = NULL,
    idle_since = CASE WHEN state IN ('enrolling', 'provisioning') THEN sqlc.arg(now)
                      WHEN state = 'offline' THEN COALESCE(idle_since, sqlc.arg(now))
                      ELSE idle_since END,
    state = CASE WHEN state IN ('enrolling', 'provisioning', 'offline') THEN 'idle'
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
-- be available (supply; a runner still coming up counts all its slots), and live runners
-- (counted against max_runners).
SELECT t.id, t.name, t.version, t.failed_version, t.max_runners, t.max_sessions,
       CAST((SELECT COUNT(*) FROM run r JOIN task k ON k.id = r.task_id
             JOIN project p ON p.id = k.project_id
             WHERE r.state = 'queued'
               AND COALESCE(r.template_id, k.template_id, p.default_template_id) = t.id) AS INTEGER) AS demand,
       CAST((SELECT COALESCE(SUM(MAX(t.max_sessions - (SELECT COUNT(*) FROM run r WHERE r.runner_id = n.id
                          AND r.state IN ('assigned', 'preparing', 'running', 'needs_input', 'pushing', 'cancelling')), 0)), 0)
             FROM runner n
             WHERE n.template_id = t.id AND n.cordoned = 0 AND n.drain_requested = 0 AND n.upgrade_sha = ''
               AND n.state IN ('launching', 'enrolling', 'provisioning', 'idle', 'busy')) AS INTEGER) AS supply,
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
