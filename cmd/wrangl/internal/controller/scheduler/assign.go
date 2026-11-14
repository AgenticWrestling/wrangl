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
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/reporesolve"
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
		if !r.RepoID.Valid && r.Kind == "work" {
			// Waiting for a repo: pick it up as soon as the task resolves to one (ADR 0012 section 4).
			if id, err := reporesolve.Resolve(ctx, q.RQ, r.TaskID); err != nil {
				errs = append(errs, fmt.Errorf("run %d: %w", r.ID, err))
			} else if id != 0 {
				if n, err := q.Q.SetRunRepo(ctx, dbgen.SetRunRepoParams{RepoID: id, ID: r.ID}); err == nil && n > 0 {
					r.RepoID = sql.NullInt64{Int64: id, Valid: true}
					s.Fleet.Bus.Run(r.ID)
				}
			}
			if !r.RepoID.Valid {
				s.block(ctx, r.ID, reporesolve.NeedsRepo)
				if err := s.NoteNeedsRepo(ctx, r.ID); err != nil {
					errs = append(errs, fmt.Errorf("run %d: %w", r.ID, err))
				}
			}
		}
		if !r.TemplateID.Valid || !r.RepoID.Valid {
			if !r.TemplateID.Valid {
				s.checkUnplaceable(ctx, r)
			}
			continue
		}
		// Work runs need a leaf ready right now; review/revise/plan runs carry their prompt.
		if r.Kind != "work" {
			if err := s.assignOne(ctx, r); err != nil {
				if errors.Is(err, sql.ErrNoRows) && r.Kind == "resolve" {
					// Resolve runs go first; say why one is still queued.
					q.Q.SetRunStatus(ctx, dbgen.SetRunStatusParams{StatusText: "waiting: no free runner slot", ID: r.ID})
				}
				if !tolerated(err) {
					errs = append(errs, fmt.Errorf("run %d: %w", r.ID, err))
				} else if errors.Is(err, sql.ErrNoRows) {
					s.checkUnplaceable(ctx, r)
				}
				continue
			}
			assigned = append(assigned, r.ID)
			continue
		}
		if _, err := reporesolve.NextReadyLeaf(ctx, q.RQ, r.TaskID, r.ID, r.RepoID.Int64); errors.Is(err, sql.ErrNoRows) {
			open, err := reporesolve.CountOpenLeaves(ctx, q.RQ, r.TaskID, r.RepoID.Int64)
			if err == nil && open == 0 {
				// Everything was finished or cancelled since it was queued.
				q.Q.CancelQueuedRun(ctx, dbgen.CancelQueuedRunParams{Now: dbnull.Str(dbtime.Format(s.Now())), ID: r.ID})
			}
			s.block(ctx, r.ID, "waiting: precursor tasks are not finished yet")
			continue // waiting on outside precursors
		} else if err != nil {
			errs = append(errs, err)
			continue
		}
		if err := s.assignOne(ctx, r); err != nil {
			if !tolerated(err) { // no idle runner is normal
				errs = append(errs, fmt.Errorf("run %d: %w", r.ID, err))
			} else if errors.Is(err, sql.ErrNoRows) {
				s.checkUnplaceable(ctx, r)
			}
			continue
		}
		assigned = append(assigned, r.ID)
	}
	s.resolveUnplaceable(ctx, assigned)
	s.resolveNeedsRepo(ctx)
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
		s.block(ctx, r.ID, "waiting: "+err.Error())
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
			s.block(ctx, r.ID, "waiting: "+serr.Error())
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

// assignOne places a run on a runner. A project that allows other templates (the host, usually)
// tries those first, so a free host is used before cloud runners, then the run's own template.
// With none allowed this is exactly the single attempt on the run's template.
func (s *Scheduler) assignOne(ctx context.Context, r dbgen.QueuedRunsForAssignmentRow) error {
	allowed, err := s.Fleet.DB.RQ.ListAllowedTemplatesForTask(ctx, r.TaskID)
	if err != nil {
		return err
	}
	for _, id := range allowed {
		if id == r.TemplateID.Int64 {
			continue
		}
		alt := r
		alt.TemplateID = sql.NullInt64{Int64: id, Valid: true}
		err := s.assignOn(ctx, alt, false)
		if err == nil {
			return nil
		}
		if !tolerated(err) {
			return err
		}
	}
	return s.assignOn(ctx, r, true)
}

