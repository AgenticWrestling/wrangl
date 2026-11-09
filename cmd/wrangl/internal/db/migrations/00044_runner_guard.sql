-- +goose Up
-- The measured guard (ADR 0009 section 6.3, ADR 0007 section 7.2): thresholds on a runner's load, memory
-- and disk (NULL = not guarded, the default), and the state it keeps. closed_by_guard = 1 closes the runner
-- to NEW assignments only; running sessions are never touched. breaches / oks count consecutive heartbeat
-- samples over the threshold / back under 90% of it, so one spike does not flap the runner.
ALTER TABLE runner_capacity ADD COLUMN guard_load1       REAL;
ALTER TABLE runner_capacity ADD COLUMN guard_mem_pct     REAL;
ALTER TABLE runner_capacity ADD COLUMN guard_disk_pct    REAL;
ALTER TABLE runner_capacity ADD COLUMN closed_by_guard   INTEGER NOT NULL DEFAULT 0;
ALTER TABLE runner_capacity ADD COLUMN guard_breaches    INTEGER NOT NULL DEFAULT 0;
ALTER TABLE runner_capacity ADD COLUMN guard_oks         INTEGER NOT NULL DEFAULT 0;
ALTER TABLE runner_capacity ADD COLUMN guard_reason      TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE runner_capacity DROP COLUMN guard_reason;
ALTER TABLE runner_capacity DROP COLUMN guard_oks;
ALTER TABLE runner_capacity DROP COLUMN guard_breaches;
ALTER TABLE runner_capacity DROP COLUMN closed_by_guard;
ALTER TABLE runner_capacity DROP COLUMN guard_disk_pct;
ALTER TABLE runner_capacity DROP COLUMN guard_mem_pct;
ALTER TABLE runner_capacity DROP COLUMN guard_load1;
