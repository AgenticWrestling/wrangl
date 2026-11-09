package dbgen

import (
	"context"
	"database/sql"
)

const getPlacementConfig = `-- name: GetPlacementConfig :one
SELECT need_vcpu, need_ram_mib, need_disk_gib FROM placement_config WHERE id = 1
`

type GetPlacementConfigRow struct {
	NeedVcpu    int64 `json:"need_vcpu"`
	NeedRamMib  int64 `json:"need_ram_mib"`
	NeedDiskGib int64 `json:"need_disk_gib"`
}

func (q *Queries) GetPlacementConfig(ctx context.Context) (GetPlacementConfigRow, error) {
	row := q.db.QueryRowContext(ctx, getPlacementConfig)
	var i GetPlacementConfigRow
	err := row.Scan(&i.NeedVcpu, &i.NeedRamMib, &i.NeedDiskGib)
	return i, err
}

const setPlacementNeeds = `-- name: SetPlacementNeeds :exec
UPDATE placement_config SET need_vcpu = ?1, need_ram_mib = ?2, need_disk_gib = ?3 WHERE id = 1
`

type SetPlacementNeedsParams struct {
	NeedVcpu    int64 `json:"need_vcpu"`
	NeedRamMib  int64 `json:"need_ram_mib"`
	NeedDiskGib int64 `json:"need_disk_gib"`
}

func (q *Queries) SetPlacementNeeds(ctx context.Context, arg SetPlacementNeedsParams) error {
	_, err := q.db.ExecContext(ctx, setPlacementNeeds, arg.NeedVcpu, arg.NeedRamMib, arg.NeedDiskGib)
	return err
}

// OptionalNeed is a project's or task's override of the need set (NULL = inherit).
type OptionalNeed struct {
	NeedVcpu    sql.NullInt64 `json:"need_vcpu"`
	NeedRamMib  sql.NullInt64 `json:"need_ram_mib"`
	NeedDiskGib sql.NullInt64 `json:"need_disk_gib"`
}

const getProjectNeed = `-- name: GetProjectNeed :one
SELECT need_vcpu, need_ram_mib, need_disk_gib FROM project_need WHERE project_id = ?
`

func (q *Queries) GetProjectNeed(ctx context.Context, projectID int64) (OptionalNeed, error) {
	row := q.db.QueryRowContext(ctx, getProjectNeed, projectID)
	var i OptionalNeed
	err := row.Scan(&i.NeedVcpu, &i.NeedRamMib, &i.NeedDiskGib)
	return i, err
}

const setProjectNeed = `-- name: SetProjectNeed :exec
INSERT INTO project_need (project_id, need_vcpu, need_ram_mib, need_disk_gib) VALUES (?1, ?2, ?3, ?4)
ON CONFLICT (project_id) DO UPDATE SET need_vcpu = excluded.need_vcpu, need_ram_mib = excluded.need_ram_mib, need_disk_gib = excluded.need_disk_gib
`

type SetProjectNeedParams struct {
	ProjectID int64 `json:"project_id"`
	OptionalNeed
}

func (q *Queries) SetProjectNeed(ctx context.Context, arg SetProjectNeedParams) error {
	_, err := q.db.ExecContext(ctx, setProjectNeed, arg.ProjectID, arg.NeedVcpu, arg.NeedRamMib, arg.NeedDiskGib)
	return err
}

const getTaskNeed = `-- name: GetTaskNeed :one
SELECT need_vcpu, need_ram_mib, need_disk_gib FROM task_need WHERE task_id = ?
`

func (q *Queries) GetTaskNeed(ctx context.Context, taskID int64) (OptionalNeed, error) {
	row := q.db.QueryRowContext(ctx, getTaskNeed, taskID)
	var i OptionalNeed
	err := row.Scan(&i.NeedVcpu, &i.NeedRamMib, &i.NeedDiskGib)
	return i, err
}

const setTaskNeed = `-- name: SetTaskNeed :exec
INSERT INTO task_need (task_id, need_vcpu, need_ram_mib, need_disk_gib) VALUES (?1, ?2, ?3, ?4)
ON CONFLICT (task_id) DO UPDATE SET need_vcpu = excluded.need_vcpu, need_ram_mib = excluded.need_ram_mib, need_disk_gib = excluded.need_disk_gib
`

