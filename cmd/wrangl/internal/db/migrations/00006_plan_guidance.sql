-- +goose Up
-- M8: the owner's guidance for a plan run, carried into its proposal (and a re-plan).
ALTER TABLE run ADD COLUMN guidance TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE run DROP COLUMN guidance;
