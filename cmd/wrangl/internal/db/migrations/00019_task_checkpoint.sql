-- +goose Up
-- The commit a runner had checked out when it checkpointed a task, so a done task can link to
-- the changes it made (GitHub commit / compare view). One row per task and run, updated if the
-- task is checkpointed again.
CREATE TABLE task_checkpoint (
    id      INTEGER PRIMARY KEY,
    task_id INTEGER NOT NULL REFERENCES task(id) ON DELETE CASCADE,
    run_id  INTEGER NOT NULL REFERENCES run(id) ON DELETE CASCADE,
    sha     TEXT NOT NULL,
    at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    UNIQUE (task_id, run_id)
);

-- +goose Down
DROP TABLE task_checkpoint;
