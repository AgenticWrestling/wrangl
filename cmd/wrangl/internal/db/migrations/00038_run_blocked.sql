-- +goose Up
-- Why a queued run is waiting, and since when (ADR 0009 section 7). blocked_reason is the text the
-- scheduler last wrote (also copied to status_text); both are cleared when the run is assigned.
ALTER TABLE run ADD COLUMN blocked_since  TEXT;
ALTER TABLE run ADD COLUMN blocked_reason TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE run DROP COLUMN blocked_reason;
ALTER TABLE run DROP COLUMN blocked_since;
