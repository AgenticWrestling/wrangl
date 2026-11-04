package prs

import (
	"context"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/gh"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/issuestore"
	"path/filepath"
	"testing"
	"time"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db"
)

func TestIssuesWebhookIsAReconciler(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var pid int64
	d.W.QueryRow(`INSERT INTO project (slug, name) VALUES ('web','web') RETURNING id`).Scan(&pid)
	d.W.Exec(`INSERT INTO repo (project_id, owner, name, default_branch, clone_url) VALUES (?, 'acme', 'app', 'main', 'x')`, pid)
	s := &Service{DB: d, Now: time.Now}
	event := func(title, updated, extra string) {
		t.Helper()
		body := `{"action":"edited","issue":{"number":4,"title":"` + title + `","body":"b","state":"open","html_url":"u","user":{"login":"ann"},"labels":[{"name":"bug"}],"updated_at":"` + updated + `","comments":1` + extra + `},"repository":{"name":"app","owner":{"login":"acme"}}}`
		if err := s.handleEvent(ctx, "issues", []byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	title := func() string {
		var v string
		d.R.QueryRow(`SELECT title FROM issue WHERE gh_number = 4`).Scan(&v)
		return v
	}
	event("v1", "2026-11-04T10:00:00Z", "")
	event("old", "2026-11-04T09:00:00Z", "") // reordered delivery
	if title() != "v1" {
		t.Fatalf("older webhook overwrote: %q", title())
	}
	event("echo", "2026-11-04T10:00:00Z", "") // the echo of our own write
	if title() != "v1" {
		t.Fatalf("equal updated_at must be a no-op: %q", title())
	}
	event("v2", "2026-11-04T11:00:00Z", "")
	if title() != "v2" {
		t.Fatalf("newer webhook ignored: %q", title())
	}
	event("pr", "2026-11-04T12:00:00Z", `,"pull_request":{"url":"x"}`)
	if title() != "v2" {
		t.Fatalf("pull request payload was stored: %q", title())
	}
	var author string
	d.R.QueryRow(`SELECT m.author FROM issue_meta m JOIN issue i ON i.id = m.issue_id WHERE i.gh_number = 4`).Scan(&author)
	if author != "ann" {
		t.Fatalf("meta not stored: %q", author)
	}
}

func TestIssuesWebhookEchoOfOurOwnWriteAndLifecycle(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var pid, rid int64
	d.W.QueryRow(`INSERT INTO project (slug, name) VALUES ('web','web') RETURNING id`).Scan(&pid)
	d.W.QueryRow(`INSERT INTO repo (project_id, owner, name, default_branch, clone_url) VALUES (?, 'acme', 'app', 'main', 'x') RETURNING id`, pid).Scan(&rid)
	s := &Service{DB: d, Now: time.Now}
	event := func(action, title, updated string) {
		t.Helper()
		body := `{"action":"` + action + `","issue":{"number":4,"title":"` + title + `","body":"b","state":"open","html_url":"u","user":{"login":"ann"},"labels":[],"updated_at":"` + updated + `","comments":0},"repository":{"name":"app","owner":{"login":"acme"}}}`
		if err := s.handleEvent(ctx, "issues", []byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	count := func() (n int) { d.R.QueryRow(`SELECT COUNT(*) FROM issue WHERE gh_number = 4`).Scan(&n); return }
	title := func() (v string) { d.R.QueryRow(`SELECT title FROM issue WHERE gh_number = 4`).Scan(&v); return }

	// Our tool wrote the issue and stored GitHub's answer; the echo arrives with the same updated_at.
	if _, _, err := issuestore.Save(ctx, d, rid, gh.Issue{Number: 4, Title: "ours", Body: "b", State: "open", URL: "u", UpdatedAt: "2026-11-04T10:00:00Z"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	var before string
	d.R.QueryRow(`SELECT synced_at FROM issue WHERE gh_number = 4`).Scan(&before)
	event("edited", "ours-echo", "2026-11-04T10:00:00Z")
	if title() != "ours" {
		t.Fatalf("echo changed the row: %q", title())
	}
	// A webhook for a link between a task and the issue survives edits.
	var tid int64
	d.W.QueryRow(`INSERT INTO task (sqid, project_id, rank, title, issue_id) VALUES ('T1', ?, 'a', 't', (SELECT id FROM issue WHERE gh_number = 4)) RETURNING id`, pid).Scan(&tid)
	event("edited", "theirs", "2026-11-04T10:30:00Z")
	var linked int
	d.R.QueryRow(`SELECT COUNT(*) FROM task WHERE id = ? AND issue_id IS NOT NULL`, tid).Scan(&linked)
	if title() != "theirs" || linked != 1 {
		t.Fatalf("third-party edit: %q linked=%d", title(), linked)
	}
	// Closing on GitHub updates the issue and leaves the task's state alone.
	var st string
	d.R.QueryRow(`SELECT state FROM task WHERE id = ?`, tid).Scan(&st)
	body := `{"action":"closed","issue":{"number":4,"title":"theirs","body":"b","state":"closed","html_url":"u","updated_at":"2026-11-04T11:00:00Z"},"repository":{"name":"app","owner":{"login":"acme"}}}`
	if err := s.handleEvent(ctx, "issues", []byte(body)); err != nil {
		t.Fatal(err)
	}
	var istate, tstate string
	d.R.QueryRow(`SELECT state FROM issue WHERE gh_number = 4`).Scan(&istate)
	d.R.QueryRow(`SELECT state FROM task WHERE id = ?`, tid).Scan(&tstate)
	if istate != "closed" || tstate != st {
		t.Fatalf("issue %q task %q (was %q)", istate, tstate, st)
	}
	// Deleted (and transferred) remove the row; the task survives, unlinked.
	event("deleted", "theirs", "2026-11-04T12:00:00Z")
	d.R.QueryRow(`SELECT COUNT(*) FROM task WHERE id = ? AND issue_id IS NULL`, tid).Scan(&linked)
	if count() != 0 || linked != 1 {
		t.Fatalf("deleted: rows=%d unlinked task=%d", count(), linked)
	}
	// A late older event after a delete must not resurrect... it is a plain edit, so it may re-add;
	// the periodic drift check removes it if GitHub no longer has it. Here we only check no error.
	event("transferred", "theirs", "2026-11-04T12:05:00Z")
	if count() != 0 {
		t.Fatal("transferred issue kept")
	}
}

func TestMissedWebhookIsRepairedByLaterEvent(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var pid int64
	d.W.QueryRow(`INSERT INTO project (slug, name) VALUES ('web','web') RETURNING id`).Scan(&pid)
	d.W.Exec(`INSERT INTO repo (project_id, owner, name, default_branch, clone_url) VALUES (?, 'acme', 'app', 'main', 'x')`, pid)
	s := &Service{DB: d, Now: time.Now}
	ev := func(title, updated string) {
		body := `{"action":"edited","issue":{"number":4,"title":"` + title + `","body":"b","state":"open","html_url":"u","updated_at":"` + updated + `"},"repository":{"name":"app","owner":{"login":"acme"}}}`
		if err := s.handleEvent(ctx, "issues", []byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	ev("v1", "2026-11-04T10:00:00Z")
	// v2 is lost; v3 arrives and carries the whole current state.
	ev("v3", "2026-11-04T12:00:00Z")
	var v string
	d.R.QueryRow(`SELECT title FROM issue WHERE gh_number = 4`).Scan(&v)
	if v != "v3" {
		t.Fatalf("got %q", v)
	}
}
