package prs

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/gh"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/errs"
)

// Webhook events for a repo of an archived project are ignored (nothing is read or written), and
// everything works again after a restore.
func TestWebhookIgnoresArchivedProjects(t *testing.T) {
	s, f := mergeableFixture(t)
	s.Now = time.Now
	ctx := context.Background()
	if _, _, err := s.DB.ArchiveProject(ctx, 1); err != nil {
		t.Fatal(err)
	}
	f.SetMergeable(1, gh.MergeConflicted)
	push := `{"ref":"refs/heads/main","repository":{"name":"app","owner":{"login":"acme"}}}`
	pull := `{"action":"synchronize","pull_request":{"number":1},"repository":{"name":"app","owner":{"login":"acme"}}}`
	issue := `{"action":"opened","issue":{"number":9,"title":"late","state":"open","html_url":"u","user":{"login":"a"},"updated_at":"2026-11-04T10:00:00Z"},"repository":{"name":"app","owner":{"login":"acme"}}}`
	for ev, body := range map[string]string{"push": push, "pull_request": pull, "issues": issue} {
		if err := s.handleEvent(ctx, ev, []byte(body)); err != nil {
			t.Fatalf("%s: %v", ev, err)
		}
	}
	if got := mergeableOf(t, s, 1); got != "unknown" {
		t.Fatalf("an archived project's PR was refreshed by a webhook: %q", got)
	}
	var n int
	s.DB.R.QueryRow(`SELECT COUNT(*) FROM issue`).Scan(&n)
	if n != 0 {
		t.Fatalf("an issue event for an archived project was stored (%d)", n)
	}
	if _, err := s.DB.Q.RestoreProject(ctx, 1); err != nil {
		t.Fatal(err)
	}
	for ev, body := range map[string]string{"push": push, "issues": issue} {
		if err := s.handleEvent(ctx, ev, []byte(body)); err != nil {
			t.Fatalf("%s after restore: %v", ev, err)
		}
	}
	if got := mergeableOf(t, s, 1); got != "conflicted" {
		t.Fatalf("after restore the webhook must work again: %q", got)
	}
	s.DB.R.QueryRow(`SELECT COUNT(*) FROM issue`).Scan(&n)
	if n != 1 {
		t.Fatalf("issue event after restore not stored (%d)", n)
	}
}

// The owner's PR actions are refused while the project is archived.
func TestPRActionsRefusedWhenArchived(t *testing.T) {
	s, _ := mergeableFixture(t)
	ctx := context.Background()
	if _, _, err := s.DB.ArchiveProject(ctx, 1); err != nil {
		t.Fatal(err)
	}
	actions := map[string]func() error{
		"approve":         func() error { return s.Approve(ctx, 1) },
		"request-changes": func() error { return s.RequestChanges(ctx, 1, "fix it") },
		"close":           func() error { return s.Close(ctx, 1) },
		"resolve":         func() error { return s.ResolveConflicts(ctx, 1) },
	}
	for name, fn := range actions {
		if err := fn(); !errors.Is(err, errs.Conflict) || !strings.Contains(err.Error(), "archived") {
			t.Errorf("%s on an archived project's PR: %v", name, err)
		}
	}
}
