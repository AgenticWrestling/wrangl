-- +goose Up
-- M6: PR flow. Review/revise/plan runs carry their prompt; PRs track head and checks.
ALTER TABLE run ADD COLUMN prompt_override TEXT NOT NULL DEFAULT '';
ALTER TABLE run ADD COLUMN override_used INTEGER NOT NULL DEFAULT 0 CHECK (override_used IN (0, 1));
ALTER TABLE run ADD COLUMN pr_id INTEGER REFERENCES pull_request (id);
ALTER TABLE run ADD COLUMN continue_branch INTEGER NOT NULL DEFAULT 0 CHECK (continue_branch IN (0, 1));
ALTER TABLE pull_request ADD COLUMN head_sha TEXT NOT NULL DEFAULT '';
ALTER TABLE pull_request ADD COLUMN checks TEXT NOT NULL DEFAULT '';
ALTER TABLE pull_request ADD COLUMN reviewed_sha TEXT NOT NULL DEFAULT '';
CREATE TABLE gh_delivery (
    id           TEXT PRIMARY KEY,
    received_at  TEXT NOT NULL
);

-- +goose Down
DROP TABLE gh_delivery;
ALTER TABLE pull_request DROP COLUMN reviewed_sha;
ALTER TABLE pull_request DROP COLUMN checks;
ALTER TABLE pull_request DROP COLUMN head_sha;
ALTER TABLE run DROP COLUMN continue_branch;
ALTER TABLE run DROP COLUMN pr_id;
ALTER TABLE run DROP COLUMN override_used;
ALTER TABLE run DROP COLUMN prompt_override;
