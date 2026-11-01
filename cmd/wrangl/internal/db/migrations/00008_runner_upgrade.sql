-- +goose Up
-- Runner self-upgrade: binary_sha is what the runner reports running; upgrade_sha is the
-- binary the controller wants it on ('' = none pending; a pending runner gets no new runs).
ALTER TABLE runner ADD COLUMN binary_sha TEXT NOT NULL DEFAULT '';
ALTER TABLE runner ADD COLUMN upgrade_sha TEXT NOT NULL DEFAULT '';
ALTER TABLE runner ADD COLUMN upgrade_sent_at TEXT;

-- +goose Down
ALTER TABLE runner DROP COLUMN upgrade_sent_at;
ALTER TABLE runner DROP COLUMN upgrade_sha;
ALTER TABLE runner DROP COLUMN binary_sha;
