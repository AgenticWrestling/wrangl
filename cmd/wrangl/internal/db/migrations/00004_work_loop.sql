-- +goose Up
-- M4: runs carry their resolved template and repo; repos their clone URL.
ALTER TABLE repo ADD COLUMN clone_url TEXT NOT NULL DEFAULT '';
ALTER TABLE run ADD COLUMN template_id INTEGER REFERENCES runner_template (id);
ALTER TABLE run ADD COLUMN repo_id INTEGER REFERENCES repo (id);
ALTER TABLE run ADD COLUMN session_uuid TEXT NOT NULL DEFAULT '';
ALTER TABLE run ADD COLUMN pushed INTEGER NOT NULL DEFAULT 0 CHECK (pushed IN (0, 1));
ALTER TABLE project ADD COLUMN auto_queue INTEGER NOT NULL DEFAULT 0 CHECK (auto_queue IN (0, 1));

-- +goose Down
ALTER TABLE project DROP COLUMN auto_queue;
ALTER TABLE run DROP COLUMN pushed;
ALTER TABLE run DROP COLUMN session_uuid;
ALTER TABLE run DROP COLUMN repo_id;
ALTER TABLE run DROP COLUMN template_id;
ALTER TABLE repo DROP COLUMN clone_url;
