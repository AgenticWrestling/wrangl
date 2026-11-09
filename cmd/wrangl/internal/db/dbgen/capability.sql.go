package dbgen

import "context"

func (q *Queries) DeleteTemplateCapsBySource(ctx context.Context, templateID int64, source string) error {
	_, err := q.db.ExecContext(ctx, `DELETE FROM template_capability WHERE template_id = ? AND source = ?`, templateID, source)
	return err
}

func (q *Queries) UpsertTemplateCap(ctx context.Context, templateID int64, key, value, source string) error {
	_, err := q.db.ExecContext(ctx, `INSERT INTO template_capability (template_id, key, value, source) VALUES (?, ?, ?, ?)
ON CONFLICT (template_id, key) DO UPDATE SET value = excluded.value, source = excluded.source`, templateID, key, value, source)
	return err
}

// TemplateCap is a tag a template provides.
type TemplateCap struct {
	TemplateID int64  `json:"template_id"`
	Key        string `json:"key"`
	Value      string `json:"value"`
	Source     string `json:"source"`
}

func (q *Queries) ListTemplateCaps(ctx context.Context) ([]TemplateCap, error) {
	rows, err := q.db.QueryContext(ctx, `SELECT template_id, key, value, source FROM template_capability ORDER BY template_id, key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TemplateCap{}
	for rows.Next() {
		var c TemplateCap
		if err := rows.Scan(&c.TemplateID, &c.Key, &c.Value, &c.Source); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (q *Queries) DeleteRunnerCapsBySource(ctx context.Context, runnerID int64, source string) error {
	_, err := q.db.ExecContext(ctx, `DELETE FROM runner_capability WHERE runner_id = ? AND source = ?`, runnerID, source)
	return err
}

func (q *Queries) UpsertRunnerCap(ctx context.Context, runnerID int64, key, value, source string) error {
	_, err := q.db.ExecContext(ctx, `INSERT INTO runner_capability (runner_id, key, value, source) VALUES (?, ?, ?, ?)
ON CONFLICT (runner_id, key) DO UPDATE SET value = excluded.value, source = excluded.source`, runnerID, key, value, source)
	return err
}

func (q *Queries) AddRunnerCapIfAbsent(ctx context.Context, runnerID int64, key, value, source string) error {
	_, err := q.db.ExecContext(ctx, `INSERT OR IGNORE INTO runner_capability (runner_id, key, value, source) VALUES (?, ?, ?, ?)`, runnerID, key, value, source)
	return err
}

func (q *Queries) DeleteRunnerCap(ctx context.Context, runnerID int64, key string) error {
	_, err := q.db.ExecContext(ctx, `DELETE FROM runner_capability WHERE runner_id = ? AND key = ?`, runnerID, key)
	return err
}

// RunnerCap is a tag a runner carries.
type RunnerCap struct {
	RunnerID int64  `json:"runner_id"`
	Key      string `json:"key"`
	Value    string `json:"value"`
	Source   string `json:"source"`
}

func (q *Queries) ListRunnerCaps(ctx context.Context) ([]RunnerCap, error) {
	rows, err := q.db.QueryContext(ctx, `SELECT runner_id, key, value, source FROM runner_capability ORDER BY runner_id, key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RunnerCap{}
	for rows.Next() {
		var c RunnerCap
		if err := rows.Scan(&c.RunnerID, &c.Key, &c.Value, &c.Source); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (q *Queries) SetRequirement(ctx context.Context, kind string, ownerID int64, key, op, value string) error {
	_, err := q.db.ExecContext(ctx, `INSERT INTO requirement (owner_kind, owner_id, key, op, value) VALUES (?, ?, ?, ?, ?)
ON CONFLICT (owner_kind, owner_id, key) DO UPDATE SET op = excluded.op, value = excluded.value`, kind, ownerID, key, op, value)
	return err
}

func (q *Queries) DeleteRequirement(ctx context.Context, kind string, ownerID int64, key string) error {
	_, err := q.db.ExecContext(ctx, `DELETE FROM requirement WHERE owner_kind = ? AND owner_id = ? AND key = ?`, kind, ownerID, key)
	return err
}

func (q *Queries) DeleteRequirements(ctx context.Context, kind string, ownerID int64) error {
	_, err := q.db.ExecContext(ctx, `DELETE FROM requirement WHERE owner_kind = ? AND owner_id = ?`, kind, ownerID)
	return err
}

// RequirementRow is one stored requirement.
type RequirementRow struct {
	Key   string `json:"key"`
	Op    string `json:"op"`
	Value string `json:"value"`
}

func (q *Queries) ListRequirements(ctx context.Context, kind string, ownerID int64) ([]RequirementRow, error) {
	rows, err := q.db.QueryContext(ctx, `SELECT key, op, value FROM requirement WHERE owner_kind = ? AND owner_id = ? ORDER BY key`, kind, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RequirementRow{}
	for rows.Next() {
		var r RequirementRow
		if err := rows.Scan(&r.Key, &r.Op, &r.Value); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (q *Queries) AddTemplateCapIfAbsent(ctx context.Context, templateID int64, key, value, source string) error {
	_, err := q.db.ExecContext(ctx, `INSERT OR IGNORE INTO template_capability (template_id, key, value, source) VALUES (?, ?, ?, ?)`, templateID, key, value, source)
	return err
}
