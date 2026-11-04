package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
)

// Values for ListProjects' archived filter.
const (
	ProjectsActive   = "active"
	ProjectsArchived = "archived"
	ProjectsAll      = "all"
)

// ArchivedQueuedReason is the error recorded on queued runs cancelled by archiving.
const ArchivedQueuedReason = "project archived"

// ActiveRunsError is returned by ArchiveProject when the project still has runs past the
// queue; archiving never orphans a running run.
type ActiveRunsError struct {
	Project string
	Runs    []dbgen.ActiveRunsOfProjectRow
}

func (e *ActiveRunsError) Error() string {
	parts := make([]string, len(e.Runs))
	for i, r := range e.Runs {
		parts[i] = fmt.Sprintf("run %d (%s, task %s %q)", r.ID, r.State, r.TaskSqid, r.TaskTitle)
	}
	return fmt.Sprintf("project %q has active runs; cancel them first: %s", e.Project, strings.Join(parts, ", "))
}

// ArchiveProject archives a project in one transaction: it refuses with *ActiveRunsError when
// runs are active, otherwise cancels the queued runs ('project archived') and sets archived_at.
// It returns the archived project and the ids of the cancelled queued runs. Archiving an
// already archived project returns sql.ErrNoRows.
func (d *DB) ArchiveProject(ctx context.Context, projectID int64) (dbgen.Project, []int64, error) {
	var p dbgen.Project
	var cancelled []int64
	err := d.Tx(ctx, func(q *dbgen.Queries) error {
		active, err := q.ActiveRunsOfProject(ctx, projectID)
		if err != nil {
			return err
		}
		if len(active) > 0 {
			cur, _ := q.GetProject(ctx, projectID)
			return &ActiveRunsError{Project: cur.Slug, Runs: active}
		}
		now := sql.NullString{String: time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), Valid: true}
		if p, err = q.ArchiveProject(ctx, dbgen.ArchiveProjectParams{Now: now, ID: projectID}); err != nil {
			return err
		}
		rows, err := q.CancelQueuedRunsOfProject(ctx, dbgen.CancelQueuedRunsOfProjectParams{Now: now, Reason: ArchivedQueuedReason, ProjectID: projectID})
		for _, r := range rows {
			cancelled = append(cancelled, r.ID)
		}
		return err
	})
	var are *ActiveRunsError
	if errors.As(err, &are) {
		return p, nil, are
	}
	return p, cancelled, err
}
