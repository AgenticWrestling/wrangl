-- +goose Up
-- How many concurrent agent sessions one runner of this template may host (each run gets its own
-- git worktree). Scheduling still places one run per runner until the slot-aware scheduler lands.
ALTER TABLE runner_template ADD COLUMN max_sessions INTEGER NOT NULL DEFAULT 4 CHECK (max_sessions >= 1);

-- +goose Down
ALTER TABLE runner_template DROP COLUMN max_sessions;
