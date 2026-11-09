package db_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
)

func open(t *testing.T) (*db.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "wrangl.db")
	d, err := db.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d, path
}

func exec(t *testing.T, d *db.DB, q string, args ...any) {
	t.Helper()
	if _, err := d.W.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func TestSchemaEndToEnd(t *testing.T) {
	d, path := open(t)
	ctx := context.Background()
	exec(t, d, `INSERT INTO runner_template (name, mise_toml, agent_json, min_vcpu, min_ram_mib, min_disk_gib) VALUES ('go','[tools]','{}',4,16384,100)`)
	p, err := d.Q.CreateProject(ctx, dbgen.CreateProjectParams{Slug: "dh", Name: "devhost", DefaultTemplateID: sql.NullInt64{Int64: 1, Valid: true}})
	if err != nil {
		t.Fatal(err)
	}
	exec(t, d, `INSERT INTO repo (project_id, owner, name) VALUES (?, 'njreid', 'devhost')`, p.ID)
	exec(t, d, `INSERT INTO task (project_id, repo_id, template_id, title, prompt, rank) VALUES (?, 1, 1, 'add cobra tree', 'do it', 'V')`, p.ID)
	exec(t, d, `INSERT INTO run (task_id) VALUES (1)`)
	for i, m := range []string{"compiling cobra commands", "disk usage at 91 percent"} {
		if err := d.Q.InsertLogLine(ctx, dbgen.InsertLogLineParams{RunID: 1, Seq: int64(i + 1), Ts: "now", Stream: "agent", Level: "info", Message: m}); err != nil {
			t.Fatal(err)
		}
	}
	// Replayed JetStream delivery: same (run, seq) is ignored, not duplicated.
	if err := d.Q.InsertLogLine(ctx, dbgen.InsertLogLineParams{RunID: 1, Seq: 2, Ts: "now", Stream: "agent", Level: "info", Message: "disk usage at 91 percent"}); err != nil {
		t.Fatal(err)
	}

	runs, err := d.RQ.ListActiveRuns(ctx)
	if err != nil || len(runs) != 1 || runs[0].TaskTitle != "add cobra tree" {
		t.Fatalf("active runs: %+v %v", runs, err)
	}
	hits, err := d.RQ.SearchLogs(ctx, dbgen.SearchLogsParams{Message: "disk", Limit: 10})
	if err != nil || len(hits) != 1 || hits[0].Message != "disk usage at 91 percent" {
		t.Fatalf("fts: %+v %v", hits, err)
	}
	if _, err := d.W.Exec(`INSERT INTO task (project_id, repo_id, template_id, title, prompt, rank) VALUES (1,1,99,'x','y','W')`); err == nil {
		t.Fatal("foreign key not enforced")
	}
	if _, err := d.W.Exec(`UPDATE run SET state = 'bogus' WHERE id = 1`); err == nil {
		t.Fatal("CHECK not enforced")
	}
	d.Close()
	var out bytes.Buffer
	if err := db.Migrate(ctx, path, "status", &out); err != nil || !strings.Contains(out.String(), "applied") {
		t.Fatalf("status: %q %v", out.String(), err)
	}
	if err := db.Migrate(ctx, path, "down", &out); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx, path, "up", &out); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx, path, "sideways", &out); err == nil {
		t.Fatal("want error for bad direction")
	}
}

