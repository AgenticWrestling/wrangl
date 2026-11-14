package stack_test

import (
	"context"
	"errors"
	"testing"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/errs"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/reporesolve"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/stack"
)

// The fixture links two repos (o/a = 1, o/b = 2) to project p.
func TestResolveOrderTwoRepos(t *testing.T) {
	s, d := setup(t)
	ctx := context.Background()
	resolve := func(id int64) int64 {
		t.Helper()
		r, err := reporesolve.Resolve(ctx, d.RQ, id)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	epic, _ := s.Create(ctx, stack.CreateParams{Project: "p", Title: "epic"})
	kid := mk(t, s, epic.Sqid.String, "kid", stack.Position{})
	if resolve(kid.ID) != 0 {
		t.Fatal("two repos, nothing set: want none")
	}
	if _, err := d.W.Exec(`UPDATE project SET default_repo_id = 2 WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	if resolve(kid.ID) != 2 {
		t.Fatal("project default")
	}
	if _, err := d.W.Exec(`UPDATE task SET repo_id = 1 WHERE id = ?`, epic.ID); err != nil {
		t.Fatal(err)
	}
	if resolve(kid.ID) != 1 {
		t.Fatal("ancestor beats default")
	}
	if _, err := d.W.Exec(`UPDATE task SET repo_id = 2 WHERE id = ?`, kid.ID); err != nil {
		t.Fatal(err)
	}
	if resolve(kid.ID) != 2 || resolve(epic.ID) != 1 {
		t.Fatal("own repo wins")
	}
}

func TestResolveSingleRepoUnchanged(t *testing.T) {
	s, d := setup(t)
	ctx := context.Background()
	if _, err := d.W.Exec(`INSERT INTO repo (project_id, owner, name) VALUES (2, 'o', 'solo')`); err != nil {
		t.Fatal(err)
	}
	tk, _ := s.Create(ctx, stack.CreateParams{Project: "q", Title: "t"})
	id, err := reporesolve.Resolve(ctx, d.RQ, tk.ID)
	if err != nil || id == 0 {
		t.Fatalf("only linked repo: %d %v", id, err)
	}
	run, err := s.Queue(ctx, tk.Sqid.String)
	if err != nil || !run.RepoID.Valid || run.RepoID.Int64 != id || run.StatusText != "" {
		t.Fatalf("run %+v %v", run, err)
	}
}

func TestResearchKeepsNotesRepoOverDefault(t *testing.T) {
	s, d := setup(t)
	ctx := context.Background()
	d.W.Exec(`INSERT INTO project_notes_repo (project_id, repo_id) VALUES (1, 1)`)
	d.W.Exec(`UPDATE project SET default_repo_id = 2 WHERE id = 1`)
	tk, _ := s.Create(ctx, stack.CreateParams{Project: "p", Title: "r", Type: "research"})
	if id, _ := reporesolve.Resolve(ctx, d.RQ, tk.ID); id != 1 {
		t.Fatalf("research resolved to %d", id)
	}
}

func TestQueueWithNoRepoWaits(t *testing.T) {
	s, d := setup(t)
	ctx := context.Background()
	tk, _ := s.Create(ctx, stack.CreateParams{Project: "p", Title: "t"})
	run, err := s.Queue(ctx, tk.Sqid.String)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := d.RQ.GetRun(ctx, run.ID)
	if got.RepoID.Valid || got.State != "queued" || got.StatusText != "waiting: needs a repo" {
		t.Fatalf("run %+v", got)
	}
}

func TestQueueSplitsSubtreeIntoOneRunPerRepo(t *testing.T) {
	s, d := setup(t) // project p links o/a (1) and o/b (2)
	ctx := context.Background()
	ms, _ := s.Create(ctx, stack.CreateParams{Project: "p", Title: "milestone"})
	a1, _ := s.Create(ctx, stack.CreateParams{Project: "p", Parent: ms.Sqid.String, Title: "a1", Repo: "o/a"})
	b1, _ := s.Create(ctx, stack.CreateParams{Project: "p", Parent: ms.Sqid.String, Title: "b1", Repo: "o/b"})
	a2, _ := s.Create(ctx, stack.CreateParams{Project: "p", Parent: ms.Sqid.String, Title: "a2", Repo: "o/a"})
	if err := s.NeedsAdd(ctx, b1.Sqid.String, a1.Sqid.String, stack.User); err != nil { // b1 needs a1: across repos
		t.Fatal(err)
	}

	runs, err := s.QueueRuns(ctx, ms.Sqid.String, "")
	if err != nil || len(runs) != 2 {
		t.Fatalf("runs: %d %v", len(runs), err)
	}
	if runs[0].RepoID.Int64 != 1 || runs[1].RepoID.Int64 != 2 || runs[0].SessionUuid == runs[1].SessionUuid {
		t.Fatalf("runs %+v", runs)
	}
	if _, err := s.Queue(ctx, ms.Sqid.String); !errors.Is(err, errs.Conflict) {
		t.Fatalf("overlapping queue not refused: %v", err)
	}

	// Run A takes only repo A's leaves, in order.
	for _, want := range []string{a1.Sqid.String, a2.Sqid.String} {
		n, err := s.NextForRun(ctx, runs[0].ID)
		if err != nil || n.Sqid != want {
			t.Fatalf("run A: %+v %v (want %s)", n, err, want)
		}
		if _, err := d.W.Exec(`UPDATE task SET state = 'in_review', completed_run_id = ? WHERE sqid = ?`, runs[0].ID, want); err != nil {
			t.Fatal(err)
		}
	}
	if n, _ := s.NextForRun(ctx, runs[0].ID); !n.Done {
		t.Fatalf("run A should be done: %+v", n)
	}
	// Run B waits on a1 (in review in another run) until it is merged.
	if n, _ := s.NextForRun(ctx, runs[1].ID); !n.Blocked || n.Done {
		t.Fatalf("run B should be blocked on the precursor: %+v", n)
	}
	d.W.Exec(`UPDATE task SET state = 'done' WHERE sqid = ?`, a1.Sqid.String)
	if n, err := s.NextForRun(ctx, runs[1].ID); err != nil || n.Sqid != b1.Sqid.String {
		t.Fatalf("run B: %+v %v", n, err)
	}
}

func TestQueueSingleRepoSubtreeStaysOneRun(t *testing.T) {
	s, _ := setup(t)
	ctx := context.Background()
	ms, _ := s.Create(ctx, stack.CreateParams{Project: "p", Title: "m", Repo: "o/a"})
	mk(t, s, ms.Sqid.String, "x", stack.Position{})
	mk(t, s, ms.Sqid.String, "y", stack.Position{})
	if runs, err := s.QueueRuns(ctx, ms.Sqid.String, ""); err != nil || len(runs) != 1 {
		t.Fatalf("runs: %d %v", len(runs), err)
	}
}
