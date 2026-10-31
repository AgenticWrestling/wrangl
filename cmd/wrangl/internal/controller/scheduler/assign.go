package scheduler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nreid/devhost/cmd/wrangl/internal/controller/natsd"
	"github.com/nreid/devhost/cmd/wrangl/internal/controller/projects"
	"github.com/nreid/devhost/cmd/wrangl/internal/db/dbgen"
	"github.com/nreid/devhost/cmd/wrangl/internal/proto"
)

func nstr(v string) sql.NullString { return sql.NullString{String: v, Valid: v != ""} }

// Assign hands queued runs to idle runners. It returns the run ids assigned.
func (s *Scheduler) Assign(ctx context.Context) ([]int64, error) {
	q := s.Fleet.DB
	queued, err := q.RQ.QueuedRunsForAssignment(ctx)
	if err != nil {
		return nil, err
	}
	var assigned []int64
	var errs []error
	for _, r := range queued {
		if !r.TemplateID.Valid || !r.RepoID.Valid {
			continue
		}
		// Only runs with a leaf ready right now (fresh run, so nothing completed in it yet).
		if _, err := q.RQ.NextReadyLeaf(ctx, dbgen.NextReadyLeafParams{RootID: r.TaskID, RunID: sql.NullInt64{Int64: r.ID, Valid: true}}); errors.Is(err, sql.ErrNoRows) {
			open, err := q.RQ.CountOpenLeaves(ctx, r.TaskID)
			if err == nil && open == 0 {
				// Everything was finished or cancelled since it was queued.
				q.Q.CancelQueuedRun(ctx, dbgen.CancelQueuedRunParams{Now: nstr(s.Now().UTC().Format(time.RFC3339Nano)), ID: r.ID})
			}
			continue // waiting on outside precursors
		} else if err != nil {
			errs = append(errs, err)
			continue
		}
		if err := s.assignOne(ctx, r); err != nil {
			if !errors.Is(err, sql.ErrNoRows) { // no idle runner is normal
				errs = append(errs, fmt.Errorf("run %d: %w", r.ID, err))
			}
			continue
		}
		assigned = append(assigned, r.ID)
	}
	return assigned, errors.Join(errs...)
}

func (s *Scheduler) assignOne(ctx context.Context, r dbgen.QueuedRunsForAssignmentRow) error {
	d := s.Fleet.DB
	tmpl, err := d.RQ.GetTemplate(ctx, r.TemplateID.Int64)
	if err != nil {
		return err
	}
	repo, err := d.RQ.GetRepo(ctx, r.RepoID.Int64)
	if err != nil {
		return err
	}
	spec, err := proto.ParseAgentSpec(tmpl.AgentJson)
	if err != nil {
		return fmt.Errorf("template %s agent block: %w", tmpl.Name, err)
	}
	if len(spec.Start) == 0 {
		return fmt.Errorf("template %s has no agent.start command", tmpl.Name)
	}
	env := map[string]string{}
	for k, name := range spec.Secrets {
		if s.Secret == nil {
			return fmt.Errorf("secret %q needed but no secret store", name)
		}
		v, err := s.Secret(name)
		if err != nil {
			return fmt.Errorf("template %s needs secret %q: %w (wrangl controller secret set %s)", tmpl.Name, name, err, name)
		}
		env[k] = strings.TrimSpace(string(v))
	}
	spec.Secrets = nil // names stay on the controller

	runner, err := d.Q.ClaimIdleRunner(ctx, r.TemplateID.Int64)
	if err != nil {
		return err // sql.ErrNoRows: no idle runner
	}
	release := func() {
		d.Q.UnassignRun(context.WithoutCancel(ctx), r.ID)
		d.Q.ReleaseRunner(context.WithoutCancel(ctx), dbgen.ReleaseRunnerParams{Now: nstr(s.Now().UTC().Format("2006-01-02T15:04:05.000Z")), ID: runner.ID})
	}
	session := runner.Name + ":" + r.TaskSqid
	n, err := d.Q.AssignRun(ctx, dbgen.AssignRunParams{RunnerID: sql.NullInt64{Int64: runner.ID, Valid: true}, SessionName: session, ID: r.ID})
	if err != nil || n != 1 {
		release()
		if err == nil {
			err = fmt.Errorf("run %d is no longer queued", r.ID)
		}
		return err
	}
	token := ""
	if s.GH != nil {
		if token, _, err = s.GH.RepoToken(ctx, projects.GHRepo(repo)); err != nil {
			release()
			return fmt.Errorf("mint GitHub token for %s/%s: %w", repo.Owner, repo.Name, err)
		}
	}
	a := proto.Assignment{
		RunID: r.ID, Kind: "work", TaskSqid: r.TaskSqid, Repo: repo.Owner + "/" + repo.Name,
		CloneURL: repo.CloneUrl, BaseBranch: repo.DefaultBranch, Branch: r.Branch, GitToken: token,
		SessionName: session, SessionUUID: r.SessionUuid, Agent: spec, Env: env,
	}
	b, _ := json.Marshal(a)
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	msg, err := s.Fleet.NC.RequestWithContext(rctx, natsd.RunnerCmdSubject(runner.ID, "assign"), b)
	if err != nil {
		release()
		return fmt.Errorf("assign to %s: %w", runner.Name, err)
	}
	var ack proto.Ack
	if json.Unmarshal(msg.Data, &ack) != nil || !ack.Accepted {
		release()
		return fmt.Errorf("%s declined run %d: %s", runner.Name, r.ID, ack.Reason)
	}
	s.Log.Info("run assigned", "run", r.ID, "task", r.TaskSqid, "runner", runner.Name)
	return nil
}
