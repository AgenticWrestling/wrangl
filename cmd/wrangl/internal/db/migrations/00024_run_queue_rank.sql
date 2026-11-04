-- +goose Up
-- Owner-chosen order within the Queue; NULL = not ranked (tree position, then FIFO).
ALTER TABLE run ADD COLUMN queue_rank REAL;

-- +goose Down
ALTER TABLE run DROP COLUMN queue_rank;
