-- +goose Up
-- One audit row per custom tool call (docs/wrangl/adr/0003): which run called which tool, how it
-- ended, how long it took and how many result bytes came back. No arguments, headers or bodies.
CREATE TABLE tool_call (
    id          INTEGER PRIMARY KEY,
    run_id      INTEGER NOT NULL REFERENCES run(id) ON DELETE CASCADE,
    tool        TEXT    NOT NULL,
    status      TEXT    NOT NULL CHECK (status IN ('ok', 'error', 'denied', 'invalid', 'pending_approval')),
    duration_ms INTEGER NOT NULL DEFAULT 0,
    bytes       INTEGER NOT NULL DEFAULT 0,
    at          TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
CREATE INDEX tool_call_run ON tool_call (run_id, id);

-- +goose Down
DROP TABLE tool_call;
