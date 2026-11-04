package db_test

import (
	"context"
	"errors"
	"testing"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
)

// archiveFixture: two projects, each with one task; returns their ids.
func archiveFixture(t *testing.T) (*db.DB, int64, int64) {
	t.Helper()
	d, _ := open(t)
	ctx := context.Background()
	a, err := d.Q.CreateProject(ctx, dbgen.CreateProjectParams{Slug: "alpha", Name: "Alpha"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := d.Q.CreateProject(ctx, dbgen.CreateProjectParams{Slug: "beta", Name: "Beta"})
	if err != nil {
		t.Fatal(err)
	}
	exec(t, d, `INSERT INTO task (project_id, rank, title, sqid) VALUES (?, 'V', 'ta', 'AAAAAA')`, a.ID)
	exec(t, d, `INSERT INTO task (project_id, rank, title, sqid) VALUES (?, 'V', 'tb', 'BBBBBB')`, b.ID)
	return d, a.ID, b.ID
}

func TestArchiveHidesAndRestores(t *testing.T) {
	d, a, _ := archiveFixture(t)
	ctx := context.Background()
	if _, _, err := d.ArchiveProject(ctx, a); err != nil {
		t.Fatal(err)
	}
	count := func(filter string) int {
		ps, err := d.RQ.ListProjects(ctx, filter)
		if err != nil {
			t.Fatal(err)
		}
		return len(ps)
	}
	if count(db.ProjectsActive) != 1 || count(db.ProjectsArchived) != 1 || count(db.ProjectsAll) != 2 {
		t.Fatalf("filters: active=%d archived=%d all=%d", count(db.ProjectsActive), count(db.ProjectsArchived), count(db.ProjectsAll))
	}
	// by-slug / by-id still find it
	p, err := d.RQ.GetProjectBySlug(ctx, "alpha")
	if err != nil || !p.ArchivedAt.Valid {
		t.Fatalf("by slug: %v %+v", err, p)
	}
	if _, err := d.RQ.GetProject(ctx, a); err != nil {
		t.Fatal(err)
	}
	// task list excludes it
	ts, err := d.RQ.ListTasks(ctx, dbgen.ListTasksParams{})
	if err != nil || len(ts) != 1 || ts[0].ProjectSlug != "beta" {
		t.Fatalf("ListTasks: %v %+v", err, ts)
	}
	// archiving twice is a no-op error; restore brings it back
	if _, _, err := d.ArchiveProject(ctx, a); err == nil {
		t.Fatal("second archive should fail")
	}
	if _, err := d.Q.RestoreProject(ctx, a); err != nil {
		t.Fatal(err)
	}
	if count(db.ProjectsActive) != 2 || count(db.ProjectsArchived) != 0 {
		t.Fatal("restore did not bring it back")
	}
	if ts, _ := d.RQ.ListTasks(ctx, dbgen.ListTasksParams{}); len(ts) != 2 {
		t.Fatalf("ListTasks after restore: %d", len(ts))
	}
}

func TestArchiveRefusedWithRunningRun(t *testing.T) {
	d, a, _ := archiveFixture(t)
	ctx := context.Background()
	exec(t, d, `INSERT INTO run (task_id, state) VALUES (1, 'running')`)
	exec(t, d, `INSERT INTO run (task_id, state) VALUES (1, 'queued')`)
	_, _, err := d.ArchiveProject(ctx, a)
	var are *db.ActiveRunsError
	if !errors.As(err, &are) || len(are.Runs) != 1 || are.Runs[0].State != "running" || are.Runs[0].TaskSqid != "AAAAAA" {
		t.Fatalf("want ActiveRunsError listing the running run, got %v", err)
	}
	p, _ := d.RQ.GetProject(ctx, a)
	if p.ArchivedAt.Valid {
		t.Fatal("project archived despite running run")
	}
	var queued int
	if err := d.R.QueryRow(`SELECT COUNT(*) FROM run WHERE state = 'queued'`).Scan(&queued); err != nil || queued != 1 {
		t.Fatalf("queued run must stay queued after refusal: %d %v", queued, err)
	}
}

func TestArchiveCancelsQueuedRuns(t *testing.T) {
	d, a, _ := archiveFixture(t)
	ctx := context.Background()
	exec(t, d, `INSERT INTO run (task_id, state) VALUES (1, 'queued')`)
	exec(t, d, `INSERT INTO run (task_id, state) VALUES (1, 'succeeded')`)
	exec(t, d, `INSERT INTO run (task_id, state) VALUES (2, 'queued')`) // other project
	_, cancelled, err := d.ArchiveProject(ctx, a)
	if err != nil || len(cancelled) != 1 || cancelled[0] != 1 {
		t.Fatalf("cancelled=%v err=%v", cancelled, err)
	}
	var state, reason string
	d.R.QueryRow(`SELECT state, error FROM run WHERE id = 1`).Scan(&state, &reason)
	if state != "cancelled" || reason != "project archived" {
		t.Fatalf("run 1: %s %q", state, reason)
	}
	d.R.QueryRow(`SELECT state FROM run WHERE id = 3`).Scan(&state)
	if state != "queued" {
		t.Fatalf("other project's run touched: %s", state)
	}
}

func TestArchivedQueuedRunsNotAssignedOrDemanded(t *testing.T) {
	d, a, _ := archiveFixture(t)
	ctx := context.Background()
	exec(t, d, `INSERT INTO run (task_id) VALUES (1)`)
	exec(t, d, `INSERT INTO run (task_id) VALUES (2)`)
	// a queued run can exist on an archived project only by direct write; it must still be skipped
	exec(t, d, `UPDATE project SET archived_at = '2026-01-01T00:00:00.000Z' WHERE id = ?`, a)
	qs, err := d.RQ.QueuedRunsForAssignment(ctx)
	if err != nil || len(qs) != 1 || qs[0].TaskID != 2 {
		t.Fatalf("QueuedRunsForAssignment: %v %+v", err, qs)
	}
	runs, _ := d.RQ.ListRuns(ctx, dbgen.ListRunsParams{Lim: 10})
	if len(runs) != 1 {
		t.Fatalf("ListRuns: %d", len(runs))
	}
	active, _ := d.RQ.ListActiveRuns(ctx)
	if len(active) != 1 {
		t.Fatalf("ListActiveRuns: %d", len(active))
	}
}
