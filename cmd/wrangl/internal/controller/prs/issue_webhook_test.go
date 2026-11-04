package prs

import (
	"context"
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
