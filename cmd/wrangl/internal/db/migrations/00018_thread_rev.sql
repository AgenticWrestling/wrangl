-- +goose Up
-- Fingerprint of the focus task's subtree when proposals were made (or last applied), so a
-- proposal is refused once the tree has changed under it.
ALTER TABLE task_thread ADD COLUMN rev TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE task_thread DROP COLUMN rev;
