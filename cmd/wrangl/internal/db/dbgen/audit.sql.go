// Hand-written in sqlc's style (sqlc is not installed here); source: queries/audit.sql.

package dbgen

import "context"

func (q *Queries) GetProjectCitationAudit(ctx context.Context, id int64) (int64, error) {
	var v int64
	err := q.db.QueryRowContext(ctx, `SELECT citation_audit FROM project WHERE id = ?`, id).Scan(&v)
	return v, err
}

func (q *Queries) SetProjectCitationAudit(ctx context.Context, on int64, id int64) error {
	_, err := q.db.ExecContext(ctx, `UPDATE project SET citation_audit = ? WHERE id = ?`, on, id)
	return err
}

// GetPRAudit returns the audited head sha and the audit result JSON ("" = none).
func (q *Queries) GetPRAudit(ctx context.Context, id int64) (sha, js string, err error) {
	err = q.db.QueryRowContext(ctx, `SELECT audit_sha, audit_json FROM pull_request WHERE id = ?`, id).Scan(&sha, &js)
	return
}

func (q *Queries) SetPRAudit(ctx context.Context, sha, js string, id int64) error {
	_, err := q.db.ExecContext(ctx, `UPDATE pull_request SET audit_sha = ?, audit_json = ? WHERE id = ?`, sha, js, id)
	return err
}

func (q *Queries) GetProjectAutoResolve(ctx context.Context, id int64) (int64, error) {
	var v int64
	err := q.db.QueryRowContext(ctx, `SELECT auto_resolve FROM project WHERE id = ?`, id).Scan(&v)
	return v, err
}

func (q *Queries) SetProjectAutoResolve(ctx context.Context, on int64, id int64) error {
	_, err := q.db.ExecContext(ctx, `UPDATE project SET auto_resolve = ? WHERE id = ?`, on, id)
	return err
}
