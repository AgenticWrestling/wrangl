-- +goose Up
-- An archived project (and all its tasks) is hidden and inert; NULL = active. Restorable.
ALTER TABLE project ADD COLUMN archived_at TEXT;

-- +goose Down
ALTER TABLE project DROP COLUMN archived_at;
