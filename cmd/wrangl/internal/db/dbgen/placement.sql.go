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

const claimableRunners = `-- name: ClaimableRunners :many
SELECT n.id, n.name, n.state, n.idle_since, n.launched, n.vcpu, n.ram_mib, n.disk_gib, t.max_sessions
FROM runner n JOIN runner_template t ON t.id = n.template_id
WHERE n.template_id = ? AND n.state IN ('idle', 'busy') AND n.cordoned = 0 AND n.drain_requested = 0
  AND n.template_version = t.version AND n.upgrade_sha = ''
`

type ClaimableRunnersRow struct {
	ID          int64          `json:"id"`
	Name        string         `json:"name"`
	State       string         `json:"state"`
	IdleSince   sql.NullString `json:"idle_since"`
	Launched    int64          `json:"launched"`
	Vcpu        int64          `json:"vcpu"`
	RamMib      int64          `json:"ram_mib"`
	DiskGib     int64          `json:"disk_gib"`
	MaxSessions int64          `json:"max_sessions"`
}

// Runners of the template a run could be placed on, before the resource and session checks.
func (q *Queries) ClaimableRunners(ctx context.Context, templateID int64) ([]ClaimableRunnersRow, error) {
	rows, err := q.db.QueryContext(ctx, claimableRunners, templateID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []ClaimableRunnersRow{}
	for rows.Next() {
		var i ClaimableRunnersRow
		if err := rows.Scan(&i.ID, &i.Name, &i.State, &i.IdleSince, &i.Launched, &i.Vcpu, &i.RamMib, &i.DiskGib, &i.MaxSessions); err != nil {
			return nil, err
		}
		items = append(items, i)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return items, rows.Err()
}

const claimRunner = `-- name: ClaimRunner :one
UPDATE runner SET state = 'busy', idle_since = NULL
WHERE runner.id = ?1 AND runner.state IN ('idle', 'busy') AND runner.cordoned = 0 AND runner.drain_requested = 0
  AND runner.upgrade_sha = ''
  AND runner.template_version = (SELECT t.version FROM runner_template t WHERE t.id = runner.template_id)
  AND (SELECT COUNT(*) FROM run r WHERE r.runner_id = runner.id
       AND r.state IN ('assigned', 'preparing', 'running', 'needs_input', 'pushing', 'cancelling'))
      < (SELECT t.max_sessions FROM runner_template t WHERE t.id = runner.template_id)
  AND (?2 = 0 OR ?2 <=
       COALESCE((SELECT c.cap_vcpu FROM runner_capacity c WHERE c.runner_id = runner.id),
                CASE WHEN runner.launched = 1 THEN runner.vcpu ELSE MIN(6, runner.vcpu * 3 / 4) END)
       - (SELECT COALESCE(SUM(r.need_vcpu), 0) FROM run r WHERE r.runner_id = runner.id
          AND r.state IN ('assigned', 'preparing', 'running', 'needs_input', 'pushing', 'cancelling')))
  AND (?3 = 0 OR ?3 <=
       COALESCE((SELECT c.cap_ram_mib FROM runner_capacity c WHERE c.runner_id = runner.id),
                CASE WHEN runner.launched = 1 THEN MAX(runner.ram_mib - 1024, 0) ELSE MIN(12288, runner.ram_mib * 3 / 4) END)
       - (SELECT COALESCE(SUM(r.need_ram_mib), 0) FROM run r WHERE r.runner_id = runner.id
          AND r.state IN ('assigned', 'preparing', 'running', 'needs_input', 'pushing', 'cancelling')))
  AND (?4 = 0 OR ?4 <=
       COALESCE((SELECT c.cap_disk_gib FROM runner_capacity c WHERE c.runner_id = runner.id),
                CASE WHEN runner.launched = 1 THEN MAX(runner.disk_gib - 8, 0) ELSE MIN(20, runner.disk_gib * 7 / 10) END)
       - (SELECT COALESCE(SUM(r.need_disk_gib), 0) FROM run r WHERE r.runner_id = runner.id
          AND r.state IN ('assigned', 'preparing', 'running', 'needs_input', 'pushing', 'cancelling')))
RETURNING id, name
`

type ClaimRunnerParams struct {
	ID          int64 `json:"id"`
	NeedVcpu    int64 `json:"need_vcpu"`
	NeedRamMib  int64 `json:"need_ram_mib"`
	NeedDiskGib int64 `json:"need_disk_gib"`
}

type ClaimRunnerRow struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// Atomically claim one runner for a run needing need_*; sql.ErrNoRows when it no longer fits.
func (q *Queries) ClaimRunner(ctx context.Context, arg ClaimRunnerParams) (ClaimRunnerRow, error) {
	row := q.db.QueryRowContext(ctx, claimRunner, arg.ID, arg.NeedVcpu, arg.NeedRamMib, arg.NeedDiskGib)
	var i ClaimRunnerRow
	err := row.Scan(&i.ID, &i.Name)
	return i, err
}
