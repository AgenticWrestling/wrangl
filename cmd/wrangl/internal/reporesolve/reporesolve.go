// Package reporesolve decides which repo a task writes to (ADR 0012 section 3). The queue and the
// scheduler both use it, so they cannot disagree.
package reporesolve

import (
	"context"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbtypes"
)

// Resolve returns the repo id the task writes to, or 0 when it needs one chosen. The order, first hit
// wins: the task's own repo, the nearest ancestor's, the project's notes repo (research tasks only),
// the project's default repo, the project's only linked repo.
func Resolve(ctx context.Context, q *dbgen.Queries, taskID int64) (int64, error) {
	t, err := q.GetTask(ctx, taskID)
	if err != nil {
		return 0, err
	}
	chain, err := q.AncestorChain(ctx, taskID) // node first, then ancestors
	if err != nil {
		return 0, err
	}
	proj, err := q.GetProject(ctx, t.ProjectID)
	if err != nil {
		return 0, err
	}
	return ResolveIn(ctx, q, proj, t.TaskType, chain)
}

// ResolveIn is Resolve for a caller that already has the project and the task's ancestor chain.
func ResolveIn(ctx context.Context, q *dbgen.Queries, proj dbgen.Project, taskType dbtypes.TaskType, chain []dbgen.Task) (int64, error) {
	for _, a := range chain {
		if a.RepoID.Valid {
			return a.RepoID.Int64, nil
		}
	}
	if taskType == dbtypes.TaskResearch { // reports live in the notes repo
		if id, err := q.GetNotesRepoID(ctx, proj.ID); err == nil {
			return id, nil
		}
	}
	if proj.DefaultRepoID.Valid {
		return proj.DefaultRepoID.Int64, nil
	}
	rc, err := q.ListProjectRepoCount(ctx, proj.ID)
	if err != nil {
		return 0, err
	}
	if rc.N == 1 {
		return rc.OnlyID, nil
	}
	return 0, nil
}

// NeedsRepo is the status of a queued run whose task resolves to no repo.
const NeedsRepo = "waiting: needs a repo"
