-- +goose Up
-- M3: template apply tracking, cordon/drain flags.
ALTER TABLE runner ADD COLUMN cordoned INTEGER NOT NULL DEFAULT 0 CHECK (cordoned IN (0, 1));
ALTER TABLE runner ADD COLUMN drain_requested INTEGER NOT NULL DEFAULT 0 CHECK (drain_requested IN (0, 1));
ALTER TABLE runner ADD COLUMN last_error TEXT NOT NULL DEFAULT '';
ALTER TABLE runner ADD COLUMN template_log TEXT NOT NULL DEFAULT '';
ALTER TABLE runner ADD COLUMN provisioning_since TEXT;
ALTER TABLE runner_template ADD COLUMN failed_version INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE runner_template DROP COLUMN failed_version;
ALTER TABLE runner DROP COLUMN provisioning_since;
ALTER TABLE runner DROP COLUMN template_log;
ALTER TABLE runner DROP COLUMN last_error;
ALTER TABLE runner DROP COLUMN drain_requested;
ALTER TABLE runner DROP COLUMN cordoned;
