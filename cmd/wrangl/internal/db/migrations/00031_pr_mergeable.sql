-- +goose Up
-- Merge-conflict detection: GitHub's mergeability of an open PR against its base.
ALTER TABLE pull_request ADD COLUMN mergeable TEXT NOT NULL DEFAULT 'unknown'
    CHECK (mergeable IN ('unknown', 'clean', 'conflicted', 'blocked'));

-- +goose Down
ALTER TABLE pull_request DROP COLUMN mergeable;
