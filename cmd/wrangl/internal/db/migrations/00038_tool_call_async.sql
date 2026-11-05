-- +goose Up
-- Async tool calls (docs/wrangl/adr/0003): the audit row is written when a long call is accepted
-- ('running') and updated once when it ends. SQLite cannot alter a CHECK, so rebuild the table.
CREATE TABLE tool_call_new (
    id          INTEGER PRIMARY KEY,
    run_id      INTEGER NOT NULL REFERENCES run(id) ON DELETE CASCADE,
    tool        TEXT    NOT NULL,
    status      TEXT    NOT NULL CHECK (status IN ('ok', 'error', 'denied', 'invalid', 'pending_approval', 'running', 'cancelled')),
    duration_ms INTEGER NOT NULL DEFAULT 0,
    bytes       INTEGER NOT NULL DEFAULT 0,
    at          TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
INSERT INTO tool_call_new (id, run_id, tool, status, duration_ms, bytes, at)
    SELECT id, run_id, tool, status, duration_ms, bytes, at FROM tool_call;
DROP TABLE tool_call;
ALTER TABLE tool_call_new RENAME TO tool_call;
CREATE INDEX tool_call_run ON tool_call (run_id, id);

-- +goose Down
CREATE TABLE tool_call_old (
    id          INTEGER PRIMARY KEY,
    run_id      INTEGER NOT NULL REFERENCES run(id) ON DELETE CASCADE,
    tool        TEXT    NOT NULL,
    status      TEXT    NOT NULL CHECK (status IN ('ok', 'error', 'denied', 'invalid', 'pending_approval')),
    duration_ms INTEGER NOT NULL DEFAULT 0,
    bytes       INTEGER NOT NULL DEFAULT 0,
    at          TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
INSERT INTO tool_call_old (id, run_id, tool, status, duration_ms, bytes, at)
    SELECT id, run_id, tool, CASE status WHEN 'running' THEN 'error' WHEN 'cancelled' THEN 'error' ELSE status END, duration_ms, bytes, at FROM tool_call;
DROP TABLE tool_call;
ALTER TABLE tool_call_old RENAME TO tool_call;
CREATE INDEX tool_call_run ON tool_call (run_id, id);
