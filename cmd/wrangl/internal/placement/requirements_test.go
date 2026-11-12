package placement

import (
	"context"
	"testing"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
)

func TestDeriveTemplateTags(t *testing.T) {
	mise := "[tools]\ngo = \"1.22\"\nnode = 'latest'\n\"asdf:foo\" = \"2.0.1\"\n\n[env]\nGO = \"x\"\n"
	tags := DeriveTemplateTags("arm64", mise, []string{"Claude", "codex"})
	want := map[string]string{"arch": "arm64", "agent:claude": "", "agent:codex": "", "tool:go": "1.22", "tool:node": "", "tool:foo": "2.0.1"}
	if len(tags) != len(want) {
		t.Fatalf("%v", tags)
	}
	for k, v := range want {
		if got, ok := tags[k]; !ok || got != v {
			t.Errorf("%s = %q (%v), want %q", k, got, ok, v)
		}
	}
}

func TestRequirementsResolveAndSnapshot(t *testing.T) {
	s, d := setup(t)
	ctx := context.Background()
	if err := s.EditRequirements(ctx, OwnerProject, 1, []string{"arch=arm64", "tool:go>=1.22"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.EditRequirements(ctx, OwnerTask, 1, []string{"tool:go>=1.23", "gpu"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.EditRequirements(ctx, OwnerTask, 1, []string{"bad key"}, nil); err == nil {
		t.Fatal("a bad requirement was accepted")
	}
	got := func() []string {
		rs, _ := Required(ctx, d.RQ, 1, 1)
		var out []string
		for _, r := range rs {
			out = append(out, r.String())
		}
		return out
	}
	if g := got(); len(g) != 3 || g[0] != "arch=arm64" || g[1] != "tool:go>=1.23" || g[2] != "gpu" {
		t.Fatalf("the task's requirement on a key replaces the project's: %v", g)
	}
	run := dbgen.Run{ID: 1, TaskID: 1}
	d.W.Exec(`INSERT INTO run (task_id) VALUES (1)`)
	if err := SnapshotRequirements(ctx, d.Q, run); err != nil {
		t.Fatal(err)
	}
	s.EditRequirements(ctx, OwnerTask, 1, nil, []string{"gpu"})
	if rs := s.RequirementStrings(ctx, OwnerRun, 1); len(rs) != 3 {
		t.Fatalf("the snapshot does not follow later edits: %v", rs)
	}
	if g := got(); len(g) != 2 {
		t.Fatalf("removed by key: %v", g)
	}
}
