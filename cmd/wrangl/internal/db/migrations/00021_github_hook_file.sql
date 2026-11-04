-- +goose Up
-- The webhook secret GitHub generates for an account's (single) App. One App is now enough: it
-- authors PRs, reads checks, merges and receives webhooks. The reviewer App is optional.
ALTER TABLE github_account ADD COLUMN hook_file TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE github_account DROP COLUMN hook_file;
