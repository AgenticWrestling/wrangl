// Hand-written in sqlc's style (sqlc is not installed here); source: queries/remote.sql.

package dbgen

import (
	"context"
	"database/sql"
)

type TemplateConnection struct {
	TemplateID       int64         `json:"template_id"`
	Mode             string        `json:"mode"`
	DriverTemplateID sql.NullInt64 `json:"driver_template_id"`
	ConnectionJson   string        `json:"connection_json"`
}

type RemoteTarget struct {
	ID                 int64          `json:"id"`
	Name               string         `json:"name"`
	TemplateID         int64          `json:"template_id"`
	InstanceID         sql.NullString `json:"instance_id"`
	Host               string         `json:"host"`
	State              string         `json:"state"`
	ProbeFailures      int64          `json:"probe_failures"`
	LastError          string         `json:"last_error"`
	LastProbeAt        sql.NullString `json:"last_probe_at"`
	IdleSince          sql.NullString `json:"idle_since"`
	OfflineSince       sql.NullString `json:"offline_since"`
	LaunchedForProject sql.NullInt64  `json:"launched_for_project"`
	CreatedAt          string         `json:"created_at"`
	TerminatedAt       sql.NullString `json:"terminated_at"`
}

const remoteTargetCols = "id, name, template_id, instance_id, host, state, probe_failures, last_error, last_probe_at, idle_since, offline_since, launched_for_project, created_at, terminated_at"

func scanTarget(row interface{ Scan(...any) error }) (RemoteTarget, error) {
	var i RemoteTarget
	err := row.Scan(&i.ID, &i.Name, &i.TemplateID, &i.InstanceID, &i.Host, &i.State, &i.ProbeFailures, &i.LastError,
		&i.LastProbeAt, &i.IdleSince, &i.OfflineSince, &i.LaunchedForProject, &i.CreatedAt, &i.TerminatedAt)
	return i, err
}