type SetTaskNeedParams struct {
	TaskID int64 `json:"task_id"`
	OptionalNeed
}

func (q *Queries) SetTaskNeed(ctx context.Context, arg SetTaskNeedParams) error {
	_, err := q.db.ExecContext(ctx, setTaskNeed, arg.TaskID, arg.NeedVcpu, arg.NeedRamMib, arg.NeedDiskGib)
	return err
}

const initRunnerCapacity = `-- name: InitRunnerCapacity :exec
INSERT INTO runner_capacity (runner_id, max_sessions) VALUES (?, ?) ON CONFLICT (runner_id) DO NOTHING
`

type InitRunnerCapacityParams struct {
	RunnerID    int64         `json:"runner_id"`
	MaxSessions sql.NullInt64 `json:"max_sessions"`
}

// At enrolment: remember the template's session ceiling; an existing row (explicit capacity) stays.
func (q *Queries) InitRunnerCapacity(ctx context.Context, arg InitRunnerCapacityParams) error {
	_, err := q.db.ExecContext(ctx, initRunnerCapacity, arg.RunnerID, arg.MaxSessions)
	return err
}

const setRunnerCapacity = `-- name: SetRunnerCapacity :exec
INSERT INTO runner_capacity (runner_id, cap_vcpu, cap_ram_mib, cap_disk_gib) VALUES (?1, ?2, ?3, ?4)
ON CONFLICT (runner_id) DO UPDATE SET cap_vcpu = excluded.cap_vcpu, cap_ram_mib = excluded.cap_ram_mib, cap_disk_gib = excluded.cap_disk_gib
`

type SetRunnerCapacityParams struct {
	RunnerID   int64         `json:"runner_id"`
	CapVcpu    sql.NullInt64 `json:"cap_vcpu"`
	CapRamMib  sql.NullInt64 `json:"cap_ram_mib"`
	CapDiskGib sql.NullInt64 `json:"cap_disk_gib"`
}

func (q *Queries) SetRunnerCapacity(ctx context.Context, arg SetRunnerCapacityParams) error {
	_, err := q.db.ExecContext(ctx, setRunnerCapacity, arg.RunnerID, arg.CapVcpu, arg.CapRamMib, arg.CapDiskGib)
	return err
}

const getRunnerCapacity = `-- name: GetRunnerCapacity :one
SELECT cap_vcpu, cap_ram_mib, cap_disk_gib, max_sessions FROM runner_capacity WHERE runner_id = ?
`

type GetRunnerCapacityRow struct {
	CapVcpu     sql.NullInt64 `json:"cap_vcpu"`
	CapRamMib   sql.NullInt64 `json:"cap_ram_mib"`
	CapDiskGib  sql.NullInt64 `json:"cap_disk_gib"`
	MaxSessions sql.NullInt64 `json:"max_sessions"`
}

func (q *Queries) GetRunnerCapacity(ctx context.Context, runnerID int64) (GetRunnerCapacityRow, error) {
	row := q.db.QueryRowContext(ctx, getRunnerCapacity, runnerID)
	var i GetRunnerCapacityRow
	err := row.Scan(&i.CapVcpu, &i.CapRamMib, &i.CapDiskGib, &i.MaxSessions)
	return i, err
}

const sumActiveNeeds = `-- name: SumActiveNeeds :one
SELECT CAST(COALESCE(SUM(need_vcpu), 0) AS INTEGER) AS vcpu, CAST(COALESCE(SUM(need_ram_mib), 0) AS INTEGER) AS ram_mib,
       CAST(COALESCE(SUM(need_disk_gib), 0) AS INTEGER) AS disk_gib
FROM run WHERE runner_id = ? AND state IN ('assigned', 'preparing', 'running', 'needs_input', 'pushing', 'cancelling')
`

type SumActiveNeedsRow struct {
	Vcpu    int64 `json:"vcpu"`
	RamMib  int64 `json:"ram_mib"`
	DiskGib int64 `json:"disk_gib"`
}

// What the runner's active runs have reserved.
func (q *Queries) SumActiveNeeds(ctx context.Context, runnerID int64) (SumActiveNeedsRow, error) {
	row := q.db.QueryRowContext(ctx, sumActiveNeeds, runnerID)
	var i SumActiveNeedsRow
	err := row.Scan(&i.Vcpu, &i.RamMib, &i.DiskGib)
	return i, err
}
