package scheduler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/dbnull"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/dbtime"
	"strings"
	"time"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/agentdef"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/gh"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/natsd"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/projects"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

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
				if !tolerated(err) {
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
				q.Q.CancelQueuedRun(ctx, dbgen.CancelQueuedRunParams{Now: dbnull.Str(dbtime.Format(s.Now())), ID: r.ID})
			}
			continue // waiting on outside precursors
		} else if err != nil {
			errs = append(errs, err)
			continue
		}
		if err := s.assignOne(ctx, r); err != nil {
			if !tolerated(err) { // no idle runner is normal
				errs = append(errs, fmt.Errorf("run %d: %w", r.ID, err))
			}
			continue
		}
		assigned = append(assigned, r.ID)
	}
	return assigned, errors.Join(errs...)
}

// tolerated: errors that mean "not now" (no idle runner, or the run is held back visibly) and
// are not reported as scheduling failures.
func tolerated(err error) bool {
	return errors.Is(err, sql.ErrNoRows) || errors.Is(err, errHeldBack)
}

// secret reads a secret from the controller's store, with the fix in the error.
func (s *Scheduler) secret(name, needer string) ([]byte, error) {
	if s.Secret == nil {
		return nil, fmt.Errorf("secret %q needed but no secret store", name)
	}
	v, err := s.Secret(name)
	if err != nil {
		return nil, fmt.Errorf("%s needs secret %q: %w (wrangl secret set %s)", needer, name, err, name)
	}
	return v, nil
}

// resolveAgent picks the run's agent spec: the one it was queued with, else the project's default
// or the template's only allowed one. Templates without allowed agents use their own agent block.
func (s *Scheduler) resolveAgent(ctx context.Context, r dbgen.QueuedRunsForAssignmentRow, tmpl dbgen.RunnerTemplate) (proto.AgentSpec, string, error) {
	d := s.Fleet.DB
	agentJSON, agentName := tmpl.AgentJson, "template "+tmpl.Name
	task, err := d.RQ.GetTask(ctx, r.TaskID)
	if err != nil {
		return proto.AgentSpec{}, "", err
	}
	runAgent, _ := d.RQ.GetRunAgent(ctx, r.ID)
	chosen, err := agentdef.Resolve(ctx, d.RQ, r.TemplateID.Int64, task.ProjectID, runAgent)
	if err != nil {
		// Not a failure the run can fix by retrying: it waits, visibly, until the owner changes the
		// agent or the template's allow-list.
		d.Q.SetRunStatus(ctx, dbgen.SetRunStatusParams{StatusText: "waiting: " + err.Error(), ID: r.ID})
		return proto.AgentSpec{}, "", errHeldBack
	}
	if chosen != nil {
		agentJSON, agentName = chosen.SpecJson, "agent "+chosen.Name
	}
	spec, err := proto.ParseAgentSpec(agentJSON)
	if err != nil {
		return spec, "", fmt.Errorf("%s: %w", agentName, err)
	}
	if len(spec.Start) == 0 {
		return spec, "", fmt.Errorf("%s has no agent.start command", agentName)
	}
	return spec, agentName, nil
}

// resolveSecrets turns the spec's secret names into values; the names stay on the controller.
func (s *Scheduler) resolveSecrets(spec *proto.AgentSpec, agentName string) (map[string]string, []proto.SecretFile, error) {
	env := map[string]string{}
	for k, name := range spec.Secrets {
		v, err := s.secret(name, agentName)
		if err != nil {
			return nil, nil, err
		}
		env[k] = strings.TrimSpace(string(v))
	}
	spec.Secrets = nil
	var files []proto.SecretFile
	for _, f := range spec.SecretFiles {
		v, err := s.secret(f.Secret, agentName)
		if err != nil {
			return nil, nil, err
		}
		files = append(files, proto.SecretFile{Path: f.Path, Content: string(v), Mode: f.Mode})
	}
	spec.SecretFiles = nil
	return env, files, nil
}

