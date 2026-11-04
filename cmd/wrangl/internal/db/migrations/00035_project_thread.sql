-- +goose Up
-- The project-level chat with a controller agent: the same shape as task_thread (see ADR 0005),
-- scoped to a project instead of a task. Execution, sessions and proposals are shared code in
-- controller/agents; only the rows live here.
CREATE TABLE project_thread (
    id         INTEGER PRIMARY KEY,
    project_id INTEGER NOT NULL REFERENCES project(id) ON DELETE CASCADE,
    at         TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    role       TEXT NOT NULL CHECK (role IN ('user', 'agent')),
    body       TEXT NOT NULL DEFAULT '',
    agent_id   INTEGER REFERENCES controller_agent(id) ON DELETE SET NULL,
    status     TEXT NOT NULL DEFAULT 'done' CHECK (status IN ('running', 'done', 'error')),
    session_id TEXT NOT NULL DEFAULT '',
    proposals  TEXT NOT NULL DEFAULT '[]',
    rev        TEXT NOT NULL DEFAULT ''
);
CREATE INDEX project_thread_project ON project_thread (project_id, id);

-- +goose Down
DROP TABLE project_thread;
