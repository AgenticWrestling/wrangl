-- +goose Up
-- M2: enrolment and lifecycle tracking for the idle reaper.
ALTER TABLE runner ADD COLUMN launched INTEGER NOT NULL DEFAULT 0 CHECK (launched IN (0, 1));
ALTER TABLE runner ADD COLUMN launched_at TEXT;
ALTER TABLE runner ADD COLUMN enrolled_at TEXT;
ALTER TABLE runner ADD COLUMN idle_since TEXT;
ALTER TABLE runner ADD COLUMN offline_since TEXT;
ALTER TABLE enrollment_token ADD COLUMN runner_name TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE enrollment_token DROP COLUMN runner_name;
ALTER TABLE runner DROP COLUMN offline_since;
ALTER TABLE runner DROP COLUMN idle_since;
ALTER TABLE runner DROP COLUMN enrolled_at;
ALTER TABLE runner DROP COLUMN launched_at;
ALTER TABLE runner DROP COLUMN launched;
