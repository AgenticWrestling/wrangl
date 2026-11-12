-- +goose Up
-- Status of each project's primary Claude session (ADR 0011 section 3), kept by the controller's
-- session reconciler. Nothing writes here unless sessions.socket is configured.
CREATE TABLE primary_session (
    project_id      INTEGER PRIMARY KEY REFERENCES project (id) ON DELETE CASCADE,
    state           TEXT    NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'running', 'backoff', 'failed', 'stopped', 'host_down')),
    pid             INTEGER NOT NULL DEFAULT 0,
    started_at      TEXT,
    exit_code       INTEGER,
    restarts        INTEGER NOT NULL DEFAULT 0,   -- times it was started again after the first start
    failures        INTEGER NOT NULL DEFAULT 0,   -- consecutive short runs or failed starts (drives the backoff)
    ever_started    INTEGER NOT NULL DEFAULT 0,   -- a later start continues with `claude -c`
    next_attempt_at TEXT,
    last_error      TEXT    NOT NULL DEFAULT '',
    updated_at      TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

-- +goose Down
DROP TABLE primary_session;
