package projects

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/gh"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/errs"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/stack"
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

func TestIssueDriftFindsMissedWebhooksAndRepairs(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	fake := gh.NewFake()
	repo := gh.Repo{Owner: "acme", Name: "app", DefaultBranch: "main", CloneURL: "file:///tmp/app.git"}
	fake.AddRepo(repo,
		gh.Issue{Number: 1, Title: "one", State: "open", UpdatedAt: "2026-11-04T10:00:00Z", Labels: []string{"b", "a"}},
		gh.Issue{Number: 2, Title: "two", State: "open", UpdatedAt: "2026-11-04T10:00:00Z"},
		gh.Issue{Number: 3, Title: "three", State: "open", UpdatedAt: "2026-11-04T10:00:00Z"})
	sq, _ := stack.NewSqids(stack.DefaultSqidsAlphabet)
	s := &Service{DB: d, GH: fake, Stack: &stack.Service{DB: d, Sqids: sq}, Now: time.Now}
	if _, err := s.Create(ctx, "web", ProjectParams{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LinkRepo(ctx, "web", "acme/app", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SyncIssues(ctx, "web"); err != nil {
		t.Fatal(err)
	}
	if ds, err := s.IssueDrift(ctx, "web", false); err != nil || len(ds) != 0 {
		t.Fatalf("in sync, label order ignored: %+v %v", ds, err)
	}
	// While webhooks were missed: 2 was edited, 3 was removed, 4 was created, and 1 was closed through the API.
	title := "two (edited)"
	if _, err := fake.EditIssue(ctx, repo, 2, gh.IssueEdit{Title: &title}); err != nil {
		t.Fatal(err)
	}
	if _, err := fake.CreateIssue(ctx, repo, "four", "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := fake.CloseIssue(ctx, repo, 1, ""); err != nil {
		t.Fatal(err)
	}
	fake.AddRepo(repo, fake.MustIssues(repo, 1, 2, 4)...) // #3 was deleted on GitHub
	ds, err := s.IssueDrift(ctx, "web", false)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]string{}
	for _, x := range ds {
		kinds[x.Ref] = x.Kind
	}
	want := map[string]string{"acme/app#1": "stale", "acme/app#2": "stale", "acme/app#3": "missing_remote", "acme/app#4": "missing_local"}
	for k, v := range want {
		if kinds[k] != v {
			t.Errorf("%s: got %q want %q (all: %+v)", k, kinds[k], v, ds)
		}
	}
	// Reporting does not change anything.
	if ds2, _ := s.IssueDrift(ctx, "web", false); len(ds2) != len(ds) {
		t.Fatal("a check must not repair")
	}
	if _, err := s.IssueDrift(ctx, "web", true); err != nil {
		t.Fatal(err)
	}
	if ds, err := s.IssueDrift(ctx, "web", false); err != nil || len(ds) != 0 {
		t.Fatalf("after repair: %+v %v", ds, err)
	}
}

func TestSetDefaultRepoMustBeLinked(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	fake := gh.NewFake()
	fake.AddRepo(gh.Repo{Owner: "acme", Name: "app", DefaultBranch: "main", CloneURL: "file:///tmp/app.git"})
	sq, _ := stack.NewSqids(stack.DefaultSqidsAlphabet)
	s := &Service{DB: d, GH: fake, Stack: &stack.Service{DB: d, Sqids: sq}, Now: time.Now}
	if _, err := s.Create(ctx, "web", ProjectParams{}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetDefaultRepo(ctx, "web", "acme/app"); !errors.Is(err, errs.Invalid) {
		t.Fatalf("unlinked default accepted: %v", err)
	}
	if _, err := s.LinkRepo(ctx, "web", "acme/app", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.SetDefaultRepo(ctx, "web", "acme/app"); err != nil {
		t.Fatal(err)
	}
	p, _ := d.RQ.GetProjectBySlug(ctx, "web")
	if !p.DefaultRepoID.Valid {
		t.Fatal("default not set")
	}
	if err := s.SetDefaultRepo(ctx, "web", ""); err != nil {
		t.Fatal(err)
	}
	if p, _ = d.RQ.GetProjectBySlug(ctx, "web"); p.DefaultRepoID.Valid {
		t.Fatal("default not cleared")
	}
}

func TestSetRepoNote(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	fake := gh.NewFake()
	fake.AddRepo(gh.Repo{Owner: "acme", Name: "app", DefaultBranch: "main", CloneURL: "file:///tmp/app.git"})
	sq, _ := stack.NewSqids(stack.DefaultSqidsAlphabet)
	s := &Service{DB: d, GH: fake, Stack: &stack.Service{DB: d, Sqids: sq}, Now: time.Now}
	if _, err := s.Create(ctx, "web", ProjectParams{}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRepoNote(ctx, "web", "acme/app", "API server"); !errors.Is(err, errs.Invalid) {
		t.Fatalf("unlinked repo: %v", err)
	}
	r, err := s.LinkRepo(ctx, "web", "acme/app", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetRepoNote(ctx, "web", "acme/app", "  API server "); err != nil {
		t.Fatal(err)
	}
	if got, _ := d.RQ.GetRepo(ctx, r.ID); got.Note != "API server" {
		t.Fatalf("note %q", got.Note)
	}
	if err := s.SetRepoNote(ctx, "web", "acme/app", strings.Repeat("x", 201)); !errors.Is(err, errs.Invalid) {
		t.Fatalf("long note: %v", err)
	}
}