// assignOn tries one template's runners. final says no other template is left to try, so a miss is
// explained on the run.
func (s *Scheduler) assignOn(ctx context.Context, r dbgen.QueuedRunsForAssignmentRow, final bool) error {
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
	if errors.Is(err, sql.ErrNoRows) {
		if final {
			s.block(ctx, r.ID, WaitReasonTarget(tmpl.Name))
		}
		return err
	}
	if err != nil {
		return err
	}
	if free, err := d.RQ.FreeRunnerSlot(ctx, slotTemplate); err == nil && free == 0 {
		if final {
			s.blockNoSlot(ctx, r.ID, slotTemplate)
		}
		return sql.ErrNoRows // no runner has a free session: nothing to mint a token for yet
	}
	// Mint the repo token before claiming a runner: a failure here must not touch the runner (a claim
	// and release every tick would keep resetting its idle clock and it would never be reaped).
	token, err := s.mintToken(ctx, r.ID, repo)
	if err != nil {
		return err
	}
	readRepos := s.readRepos(ctx, r, repo)
	runner, err := s.claimRunner(ctx, r.ID, slotTemplate, slotTemplate != r.TemplateID.Int64)
	if err != nil {
		if final && errors.Is(err, sql.ErrNoRows) {
			s.blockNoSlot(ctx, r.ID, slotTemplate)
		}
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
	d.Q.CancelRunQuestions(ctx, sql.NullInt64{Int64: r.ID, Valid: true})
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
		MaxSessions: int(maxSessions), Target: target, ReadRepos: readRepos,
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
	s.block(ctx, runID, status)
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

// block shows on a queued run why it waits (status text, blocked_reason, blocked_since). It writes
// only when the text changes, so a run that waits for hours costs no writes per tick.
func (s *Scheduler) block(ctx context.Context, runID int64, reason string) {
	if len(reason) > 300 {
		reason = reason[:300]
	}
	d := s.Fleet.DB
	n, err := d.Q.SetRunBlocked(ctx, dbgen.SetRunBlockedParams{Reason: reason, Now: dbnull.Str(dbtime.Format(s.Now())), ID: runID})
	if err == nil && n > 0 {
		s.Fleet.Bus.Run(runID) // live pages show the new reason
	}
}

// blockNoSlot explains a run that found no claimable runner slot on a template.
func (s *Scheduler) blockNoSlot(ctx context.Context, runID, templateID int64) {
	d := s.Fleet.DB
	if run, err := d.RQ.GetRun(ctx, runID); err == nil && strings.HasPrefix(run.BlockedReason, "waiting: hourly budget") {
		return // the launch planner said why: the budget
	}
	t, err := d.RQ.GetTemplate(ctx, templateID)
	if err != nil {
		return
	}
	c, err := d.RQ.TemplateRunnerCounts(ctx, templateID)
	if err != nil {
		return
	}
	s.block(ctx, runID, WaitReason(t.Name, c.Usable, c.Launching, c.Unavailable))
}

// WaitReason is the text on a queued run that found no free runner slot: how many runners of the
// template are full, still starting, or live but not claimable.
func WaitReason(template string, full, starting, unavailable int64) string {
	var parts []string
	if full > 0 {
		parts = append(parts, fmt.Sprintf("%d full", full))
	}
	if starting > 0 {
		parts = append(parts, fmt.Sprintf("%d starting", starting))
	}
	if unavailable > 0 {
		parts = append(parts, fmt.Sprintf("%d cordoned, draining or updating", unavailable))
	}
	if len(parts) == 0 {
		return fmt.Sprintf("waiting: no runner of template %s yet", template)
	}
	return fmt.Sprintf("waiting: no free slot on template %s (%s)", template, strings.Join(parts, ", "))
}

// WaitReasonTarget is the text for a remote-template run with no free target.
func WaitReasonTarget(template string) string {
	return fmt.Sprintf("waiting: no free target for template %s", template)
}