func TestTxRollsBack(t *testing.T) {
	d, _ := open(t)
	ctx := context.Background()
	boom := errors.New("boom")
	err := d.Tx(ctx, func(q *dbgen.Queries) error {
		if _, err := q.CreateProject(ctx, dbgen.CreateProjectParams{Slug: "a", Name: "A"}); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
	if ps, _ := d.RQ.ListProjects(ctx, db.ProjectsActive); len(ps) != 0 {
		t.Fatal("rolled-back insert is visible")
	}
	if err := d.Tx(ctx, func(q *dbgen.Queries) error {
		_, err := q.CreateProject(ctx, dbgen.CreateProjectParams{Slug: "a", Name: "A"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if ps, _ := d.RQ.ListProjects(ctx, db.ProjectsActive); len(ps) != 1 {
		t.Fatal("committed insert not visible to reader")
	}
}

func TestPlanKinds(t *testing.T) {
	d, _ := open(t)
	for _, s := range []string{
		`INSERT INTO project (slug, name) VALUES ('a','A')`,
		`INSERT INTO task (project_id, rank, title) VALUES (1, 'V', 't')`,
		`INSERT INTO run (task_id, kind) VALUES (1, 'plan')`,
		`INSERT INTO question (task_id, run_id, kind, prompt, payload_json) VALUES (1, 1, 'plan_review', 'accept split?', '{"children":[]}')`,
	} {
		exec(t, d, s)
	}
	if _, err := d.W.Exec(`INSERT INTO run (task_id, kind) VALUES (1, 'bogus')`); err == nil {
		t.Fatal("run.kind CHECK not enforced")
	}
}

func TestMetaSetIfAbsent(t *testing.T) {
	d, _ := open(t)
	ctx := context.Background()
	d.Q.SetMetaIfAbsent(ctx, dbgen.SetMetaIfAbsentParams{Key: "k", Value: "first"})
	d.Q.SetMetaIfAbsent(ctx, dbgen.SetMetaIfAbsentParams{Key: "k", Value: "second"})
	if v, err := d.RQ.GetMeta(ctx, "k"); err != nil || v != "first" {
		t.Fatalf("%q %v", v, err)
	}
}

// sqlc silently leaves sqlc.arg()/sqlc.narg() unrewritten in some positions (e.g. inside
// CASE WHEN ... IN (...)); SQLite then fails at runtime. Catch it at test time.
func TestGeneratedSQLHasNoUnrewrittenArgs(t *testing.T) {
	files, _ := filepath.Glob("dbgen/*.sql.go")
	if len(files) == 0 {
		t.Fatal("no generated files found")
	}
	for _, f := range files {
		b, _ := os.ReadFile(f)
		for i, line := range strings.Split(string(b), "\n") {
			if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "--") {
				continue // comments may mention the macros
			}
			if strings.Contains(line, "sqlc.arg(") || strings.Contains(line, "sqlc.narg(") || strings.Contains(line, "sqlc.embed(") {
				t.Errorf("%s:%d: unrewritten sqlc macro: %s", f, i+1, strings.TrimSpace(line))
			}
		}
	}
}

// An untyped (interface{}) sqlc parameter left at its zero value becomes NULL, and
// `NULL = ”` is never true, so optional filters silently match nothing. CAST every such
// parameter in the query so sqlc gives it a real type.
func TestGeneratedParamsAreTyped(t *testing.T) {
	files, _ := filepath.Glob("dbgen/*.sql.go")
	field := regexp.MustCompile(`^\t[A-Z]\w*\s+interface\{\}\s+` + "`")
	for _, f := range files {
		b, _ := os.ReadFile(f)
		for i, line := range strings.Split(string(b), "\n") {
			if field.MatchString(line) {
				t.Errorf("%s:%d: untyped parameter (CAST it in the query): %s", f, i+1, strings.TrimSpace(line))
			}
		}
	}
}

func TestPlanningTaskTypesAndTargetDate(t *testing.T) {
	d, _ := open(t)
	exec(t, d, `INSERT INTO runner_template (name, mise_toml, agent_json, min_vcpu, min_ram_mib, min_disk_gib) VALUES ('go','[tools]','{}',4,16384,100)`)
	exec(t, d, `INSERT INTO project (slug, name) VALUES ('p', 'p')`)
	for i, typ := range []string{"release", "milestone", "feature", "code"} {
		exec(t, d, `INSERT INTO task (project_id, title, rank, task_type, target_date) VALUES (1, ?, ?, ?, '2026-12-01')`, typ, string(rune('A'+i)), typ)
	}
	// The widened CHECK is visible to every connection, and still rejects unknown types.
	var n int
	if err := d.R.QueryRow(`SELECT COUNT(*) FROM task WHERE task_type IN ('release','milestone','feature') AND target_date = '2026-12-01'`).Scan(&n); err != nil || n != 3 {
		t.Fatalf("planning rows: %d %v", n, err)
	}
	if _, err := d.W.Exec(`INSERT INTO task (project_id, title, rank, task_type) VALUES (1, 'x', 'Z', 'epic')`); err == nil {
		t.Fatal("unknown task type accepted")
	}
}

// Migration 00026 wraps each template's agent_json as an agent: 'claude' for go-claude, one
// agent for identical blocks, a template-name suffix when the same binary has different blocks,
// and the project's default agent follows its default template. The data statements are
// replayed over existing templates (the migration itself ran on an empty database).
func TestAgentMigrationWrapsTemplates(t *testing.T) {
	d, _ := open(t)
	ctx := context.Background()
	raw, err := os.ReadFile("migrations/00026_agent_definitions.sql")
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	body = body[strings.Index(body, "-- Wrap every"):strings.Index(body, "-- +goose Down")]
	exec(t, d, `DELETE FROM template_agent`)
	exec(t, d, `DELETE FROM agent`)
	exec(t, d, `INSERT INTO runner_template (name, mise_toml, agent_json, min_vcpu, min_ram_mib, min_disk_gib) VALUES
	  ('go-claude','','{"start":["claude","-p"]}',2,4096,20),
	  ('rust-claude','','{"start":["claude","-p"]}',2,4096,20),
	  ('claude-int','','{"start":["claude"],"drive":"interactive"}',2,4096,20),
	  ('gem','','{"start":["agy","-p"]}',2,4096,20),
	  ('host','','{}',2,4096,20)`)
	exec(t, d, `INSERT INTO project (slug, name, default_template_id) VALUES ('p','P',1)`)
	for _, stmt := range strings.Split(body, ";\n\n") {
		if s := strings.TrimSpace(stmt); s != "" {
			exec(t, d, s)
		}
	}
	names := map[string][]string{}
	for _, tn := range []string{"go-claude", "rust-claude", "claude-int", "gem", "host"} {
		tp, _ := d.RQ.GetTemplateByName(ctx, tn)
		as, _ := d.RQ.ListTemplateAgents(ctx, tp.ID)
		for _, a := range as {
			names[tn] = append(names[tn], a.Name)
		}
	}
	want := map[string]string{"go-claude": "claude", "rust-claude": "claude", "claude-int": "claude-claude-int", "gem": "antigravity"}
	for tn, w := range want {
		if len(names[tn]) != 1 || names[tn][0] != w {
			t.Errorf("%s: agents %v, want [%s]", tn, names[tn], w)
		}
	}
	if len(names["host"]) != 0 {
		t.Errorf("host (no agent) got %v", names["host"])
	}
	p, _ := d.RQ.GetProjectBySlug(ctx, "p")
	if id, err := d.RQ.GetProjectDefaultAgent(ctx, p.ID); err != nil || !id.Valid {
		t.Errorf("project default agent not set: %v %v", id, err)
	}
}

// Migration 00045 drops project_template_limit; its down recreates and reseeds the table (1 for a
// project's default template, 0 for every other) the way 00025 did.
func TestProjectTemplateLimitDownRecreatesIt(t *testing.T) {
	d, _ := open(t)
	ctx := context.Background()
	var n int
	d.R.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = 'project_template_limit'`).Scan(&n)
	if n != 0 {
		t.Fatal("the table should be gone after the up migration")
	}
	exec(t, d, `INSERT INTO runner_template (name, mise_toml, agent_json, min_vcpu, min_ram_mib, min_disk_gib) VALUES ('go','[tools]','{}',2,4096,20), ('mac','[tools]','{}',2,4096,20)`)
	exec(t, d, `INSERT INTO project (slug, name, default_template_id) VALUES ('a','A',1), ('b','B',2)`)
	raw, err := os.ReadFile("migrations/00045_drop_project_template_limit.sql")
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	body = body[strings.Index(body, "-- +goose Down")+len("-- +goose Down"):]
	if _, err := d.W.ExecContext(ctx, body); err != nil {
		t.Fatal(err)
	}
	var sum, rows int
	d.R.QueryRow(`SELECT COUNT(*), SUM(max_instances) FROM project_template_limit`).Scan(&rows, &sum)
	if rows != 4 || sum != 2 {
		t.Fatalf("reseed: %d rows, sum %d", rows, sum)
	}
}
