-- +goose Up
-- Controller agents: lightweight agent CLIs that run on the controller host itself to answer
-- questions about, and make small edits to, the task tree ("Ask?" on a task). Heavy work (builds,
-- tests) belongs on runners; these only talk and call the wrangl CLI.
CREATE TABLE controller_agent (
    id          INTEGER PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE,
    kind        TEXT NOT NULL DEFAULT 'claude' CHECK (kind IN ('claude')),
    command     TEXT NOT NULL DEFAULT 'claude',      -- binary on the controller's PATH
    model       TEXT NOT NULL DEFAULT '',            -- '' = the CLI's default
    -- how it authenticates: 'cli' = the already installed and logged-in CLI; 'secret' = a value
    -- from the secrets store exported as auth_env (e.g. a `claude setup-token` token).
    auth        TEXT NOT NULL DEFAULT 'cli' CHECK (auth IN ('cli', 'secret')),
    auth_secret TEXT NOT NULL DEFAULT '',
    auth_env    TEXT NOT NULL DEFAULT 'CLAUDE_CODE_OAUTH_TOKEN',
    max_turns   INTEGER NOT NULL DEFAULT 12,
    timeout_s   INTEGER NOT NULL DEFAULT 300,
    is_default  INTEGER NOT NULL DEFAULT 0,
    enabled     INTEGER NOT NULL DEFAULT 1
);
INSERT INTO controller_agent (name, is_default) VALUES ('claude', 1);

-- A conversation about a task (and its subtree). Agent messages carry the CLI session id so a
-- follow-up resumes the same session.
CREATE TABLE task_thread (
    id         INTEGER PRIMARY KEY,
    task_id    INTEGER NOT NULL REFERENCES task(id) ON DELETE CASCADE,
    at         TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    role       TEXT NOT NULL CHECK (role IN ('user', 'agent')),
    body       TEXT NOT NULL DEFAULT '',
    agent_id   INTEGER REFERENCES controller_agent(id) ON DELETE SET NULL,
    status     TEXT NOT NULL DEFAULT 'done' CHECK (status IN ('running', 'done', 'error')),
    session_id TEXT NOT NULL DEFAULT ''
);
CREATE INDEX task_thread_task ON task_thread (task_id, id);

-- +goose Down
DROP TABLE task_thread;
DROP TABLE controller_agent;
