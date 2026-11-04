-- +goose Up
-- A committed static HTML prototype can be handed back as a deliverable when the runner cannot
-- publish a Claude Artifact. The file is served in a sandboxed iframe under a strict CSP.
CREATE TABLE deliverable_new (
    id         INTEGER PRIMARY KEY,
    task_id    INTEGER NOT NULL REFERENCES task (id) ON DELETE CASCADE,
    run_id     INTEGER REFERENCES run (id) ON DELETE SET NULL,
    kind       TEXT    NOT NULL CHECK (kind IN ('image', 'artifact_url', 'link', 'report_md', 'prototype_html')),
    title      TEXT    NOT NULL DEFAULT '',
    url        TEXT    NOT NULL DEFAULT '',
    file       TEXT    NOT NULL DEFAULT '',
    mime       TEXT    NOT NULL DEFAULT '',
    created_at TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
INSERT INTO deliverable_new SELECT id, task_id, run_id, kind, title, url, file, mime, created_at FROM deliverable;
DROP TABLE deliverable;
ALTER TABLE deliverable_new RENAME TO deliverable;
CREATE INDEX deliverable_task ON deliverable (task_id);
CREATE INDEX deliverable_created ON deliverable (created_at);

-- +goose Down
DELETE FROM deliverable WHERE kind = 'prototype_html';
CREATE TABLE deliverable_old (
    id         INTEGER PRIMARY KEY,
    task_id    INTEGER NOT NULL REFERENCES task (id) ON DELETE CASCADE,
    run_id     INTEGER REFERENCES run (id) ON DELETE SET NULL,
    kind       TEXT    NOT NULL CHECK (kind IN ('image', 'artifact_url', 'link', 'report_md')),
    title      TEXT    NOT NULL DEFAULT '',
    url        TEXT    NOT NULL DEFAULT '',
    file       TEXT    NOT NULL DEFAULT '',
    mime       TEXT    NOT NULL DEFAULT '',
    created_at TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
INSERT INTO deliverable_old SELECT * FROM deliverable;
DROP TABLE deliverable;
ALTER TABLE deliverable_old RENAME TO deliverable;
CREATE INDEX deliverable_task ON deliverable (task_id);
CREATE INDEX deliverable_created ON deliverable (created_at);
