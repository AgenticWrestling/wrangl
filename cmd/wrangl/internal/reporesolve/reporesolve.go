// Package reporesolve decides which repo a task writes to (ADR 0012 section 3). The queue and the
// scheduler both use it, so they cannot disagree.
package reporesolve

import (
	"context"
	"database/sql"

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

// OpenLeafRepos returns the distinct repos (0 = needs one) the open leaves under root resolve to, in
// the order the leaves are worked.
func OpenLeafRepos(ctx context.Context, q *dbgen.Queries, rootID int64) ([]int64, error) {
	ids, err := q.OpenLeafIDs(ctx, rootID)
	if err != nil {
		return nil, err
	}
	var out []int64
	seen := map[int64]bool{}
	for _, id := range ids {
		r, err := Resolve(ctx, q, id)
		if err != nil {
			return nil, err
		}
		if !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	return out, nil
}

// CountOpenLeaves is the number of open leaves under root that resolve to repoID (0 = needs one).
func CountOpenLeaves(ctx context.Context, q *dbgen.Queries, rootID, repoID int64) (int64, error) {
	ids, err := q.OpenLeafIDs(ctx, rootID)
	if err != nil {
		return 0, err
	}
	var n int64
	for _, id := range ids {
		if r, err := Resolve(ctx, q, id); err != nil {
			return 0, err
		} else if r == repoID {
			n++
		}
	}
	return n, nil
}

// NextReadyLeaf is dbgen's NextReadyLeaf restricted to the leaves that resolve to repoID: the run's
// own repo. sql.ErrNoRows when there is none.
func NextReadyLeaf(ctx context.Context, q *dbgen.Queries, rootID, runID, repoID int64) (dbgen.NextReadyLeafRow, error) {
	leaves, err := q.ReadyLeaves(ctx, dbgen.ReadyLeavesParams{RootID: rootID, RunID: sql.NullInt64{Int64: runID, Valid: true}})
	if err != nil {
		return dbgen.NextReadyLeafRow{}, err
	}
	for _, l := range leaves {
		if r, err := Resolve(ctx, q, l.ID); err != nil {
			return dbgen.NextReadyLeafRow{}, err
		} else if r == repoID {
			return dbgen.NextReadyLeafRow(l), nil
		}
	}
	return dbgen.NextReadyLeafRow{}, sql.ErrNoRows
}

// Name is the owner/name of a repo id ("" for 0 or an unknown id).
func Name(ctx context.Context, q *dbgen.Queries, repoID int64) string {
	if repoID == 0 {
		return ""
	}
	r, err := q.GetRepo(ctx, repoID)
	if err != nil {
		return ""
	}
	return r.Owner + "/" + r.Name
}
