package runs_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/errs"
)

func TestToolFailureReachesTheInboxOnce(t *testing.T) {
	m := newMaint(t)
	ctx := context.Background()
	run := m.startRun(time.Minute)
	open := func() int {
		return m.count(`SELECT COUNT(*) FROM question WHERE kind='tool_error' AND state='open' AND run_id=?`, run)
	}

	m.rs.NoteToolError(ctx, run, "issue_get", fmt.Errorf("github: 403 forbidden"))
	m.rs.NoteToolError(ctx, run, "issue_get", fmt.Errorf("github: 403 forbidden again"))
	if n := open(); n != 1 {
		t.Fatalf("want one open item for the same run and tool, got %d", n)
	}
	m.rs.NoteToolError(ctx, run, "issue_comment", fmt.Errorf("github: 403"))
	if n := open(); n != 2 {
		t.Fatalf("a different tool gets its own item, got %d", n)
	}
	// The run keeps going: a notice never parks it as needing input.
	if n := m.count(`SELECT COUNT(*) FROM run WHERE id=? AND state='running'`, run); n != 1 {
		t.Fatal("run should still be running")
	}
	// Agent mistakes are not surfaced.
	m.rs.NoteToolError(ctx, run, "task_get", fmt.Errorf("%w: no such task", errs.NotFound))
	m.rs.NoteToolError(ctx, run, "task_create", errors.Join(errs.Invalid, errors.New("bad")))
	if n := open(); n != 2 {
		t.Fatalf("agent mistakes must not create items, got %d", n)
	}
	// Dismissing works and the run's end cancels the rest.
	var id int64
	m.d.R.QueryRow(`SELECT id FROM question WHERE kind='tool_error' LIMIT 1`).Scan(&id)
	if err := m.rs.Answer(ctx, id, "dismiss", ""); err != nil {
		t.Fatal(err)
	}
	if n := open(); n != 1 {
		t.Fatalf("after dismiss want 1 open, got %d", n)
	}
}
