-- +goose Up
-- Optional policy (ADR 0009 section 6.3, decision 7): a run waiting on the owner (needs_input) for longer
-- than this many seconds stops reserving vCPU on its runner. RAM and disk stay reserved. 0 = off (default).
ALTER TABLE placement_config ADD COLUMN needs_input_grace_s INTEGER NOT NULL DEFAULT 0 CHECK (needs_input_grace_s >= 0);

-- +goose Down
ALTER TABLE placement_config DROP COLUMN needs_input_grace_s;
