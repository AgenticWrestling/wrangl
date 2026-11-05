-- +goose Up
-- The system-wide custom tool library (docs/wrangl/adr/0003). kdl is the owner's source of truth;
-- decl_json is the runner-visible declaration derived from it (name, description, effect, run
-- kinds, input schema) and never holds argv, URLs or secret names.
CREATE TABLE tool (
    id          INTEGER PRIMARY KEY,
    name        TEXT    NOT NULL UNIQUE,
    kind        TEXT    NOT NULL CHECK (kind IN ('exec', 'http', 'mcp_remote')),
    effect      TEXT    NOT NULL CHECK (effect IN ('read', 'write', 'destructive')),
    description TEXT    NOT NULL DEFAULT '',
    kdl         TEXT    NOT NULL,
    decl_json   TEXT    NOT NULL,
    version     INTEGER NOT NULL DEFAULT 1,
    created_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

-- A tool is enabled for a project while a row exists.
CREATE TABLE project_tool (
    project_id INTEGER NOT NULL REFERENCES project (id) ON DELETE CASCADE,
    tool_id    INTEGER NOT NULL REFERENCES tool (id) ON DELETE CASCADE,
    created_at TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    PRIMARY KEY (project_id, tool_id)
);
CREATE INDEX project_tool_tool ON project_tool (tool_id);

-- +goose Down
DROP TABLE project_tool;
DROP TABLE tool;
