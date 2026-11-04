package scheduler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/agentdef"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/gh"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/natsd"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/projects"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
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
		// Work runs need a leaf ready right now; review/revise/plan runs carry their prompt.
		if r.Kind != "work" {
			if err := s.assignOne(ctx, r); err != nil {
				if !errors.Is(err, sql.ErrNoRows) && !errors.Is(err, errWaitingForApp) {
					errs = append(errs, fmt.Errorf("run %d: %w", r.ID, err))
				}
				continue
			}
			assigned = append(assigned, r.ID)
			continue
		}
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
			if !errors.Is(err, sql.ErrNoRows) && !errors.Is(err, errWaitingForApp) { // no idle runner is normal
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
	// The run's agent: the one it was queued with, else the project's default or the template's only
	// allowed one. Templates without allowed agents still use their own agent block.
	agentJSON, agentName := tmpl.AgentJson, "template "+tmpl.Name
	task, err := d.RQ.GetTask(ctx, r.TaskID)
	if err != nil {
		return err
	}
	runAgent, _ := d.RQ.GetRunAgent(ctx, r.ID)
	chosen, err := agentdef.Resolve(ctx, d.RQ, r.TemplateID.Int64, task.ProjectID, runAgent)
	if err != nil {
		// Not a failure the run can fix by retrying: it waits, visibly, until the owner changes the
		// agent or the template's allow-list.
		d.Q.SetRunStatus(ctx, dbgen.SetRunStatusParams{StatusText: "waiting: " + err.Error(), ID: r.ID})
		return errWaitingForApp
	}
	if chosen != nil {
		agentJSON, agentName = chosen.SpecJson, "agent "+chosen.Name
	}
	spec, err := proto.ParseAgentSpec(agentJSON)
	if err != nil {
		return fmt.Errorf("%s: %w", agentName, err)
	}
	if len(spec.Start) == 0 {
		return fmt.Errorf("%s has no agent.start command", agentName)
	}
	env := map[string]string{}
	for k, name := range spec.Secrets {
		if s.Secret == nil {
			return fmt.Errorf("secret %q needed but no secret store", name)
		}
		v, err := s.Secret(name)
		if err != nil {
			return fmt.Errorf("%s needs secret %q: %w (wrangl controller secret set %s)", agentName, name, err, name)
		}
		env[k] = strings.TrimSpace(string(v))
	}
	spec.Secrets = nil // names stay on the controller
	var files []proto.SecretFile
	for _, f := range spec.SecretFiles {
		if s.Secret == nil {
			return fmt.Errorf("secret %q needed but no secret store", f.Secret)
		}
		v, err := s.Secret(f.Secret)
		if err != nil {
			return fmt.Errorf("%s needs secret %q: %w (wrangl controller secret set %s)", agentName, f.Secret, err, f.Secret)
		}
		files = append(files, proto.SecretFile{Path: f.Path, Content: string(v), Mode: f.Mode})
	}
	spec.SecretFiles = nil

	if free, err := d.RQ.FreeRunnerSlot(ctx, r.TemplateID.Int64); err == nil && free == 0 {
		return sql.ErrNoRows // no runner has a free session: nothing to mint a token for yet
	}
	// Mint the repo token before claiming a runner: a failure here must not touch the runner (a claim
	// and release every tick would keep resetting its idle clock and it would never be reaped).
	token := ""
	if s.GH != nil {
		var err error
		if token, _, err = s.GH.RepoToken(ctx, projects.GHRepo(repo)); err != nil {
			var noApp *gh.NoAppError
			if errors.As(err, &noApp) || errors.Is(err, gh.ErrNotInstalled) {
				// Not a failure: the run waits, visibly, until the owner sets the App up.
				s.waitingForApp(ctx, r.ID, repo.Owner, repo.Name, err)
				return errWaitingForApp
			}
			return fmt.Errorf("mint GitHub token for %s/%s: %w", repo.Owner, repo.Name, err)
		}
	}
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
	a := proto.Assignment{
		RunID: r.ID, Kind: r.Kind, Continue: r.ContinueBranch == 1, TaskSqid: r.TaskSqid, Repo: repo.Owner + "/" + repo.Name,
		CloneURL: repo.CloneUrl, BaseBranch: repo.DefaultBranch, Branch: r.Branch, GitToken: token,
		SessionName: session, SessionUUID: r.SessionUuid, Agent: spec, Env: env, SecretFiles: files,
		MaxSessions: int(tmpl.MaxSessions),
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

// errWaitingForApp marks a run held back because its repository's owner has no usable GitHub App.
var errWaitingForApp = errors.New("waiting for a GitHub App")

// waitingForApp shows on the run why it is waiting and, once per owner per hour, tells the owner
// where to fix it (OnNeedApp: a push notification linking to the Settings flow).
func (s *Scheduler) waitingForApp(ctx context.Context, runID int64, owner, name string, cause error) {
	status := "waiting: " + cause.Error()
	if len(status) > 300 {
		status = status[:300]
	}
	s.Fleet.DB.Q.SetRunStatus(ctx, dbgen.SetRunStatusParams{StatusText: status, ID: runID})
	s.mu.Lock()
	if s.askedApp == nil {
		s.askedApp = map[string]time.Time{}
	}
	key := strings.ToLower(owner)
	due := s.Now().Sub(s.askedApp[key]) > time.Hour
	if due {
		s.askedApp[key] = s.Now()
	}
	s.mu.Unlock()
	if due && s.OnNeedApp != nil {
		s.OnNeedApp(owner, name, cause)
	}
}
