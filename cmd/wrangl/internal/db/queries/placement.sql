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
