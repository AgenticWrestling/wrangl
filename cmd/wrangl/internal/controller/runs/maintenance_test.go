package runs_test

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/runs"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/events"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/stack"
)

type maint struct {
	t    *testing.T
	d    *db.DB
	rs   *runs.Service
	st   *stack.Service
	task int64
	sqid string
	now  time.Time
}

func newMaint(t *testing.T) *maint {
	t.Helper()
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	for _, q := range []string{
		`INSERT INTO runner_template (name, mise_toml, agent_json, min_vcpu, min_ram_mib, min_disk_gib) VALUES ('go','','{}',1,1,1)`,
		`INSERT INTO runner (name, template_id) VALUES ('r1', 1)`,
		`INSERT INTO project (slug, name, default_template_id) VALUES ('p','p',1)`,
		`INSERT INTO repo (project_id, owner, name) VALUES (1,'o','a')`,
	} {
		if _, err := d.W.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	sq, _ := stack.NewSqids(stack.DefaultSqidsAlphabet)
	st := &stack.Service{DB: d, Sqids: sq, Bus: &events.Bus{}}
	task, err := st.Create(ctx, stack.CreateParams{Project: "p", Title: "do it", Actor: stack.User})
	if err != nil {
		t.Fatal(err)
	}
	m := &maint{t: t, d: d, st: st, task: task.ID, sqid: task.Sqid.String, now: time.Now().UTC()}
	m.rs = &runs.Service{DB: d, Stack: st, Log: slog.Default(), Bus: &events.Bus{}, Now: func() time.Time { return m.now }}
	return m
}

func (m *maint) exec(q string, a ...any) {
	m.t.Helper()
	if _, err := m.d.W.Exec(q, a...); err != nil {
		m.t.Fatalf("%s: %v", q, err)
	}
}

func (m *maint) count(q string, a ...any) (n int) {
	m.t.Helper()
	if err := m.d.R.QueryRow(q, a...).Scan(&n); err != nil {
		m.t.Fatal(err)
	}
	return n
}

// startRun queues a run for the task and puts it on runner r1, running since startedAgo.
func (m *maint) startRun(startedAgo time.Duration) int64 {
	m.t.Helper()
	run, err := m.st.Queue(context.Background(), m.sqid)
	if err != nil {
		m.t.Fatal(err)
	}
	m.exec(`UPDATE run SET state='running', runner_id=1, current_task_id=?, started_at=? WHERE id=?`,
		m.task, m.now.Add(-startedAgo).Format("2006-01-02T15:04:05.000Z"), run.ID)
	m.exec(`UPDATE task SET state='in_progress' WHERE id=?`, m.task)
	return run.ID
}

func TestLostRunnerRetriesOnceThenAsksTheOwner(t *testing.T) {
	m := newMaint(t)
	ctx := context.Background()
	m.startRun(time.Hour)
	m.exec(`UPDATE runner SET state='offline', offline_since=?`, m.now.Add(-2*runs.LostAfter).Format("2006-01-02T15:04:05.000Z"))

	if err := m.rs.SweepLost(ctx); err != nil {
		t.Fatal(err)
	}
	if n := m.count(`SELECT COUNT(*) FROM run WHERE state='failed' AND error='runner lost'`); n != 1 {
		t.Fatalf("lost runs: %d", n)
	}
	if n := m.count(`SELECT COUNT(*) FROM run WHERE state='queued'`); n != 1 {
		t.Fatalf("first loss must queue a retry, queued runs: %d", n)
	}
	if n := m.count(`SELECT COUNT(*) FROM question`); n != 0 {
		t.Fatalf("first loss must not bother the owner: %d questions", n)
	}

	// The retry is lost too: no third run on its own; the owner is asked.
	m.exec(`UPDATE run SET state='running', runner_id=1, current_task_id=? WHERE state='queued'`, m.task)
	if err := m.rs.SweepLost(ctx); err != nil {
		t.Fatal(err)
	}
	if n := m.count(`SELECT COUNT(*) FROM run WHERE state='queued'`); n != 0 {
		t.Fatalf("second loss queued another run on its own")
	}
	qs, err := m.rs.Questions(ctx, false)
	if err != nil || len(qs) != 1 || qs[0].Kind != "approval" {
		t.Fatalf("questions: %+v %v", qs, err)
	}
	// Approving tries again.
	if err := m.rs.Answer(ctx, qs[0].ID, "approve", ""); err != nil {
		t.Fatal(err)
	}
	if n := m.count(`SELECT COUNT(*) FROM run WHERE state='queued'`); n != 1 {
		t.Fatalf("approve must queue a run, queued: %d", n)
	}
}

func TestSilentRunIsRaisedOnce(t *testing.T) {
	m := newMaint(t)
	ctx := context.Background()
	id := m.startRun(runs.StuckAfter + time.Minute)

	if err := m.rs.SweepStuck(ctx); err != nil {
		t.Fatal(err)
	}
	m.rs.SweepStuck(ctx) // not again while the question stands
	qs, _ := m.rs.Questions(ctx, false)
	if len(qs) != 1 || qs[0].RunID != id || qs[0].Kind != "approval" {
		t.Fatalf("questions: %+v", qs)
	}
	var op string
	m.d.R.QueryRow(`SELECT json_extract(payload_json,'$.op') FROM question`).Scan(&op)
	if op != "cancel_run" {
		t.Fatalf("op %q", op)
	}
}

func TestActiveRunIsNotStuck(t *testing.T) {
	m := newMaint(t)
	id := m.startRun(2 * runs.StuckAfter)
	// A log line a minute ago means the run is alive.
	m.exec(`INSERT INTO log_line (run_id, seq, ts, stream, message) VALUES (?, 1, ?, 'agent', 'working')`,
		id, m.now.Add(-time.Minute).Format("2006-01-02T15:04:05.000Z"))
	if err := m.rs.SweepStuck(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := m.count(`SELECT COUNT(*) FROM question`); n != 0 {
		t.Fatalf("a run with recent output was reported: %d", n)
	}
	// A run waiting for an answer is the owner's move, not stuck.
	m.exec(`UPDATE run SET state='needs_input' WHERE id=?`, id)
	m.exec(`DELETE FROM log_line`)
	m.rs.SweepStuck(context.Background())
	if n := m.count(`SELECT COUNT(*) FROM question`); n != 0 {
		t.Fatalf("a run in needs_input was reported: %d", n)
	}
}
