-- +goose Up
-- Remote runners (docs/wrangl/adr/0002): a template in mode 'remote' describes a TARGET machine
-- (a Mac, an Android emulator host) that runs no wrangl binary. The agent runs in a session on a
-- driver (an ordinary runner of connection.driver_template) and reaches the target over SSH/ADB.
CREATE TABLE template_connection (
    template_id     INTEGER PRIMARY KEY REFERENCES runner_template (id) ON DELETE CASCADE,
    mode            TEXT    NOT NULL DEFAULT 'local' CHECK (mode IN ('local', 'remote')),
    driver_template_id INTEGER REFERENCES runner_template (id),
    connection_json TEXT    NOT NULL DEFAULT '{}'
);

-- A controller-owned record of a launched target. No enrolment, no heartbeat: the controller
-- probes it. Sessions using it are the runs in run_target that are still active.
CREATE TABLE remote_target (
    id                  INTEGER PRIMARY KEY,
    name                TEXT    NOT NULL UNIQUE,
    template_id         INTEGER NOT NULL REFERENCES runner_template (id),
    instance_id         TEXT,
    host                TEXT    NOT NULL DEFAULT '',
    state               TEXT    NOT NULL DEFAULT 'launching' CHECK (state IN ('launching', 'ready', 'offline', 'draining', 'terminated')),
    probe_failures      INTEGER NOT NULL DEFAULT 0,
    last_error          TEXT    NOT NULL DEFAULT '',
    last_probe_at       TEXT,
    idle_since          TEXT,
    offline_since       TEXT,
    launched_for_project INTEGER REFERENCES project (id) ON DELETE SET NULL,
    created_at          TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    terminated_at       TEXT
);

CREATE TABLE run_target (
    run_id    INTEGER PRIMARY KEY REFERENCES run (id) ON DELETE CASCADE,
    target_id INTEGER NOT NULL REFERENCES remote_target (id)
);

-- +goose Down
DROP TABLE run_target;
DROP TABLE remote_target;
DROP TABLE template_connection;
