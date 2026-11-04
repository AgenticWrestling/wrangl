-- +goose Up
-- A project's notes repo: one of its linked repos given the role "notes". Research tasks work
-- there (reports under research/), not in the code repo.
CREATE TABLE project_notes_repo (
    project_id INTEGER PRIMARY KEY REFERENCES project (id) ON DELETE CASCADE,
    repo_id    INTEGER NOT NULL REFERENCES repo (id) ON DELETE CASCADE
);

-- +goose Down
DROP TABLE project_notes_repo;
