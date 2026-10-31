package projects

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nreid/devhost/cmd/wrangl/internal/controller/gh"
	"github.com/nreid/devhost/cmd/wrangl/internal/db"
	"github.com/nreid/devhost/cmd/wrangl/internal/errs"
	"github.com/nreid/devhost/cmd/wrangl/internal/stack"
)

func TestProjectsReposIssues(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	d.W.Exec(`INSERT INTO runner_template (name, mise_toml, agent_json, min_vcpu, min_ram_mib, min_disk_gib) VALUES ('go','','{}',1,1,1)`)
	fake := gh.NewFake()
	fake.AddRepo(gh.Repo{Owner: "acme", Name: "app", DefaultBranch: "main", CloneURL: "file:///tmp/app.git"},
		gh.Issue{Number: 7, Title: "crash on start", Body: "stack trace", State: "open", URL: "https://github.com/acme/app/issues/7"},
		gh.Issue{Number: 8, Title: "old", State: "closed"})
	sq, _ := stack.NewSqids(stack.DefaultSqidsAlphabet)
	s := &Service{DB: d, GH: fake, Stack: &stack.Service{DB: d, Sqids: sq}, Now: time.Now}

	tmpl := "go"
	if _, err := s.Create(ctx, "web", ProjectParams{Template: &tmpl}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(ctx, "web", ProjectParams{}); !errors.Is(err, errs.Conflict) {
		t.Fatalf("duplicate: %v", err)
	}
	if _, err := s.Create(ctx, "Bad Slug", ProjectParams{}); !errors.Is(err, errs.Invalid) {
		t.Fatalf("bad slug: %v", err)
	}
	prio := int64(5)
	if p, err := s.Update(ctx, "web", ProjectParams{Priority: &prio}); err != nil || p.Priority != 5 || !p.DefaultTemplateID.Valid {
		t.Fatalf("update: %+v %v", p, err)
	}

	r, err := s.LinkRepo(ctx, "web", "acme/app", "")
	if err != nil || r.DefaultBranch != "main" || r.CloneUrl != "file:///tmp/app.git" || !r.GhInstallationID.Valid {
		t.Fatalf("link: %+v %v", r, err)
	}
	if _, err := s.LinkRepo(ctx, "web", "acme/nope", ""); !errors.Is(err, errs.Invalid) {
		t.Fatalf("app not installed: %v", err)
	}
	if n, err := s.SyncIssues(ctx, "web"); err != nil || n != 2 {
		t.Fatalf("sync: %d %v", n, err)
	}
	task, err := s.IssueToTask(ctx, "web", "acme/app#7", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if task.Title != "#7 crash on start" || !strings.Contains(task.Prompt, "stack trace") || !task.IssueID.Valid || !task.RepoID.Valid {
		t.Fatalf("task %+v", task)
	}
	if err := s.UnlinkRepo(ctx, "web", "acme/app"); !errors.Is(err, errs.Conflict) {
		t.Fatalf("unlink while used: %v", err)
	}
	(&Service{DB: d}).GH = nil
	if _, err := (&Service{DB: d}).LinkRepo(ctx, "web", "acme/app", ""); !errors.Is(err, ErrNoGitHub) {
		t.Fatalf("no github: %v", err)
	}
}
