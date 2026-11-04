package prs

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/gh"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/runs"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/events"
)

func adoptFixture(t *testing.T) (*Service, *gh.Fake, gh.Repo) {
	t.Helper()
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	for _, q := range []string{
		`INSERT INTO runner_template (name, mise_toml, agent_json, min_vcpu, min_ram_mib, min_disk_gib) VALUES ('t','','{}',1,1,1)`,
		`INSERT INTO project (slug, name, default_template_id) VALUES ('p','P',1)`,
		`INSERT INTO project (slug, name, default_template_id) VALUES ('q','Q',1)`,
		`INSERT INTO repo (project_id, owner, name, default_branch, clone_url) VALUES (1,'acme','app','main','https://x')`,
		`INSERT INTO task (project_id, rank, title, sqid) VALUES (1,'V001','branch task','brn123')`,
		`INSERT INTO task (project_id, rank, title, sqid) VALUES (1,'V002','tag task','tag456')`,
		`INSERT INTO task (project_id, rank, title, sqid) VALUES (2,'V001','other project','oth789')`,
		`INSERT INTO run (task_id, state, kind, repo_id, branch) VALUES (1,'succeeded','work',1,'wrangl/brn123-1')`,
		`INSERT INTO run (task_id, state, kind, repo_id, branch) VALUES (2,'succeeded','work',1,'wrangl/tag456-1')`,
		`INSERT INTO run (task_id, state, kind, branch) VALUES (3,'succeeded','work','wrangl/oth789-1')`,
	} {
		if _, err := d.W.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	f := gh.NewFake()
	r := gh.Repo{Owner: "acme", Name: "app", DefaultBranch: "main"}
	f.AddRepo(r)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	rs := &runs.Service{DB: d, Log: log, Bus: &events.Bus{}, Now: time.Now}
	return &Service{DB: d, Author: f, Runs: rs, Bus: rs.Bus, Now: time.Now, Log: log}, f, r
}

func prCount(t *testing.T, s *Service) int {
	t.Helper()
	var n int
	if err := s.DB.R.QueryRow(`SELECT COUNT(*) FROM pull_request`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestAdoptByBranch(t *testing.T) {
	s, f, r := adoptFixture(t)
	p := f.OpenPR(r, "wrangl/brn123-1", "main", "whatever", "")
	ids, err := s.Adopt(context.Background(), 1)
	if err != nil || len(ids) != 1 {
		t.Fatalf("adopted %v %v", ids, err)
	}
	pr, _ := s.DB.RQ.GetPR(context.Background(), ids[0])
	if pr.GhNumber != p.Number || pr.TaskID != 1 || pr.RunID != 1 || pr.HeadBranch != "wrangl/brn123-1" {
		t.Fatalf("pr: %+v", pr)
	}
	// The same pr_review Inbox item a wrangl-opened PR gets.
	qs, _ := s.Runs.Questions(context.Background(), false)
	if len(qs) != 1 || qs[0].Kind != "pr_review" || qs[0].Task != "brn123" {
		t.Fatalf("inbox: %+v", qs)
	}
}

func TestAdoptByTitleTag(t *testing.T) {
	s, f, r := adoptFixture(t)
	f.OpenPR(r, "agent/feature-x", "main", "[tag456] add the thing", "")
	ids, err := s.Adopt(context.Background(), 1)
	if err != nil || len(ids) != 1 {
		t.Fatalf("adopted %v %v", ids, err)
	}
	pr, _ := s.DB.RQ.GetPR(context.Background(), ids[0])
	if pr.TaskID != 2 || pr.RunID != 2 || pr.HeadBranch != "agent/feature-x" {
		t.Fatalf("pr: %+v", pr)
	}
}

func TestAdoptSkips(t *testing.T) {
	ctx := context.Background()
	s, f, r := adoptFixture(t)
	// Already tracked for that head (a wrangl-opened PR).
	s.DB.W.Exec(`INSERT INTO pull_request (task_id, repo_id, run_id, gh_number, url, head_branch) VALUES (1,1,1,90,'u','wrangl/brn123-1')`)
	f.OpenPR(r, "wrangl/brn123-1", "main", "[brn123] x", "")
	// A fork, a different base, no task, a task of another project.
	f.OpenPR(r, "wrangl/tag456-1", "main", "[tag456] x", "someone/app")
	f.OpenPR(r, "wrangl/tag456-1", "develop", "[tag456] x", "")
	f.OpenPR(r, "feature/unrelated", "main", "no tag here", "")
	f.OpenPR(r, "wrangl/oth789-1", "main", "[oth789] x", "")
	f.OpenPR(r, "feature/unknown", "main", "[nope99] x", "")
	if ids, err := s.Adopt(ctx, 1); err != nil || len(ids) != 0 {
		t.Fatalf("adopted %v %v", ids, err)
	}
	if n := prCount(t, s); n != 1 {
		t.Fatalf("PR rows: %d", n)
	}
}

func TestAdoptIdempotent(t *testing.T) {
	ctx := context.Background()
	s, f, r := adoptFixture(t)
	f.OpenPR(r, "wrangl/brn123-1", "main", "x", "")
	f.OpenPR(r, "other", "main", "[tag456] y", "")
	if ids, _ := s.Adopt(ctx, 1); len(ids) != 2 {
		t.Fatalf("first: %v", ids)
	}
	if ids, err := s.Adopt(ctx, 1); err != nil || len(ids) != 0 {
		t.Fatalf("repeat: %v %v", ids, err)
	}
	if n := prCount(t, s); n != 2 {
		t.Fatalf("PR rows: %d", n)
	}
}
