-- +goose Up
-- Whether a run gets read-only checkouts of the project's other linked repos (ADR 0012 section 6).
ALTER TABLE project ADD COLUMN read_repos TEXT NOT NULL DEFAULT 'all' CHECK (read_repos IN ('all', 'none'));

-- Something the run's daemon noticed that the owner should see (a read-only checkout was modified).
CREATE TABLE run_warning (
    run_id     INTEGER PRIMARY KEY REFERENCES run (id) ON DELETE CASCADE,
    text       TEXT NOT NULL,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

-- +goose Down
DROP TABLE run_warning;
ALTER TABLE project DROP COLUMN read_repos;
