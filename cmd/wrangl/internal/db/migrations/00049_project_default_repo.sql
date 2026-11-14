-- +goose Up
-- The repo a task resolves to when neither it nor an ancestor names one (ADR 0012 section 3). Optional.
ALTER TABLE project ADD COLUMN default_repo_id INTEGER REFERENCES repo (id) ON DELETE SET NULL;

-- +goose Down
ALTER TABLE project DROP COLUMN default_repo_id;