func (q *Queries) targets(ctx context.Context, query string, args ...any) ([]RemoteTarget, error) {
	rows, err := q.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RemoteTarget{}
	for rows.Next() {
		i, err := scanTarget(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// name: SetTemplateConnection :exec
func (q *Queries) SetTemplateConnection(ctx context.Context, arg TemplateConnection) error {
	_, err := q.db.ExecContext(ctx, `INSERT INTO template_connection (template_id, mode, driver_template_id, connection_json) VALUES (?1, ?2, ?3, ?4)
ON CONFLICT (template_id) DO UPDATE SET mode = excluded.mode, driver_template_id = excluded.driver_template_id, connection_json = excluded.connection_json`,
		arg.TemplateID, arg.Mode, arg.DriverTemplateID, arg.ConnectionJson)
	return err
}

// name: GetTemplateConnection :one (no row: a local template)
func (q *Queries) GetTemplateConnection(ctx context.Context, templateID int64) (TemplateConnection, error) {
	var i TemplateConnection
	err := q.db.QueryRowContext(ctx, `SELECT template_id, mode, driver_template_id, connection_json FROM template_connection WHERE template_id = ?`, templateID).
		Scan(&i.TemplateID, &i.Mode, &i.DriverTemplateID, &i.ConnectionJson)
	if err == sql.ErrNoRows {
		return TemplateConnection{TemplateID: templateID, Mode: "local", ConnectionJson: "{}"}, nil
	}
	return i, err
}

// name: ListRemoteTemplateIDs :many
func (q *Queries) ListRemoteTemplateIDs(ctx context.Context) ([]int64, error) {
	rows, err := q.db.QueryContext(ctx, `SELECT template_id FROM template_connection WHERE mode = 'remote'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

type CreateRemoteTargetParams struct {
	Name               string
	TemplateID         int64
	InstanceID         sql.NullString
	Host               string
	LaunchedForProject sql.NullInt64
}

// name: CreateRemoteTarget :one
func (q *Queries) CreateRemoteTarget(ctx context.Context, a CreateRemoteTargetParams) (RemoteTarget, error) {
	return scanTarget(q.db.QueryRowContext(ctx, `INSERT INTO remote_target (name, template_id, instance_id, host, launched_for_project) VALUES (?, ?, ?, ?, ?) RETURNING `+remoteTargetCols,
		a.Name, a.TemplateID, a.InstanceID, a.Host, a.LaunchedForProject))
}

// name: GetRemoteTarget :one
func (q *Queries) GetRemoteTarget(ctx context.Context, id int64) (RemoteTarget, error) {
	return scanTarget(q.db.QueryRowContext(ctx, `SELECT `+remoteTargetCols+` FROM remote_target WHERE id = ?`, id))
}

// name: GetRemoteTargetByName :one
func (q *Queries) GetRemoteTargetByName(ctx context.Context, name string) (RemoteTarget, error) {
	return scanTarget(q.db.QueryRowContext(ctx, `SELECT `+remoteTargetCols+` FROM remote_target WHERE name = ?`, name))
}

// name: ListRemoteTargets :many (live ones unless all)
func (q *Queries) ListRemoteTargets(ctx context.Context, all bool) ([]RemoteTarget, error) {
	if all {
		return q.targets(ctx, `SELECT `+remoteTargetCols+` FROM remote_target ORDER BY id`)
	}
	return q.targets(ctx, `SELECT `+remoteTargetCols+` FROM remote_target WHERE state <> 'terminated' ORDER BY id`)
}

type SetRemoteTargetStateParams struct {
	State         string
	ProbeFailures int64
	LastError     string
	Now           string
	IdleSince     sql.NullString
	OfflineSince  sql.NullString
	ID            int64
}

// name: SetRemoteTargetState :exec
func (q *Queries) SetRemoteTargetState(ctx context.Context, a SetRemoteTargetStateParams) error {
	_, err := q.db.ExecContext(ctx, `UPDATE remote_target SET state = ?, probe_failures = ?, last_error = ?, last_probe_at = ?, idle_since = ?, offline_since = ? WHERE id = ?`,
		a.State, a.ProbeFailures, a.LastError, a.Now, a.IdleSince, a.OfflineSince, a.ID)
	return err
}

// name: MarkRemoteTargetTerminated :exec
func (q *Queries) MarkRemoteTargetTerminated(ctx context.Context, id int64, now string) error {
	_, err := q.db.ExecContext(ctx, `UPDATE remote_target SET state = 'terminated', terminated_at = ?, idle_since = NULL WHERE id = ?`, now, id)
	return err
}

// name: SetRemoteTargetHost :exec
func (q *Queries) SetRemoteTargetHost(ctx context.Context, id int64, host string) error {
	_, err := q.db.ExecContext(ctx, `UPDATE remote_target SET host = ? WHERE id = ?`, host, id)
	return err
}

const activeRunStates = `('assigned', 'preparing', 'running', 'needs_input', 'pushing', 'cancelling')`

// name: CountTargetSessions :one (runs using the target that are still active)
func (q *Queries) CountTargetSessions(ctx context.Context, targetID int64) (int64, error) {
	var n int64
	err := q.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM run_target t JOIN run r ON r.id = t.run_id WHERE t.target_id = ? AND r.state IN `+activeRunStates, targetID).Scan(&n)
	return n, err
}

// name: ActiveRunsOnTarget :many
func (q *Queries) ActiveRunsOnTarget(ctx context.Context, targetID int64) ([]int64, error) {
	rows, err := q.db.QueryContext(ctx, `SELECT r.id FROM run_target t JOIN run r ON r.id = t.run_id WHERE t.target_id = ? AND r.state IN `+activeRunStates, targetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// name: ClaimTarget :one — a ready target of the template with a free session slot, busiest first
// (fewer instances); sql.ErrNoRows when none.
func (q *Queries) ClaimTarget(ctx context.Context, templateID int64) (RemoteTarget, error) {
	return scanTarget(q.db.QueryRowContext(ctx, `SELECT `+remoteTargetCols+` FROM remote_target g
WHERE g.template_id = ? AND g.state = 'ready'
  AND (SELECT COUNT(*) FROM run_target t JOIN run r ON r.id = t.run_id WHERE t.target_id = g.id AND r.state IN `+activeRunStates+`)
      < (SELECT max_sessions FROM runner_template WHERE id = g.template_id)
ORDER BY (SELECT COUNT(*) FROM run_target t JOIN run r ON r.id = t.run_id WHERE t.target_id = g.id AND r.state IN `+activeRunStates+`) DESC, g.id
LIMIT 1`, templateID))
}

// name: BindRunTarget :exec
func (q *Queries) BindRunTarget(ctx context.Context, runID, targetID int64) error {
	_, err := q.db.ExecContext(ctx, `INSERT INTO run_target (run_id, target_id) VALUES (?, ?) ON CONFLICT (run_id) DO UPDATE SET target_id = excluded.target_id`, runID, targetID)
	return err
}

// name: UnbindRunTarget :exec
func (q *Queries) UnbindRunTarget(ctx context.Context, runID int64) error {
	_, err := q.db.ExecContext(ctx, `DELETE FROM run_target WHERE run_id = ?`, runID)
	return err
}

// name: GetRunTarget :one
func (q *Queries) GetRunTarget(ctx context.Context, runID int64) (int64, error) {
	var id int64
	err := q.db.QueryRowContext(ctx, `SELECT target_id FROM run_target WHERE run_id = ?`, runID).Scan(&id)
	return id, err
}

// name: TargetDemand :many — per remote template: queued runs wanting it, free slots on live
// (launching or ready) targets, live targets.
type TargetDemandRow struct {
	TemplateID  int64
	Name        string
	MaxRunners  int64
	MaxSessions int64
	Demand      int64
	Supply      int64
	Live        int64
}

func (q *Queries) TargetDemand(ctx context.Context) ([]TargetDemandRow, error) {
	rows, err := q.db.QueryContext(ctx, `SELECT t.id, t.name, t.max_runners, t.max_sessions,
  (SELECT COUNT(*) FROM run r JOIN task k ON k.id = r.task_id JOIN project p ON p.id = k.project_id
     WHERE r.state = 'queued' AND COALESCE(r.template_id, k.template_id, p.default_template_id) = t.id),
  (SELECT COALESCE(SUM(MAX(t.max_sessions - (SELECT COUNT(*) FROM run_target rt JOIN run r ON r.id = rt.run_id
        WHERE rt.target_id = g.id AND r.state IN `+activeRunStates+`), 0)), 0)
     FROM remote_target g WHERE g.template_id = t.id AND g.state IN ('launching', 'ready')),
  (SELECT COUNT(*) FROM remote_target g WHERE g.template_id = t.id AND g.state <> 'terminated')
FROM runner_template t JOIN template_connection c ON c.template_id = t.id AND c.mode = 'remote' ORDER BY t.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TargetDemandRow
	for rows.Next() {
		var r TargetDemandRow
		if err := rows.Scan(&r.TemplateID, &r.Name, &r.MaxRunners, &r.MaxSessions, &r.Demand, &r.Supply, &r.Live); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// name: CountProjectTargets :one — live targets launched for a project on a template.
func (q *Queries) CountProjectTargets(ctx context.Context, projectID, templateID int64) (int64, error) {
	var n int64
	err := q.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM remote_target WHERE launched_for_project = ? AND template_id = ? AND state <> 'terminated'`, projectID, templateID).Scan(&n)
	return n, err
}
