package plans

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/errs"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/stack"
)

// Planning and queueing are refused for tasks of an archived project, and work after a restore.
func TestQueueRefusedWhileProjectArchived(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for _, q := range []string{
		`INSERT INTO runner_template (name, mise_toml, agent_json, min_vcpu, min_ram_mib, min_disk_gib) VALUES ('go','','{}',1,1,1)`,
		`INSERT INTO project (slug, name, default_template_id) VALUES ('p','p',1)`,
		`INSERT INTO repo (project_id, owner, name) VALUES (1,'o','a')`,
	} {
		if _, err := d.W.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	sq, _ := stack.NewSqids(stack.DefaultSqidsAlphabet)
	st := &stack.Service{DB: d, Sqids: sq}
	s := &Service{DB: d, Stack: st}
	task, err := st.Create(ctx, stack.CreateParams{Project: "p", Title: "x", Actor: stack.User})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.ArchiveProject(ctx, 1); err != nil {
		t.Fatal(err)
	}
	sqid := task.Sqid.String
	if _, err := s.Queue(ctx, sqid, ""); !errors.Is(err, errs.Conflict) || !strings.Contains(err.Error(), "archived") {
		t.Errorf("plan: %v", err)
	}
	if _, err := s.QueueWork(ctx, sqid); !errors.Is(err, errs.Conflict) || !strings.Contains(err.Error(), "archived") {
		t.Errorf("queue: %v", err)
	}
	if _, err := d.Q.RestoreProject(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.QueueWork(ctx, sqid); err != nil {
		t.Fatalf("queue after restore: %v", err)
	}
}
