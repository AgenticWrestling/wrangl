-- +goose Up
-- Operational log shown on the Logs page next to runs: runner launch/terminate/offline,
-- template (mise config) applies, binary upgrades. Runs themselves stay in the run table
-- and are merged in at read time, so a run is just one kind of log entry.
CREATE TABLE log_entry (
    id      INTEGER PRIMARY KEY,
    at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    kind    TEXT NOT NULL,                       -- runner | template | upgrade
    level   TEXT NOT NULL DEFAULT 'info' CHECK (level IN ('info', 'warn', 'error')),
    subject TEXT NOT NULL DEFAULT '',            -- runner or template name
    title   TEXT NOT NULL,
    detail  TEXT NOT NULL DEFAULT ''
);
CREATE INDEX log_entry_at ON log_entry (at DESC);

-- +goose Down
DROP TABLE log_entry;
