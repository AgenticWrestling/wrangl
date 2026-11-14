package scheduler

import (
	"context"
	"strings"
	"testing"
)

// Two repos linked, a parent task and a child with a queued run and no repo anywhere.
func needsRepoFixture(t *testing.T) (*Scheduler, func(q string) (n int)) {
	s, _, _, d := setup(t, 1)
	d.W.Exec(`UPDATE runner_template SET agent_json = '{"start":["true"]}', max_sessions = 1`)
	d.W.Exec(`INSERT INTO repo (project_id, owner, name, default_branch, clone_url) VALUES (1, 'o', 'a', 'main', 'file:///x'), (1, 'o', 'b', 'main', 'file:///y')`)
	d.W.Exec(`INSERT INTO task (project_id, rank, title) VALUES (1, 'V001', 'epic')`)
	d.W.Exec(`INSERT INTO task (project_id, parent_id, rank, title) VALUES (1, 1, 'V001', 'kid')`)
	d.W.Exec(`INSERT INTO run (task_id, template_id, status_text) VALUES (2, 1, 'waiting: needs a repo')`)
	return s, func(q string) (n int) { d.R.QueryRow(q).Scan(&n); return }
}

func TestNeedsRepoItemLifecycleAndAnswers(t *testing.T) {
	s, count := needsRepoFixture(t)
	ctx := context.Background()
	s.Assign(ctx)
	s.Assign(ctx) // one open item per run
	if n := count(`SELECT COUNT(*) FROM question WHERE kind = 'needs_repo' AND state = 'open'`); n != 1 {
		t.Fatalf("open items: %d", n)
	}
	rows, _ := s.Fleet.DB.R.Query(`SELECT value FROM question_option ORDER BY position`)
	var vals []string
	for rows.Next() {
		var v string
		rows.Scan(&v)
		vals = append(vals, v)
	}
	rows.Close()
	if got := strings.Join(vals, " "); got != "repo:o/a repo_tree:o/a repo:o/b repo_tree:o/b link_repo cancel_run" {
		t.Fatalf("options: %s", got)
	}
	q, _ := s.Fleet.DB.RQ.GetQuestion(ctx, 1)

	if err := s.AnswerNeedsRepo(ctx, q, "repo:o/zzz", ""); err == nil {
		t.Fatal("unlinked repo accepted")
	}
	if err := s.AnswerNeedsRepo(ctx, q, "link_repo", ""); err != nil || count(`SELECT COUNT(*) FROM task WHERE repo_id IS NOT NULL`) != 0 {
		t.Fatalf("link_repo changed something: %v", err)
	}
	// The whole subtree: the topmost ancestor without a repo gets it; the run takes it.
	if err := s.AnswerNeedsRepo(ctx, q, "repo_tree:o/b", ""); err != nil {
		t.Fatal(err)
	}
	if count(`SELECT repo_id FROM task WHERE id = 1`) != 2 || count(`SELECT COALESCE(repo_id, 0) FROM task WHERE id = 2`) != 0 {
		t.Fatal("repo not set on the ancestor only")
	}
	if count(`SELECT COALESCE(repo_id, 0) FROM run`) != 2 {
		t.Fatal("run did not take the repo")
	}
	s.Assign(ctx) // closes the item
	if n := count(`SELECT COUNT(*) FROM question WHERE kind = 'needs_repo' AND state = 'open'`); n != 0 {
		t.Fatalf("item still open: %d", n)
	}
}

func TestNeedsRepoAnswerThisTaskAndCancel(t *testing.T) {
	s, count := needsRepoFixture(t)
	ctx := context.Background()
	s.Assign(ctx)
	q, _ := s.Fleet.DB.RQ.GetQuestion(ctx, 1)
	if err := s.AnswerNeedsRepo(ctx, q, "repo:o/a", ""); err != nil {
		t.Fatal(err)
	}
	if count(`SELECT repo_id FROM task WHERE id = 2`) != 1 || count(`SELECT COALESCE(repo_id, 0) FROM task WHERE id = 1`) != 0 {
		t.Fatal("repo not set on this task only")
	}
	cancelled := 0
	s.CancelRun = func(context.Context, int64) error { cancelled++; return nil }
	if err := s.AnswerNeedsRepo(ctx, q, "cancel_run", ""); err != nil || cancelled != 1 {
		t.Fatalf("cancel: %v %d", err, cancelled)
	}
}

func TestNeedsRepoItemClosesWhenRunCancelled(t *testing.T) {
	s, count := needsRepoFixture(t)
	ctx := context.Background()
	s.Assign(ctx)
	s.Fleet.DB.W.Exec(`UPDATE run SET state = 'cancelled'`)
	s.Assign(ctx)
	if n := count(`SELECT COUNT(*) FROM question WHERE kind = 'needs_repo' AND state = 'open'`); n != 0 {
		t.Fatalf("item still open: %d", n)
	}
}

// Two runs over one milestone, one per repo: the run whose precursor is in the other repo waits with
// the precursor reason; the run with no open leaves of its repo left is cancelled.
func TestPerRepoRunsWaitOnCrossRepoPrecursor(t *testing.T) {
	s, _, _, d := setup(t, 1)
	ctx := context.Background()
	d.W.Exec(`UPDATE runner_template SET agent_json = '{"start":["true"]}', max_sessions = 1`)
	d.W.Exec(`INSERT INTO repo (project_id, owner, name, default_branch, clone_url) VALUES (1, 'o', 'a', 'main', 'file:///x'), (1, 'o', 'b', 'main', 'file:///y')`)
	if _, err := d.W.Exec(`INSERT INTO task (id, project_id, rank, title) VALUES (1, 1, 'V001', 'ms')`); err != nil {
		t.Fatal(err)
	}
	d.W.Exec(`INSERT INTO task (id, project_id, parent_id, rank, title, repo_id, state, completed_run_id) VALUES (2, 1, 1, 'V001', 'a1', 1, 'in_review', 999)`)
	d.W.Exec(`INSERT INTO task (id, project_id, parent_id, rank, title, repo_id) VALUES (3, 1, 1, 'V002', 'b1', 2)`)
	if _, err := d.W.Exec(`INSERT INTO task_dependency (task_id, depends_on_id, created_by) VALUES (3, 2, 'user')`); err != nil {
		t.Fatal(err)
	}
	d.W.Exec(`INSERT INTO run (task_id, template_id, repo_id) VALUES (1, 1, 1), (1, 1, 2)`)
	s.Assign(ctx)
	var stA, stB, txtB string
	d.R.QueryRow(`SELECT state FROM run WHERE repo_id = 1`).Scan(&stA)
	d.R.QueryRow(`SELECT state, status_text FROM run WHERE repo_id = 2`).Scan(&stB, &txtB)
	if stA != "cancelled" {
		t.Fatalf("run A (nothing left in its repo): %s", stA)
	}
	if stB != "queued" || txtB != "waiting: precursor tasks are not finished yet" {
		t.Fatalf("run B: %s %q", stB, txtB)
	}
}
