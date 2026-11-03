package stack_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nreid/devhost/cmd/wrangl/internal/db/dbtypes"
	"github.com/nreid/devhost/cmd/wrangl/internal/errs"
	"github.com/nreid/devhost/cmd/wrangl/internal/stack"
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
