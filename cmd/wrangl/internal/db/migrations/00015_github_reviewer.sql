-- +goose Up
-- A second App per account: it reviews, approves and merges pull requests (an author App cannot
-- approve its own PRs) and receives webhooks. Created through the same manifest flow.
ALTER TABLE github_account ADD COLUMN reviewer_app_id INTEGER;
ALTER TABLE github_account ADD COLUMN reviewer_slug TEXT NOT NULL DEFAULT '';
ALTER TABLE github_account ADD COLUMN reviewer_html_url TEXT NOT NULL DEFAULT '';
ALTER TABLE github_account ADD COLUMN reviewer_key_file TEXT NOT NULL DEFAULT '';
ALTER TABLE github_account ADD COLUMN reviewer_hook_file TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE github_account DROP COLUMN reviewer_hook_file;
ALTER TABLE github_account DROP COLUMN reviewer_key_file;
ALTER TABLE github_account DROP COLUMN reviewer_html_url;
ALTER TABLE github_account DROP COLUMN reviewer_slug;
ALTER TABLE github_account DROP COLUMN reviewer_app_id;
