-- +goose Up
-- Research citation audit: an opt-in review run that re-fetches every URL a research PR cites.
ALTER TABLE project ADD COLUMN citation_audit INTEGER NOT NULL DEFAULT 0 CHECK (citation_audit IN (0, 1));
-- The head commit that was audited and the audit's result (JSON), per PR.
ALTER TABLE pull_request ADD COLUMN audit_sha TEXT NOT NULL DEFAULT '';
ALTER TABLE pull_request ADD COLUMN audit_json TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE pull_request DROP COLUMN audit_json;
ALTER TABLE pull_request DROP COLUMN audit_sha;
ALTER TABLE project DROP COLUMN citation_audit;
