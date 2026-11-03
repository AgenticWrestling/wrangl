-- +goose Up
-- Deliverables: what a run hands back besides a pull request (mockup images, a prototype
-- link, a research report). Image and report bytes live in files under the data dir;
-- rows older than the retention period are swept (files and rows).
CREATE TABLE deliverable (
    id         INTEGER PRIMARY KEY,
    task_id    INTEGER NOT NULL REFERENCES task (id) ON DELETE CASCADE,
    run_id     INTEGER REFERENCES run (id) ON DELETE SET NULL,
    kind       TEXT    NOT NULL CHECK (kind IN ('image', 'artifact_url', 'link', 'report_md')),
    title      TEXT    NOT NULL DEFAULT '',
    url        TEXT    NOT NULL DEFAULT '',  -- artifact_url / link
    file       TEXT    NOT NULL DEFAULT '',  -- image / report_md: file name inside the deliverables dir
    mime       TEXT    NOT NULL DEFAULT '',
    created_at TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
CREATE INDEX deliverable_task ON deliverable (task_id);
CREATE INDEX deliverable_created ON deliverable (created_at);

-- +goose Down
DROP TABLE deliverable;
