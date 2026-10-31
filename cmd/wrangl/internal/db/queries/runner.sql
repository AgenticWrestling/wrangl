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
       (r.state = 'idle'    AND r.idle_since    <= strftime('%Y-%m-%dT%H:%M:%fZ', sqlc.arg(now), '-' || t.idle_timeout_s || ' seconds'))
    OR (r.state = 'offline' AND r.offline_since <= strftime('%Y-%m-%dT%H:%M:%fZ', sqlc.arg(now), '-' || t.idle_timeout_s || ' seconds'))
    OR (r.state IN ('launching', 'enrolling') AND r.launched_at <= sqlc.arg(enroll_deadline))
);

-- name: MarkTerminated :exec
UPDATE runner SET state = 'terminated', terminated_at = ? WHERE id = ?;

-- name: UpdateRunnerState :exec
UPDATE runner SET state = ? WHERE id = ?;
