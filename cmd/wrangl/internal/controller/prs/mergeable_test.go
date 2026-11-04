package prs

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/gh"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/runs"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
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

// autoMergeFixture: PR 1 is reviewed low-risk with every gate green on a project with auto-merge on.
func autoMergeFixture(t *testing.T, autoResolve bool) (*Service, *gh.Fake, *[]int64) {
	t.Helper()
	s, f := mergeableFixture(t)
	f.SetDefaultChecks(gh.ChecksSuccess)
	s.Runs = &runs.Service{DB: s.DB, Log: s.Log, Bus: &events.Bus{}, Now: time.Now}
	s.Now = time.Now
	s.DB.W.Exec(`UPDATE project SET auto_merge = 1, auto_resolve = ? WHERE id = 1`, autoResolve)
	if err := s.DB.Q.SetPRReview(context.Background(), dbgen.SetPRReviewParams{Risk: nstr("low"), Summary: "ok", ReviewedSha: "", ID: 1}); err != nil {
		t.Fatal(err)
	}
	var queued []int64
	s.QueueResolve = func(_ context.Context, pr dbgen.PullRequest) error { queued = append(queued, pr.ID); return nil }
	return s, f, &queued
}

func TestAutoMergeMergesACleanPR(t *testing.T) {
	s, _, queued := autoMergeFixture(t, true)
	if err := s.Evaluate(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	pr, _ := s.DB.RQ.GetPR(context.Background(), 1)
	if pr.State != "merged" || len(*queued) != 0 {
		t.Fatalf("state %q, resolve queued %v", pr.State, *queued)
	}
}

func TestAutoMergeSkipsConflictedPRAndQueuesResolve(t *testing.T) {
	s, f, queued := autoMergeFixture(t, true)
	f.SetMergeable(1, gh.MergeConflicted)
	for i := 0; i < 2; i++ {
		if err := s.Evaluate(context.Background(), 1); err != nil {
			t.Fatal(err)
		}
	}
	pr, _ := s.DB.RQ.GetPR(context.Background(), 1)
	if pr.State != "open" {
		t.Fatalf("a conflicted PR was merged: %q", pr.State)
	}
	if len(*queued) == 0 || (*queued)[0] != 1 {
		t.Fatalf("resolve not queued: %v", *queued)
	}
	var n int
	s.DB.R.QueryRow(`SELECT COUNT(*) FROM question WHERE kind='pr_review'`).Scan(&n)
	if n != 0 {
		t.Fatalf("auto-resolve on must not raise an Inbox item, got %d", n)
	}
}

func TestAutoMergeConflictedWithoutAutoResolveRaisesInboxItem(t *testing.T) {
	s, f, queued := autoMergeFixture(t, false)
	f.SetMergeable(1, gh.MergeConflicted)
	for i := 0; i < 2; i++ { // twice: still one item
		if err := s.Evaluate(context.Background(), 1); err != nil {
			t.Fatal(err)
		}
	}
	pr, _ := s.DB.RQ.GetPR(context.Background(), 1)
	var n int
	var detail string
	s.DB.R.QueryRow(`SELECT COUNT(*), COALESCE(MAX(detail_md),'') FROM question WHERE kind='pr_review'`).Scan(&n, &detail)
	if pr.State != "open" || len(*queued) != 0 || n != 1 || !strings.Contains(detail, "Merge conflicts") {
		t.Fatalf("state %q queued %v items %d detail %q", pr.State, *queued, n, detail)
	}
}

func resolveRuns(t *testing.T, s *Service) (n int, prompt string) {
	t.Helper()
	s.DB.R.QueryRow(`SELECT COUNT(*), COALESCE(MAX(prompt_override),'') FROM run WHERE kind = 'resolve'`).Scan(&n, &prompt)
	return
}

func TestResolveConflictsQueuesOneRun(t *testing.T) {
	s, f := mergeableFixture(t)
	ctx := context.Background()
	f.SetMergeable(1, gh.MergeConflicted)
	if err := s.ResolveConflicts(ctx, 1); err != nil {
		t.Fatal(err)
	}
	n, prompt := resolveRuns(t, s)
	if n != 1 {
		t.Fatalf("%d resolve runs", n)
	}
	for _, want := range []string{"git merge origin/main", "NEVER force-push", "question tool", "report_status", "check.sh"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lacks %q:\n%s", want, prompt)
		}
	}
	// A second click while the run is active is refused; still one run.
	if err := s.ResolveConflicts(ctx, 1); err == nil {
		t.Fatal("a second resolve run was accepted")
	}
	if err := s.autoResolve(ctx, mustPR(t, s, 1)); err != nil {
		t.Fatal(err)
	}
	if n, _ := resolveRuns(t, s); n != 1 {
		t.Fatalf("%d resolve runs after retries", n)
	}
	var logs int
	s.DB.R.QueryRow(`SELECT COUNT(*) FROM log_entry WHERE kind = 'pr'`).Scan(&logs)
	if logs != 1 {
		t.Fatalf("%d timeline entries", logs)
	}
}

func TestResolveConflictsCleanPRQueuesNothing(t *testing.T) {
	s, _ := mergeableFixture(t)
	if err := s.ResolveConflicts(context.Background(), 1); err == nil {
		t.Fatal("a clean PR was accepted")
	}
	if n, _ := resolveRuns(t, s); n != 0 {
		t.Fatalf("%d resolve runs for a clean PR", n)
	}
}

func TestAutoMergeQueuesResolveRunOnce(t *testing.T) {
	s, f, _ := autoMergeFixture(t, true)
	s.QueueResolve = nil // the real path
	f.SetMergeable(1, gh.MergeConflicted)
	for i := 0; i < 3; i++ {
		if err := s.Evaluate(context.Background(), 1); err != nil {
			t.Fatal(err)
		}
	}
	if n, _ := resolveRuns(t, s); n != 1 {
		t.Fatalf("%d resolve runs", n)
	}
}

func mustPR(t *testing.T, s *Service, id int64) dbgen.PullRequest {
	t.Helper()
	pr, err := s.DB.RQ.GetPR(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return pr
}
