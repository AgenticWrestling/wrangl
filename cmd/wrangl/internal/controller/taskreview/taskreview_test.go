package taskreview_test

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/runs"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/taskreview"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/events"
)

// A task completed by a run that is still alive (needs_input on a system question) must
// show its review card now, not once the run finishes.
func TestReviewCardWhileRunAlive(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	for _, q := range []string{
		`INSERT INTO runner_template (name, mise_toml, agent_json, min_vcpu, min_ram_mib, min_disk_gib) VALUES ('go','','{}',1,1,1)`,
		`INSERT INTO project (slug, name) VALUES ('p','p')`,
		`INSERT INTO task (project_id, title, rank, state, completed_run_id, completion_summary) VALUES (1, 'done work', 'A', 'in_review', 1, 'all done')`,
		`INSERT INTO run (task_id, state) VALUES (1, 'needs_input')`,
	} {
		if _, err := d.W.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	bus := &events.Bus{}
	rs := &runs.Service{DB: d, Bus: bus, Log: log}
	s := &taskreview.Service{DB: d, Runs: rs, Bus: bus, Log: log}
	count := func() (n int) {
		d.R.QueryRow(`SELECT COUNT(*) FROM question WHERE kind='task_review' AND state='open' AND task_id=1`).Scan(&n)
		return
	}
	s.Sync(ctx)
	s.Sync(ctx) // deduped
	if n := count(); n != 1 {
		t.Fatalf("open review cards while run alive: %d, want 1", n)
	}
	var st string
	d.R.QueryRow(`SELECT state FROM run WHERE id=1`).Scan(&st)
	if st != "needs_input" {
		t.Fatalf("run state %s", st)
	}
	// Task moves on: the card closes.
	d.W.Exec(`UPDATE task SET state='done' WHERE id=1`)
	s.Sync(ctx)
	if n := count(); n != 0 {
		t.Fatalf("card not closed: %d", n)
	}
}
