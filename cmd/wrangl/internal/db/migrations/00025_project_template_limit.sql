-- +goose Up
-- How many runner instances a project's demand may cause, per template. Seeded: 1 for the
-- project's default template, 0 for every other (expensive templates are opt-in). A missing
-- row (a project or template created later) means the same default. The template's max_runners
-- stays an account-wide ceiling: effective cap = min(project cap, template max_runners).
CREATE TABLE project_template_limit (
    project_id    INTEGER NOT NULL REFERENCES project (id) ON DELETE CASCADE,
    template_id   INTEGER NOT NULL REFERENCES runner_template (id) ON DELETE CASCADE,
    max_instances INTEGER NOT NULL CHECK (max_instances >= 0),
    PRIMARY KEY (project_id, template_id)
);
INSERT INTO project_template_limit (project_id, template_id, max_instances)
SELECT p.id, t.id, CASE WHEN p.default_template_id = t.id THEN 1 ELSE 0 END
FROM project p CROSS JOIN runner_template t;

-- The project whose demand caused a launch (NULL: launched by hand).
ALTER TABLE runner ADD COLUMN launched_for_project INTEGER REFERENCES project (id) ON DELETE SET NULL;

-- +goose Down
ALTER TABLE runner DROP COLUMN launched_for_project;
DROP TABLE project_template_limit;
