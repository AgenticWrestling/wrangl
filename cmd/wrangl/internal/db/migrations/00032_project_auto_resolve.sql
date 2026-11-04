-- +goose Up
-- Smart Merge: when on, auto-merge queues a resolve run for a conflicted PR instead of
-- asking the owner.
ALTER TABLE project ADD COLUMN auto_resolve INTEGER NOT NULL DEFAULT 0 CHECK (auto_resolve IN (0, 1));

-- +goose Down
ALTER TABLE project DROP COLUMN auto_resolve;
