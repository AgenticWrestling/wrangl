package stack_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbtypes"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/errs"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/stack"
)

func TestTaskTypes(t *testing.T) {
	s, d := setup(t)
	ctx := context.Background()
	plain := mk(t, s, "", "plain", stack.Position{})
	if plain.TaskType != dbtypes.TaskCode {
		t.Fatalf("default type %q", plain.TaskType)
	}
	spec, err := s.Create(ctx, stack.CreateParams{Project: "p", Title: "write the spec", Type: "docs"})
	if err != nil || spec.TaskType != dbtypes.TaskDocs {
		t.Fatalf("typed create: %q %v", spec.TaskType, err)
	}
	if kid := mk(t, s, spec.Sqid.String, "section", stack.Position{}); kid.TaskType != dbtypes.TaskDocs {
		t.Fatalf("subtask did not inherit: %q", kid.TaskType)
	}
	if _, err := s.Create(ctx, stack.CreateParams{Project: "p", Title: "x", Type: "poetry"}); !errors.Is(err, errs.Invalid) {
		t.Fatalf("bad type accepted: %v", err)
	}
	if _, err := d.W.ExecContext(ctx, `UPDATE task SET task_type = 'poetry' WHERE id = ?`, plain.ID); err == nil {
		t.Fatal("the CHECK constraint let an unknown type in")
	}
	got, err := s.SetType(ctx, plain.Sqid.String, "docs", stack.User)
	if err != nil || got.TaskType != dbtypes.TaskDocs {
		t.Fatalf("SetType: %q %v", got.TaskType, err)
	}
	nodes, _ := stack.ParseOutline("- [review] check the parser\n- [Docs] update README\n- [nope] keep brackets\n- plain")
	want := []string{"review/check the parser", "docs/update README", "/[nope] keep brackets", "/plain"}
	for i, n := range nodes {
		if n.Type+"/"+n.Title != want[i] {
			t.Fatalf("outline node %d: %q/%q", i, n.Type, n.Title)
		}
	}
	created, err := s.Split(ctx, spec.Sqid.String, nodes, false, false, stack.User)
	if err != nil {
		t.Fatal(err)
	}
	if created[0].TaskType != dbtypes.TaskReview || created[3].TaskType != dbtypes.TaskDocs {
		t.Fatalf("split types: %q %q", created[0].TaskType, created[3].TaskType)
	}
	rows, _ := d.RQ.ProjectTree(ctx, 1)
	seen := map[dbtypes.TaskType]bool{}
	for _, r := range rows {
		seen[r.TaskType] = true
	}
	if !seen[dbtypes.TaskDocs] || !seen[dbtypes.TaskReview] {
		t.Fatalf("ProjectTree types: %v", seen)
	}
	if g := dbtypes.TaskDocs.Guidance(); !strings.Contains(g, "Change documentation only") {
		t.Fatal(g)
	}
}

func TestReleaseWaitsForShipIt(t *testing.T) {
	s, _ := setup(t)
	ctx := context.Background()
	rel, err := s.Create(ctx, stack.CreateParams{Project: "p", Title: "v1", Type: "release", Actor: stack.User})
	if err != nil {
		t.Fatal(err)
	}
	ms := mkTyped(t, s, rel.Sqid.String, "m1", "milestone")
	leaf := mkTyped(t, s, ms.Sqid.String, "work", "")
	if leaf.TaskType != dbtypes.TaskCode {
		t.Fatalf("children of a planning type are ordinary tasks, got %s", leaf.TaskType)
	}
	state := func(sqid string) string {
		cur, err := s.Task(ctx, s.DB.RQ, sqid)
		if err != nil {
			t.Fatal(err)
		}
		return cur.State
	}
	if err := s.Complete(ctx, leaf.Sqid.String, "done", stack.User); err != nil {
		t.Fatal(err)
	}
	if got := state(ms.Sqid.String); got != "done" {
		t.Fatalf("a milestone completes by itself, got %s", got)
	}
	if got := state(rel.Sqid.String); got != "in_review" {
		t.Fatalf("a release waits for ship-it, got %s", got)
	}
	if err := s.Complete(ctx, rel.Sqid.String, "shipped", stack.User); err != nil {
		t.Fatal(err)
	}
	if got := state(rel.Sqid.String); got != "done" {
		t.Fatalf("after ship-it: %s", got)
	}
	// New work reopens it; finishing that asks again.
	more := mkTyped(t, s, ms.Sqid.String, "more work", "")
	if got := state(rel.Sqid.String); got != "open" {
		t.Fatalf("new work should reopen the release, got %s", got)
	}
	s.Complete(ctx, more.Sqid.String, "", stack.User)
	if got := state(rel.Sqid.String); got != "in_review" {
		t.Fatalf("finished again: %s", got)
	}
}

func mkTyped(t *testing.T, s *stack.Service, parent, title, typ string) dbgen.Task {
	t.Helper()
	tk, err := s.Create(context.Background(), stack.CreateParams{Project: "p", Parent: parent, Title: title, Type: typ, Actor: stack.User})
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

func TestQueueRefusesChildlessPlanningTask(t *testing.T) {
	s, _ := setup(t)
	ctx := context.Background()
	f, err := s.Create(ctx, stack.CreateParams{Project: "p", Title: "a feature", Type: "feature", Repo: "o/a", Actor: stack.User})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Queue(ctx, f.Sqid.String); !errors.Is(err, errs.Conflict) || !strings.Contains(err.Error(), "plan it first") {
		t.Fatalf("want a conflict telling the owner to plan it, got %v", err)
	}
	mkTyped(t, s, f.Sqid.String, "the work", "")
	if _, err := s.Queue(ctx, f.Sqid.String); err != nil {
		t.Fatalf("a feature with subtasks queues normally: %v", err)
	}
}
