-- +goose Up
-- Actions a controller agent suggests in a reply; the owner approves each with one click.
-- JSON array of {label, description, action, args, state, result}.
ALTER TABLE task_thread ADD COLUMN proposals TEXT NOT NULL DEFAULT '[]';

-- +goose Down
ALTER TABLE task_thread DROP COLUMN proposals;
