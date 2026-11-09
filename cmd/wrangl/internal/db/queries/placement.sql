-- name: GetPlacementConfig :one
SELECT need_vcpu, need_ram_mib, need_disk_gib FROM placement_config WHERE id = 1;

-- name: SetPlacementNeeds :exec
UPDATE placement_config SET need_vcpu = ?, need_ram_mib = ?, need_disk_gib = ? WHERE id = 1;

-- name: GetProjectNeed :one
SELECT need_vcpu, need_ram_mib, need_disk_gib FROM project_need WHERE project_id = ?;

-- name: SetProjectNeed :exec
INSERT INTO project_need (project_id, need_vcpu, need_ram_mib, need_disk_gib) VALUES (?, ?, ?, ?)
ON CONFLICT (project_id) DO UPDATE SET need_vcpu = excluded.need_vcpu, need_ram_mib = excluded.need_ram_mib, need_disk_gib = excluded.need_disk_gib;

-- name: GetTaskNeed :one
SELECT need_vcpu, need_ram_mib, need_disk_gib FROM task_need WHERE task_id = ?;

-- name: SetTaskNeed :exec
INSERT INTO task_need (task_id, need_vcpu, need_ram_mib, need_disk_gib) VALUES (?, ?, ?, ?)
ON CONFLICT (task_id) DO UPDATE SET need_vcpu = excluded.need_vcpu, need_ram_mib = excluded.need_ram_mib, need_disk_gib = excluded.need_disk_gib;

-- name: InitRunnerCapacity :exec
-- At enrolment: remember the template's session ceiling; an existing row (explicit capacity) stays.
INSERT INTO runner_capacity (runner_id, max_sessions) VALUES (?, ?) ON CONFLICT (runner_id) DO NOTHING;

-- name: SetRunnerCapacity :exec
INSERT INTO runner_capacity (runner_id, cap_vcpu, cap_ram_mib, cap_disk_gib) VALUES (?, ?, ?, ?)
ON CONFLICT (runner_id) DO UPDATE SET cap_vcpu = excluded.cap_vcpu, cap_ram_mib = excluded.cap_ram_mib, cap_disk_gib = excluded.cap_disk_gib;

-- name: GetRunnerCapacity :one
SELECT cap_vcpu, cap_ram_mib, cap_disk_gib, max_sessions FROM runner_capacity WHERE runner_id = ?;

-- name: SumActiveNeeds :one
-- What the runner's active runs have reserved.
SELECT CAST(COALESCE(SUM(need_vcpu), 0) AS INTEGER) AS vcpu, CAST(COALESCE(SUM(need_ram_mib), 0) AS INTEGER) AS ram_mib,
       CAST(COALESCE(SUM(need_disk_gib), 0) AS INTEGER) AS disk_gib
FROM run WHERE runner_id = ? AND state IN ('assigned', 'preparing', 'running', 'needs_input', 'pushing', 'cancelling');
