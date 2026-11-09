-- +goose Up
-- Templates a project's runs may use besides the template they were queued on (ADR 0009 stage 1).
-- Empty = today's behaviour: only the run's own template. The usual entry is the host template, so
-- a project can use the idle controller host and overflow to cloud runners when it is full.
CREATE TABLE project_allowed_template (
    project_id  INTEGER NOT NULL REFERENCES project (id) ON DELETE CASCADE,
    template_id INTEGER NOT NULL REFERENCES runner_template (id) ON DELETE CASCADE,
    PRIMARY KEY (project_id, template_id)
);

-- +goose Down
DROP TABLE project_allowed_template;
