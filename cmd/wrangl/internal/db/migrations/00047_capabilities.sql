-- +goose Up
-- Capability tags and requirement sets (ADR 0007 stage 2, ADR 0009 stage 6). A template provides tags
-- (derived from its arch, agent allow-list and mise tools, plus its `provides:`), a runner carries them
-- (copied from its template, plus what the runner probed about itself, plus manual ones), and a project,
-- task or run states requirements against them. No requirements exist until an owner adds some, so
-- placement is unchanged by default.
CREATE TABLE template_capability (
    template_id INTEGER NOT NULL REFERENCES runner_template (id) ON DELETE CASCADE,
    key         TEXT    NOT NULL,
    value       TEXT    NOT NULL DEFAULT '',
    source      TEXT    NOT NULL CHECK (source IN ('derived', 'provides')),
    PRIMARY KEY (template_id, key)
);
CREATE TABLE runner_capability (
    runner_id INTEGER NOT NULL REFERENCES runner (id) ON DELETE CASCADE,
    key       TEXT    NOT NULL,
    value     TEXT    NOT NULL DEFAULT '',
    source    TEXT    NOT NULL CHECK (source IN ('template', 'probe', 'manual')),
    PRIMARY KEY (runner_id, key)
);
CREATE TABLE requirement (
    owner_kind TEXT    NOT NULL CHECK (owner_kind IN ('project', 'task', 'run')),
    owner_id   INTEGER NOT NULL,
    key        TEXT    NOT NULL,
    op         TEXT    NOT NULL CHECK (op IN ('=', '>=', 'in', 'exists')),
    value      TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (owner_kind, owner_id, key)
);

-- +goose Down
DROP TABLE requirement;
DROP TABLE runner_capability;
DROP TABLE template_capability;
