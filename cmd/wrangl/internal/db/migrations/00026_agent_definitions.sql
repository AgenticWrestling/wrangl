-- +goose Up
-- Named agent definitions, separate from runner templates (docs/wrangl/adr/0001). spec_json is the
-- same document a template used to embed as agent_json.
CREATE TABLE agent (
    id          INTEGER PRIMARY KEY,
    name        TEXT    NOT NULL UNIQUE,
    description TEXT    NOT NULL DEFAULT '',
    spec_json   TEXT    NOT NULL,
    version     INTEGER NOT NULL DEFAULT 1,
    created_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

-- Which agents may run on a template.
CREATE TABLE template_agent (
    template_id INTEGER NOT NULL REFERENCES runner_template (id) ON DELETE CASCADE,
    agent_id    INTEGER NOT NULL REFERENCES agent (id) ON DELETE CASCADE,
    PRIMARY KEY (template_id, agent_id)
);

ALTER TABLE project ADD COLUMN default_agent_id INTEGER REFERENCES agent (id) ON DELETE SET NULL;
-- The agent a run was queued with (NULL: resolved from the template, as before agents existed).
ALTER TABLE run ADD COLUMN agent_id INTEGER REFERENCES agent (id) ON DELETE SET NULL;

-- Wrap every template's agent block as an agent. Named after the binary (agy -> antigravity);
-- templates with byte-identical blocks share one agent; when one binary has several different
-- blocks the oldest template's keeps the plain name and the others get their first template's name. No behaviour change: agent_json is kept and still
-- read when a template has no allowed agents.
INSERT INTO agent (name, spec_json)
WITH specs AS (
    SELECT agent_json AS spec,
           CASE json_extract(agent_json, '$.start[0]') WHEN 'agy' THEN 'antigravity'
                ELSE json_extract(agent_json, '$.start[0]') END AS base,
           MIN(id) AS first_id, MIN(name) AS first_template
    FROM runner_template
    WHERE json_valid(agent_json) AND json_extract(agent_json, '$.start[0]') IS NOT NULL
    GROUP BY agent_json
)
SELECT CASE WHEN s.first_id = (SELECT MIN(s2.first_id) FROM specs s2 WHERE s2.base = s.base)
            THEN s.base ELSE s.base || '-' || s.first_template END, s.spec
FROM specs s;

INSERT INTO template_agent (template_id, agent_id)
SELECT t.id, a.id FROM runner_template t JOIN agent a ON a.spec_json = t.agent_json;

UPDATE project SET default_agent_id =
    (SELECT ta.agent_id FROM template_agent ta WHERE ta.template_id = project.default_template_id);

-- +goose Down
ALTER TABLE run DROP COLUMN agent_id;
ALTER TABLE project DROP COLUMN default_agent_id;
DROP TABLE template_agent;
DROP TABLE agent;
