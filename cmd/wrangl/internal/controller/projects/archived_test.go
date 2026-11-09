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

// Editing an archived project (settings, repos, limits, issue sync) is refused with a 409 naming
// the archive; restoring it makes the same calls work.
func TestArchivedProjectIsReadOnlyUntilRestored(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	d.W.Exec(`INSERT INTO runner_template (name, mise_toml, agent_json, min_vcpu, min_ram_mib, min_disk_gib) VALUES ('go','','{}',1,1,1)`)
	fake := gh.NewFake()
	fake.AddRepo(gh.Repo{Owner: "acme", Name: "app", DefaultBranch: "main", CloneURL: "file:///tmp/app.git"})
	sq, _ := stack.NewSqids(stack.DefaultSqidsAlphabet)
	s := &Service{DB: d, GH: fake, Stack: &stack.Service{DB: d, Sqids: sq}, Now: time.Now}
	if _, err := s.Create(ctx, "web", ProjectParams{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LinkRepo(ctx, "web", "acme/app", ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Archive(ctx, "web"); err != nil {
		t.Fatal(err)
	}
	prio := int64(3)
	ops := map[string]func() error{
		"update":    func() error { _, err := s.Update(ctx, "web", ProjectParams{Priority: &prio}); return err },
		"notes":     func() error { return s.SetNotesRepo(ctx, "web", "acme/app") },
		"link":      func() error { _, err := s.LinkRepo(ctx, "web", "acme/app", ""); return err },
		"unlink":    func() error { return s.UnlinkRepo(ctx, "web", "acme/app") },
		"sync":      func() error { _, err := s.SyncIssues(ctx, "web"); return err },
		"issuetask": func() error { _, err := s.IssueToTask(ctx, "web", "acme/app#1", "", ""); return err },
	}
	for name, op := range ops {
		if err := op(); !errors.Is(err, errs.Conflict) || !strings.Contains(err.Error(), "archived") {
			t.Errorf("%s on an archived project: %v", name, err)
		}
	}
	if _, err := s.Restore(ctx, "web"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update(ctx, "web", ProjectParams{Priority: &prio}); err != nil {
		t.Fatalf("update after restore: %v", err)
	}
	if _, err := s.SyncIssues(ctx, "web"); err != nil {
		t.Fatalf("sync after restore: %v", err)
	}
}
