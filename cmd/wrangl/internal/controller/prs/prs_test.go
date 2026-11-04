package prs

import (
	"context"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/gh"
)

func TestProtectedGlobs(t *testing.T) {
	globs := []string{".github/**", "**/migrations/**", "go.mod", "**/*.tf", "Dockerfile*"}
	files := []gh.PRFile{
		{Path: ".github/workflows/ci.yml"}, {Path: "db/migrations/0001.sql"}, {Path: "migrations/x.sql"},
		{Path: "go.mod"}, {Path: "infra/main.tf"}, {Path: "main.tf"}, {Path: "Dockerfile.dev"},
		{Path: "cmd/go.mod.bak"}, {Path: "src/main.go"}, {Path: "docs/github.md"},
	}
	got := strings.Join(Protected(files, globs), ",")
	want := ".github/workflows/ci.yml,db/migrations/0001.sql,migrations/x.sql,go.mod,infra/main.tf,main.tf,Dockerfile.dev"
	if got != want {
		t.Fatalf("protected:\n got %s\nwant %s", got, want)
	}
}

func TestCanReview(t *testing.T) {
	s := &Service{}
	if s.canReview("o") {
		t.Fatal("no reviewer client")
	}
	s.Reviewer = &gh.Fake{}
	if !s.canReview("o") {
		t.Fatal("a plain reviewer client serves every owner")
	}
	s.HasReviewer = func(o string) bool { return o == "mine" }
	if s.canReview("other") || !s.canReview("mine") {
		t.Fatal("per-owner reviewer Apps")
	}
	if s.gh(gh.Repo{Owner: "other"}) != s.Author {
		t.Fatal("owners without a reviewer App are read through the author App")
	}
}

// recAPI records the approvals and merges made through it; everything else is unused.
type recAPI struct {
	gh.API
	reviews, merges int
	mergeErr        error
}

func (r *recAPI) Review(context.Context, gh.Repo, int64, string, string) error {
	r.reviews++
	return nil
}
func (r *recAPI) Merge(context.Context, gh.Repo, int64, string) error {
	r.merges++
	return r.mergeErr
}

func mergeFixture(t *testing.T) (*Service, dbgen.PullRequest) {
	t.Helper()
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	d.W.Exec(`INSERT INTO runner_template (name, mise_toml, agent_json, min_vcpu, min_ram_mib, min_disk_gib) VALUES ('t','','{}',1,1,1)`)
	d.W.Exec(`INSERT INTO project (slug, name, default_template_id) VALUES ('p','P',1)`)
	d.W.Exec(`INSERT INTO repo (project_id, owner, name, default_branch, clone_url) VALUES (1,'acme','app','main','file:///x')`)
	d.W.Exec(`INSERT INTO task (project_id, rank, title, sqid) VALUES (1,'V001','a task','abc123')`)
	d.W.Exec(`INSERT INTO run (task_id, state) VALUES (1,'succeeded')`)
	d.W.Exec(`INSERT INTO pull_request (task_id, repo_id, run_id, gh_number, url, head_branch) VALUES (1,1,1,7,'https://github.com/acme/app/pull/7','wrangl/abc123-1')`)
	pr, err := d.RQ.GetPR(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	return &Service{DB: d, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, pr
}

// One App is enough: with no reviewer App, "Approve & merge" merges as the author App and posts
// no approval.
func TestMergeWithoutReviewerApp(t *testing.T) {
	s, pr := mergeFixture(t)
	author := &recAPI{}
	s.Author = author
	if err := s.merge(context.Background(), pr, "user_approved", "ok"); err != nil {
		t.Fatal(err)
	}
	if author.merges != 1 || author.reviews != 0 {
		t.Fatalf("merges=%d reviews=%d, want 1 merge and no approval", author.merges, author.reviews)
	}
	if got, _ := s.DB.RQ.GetPR(context.Background(), pr.ID); got.State != "merged" {
		t.Fatalf("PR state %q", got.State)
	}
}

// With the optional reviewer App configured it approves first, then merges as before.
func TestMergeWithReviewerAppApprovesFirst(t *testing.T) {
	s, pr := mergeFixture(t)
	author, reviewer := &recAPI{}, &recAPI{}
	s.Author, s.Reviewer = author, reviewer
	if err := s.merge(context.Background(), pr, "user_approved", "ok"); err != nil {
		t.Fatal(err)
	}
	if reviewer.reviews != 1 || reviewer.merges != 1 || author.merges != 0 {
		t.Fatalf("reviewer %+v author %+v", reviewer, author)
	}
}

// If GitHub refuses the merge because approvals are required, say how to fix it.
func TestMergeRefusedExplainsTheFix(t *testing.T) {
	s, pr := mergeFixture(t)
	s.Author = &recAPI{mergeErr: &gh.HTTPError{Status: 405, Body: "Pull request is not mergeable"}}
	err := s.merge(context.Background(), pr, "user_approved", "ok")
	if err == nil || !strings.Contains(err.Error(), "bypass") || !strings.Contains(err.Error(), "reviewer App") {
		t.Fatalf("want guidance on bypass list / reviewer App, got: %v", err)
	}
	if got, _ := s.DB.RQ.GetPR(context.Background(), pr.ID); got.State != "open" {
		t.Fatalf("a refused merge must leave the PR open: %q", got.State)
	}
}
