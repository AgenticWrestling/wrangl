-- +goose Up
CREATE TABLE meta (
    key    TEXT PRIMARY KEY,
    value  TEXT NOT NULL
);

CREATE TABLE runner_template (
    id              INTEGER PRIMARY KEY,
    name            TEXT    NOT NULL UNIQUE,
    description     TEXT    NOT NULL DEFAULT '',
    mise_toml       TEXT    NOT NULL,
    setup_script    TEXT    NOT NULL DEFAULT '',
    agent_json      TEXT    NOT NULL,
    min_vcpu        INTEGER NOT NULL CHECK (min_vcpu > 0),
    min_ram_mib     INTEGER NOT NULL CHECK (min_ram_mib > 0),
    min_disk_gib    INTEGER NOT NULL CHECK (min_disk_gib > 0),
    arch            TEXT    NOT NULL DEFAULT 'arm64' CHECK (arch IN ('arm64', 'x86_64')),
    instance_types  TEXT    NOT NULL DEFAULT '[]',
    ami             TEXT    NOT NULL DEFAULT '/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-arm64',
    max_runners     INTEGER NOT NULL DEFAULT 1 CHECK (max_runners >= 0),
    idle_timeout_s  INTEGER NOT NULL DEFAULT 1800,
    version         INTEGER NOT NULL DEFAULT 1,
    created_at      TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at      TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE TABLE runner (
    id                INTEGER PRIMARY KEY,
    name              TEXT    NOT NULL UNIQUE,
    template_id       INTEGER NOT NULL REFERENCES runner_template (id),
    template_version  INTEGER NOT NULL DEFAULT 0,
    instance_id       TEXT    UNIQUE,
    instance_type     TEXT    NOT NULL DEFAULT '',
    az                TEXT    NOT NULL DEFAULT '',
    private_ip        TEXT    NOT NULL DEFAULT '',
    arch              TEXT    NOT NULL DEFAULT '',
    vcpu              INTEGER NOT NULL DEFAULT 0,
    ram_mib           INTEGER NOT NULL DEFAULT 0,
    disk_gib          INTEGER NOT NULL DEFAULT 0,
    nkey_public       TEXT    UNIQUE,
    state             TEXT    NOT NULL DEFAULT 'launching' CHECK (state IN (
                          'launching', 'enrolling', 'provisioning', 'idle', 'busy',
                          'cordoned', 'draining', 'offline', 'terminated')),
    max_concurrent    INTEGER NOT NULL DEFAULT 1,
    last_heartbeat_at TEXT,
    last_metrics_json TEXT    NOT NULL DEFAULT '{}',
    created_at        TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    terminated_at     TEXT
);
CREATE INDEX runner_state ON runner (state);

CREATE TABLE enrollment_token (
    id           INTEGER PRIMARY KEY,
    token_hash   BLOB    NOT NULL UNIQUE,
    template_id  INTEGER NOT NULL REFERENCES runner_template (id),
    instance_id  TEXT,
    runner_id    INTEGER REFERENCES runner (id),
    expires_at   TEXT    NOT NULL,
    used_at      TEXT,
    created_at   TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE TABLE project (
    id                   INTEGER PRIMARY KEY,
    slug                 TEXT    NOT NULL UNIQUE,
    name                 TEXT    NOT NULL,
    description          TEXT    NOT NULL DEFAULT '',
    default_template_id  INTEGER REFERENCES runner_template (id),
    plan_template_id     INTEGER REFERENCES runner_template (id),  -- NULL = default template
    auto_merge           INTEGER NOT NULL DEFAULT 0 CHECK (auto_merge IN (0, 1)),
    protected_paths      TEXT    NOT NULL DEFAULT '[".github/**","**/migrations/**","go.mod","**/*.tf","Dockerfile*"]',
    max_auto_merge_lines INTEGER NOT NULL DEFAULT 200,
    priority             INTEGER NOT NULL DEFAULT 0,
    created_at           TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at           TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE TABLE repo (
    id                  INTEGER PRIMARY KEY,
    project_id          INTEGER NOT NULL REFERENCES project (id) ON DELETE CASCADE,
    owner               TEXT    NOT NULL,
    name                TEXT    NOT NULL,
    default_branch      TEXT    NOT NULL DEFAULT 'main',
    gh_installation_id  INTEGER,
    created_at          TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    UNIQUE (project_id, owner, name)
);

CREATE TABLE issue (
    id         INTEGER PRIMARY KEY,
    repo_id    INTEGER NOT NULL REFERENCES repo (id) ON DELETE CASCADE,
    gh_number  INTEGER NOT NULL,
    title      TEXT    NOT NULL,
    body       TEXT    NOT NULL DEFAULT '',
    state      TEXT    NOT NULL CHECK (state IN ('open', 'closed')),
    url        TEXT    NOT NULL,
    synced_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    UNIQUE (repo_id, gh_number)
);

CREATE TABLE task (
    id                  INTEGER PRIMARY KEY,
    sqid                TEXT    UNIQUE,  -- set in the creating tx; immutable (trigger below)
    project_id          INTEGER NOT NULL REFERENCES project (id) ON DELETE CASCADE,
    parent_id           INTEGER REFERENCES task (id) ON DELETE CASCADE,
    rank                TEXT    NOT NULL,  -- fractional index, unique among siblings
    repo_id             INTEGER REFERENCES repo (id),             -- NULL = inherit from ancestor
    issue_id            INTEGER REFERENCES issue (id) ON DELETE SET NULL,
    template_id         INTEGER REFERENCES runner_template (id),  -- NULL = inherit / project default
    title               TEXT    NOT NULL,
    prompt              TEXT    NOT NULL DEFAULT '',
    state               TEXT    NOT NULL DEFAULT 'open' CHECK (state IN (
                            'open', 'in_progress', 'in_review', 'done', 'cancelled')),
    created_by          TEXT    NOT NULL DEFAULT 'user',  -- user | agent:<run id> | system
    completion_summary  TEXT    NOT NULL DEFAULT '',
    completed_run_id    INTEGER,  -- run whose agent completed it (no FK: run is created later)
    created_at          TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at          TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    completed_at        TEXT,
    CHECK (parent_id IS NULL OR parent_id <> id)
);
CREATE INDEX task_project_state ON task (project_id, state);
CREATE INDEX task_parent ON task (parent_id);
CREATE UNIQUE INDEX task_rank_child ON task (parent_id, rank) WHERE parent_id IS NOT NULL;
CREATE UNIQUE INDEX task_rank_root  ON task (project_id, rank) WHERE parent_id IS NULL;

-- +goose StatementBegin
CREATE TRIGGER task_sqid_immutable BEFORE UPDATE OF sqid ON task
WHEN old.sqid IS NOT NULL AND new.sqid IS NOT old.sqid BEGIN
    SELECT RAISE(ABORT, 'task.sqid is immutable');
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER task_parent_project_ins BEFORE INSERT ON task
WHEN new.parent_id IS NOT NULL
 AND (SELECT project_id FROM task WHERE id = new.parent_id) IS NOT new.project_id BEGIN
    SELECT RAISE(ABORT, 'parent task must be in the same project');
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER task_parent_project_upd BEFORE UPDATE OF parent_id, project_id ON task
WHEN new.parent_id IS NOT NULL
 AND (SELECT project_id FROM task WHERE id = new.parent_id) IS NOT new.project_id BEGIN
    SELECT RAISE(ABORT, 'parent task must be in the same project');
END;
-- +goose StatementEnd

CREATE TABLE task_dependency (
    task_id        INTEGER NOT NULL REFERENCES task (id) ON DELETE CASCADE,
    depends_on_id  INTEGER NOT NULL REFERENCES task (id) ON DELETE CASCADE,
    created_by     TEXT    NOT NULL DEFAULT 'user',
    created_at     TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    PRIMARY KEY (task_id, depends_on_id),
    CHECK (task_id <> depends_on_id)
);
CREATE INDEX task_dependency_rev ON task_dependency (depends_on_id);
-- +goose StatementBegin
CREATE TRIGGER task_dependency_project BEFORE INSERT ON task_dependency
WHEN (SELECT project_id FROM task WHERE id = new.task_id)
  IS NOT (SELECT project_id FROM task WHERE id = new.depends_on_id) BEGIN
    SELECT RAISE(ABORT, 'precursor must be in the same project');
END;
-- +goose StatementEnd

-- Audit log of work-stack changes. No FK on task: entries outlive deleted tasks.
CREATE TABLE task_event (
    id         INTEGER PRIMARY KEY,
    task_sqid  TEXT    NOT NULL,
    run_id     INTEGER REFERENCES run (id) ON DELETE SET NULL,
    actor      TEXT    NOT NULL,  -- user | agent:<run id> | system
    op         TEXT    NOT NULL CHECK (op IN (
                   'create', 'update', 'move', 'rerank', 'complete', 'reopen', 'cancel',
                   'delete', 'add_precursor', 'remove_precursor')),
    diff_json  TEXT    NOT NULL DEFAULT '{}',
    ts         TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
CREATE INDEX task_event_sqid ON task_event (task_sqid);

CREATE TABLE run (
    id           INTEGER PRIMARY KEY,
    task_id      INTEGER NOT NULL REFERENCES task (id) ON DELETE CASCADE,  -- assignment root
    current_task_id INTEGER REFERENCES task (id) ON DELETE SET NULL,       -- subtask in progress
    runner_id    INTEGER REFERENCES runner (id),
    kind         TEXT    NOT NULL DEFAULT 'work' CHECK (kind IN ('work', 'review', 'revise', 'plan')),
    attempt      INTEGER NOT NULL DEFAULT 1,
    state        TEXT    NOT NULL DEFAULT 'queued' CHECK (state IN (
                     'queued', 'assigned', 'preparing', 'running', 'needs_input', 'blocked',
                     'pushing', 'succeeded', 'failed', 'cancelling', 'cancelled')),
    branch       TEXT    NOT NULL DEFAULT '',
    session_name TEXT    NOT NULL DEFAULT '',  -- <runner>:<task sqid>
    base_sha     TEXT    NOT NULL DEFAULT '',
    head_sha     TEXT    NOT NULL DEFAULT '',
    status_text  TEXT    NOT NULL DEFAULT '',
    exit_code    INTEGER,
    error        TEXT    NOT NULL DEFAULT '',
    queued_at    TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    started_at   TEXT,
    finished_at  TEXT
);
CREATE INDEX run_state ON run (state);
CREATE INDEX run_task ON run (task_id);
CREATE INDEX run_runner ON run (runner_id);

CREATE TABLE pull_request (
    id                 INTEGER PRIMARY KEY,
    task_id            INTEGER NOT NULL REFERENCES task (id) ON DELETE CASCADE,
    repo_id            INTEGER NOT NULL REFERENCES repo (id),
    run_id             INTEGER NOT NULL REFERENCES run (id),
    gh_number          INTEGER NOT NULL,
    url                TEXT    NOT NULL,
    head_branch        TEXT    NOT NULL,
    state              TEXT    NOT NULL DEFAULT 'open' CHECK (state IN ('open', 'merged', 'closed')),
    risk               TEXT    CHECK (risk IN ('low', 'medium', 'high')),
    review_summary_md  TEXT    NOT NULL DEFAULT '',
    gates_json         TEXT    NOT NULL DEFAULT '{}',
    decision           TEXT    CHECK (decision IN (
                           'auto_merged', 'user_approved', 'changes_requested', 'closed')),
    created_at         TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at         TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    UNIQUE (repo_id, gh_number)
);

CREATE TABLE log_line (
    id       INTEGER PRIMARY KEY,
    run_id   INTEGER NOT NULL REFERENCES run (id) ON DELETE CASCADE,
    seq      INTEGER NOT NULL,
    ts       TEXT    NOT NULL,
    stream   TEXT    NOT NULL CHECK (stream IN ('agent', 'runner', 'hook', 'system')),
    level    TEXT    NOT NULL DEFAULT 'info',
    message  TEXT    NOT NULL,
    UNIQUE (run_id, seq)
);

CREATE VIRTUAL TABLE log_line_fts USING fts5 (
    message, content = 'log_line', content_rowid = 'id'
);
-- +goose StatementBegin
CREATE TRIGGER log_line_ai AFTER INSERT ON log_line BEGIN
    INSERT INTO log_line_fts (rowid, message) VALUES (new.id, new.message);
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER log_line_ad AFTER DELETE ON log_line BEGIN
    INSERT INTO log_line_fts (log_line_fts, rowid, message) VALUES ('delete', old.id, old.message);
END;
-- +goose StatementEnd

CREATE TABLE hook_event (
    id            INTEGER PRIMARY KEY,
    run_id        INTEGER NOT NULL REFERENCES run (id) ON DELETE CASCADE,
    seq           INTEGER NOT NULL,
    ts            TEXT    NOT NULL,
    name          TEXT    NOT NULL,
    payload_json  TEXT    NOT NULL DEFAULT '{}',
    UNIQUE (run_id, seq)
);

CREATE TABLE question (
    id                INTEGER PRIMARY KEY,
    task_id           INTEGER NOT NULL REFERENCES task (id) ON DELETE CASCADE,
    run_id            INTEGER REFERENCES run (id) ON DELETE SET NULL,
    kind              TEXT    NOT NULL CHECK (kind IN ('agent', 'pr_review', 'approval', 'plan_review', 'system')),
    prompt            TEXT    NOT NULL,
    detail_md         TEXT    NOT NULL DEFAULT '',
    allow_free_text   INTEGER NOT NULL DEFAULT 0 CHECK (allow_free_text IN (0, 1)),
    payload_json      TEXT    NOT NULL DEFAULT '{}',  -- e.g. plan_review: proposed split tree
    state             TEXT    NOT NULL DEFAULT 'open' CHECK (state IN (
                          'open', 'answered', 'cancelled')),
    answer_option_id  INTEGER REFERENCES question_option (id),
    answer_text       TEXT    NOT NULL DEFAULT '',
    created_at        TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    answered_at       TEXT
);
CREATE INDEX question_state ON question (state);

CREATE TABLE question_option (
    id           INTEGER PRIMARY KEY,
    question_id  INTEGER NOT NULL REFERENCES question (id) ON DELETE CASCADE,
    position     INTEGER NOT NULL,
    label        TEXT    NOT NULL,
    value        TEXT    NOT NULL,
    description  TEXT    NOT NULL DEFAULT '',
    UNIQUE (question_id, position),
    UNIQUE (question_id, value)
);

CREATE TABLE push_subscription (
    id               INTEGER PRIMARY KEY,
    endpoint         TEXT    NOT NULL UNIQUE,
    p256dh           TEXT    NOT NULL,
    auth             TEXT    NOT NULL,
    user_agent       TEXT    NOT NULL DEFAULT '',
    failures         INTEGER NOT NULL DEFAULT 0,
    last_success_at  TEXT,
    created_at       TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

-- +goose Down
DROP TABLE push_subscription;
DROP TABLE question_option;
DROP TABLE question;
DROP TABLE hook_event;
DROP TRIGGER log_line_ad;
DROP TRIGGER log_line_ai;
DROP TABLE log_line_fts;
DROP TABLE log_line;
DROP TABLE pull_request;
DROP TABLE run;
DROP TABLE task_event;
DROP TABLE task_dependency;
DROP TABLE task;
DROP TABLE issue;
DROP TABLE repo;
DROP TABLE project;
DROP TABLE enrollment_token;
DROP TABLE runner;
DROP TABLE runner_template;
DROP TABLE meta;
