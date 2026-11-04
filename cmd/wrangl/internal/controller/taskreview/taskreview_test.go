package taskreview_test

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/runs"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/taskreview"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/events"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/stack"
)

type env struct {
	d  *db.DB
	st *stack.Service
	rs *runs.Service
	s  *taskreview.Service
}

func setup(t *testing.T) *env {
	t.Helper()
	d, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	for _, q := range []string{
		`INSERT INTO runner_template (name, mise_toml, agent_json, min_vcpu, min_ram_mib, min_disk_gib) VALUES ('go','','{}',1,1,1)`,
		`INSERT INTO project (slug, name, default_template_id) VALUES ('p','p',1)`,
		`INSERT INTO repo (project_id, owner, name) VALUES (1,'o','a')`,
	} {
		if _, err := d.W.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	sq, _ := stack.NewSqids(stack.DefaultSqidsAlphabet)
	bus := &events.Bus{}
	st := &stack.Service{DB: d, Sqids: sq, Bus: bus}
	rs := &runs.Service{DB: d, Stack: st, Log: slog.Default(), Bus: bus, Now: time.Now, AnswerHandlers: map[string]runs.AnswerHandler{}}
	s := &taskreview.Service{DB: d, Runs: rs, Stack: st, Log: slog.Default()}
	rs.AnswerHandlers[taskreview.Kind] = s.Answer
	return &env{d, st, rs, s}
}

// review makes a task the agent completed in review (run succeeded) and returns it.
func (e *env) review(t *testing.T, title string) (sqid string, id, run int64) {
	t.Helper()
	ctx := context.Background()
	task, err := e.st.Create(ctx, stack.CreateParams{Project: "p", Title: title, Prompt: "build it", Actor: stack.User})
	if err != nil {
		t.Fatal(err)
	}
	r, err := e.st.Queue(ctx, task.Sqid.String)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.d.W.Exec(`UPDATE task SET state='in_review', completion_summary='already built', completed_run_id=? WHERE id=?`, r.ID, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.d.W.Exec(`UPDATE run SET state='succeeded' WHERE id=?`, r.ID); err != nil {
		t.Fatal(err)
	}
	return task.Sqid.String, task.ID, r.ID
}

func (e *env) open(t *testing.T) []int64 {
	t.Helper()
	qs, err := e.rs.Questions(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for _, q := range qs {
		if q.Kind == taskreview.Kind {
			ids = append(ids, q.ID)
		}
	}
	return ids
}

func (e *env) state(t *testing.T, sqid string) (string, string) {
	t.Helper()
	cur, err := e.st.Task(context.Background(), e.d.RQ, sqid)
	if err != nil {
		t.Fatal(err)
	}
	return cur.State, cur.Prompt
}

func TestCreateOnceWithSummaryAndRun(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	_, _, run := e.review(t, "already done")
	e.s.Sync(ctx)
	e.s.Sync(ctx) // idempotent
	ids := e.open(t)
	if len(ids) != 1 {
		t.Fatalf("items: %v", ids)
	}
	q, _ := e.rs.Question(ctx, ids[0])
	if !strings.Contains(q.DetailMD, "already built") || !strings.Contains(q.DetailMD, "/run/") || q.TaskTitle != "already done" {
		t.Fatalf("item: %+v", q)
	}
	_ = run
}

func TestNoItemWithOpenPROrRunning(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	_, _, run := e.review(t, "has a PR")
	e.d.W.Exec(`INSERT INTO pull_request (task_id, repo_id, run_id, gh_number, url, head_branch) VALUES (1,1,?,1,'u','wrangl/x')`, run)
	e.s.Sync(ctx)
	if ids := e.open(t); len(ids) != 0 {
		t.Fatalf("PR tracked, got items %v", ids)
	}
	// A run that has not ended is not ready: the PR step may still be coming.
	e.d.W.Exec(`UPDATE pull_request SET state='closed'`)
	e.d.W.Exec(`UPDATE run SET state='running'`)
	e.s.Sync(ctx)
	if ids := e.open(t); len(ids) != 0 {
		t.Fatalf("run still going, got items %v", ids)
	}
}

func TestAcceptCompletes(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	sqid, _, _ := e.review(t, "accept me")
	e.s.Sync(ctx)
	ids := e.open(t)
	if err := e.rs.Answer(ctx, ids[0], "accept", ""); err != nil {
		t.Fatal(err)
	}
	if st, _ := e.state(t, sqid); st != "done" {
		t.Fatalf("state %s", st)
	}
	e.s.Sync(ctx)
	if len(e.open(t)) != 0 {
		t.Fatal("item still open")
	}
}

func TestReopenAppendsNote(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	sqid, _, _ := e.review(t, "reopen me")
	e.s.Sync(ctx)
	ids := e.open(t)
	if err := e.rs.Answer(ctx, ids[0], "reopen", "  "); err == nil {
		t.Fatal("reopen without a note must be refused")
	}
	if len(e.open(t)) != 1 {
		t.Fatal("a refused answer must leave the item open")
	}
	if err := e.rs.Answer(ctx, ids[0], "reopen", "the tests are missing"); err != nil {
		t.Fatal(err)
	}
	st, prompt := e.state(t, sqid)
	if st != "open" || !strings.Contains(prompt, "the tests are missing") || !strings.HasPrefix(prompt, "build it") {
		t.Fatalf("after reopen: %s %q", st, prompt)
	}
	if len(e.open(t)) != 0 {
		t.Fatal("item still open")
	}
}

func TestAutoResolve(t *testing.T) {
	ctx := context.Background()
	for _, how := range []string{"complete", "reopen", "cancel", "pr"} {
		t.Run(how, func(t *testing.T) {
			e := setup(t)
			sqid, _, run := e.review(t, "t")
			e.s.Sync(ctx)
			if len(e.open(t)) != 1 {
				t.Fatal("no item")
			}
			var err error
			switch how {
			case "complete":
				err = e.st.Complete(ctx, sqid, "", stack.User)
			case "reopen":
				err = e.st.Reopen(ctx, sqid, stack.User)
			case "cancel":
				err = e.st.Cancel(ctx, sqid, "", stack.User)
			case "pr":
				_, err = e.d.W.Exec(`INSERT INTO pull_request (task_id, repo_id, run_id, gh_number, url, head_branch) VALUES (1,1,?,1,'u','b')`, run)
			}
			if err != nil {
				t.Fatal(err)
			}
			if n, err := e.s.Resolve(ctx); err != nil || n != 1 {
				t.Fatalf("resolved %d %v", n, err)
			}
			if len(e.open(t)) != 0 {
				t.Fatal("item still open")
			}
		})
	}
}

func TestBackfill(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.review(t, "one")
	e.review(t, "two")
	if len(e.open(t)) != 0 {
		t.Fatal("no items before the sync")
	}
	n, err := e.s.Create(ctx)
	if err != nil || n != 2 || len(e.open(t)) != 2 {
		t.Fatalf("backfilled %d %v", n, err)
	}
}
