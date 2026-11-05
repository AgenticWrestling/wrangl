package tools

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/errs"
)

const kdl = `tool "deploy_status" {
  description "Show the deployment status of an environment"
  effect "read"
  run-kinds "work" "revise"
  input { param "env" type="string" { enum "staging" "prod" } }
  exec {
    argv "kubectl" "get" "deploy" "-n" "{{input.env}}"
    secret "KUBECONFIG" name="kube.prod" as="file"
  }
}`

func newSvc(t *testing.T) *Service {
	t.Helper()
	d, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if _, err := d.W.Exec(`INSERT INTO project (slug, name) VALUES ('alpha','Alpha'), ('beta','Beta')`); err != nil {
		t.Fatal(err)
	}
	return &Service{DB: d}
}

func TestLibraryLifecycle(t *testing.T) {
	ctx := context.Background()
	s := newSvc(t)
	tool, err := s.Add(ctx, kdl)
	if err != nil {
		t.Fatal(err)
	}
	if tool.Name != "deploy_status" || tool.Kind != "exec" || tool.Effect != "read" || tool.Version != 1 || tool.Kdl != kdl {
		t.Fatalf("%+v", tool)
	}
	// the stored declaration is runner-safe
	for _, leak := range []string{"kubectl", "kube.prod", "KUBECONFIG", "argv"} {
		if strings.Contains(tool.DeclJson, leak) {
			t.Errorf("declaration leaks %q: %s", leak, tool.DeclJson)
		}
	}
	if _, err := s.Add(ctx, kdl); !errors.Is(err, errs.Conflict) {
		t.Fatalf("duplicate add: %v", err)
	}
	if _, err := s.Add(ctx, `tool "x" {`); !errors.Is(err, errs.Invalid) {
		t.Fatalf("invalid add: %v", err)
	}

	up, err := s.Update(ctx, "deploy_status", strings.Replace(kdl, "Show the", "Display the", 1))
	if err != nil || up.Version != 2 || !strings.HasPrefix(up.Description, "Display") {
		t.Fatalf("update: %v %+v", err, up)
	}
	if _, err := s.Update(ctx, "deploy_status", strings.Replace(kdl, "deploy_status", "other", 1)); !errors.Is(err, errs.Invalid) {
		t.Fatalf("rename: %v", err)
	}
	if _, err := s.Update(ctx, "ghost", kdl); !errors.Is(err, errs.NotFound) {
		t.Fatalf("update unknown: %v", err)
	}
	if got, _ := s.Get(ctx, "deploy_status"); got.Version != 2 {
		t.Fatalf("failed update changed the tool: %+v", got)
	}

	if err := s.Enable(ctx, "alpha", "deploy_status"); err != nil {
		t.Fatal(err)
	}
	if err := s.Enable(ctx, "alpha", "deploy_status"); err != nil {
		t.Fatalf("enable is idempotent: %v", err)
	}
	if _, err := s.Enabled(ctx, "alpha", "deploy_status"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Enabled(ctx, "beta", "deploy_status"); !errors.Is(err, errs.Conflict) {
		t.Fatalf("beta: %v", err)
	}
	if err := s.Enable(ctx, "nope", "deploy_status"); !errors.Is(err, errs.NotFound) {
		t.Fatalf("enable unknown project: %v", err)
	}
	if err := s.Enable(ctx, "alpha", "ghost"); !errors.Is(err, errs.NotFound) {
		t.Fatalf("enable unknown tool: %v", err)
	}
	if ts, _ := s.ForProject(ctx, "alpha"); len(ts) != 1 {
		t.Fatalf("alpha tools: %v", ts)
	}
	if ts, _ := s.ForProject(ctx, "beta"); len(ts) != 0 {
		t.Fatalf("beta tools: %v", ts)
	}
	if l, _ := s.List(ctx); len(l) != 1 || len(l[0].Projects) != 1 || l[0].Projects[0] != "alpha" {
		t.Fatalf("list: %+v", l)
	}

	if err := s.Remove(ctx, "deploy_status", false); !errors.Is(err, errs.Conflict) {
		t.Fatalf("remove while enabled: %v", err)
	}
	if err := s.Disable(ctx, "alpha", "deploy_status"); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(ctx, "deploy_status", false); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(ctx, "deploy_status", false); !errors.Is(err, errs.NotFound) {
		t.Fatalf("remove twice: %v", err)
	}
}

func TestForceRemoveAndProjectCascade(t *testing.T) {
	ctx := context.Background()
	s := newSvc(t)
	s.Add(ctx, kdl)
	s.Enable(ctx, "alpha", "deploy_status")
	s.Enable(ctx, "beta", "deploy_status")
	if _, err := s.DB.W.Exec(`DELETE FROM project WHERE slug='beta'`); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(ctx, "deploy_status"); len(got.Projects) != 1 {
		t.Fatalf("project delete should drop enablement: %v", got.Projects)
	}
	if err := s.Remove(ctx, "deploy_status", true); err != nil {
		t.Fatal(err)
	}
	var n int
	s.DB.W.QueryRow(`SELECT COUNT(*) FROM project_tool`).Scan(&n)
	if n != 0 {
		t.Fatalf("enablement rows left: %d", n)
	}
}
