-- +goose Up
-- What a runner can hold, for resource-based placement (ADR 0009 section 6.1). NULL cap_* = derive
-- from the runner's facts (a launched box: facts minus the reserve; the adopted host: the declared
-- default); a value is the owner's explicit setting (`wrangl fleet set --capacity`). max_sessions is
-- the optional per-runner ceiling, copied from the template when the runner enrols. A side table, like
-- the need overrides, so the wide runner row and every query selecting it stay as they are.
CREATE TABLE runner_capacity (
    runner_id     INTEGER PRIMARY KEY REFERENCES runner (id) ON DELETE CASCADE,
    cap_vcpu      INTEGER CHECK (cap_vcpu IS NULL OR cap_vcpu >= 0),
    cap_ram_mib   INTEGER CHECK (cap_ram_mib IS NULL OR cap_ram_mib >= 0),
    cap_disk_gib  INTEGER CHECK (cap_disk_gib IS NULL OR cap_disk_gib >= 0),
    max_sessions  INTEGER CHECK (max_sessions IS NULL OR max_sessions >= 1)
);

-- +goose Down
DROP TABLE runner_capacity;
