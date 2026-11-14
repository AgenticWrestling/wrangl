package stack_test

import (
	"context"
	"testing"

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
