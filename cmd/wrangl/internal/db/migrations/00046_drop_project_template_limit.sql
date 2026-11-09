-- +goose Up
-- ADR 0009 stage 5: per-project instance caps are gone. Launching is bounded by the hourly budget (when
-- one is set), by each template's max_runners, and by what queued runs need. The table is the one
-- irreversible step of the stage; the down migration recreates and reseeds it the way 00025 did.
DROP TABLE project_template_limit;

-- +goose Down
CREATE TABLE project_template_limit (
    project_id    INTEGER NOT NULL REFERENCES project (id) ON DELETE CASCADE,
    template_id   INTEGER NOT NULL REFERENCES runner_template (id) ON DELETE CASCADE,
    max_instances INTEGER NOT NULL CHECK (max_instances >= 0),
    PRIMARY KEY (project_id, template_id)
);
INSERT INTO project_template_limit (project_id, template_id, max_instances)
SELECT p.id, t.id, CASE WHEN p.default_template_id = t.id THEN 1 ELSE 0 END
FROM project p CROSS JOIN runner_template t;