// claimTarget handles a remote template: a free target of its template (the machine the agent
// drives) is claimed, and the driver template (where the agent runs) supplies the session slot.
// For an ordinary template it returns the template's own slot and a nil target.
func (s *Scheduler) claimTarget(ctx context.Context, r dbgen.QueuedRunsForAssignmentRow, tmpl dbgen.RunnerTemplate) (slotTemplate, maxSessions int64, target *proto.TargetConn, targetRow dbgen.RemoteTarget, err error) {
	d := s.Fleet.DB
	slotTemplate, maxSessions = r.TemplateID.Int64, tmpl.MaxSessions
	mode, conn, driver, err := s.Fleet.Connection(ctx, r.TemplateID.Int64)
	if err != nil || mode != "remote" {
		return
	}
	if targetRow, err = d.RQ.ClaimTarget(ctx, r.TemplateID.Int64); err != nil {
		return // sql.ErrNoRows: no ready target with a free session; one is launched on demand
	}
	dt, err := d.RQ.GetTemplate(ctx, driver)
	if err != nil {
		return
	}
	slotTemplate, maxSessions = driver, dt.MaxSessions
	key := ""
	if conn.Secret != "" {
		if s.Secret == nil {
			err = fmt.Errorf("secret %q needed but no secret store", conn.Secret)
			return
		}
		v, serr := s.secret(conn.Secret, "template "+tmpl.Name)
		if serr != nil {
			d.Q.SetRunStatus(ctx, dbgen.SetRunStatusParams{StatusText: "waiting: " + serr.Error(), ID: r.ID})
			err = errHeldBack
			return
		}
		key = string(v)
	}
	target = &proto.TargetConn{Name: targetRow.Name, Protocol: conn.Protocol, User: conn.User, Port: conn.Port, Via: conn.Via,
		InstanceID: targetRow.InstanceID.String, Host: targetRow.Host, Key: key, Reset: conn.Reset}
	return
}

// mintToken makes the run's repo token (empty without GitHub configured). A repository whose
// owner has no usable App is not a failure: the run waits, visibly, until the App is set up.
func (s *Scheduler) mintToken(ctx context.Context, runID int64, repo dbgen.Repo) (string, error) {
	if s.GH == nil {
		return "", nil
	}
	token, _, err := s.GH.RepoToken(ctx, projects.GHRepo(repo))
	if err != nil {
		var noApp *gh.NoAppError
		if errors.As(err, &noApp) || errors.Is(err, gh.ErrNotInstalled) {
			s.waitingForApp(ctx, runID, repo.Owner, repo.Name, err)
			return "", errHeldBack
		}
		return "", fmt.Errorf("mint GitHub token for %s/%s: %w", repo.Owner, repo.Name, err)
	}
	return token, nil
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
	spec, agentName, err := s.resolveAgent(ctx, r, tmpl)
	if err != nil {
		return err
	}
	env, files, err := s.resolveSecrets(&spec, agentName)
	if err != nil {
		return err
	}
	// A remote run needs two slots at once: a free target and a free session on a driver runner.
	// Neither is claimed until both exist.
	slotTemplate, maxSessions, target, targetRow, err := s.claimTarget(ctx, r, tmpl)
	if err != nil {
		return err
	}
	if free, err := d.RQ.FreeRunnerSlot(ctx, slotTemplate); err == nil && free == 0 {
		return sql.ErrNoRows // no runner has a free session: nothing to mint a token for yet
	}
	// Mint the repo token before claiming a runner: a failure here must not touch the runner (a claim
	// and release every tick would keep resetting its idle clock and it would never be reaped).
	token, err := s.mintToken(ctx, r.ID, repo)
	if err != nil {
		return err
	}
	runner, err := d.Q.ClaimIdleRunner(ctx, slotTemplate)
	if err != nil {
		return err // sql.ErrNoRows: no idle runner
	}
	release := func() {
		d.Q.UnbindRunTarget(context.WithoutCancel(ctx), r.ID)
		d.Q.UnassignRun(context.WithoutCancel(ctx), r.ID)
		d.Q.ReleaseRunner(context.WithoutCancel(ctx), dbgen.ReleaseRunnerParams{Now: dbnull.Str(dbtime.Format(s.Now())), ID: runner.ID})
		s.Fleet.Bus.Run(r.ID) // back to queued
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
	s.Fleet.Bus.Run(r.ID) // queued -> assigned: live pages move it from Queue to In progress
	if target != nil {
		if err := d.Q.BindRunTarget(ctx, r.ID, targetRow.ID); err != nil {
			release()
			return err
		}
	}
	a := proto.Assignment{
		RunID: r.ID, Kind: r.Kind, Continue: r.ContinueBranch == 1, TaskSqid: r.TaskSqid, Repo: repo.Owner + "/" + repo.Name,
		CloneURL: repo.CloneUrl, BaseBranch: repo.DefaultBranch, Branch: r.Branch, GitToken: token,
		SessionName: session, SessionUUID: r.SessionUuid, Agent: spec, Env: env, SecretFiles: files,
		MaxSessions: int(maxSessions), Target: target,
	}
	if s.Tools != nil {
		var decls []proto.ToolDecl
		task, err := d.RQ.GetTask(ctx, r.TaskID)
		if err == nil {
			decls, err = s.Tools(ctx, task.ProjectID, r.Kind)
		}
		if err != nil {
			s.Log.Warn("tool declarations", "run", r.ID, "err", err) // the run still starts, without custom tools
		} else {
			a.Tools = decls
		}
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

// errHeldBack marks a run held back visibly (no usable GitHub App, agent or secret to resolve); the
// scheduler retries it on later ticks.
var errHeldBack = errors.New("run held back")

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
