-- +goose Up
-- A one-line note on a linked repo ("API server", "web client") shown to agents by repo_list
-- (ADR 0012 section 5).
ALTER TABLE repo ADD COLUMN note TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE repo DROP COLUMN note;
