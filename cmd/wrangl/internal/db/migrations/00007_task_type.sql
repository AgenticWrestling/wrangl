-- +goose Up
-- Task type (an enum: SQLite has none, so TEXT + CHECK; Go side is dbtypes.TaskType).
ALTER TABLE task ADD COLUMN task_type TEXT NOT NULL DEFAULT 'code'
    CHECK (task_type IN ('code', 'ux', 'architecture', 'spec', 'docs', 'review'));

-- +goose Down
ALTER TABLE task DROP COLUMN task_type;
