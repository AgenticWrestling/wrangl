-- +goose Up
-- Resource needs (ADR 0009 sections 4 and 5): a global default set, per-project and per-task
-- overrides (NULL = inherit), and the snapshot a run carries. The defaults are 0/0/0, the migration-only
-- value under which placement behaves exactly as before (section 10.1); owners enter real values later.
-- The project and task overrides live in side tables rather than as columns, to keep the wide task and
-- project rows (and every query that selects them) untouched.
CREATE TABLE placement_config (
    id            INTEGER PRIMARY KEY CHECK (id = 1),
    need_vcpu     INTEGER NOT NULL DEFAULT 0 CHECK (need_vcpu >= 0),
    need_ram_mib  INTEGER NOT NULL DEFAULT 0 CHECK (need_ram_mib >= 0),
    need_disk_gib INTEGER NOT NULL DEFAULT 0 CHECK (need_disk_gib >= 0)
);
INSERT INTO placement_config (id) VALUES (1);

CREATE TABLE project_need (
    project_id    INTEGER PRIMARY KEY REFERENCES project (id) ON DELETE CASCADE,
    need_vcpu     INTEGER CHECK (need_vcpu IS NULL OR need_vcpu >= 1),
    need_ram_mib  INTEGER CHECK (need_ram_mib IS NULL OR need_ram_mib >= 512),
    need_disk_gib INTEGER CHECK (need_disk_gib IS NULL OR need_disk_gib >= 1)
);

CREATE TABLE task_need (
    task_id       INTEGER PRIMARY KEY REFERENCES task (id) ON DELETE CASCADE,
    need_vcpu     INTEGER CHECK (need_vcpu IS NULL OR need_vcpu >= 1),
    need_ram_mib  INTEGER CHECK (need_ram_mib IS NULL OR need_ram_mib >= 512),
    need_disk_gib INTEGER CHECK (need_disk_gib IS NULL OR need_disk_gib >= 1)
);

-- What a queued run reserves (written when the run is queued; legacy runs keep 0/0/0).
ALTER TABLE run ADD COLUMN need_vcpu     INTEGER NOT NULL DEFAULT 0;
ALTER TABLE run ADD COLUMN need_ram_mib  INTEGER NOT NULL DEFAULT 0;
ALTER TABLE run ADD COLUMN need_disk_gib INTEGER NOT NULL DEFAULT 0;
ALTER TABLE run ADD COLUMN need_source   TEXT    NOT NULL DEFAULT '{}';

-- +goose Down
ALTER TABLE run DROP COLUMN need_source;
ALTER TABLE run DROP COLUMN need_disk_gib;
ALTER TABLE run DROP COLUMN need_ram_mib;
ALTER TABLE run DROP COLUMN need_vcpu;
DROP TABLE task_need;
DROP TABLE project_need;
DROP TABLE placement_config;
