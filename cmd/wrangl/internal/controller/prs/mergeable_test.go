package prs

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/gh"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/events"
)

// two open PRs (7 on wrangl/a, 8 on wrangl/b) in acme/app, tracked against a fake GitHub.
func mergeableFixture(t *testing.T) (*Service, *gh.Fake) {
	t.Helper()
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	d.W.Exec(`INSERT INTO runner_template (name, mise_toml, agent_json, min_vcpu, min_ram_mib, min_disk_gib) VALUES ('t','','{}',1,1,1)`)
	d.W.Exec(`INSERT INTO project (slug, name, default_template_id) VALUES ('p','P',1)`)
	d.W.Exec(`INSERT INTO repo (project_id, owner, name, default_branch, clone_url) VALUES (1,'acme','app','main','x')`)
	d.W.Exec(`INSERT INTO task (project_id, rank, title, sqid) VALUES (1,'V001','a task','abc123')`)
	d.W.Exec(`INSERT INTO run (task_id, state) VALUES (1,'succeeded')`)
	f := gh.NewFake()
	f.SetDefaultChecks(gh.ChecksPending) // Evaluate stops after recording gates
	repo := gh.Repo{Owner: "acme", Name: "app", DefaultBranch: "main", CloneURL: "x"}
	f.AddRepo(repo)
	for i, br := range []string{"wrangl/a", "wrangl/b"} {
		pr, err := f.CreatePR(ctx, repo, br, "main", "t", "b")
		if err != nil {
			t.Fatal(err)
		}
		d.W.Exec(`INSERT INTO pull_request (task_id, repo_id, run_id, gh_number, url, head_branch) VALUES (1,1,1,?,?,?)`, pr.Number, pr.URL, br)
		_ = i
	}
	return &Service{DB: d, Author: f, Bus: &events.Bus{}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, f
}

func mergeableOf(t *testing.T, s *Service, id int64) string {
	t.Helper()
	pr, err := s.DB.RQ.GetPR(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return pr.Mergeable
}

func TestEvaluateAndPullRequestWebhookRefreshMergeable(t *testing.T) {
	s, f := mergeableFixture(t)
	ctx := context.Background()
	if got := mergeableOf(t, s, 1); got != "unknown" {
		t.Fatalf("initial %q", got)
	}
	if err := s.Evaluate(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if got := mergeableOf(t, s, 1); got != "clean" {
		t.Fatalf("after Evaluate %q", got)
	}
	f.SetMergeable(1, gh.MergeConflicted)
	body := `{"action":"synchronize","pull_request":{"number":1},"repository":{"name":"app","owner":{"login":"acme"}}}`
	if err := s.handleEvent(ctx, "pull_request", []byte(body)); err != nil {
		t.Fatal(err)
	}
	if got := mergeableOf(t, s, 1); got != "conflicted" {
		t.Fatalf("after pull_request webhook %q", got)
	}
}

func TestPushToBaseRefreshesOtherOpenPRs(t *testing.T) {
	s, f := mergeableFixture(t)
	ctx := context.Background()
	f.SetMergeable(1, gh.MergeConflicted)
	f.SetMergeable(2, gh.MergeBlocked)
	body := `{"ref":"refs/heads/main","repository":{"name":"app","owner":{"login":"acme"}}}`
	if err := s.handleEvent(ctx, "push", []byte(body)); err != nil {
		t.Fatal(err)
	}
	if a, b := mergeableOf(t, s, 1), mergeableOf(t, s, 2); a != "conflicted" || b != "blocked" {
		t.Fatalf("PR1 %q PR2 %q", a, b)
	}
	// A push to a PR's own head branch fully evaluates that PR and still refreshes the rest.
	f.SetMergeable(1, gh.MergeClean)
	f.SetMergeable(2, gh.MergeClean)
	body = `{"ref":"refs/heads/wrangl/a","repository":{"name":"app","owner":{"login":"acme"}}}`
	if err := s.handleEvent(ctx, "push", []byte(body)); err != nil {
		t.Fatal(err)
	}
	if a, b := mergeableOf(t, s, 1), mergeableOf(t, s, 2); a != "clean" || b != "clean" {
		t.Fatalf("PR1 %q PR2 %q", a, b)
	}
}

func TestReconcileRefreshesMergeable(t *testing.T) {
	s, f := mergeableFixture(t)
	f.SetMergeable(2, gh.MergeConflicted)
	s.Reconcile(context.Background())
	if a, b := mergeableOf(t, s, 1), mergeableOf(t, s, 2); a != "clean" || b != "conflicted" {
		t.Fatalf("PR1 %q PR2 %q", a, b)
	}
}
