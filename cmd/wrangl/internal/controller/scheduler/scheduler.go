// Package scheduler decides when to launch runners. M3 does scale-up from queued runs;
// assigning runs to runners arrives with M4.
package scheduler

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/dbtime"
	"log/slog"
	"sync"
	"time"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/fleet"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/gh"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/errs"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

// Backoff after a failed launch, per template: doubles from MinBackoff to MaxBackoff.
const (
	MinBackoff = time.Minute
	MaxBackoff = 30 * time.Minute
)

type backoff struct {
	until time.Time
	next  time.Duration
}

// Scheduler launches runners to meet queued demand, within each template's max_runners.
type Scheduler struct {
	Fleet  *fleet.Service
	Now    func() time.Time
	Log    *slog.Logger
	GH     gh.API                            // nil: no tokens (file:// repos in tests)
	Secret func(name string) ([]byte, error) // controller secret store
	// Tools lists the declarations of the project's enabled custom tools for a run kind (nil: none).
	// Declarations only: no argv, URLs or secrets reach a runner.
	Tools func(ctx context.Context, projectID int64, runKind string) ([]proto.ToolDecl, error)
	// OnNeedApp is called (at most hourly per owner) when queued work cannot start because the
	// repository owner has no GitHub App, or it is not installed on the repository.
	OnNeedApp func(owner, repo string, cause error)

	// OnLaunchStuck is called (at most hourly per template) when launching is paused because
	// recent runners of the template never enrolled.
	OnLaunchStuck func(template string, failed int64)

	mu         sync.Mutex
	askedStuck map[int64]time.Time
	backoff    map[int64]*backoff
	warned     map[int64]int64 // template id -> failed version already logged
	askedApp   map[string]time.Time
}

// MaxEnrolFailures never-enrolled runners within EnrolFailWindow pause launching for a template.
const (
	MaxEnrolFailures = 3
	EnrolFailWindow  = time.Hour
)

// Tick runs one scale-up pass and returns how many runners it launched per template.
func (s *Scheduler) Tick(ctx context.Context) (map[string]int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.backoff == nil {
		s.backoff, s.warned = map[int64]*backoff{}, map[int64]int64{}
	}
	rows, err := s.Fleet.DB.RQ.TemplateDemand(ctx)
	if err != nil {
		return nil, err
	}
	pdem, err := s.Fleet.DB.RQ.ProjectTemplateDemand(ctx)
	if err != nil {
		return nil, err
	}
	now := s.Now()
	launched := map[string]int{}
	var errs []error
	remoteIDs, _ := s.Fleet.DB.RQ.ListRemoteTemplateIDs(ctx)
	remote := map[int64]bool{}
	for _, id := range remoteIDs {
		remote[id] = true
	}
	for _, t := range rows {
		if remote[t.ID] {
			continue // remote templates launch targets, below
		}
		need := t.Demand - t.Supply // sessions still without a slot
		perHost := max(t.MaxSessions, 1)
		hosts := (need + perHost - 1) / perHost // each new runner brings max_sessions slots
		room := t.MaxRunners - t.Live
		n := min(hosts, room)
		if n <= 0 {
			continue
		}
		if t.FailedVersion == t.Version {
			if s.warned[t.ID] != t.Version {
				s.Log.Warn("not launching: template's current version failed to apply; fix it with `wrangl template edit`",
					"template", t.Name, "version", t.Version, "queued", t.Demand)
				s.warned[t.ID] = t.Version
			}
			continue
		}
		b := s.backoff[t.ID]
		if b != nil && now.Before(b.until) {
			continue
		}
		// Launch, wait, reap, launch... forever costs money and tells nobody: after
		// MaxEnrolFailures never-enrolled runners within EnrolFailWindow, stop and ask the owner.
		failed, ferr := s.Fleet.DB.RQ.CountFailedEnrolments(ctx, dbgen.CountFailedEnrolmentsParams{
			TemplateID: t.ID, TerminatedAt: dbtime.Format(now.Add(-EnrolFailWindow))})
		if ferr == nil && failed >= MaxEnrolFailures {
			if s.askedStuck == nil {
				s.askedStuck = map[int64]time.Time{}
			}
			if now.Sub(s.askedStuck[t.ID]) > time.Hour {
				s.askedStuck[t.ID] = now
				s.Log.Error("not launching: runners of this template keep failing to enrol", "template", t.Name, "failed", failed)
				if s.OnLaunchStuck != nil {
					s.OnLaunchStuck(t.Name, failed)
				}
			}
			continue
		}
		failedLaunch := false
		for _, a := range PlanLaunches(t.Demand-t.Supply, perHost, n, append(projectDemands(pdem, t.ID, t.MaxRunners), s.driverDemands(ctx, pdem, t.ID, t.MaxRunners)...)) {
			for i := int64(0); i < a.Hosts && !failedLaunch; i++ {
				r, err := s.Fleet.Launch(ctx, t.Name)
				if err != nil {
					next := MinBackoff
					if b != nil {
						next = min(b.next*2, MaxBackoff)
					}
					s.backoff[t.ID] = &backoff{until: now.Add(next), next: next}
					s.Log.Error("launch failed; backing off", "template", t.Name, "retry_in", next, "err", err)
					errs = append(errs, err)
					failedLaunch = true
					break
				}
				s.Fleet.DB.Q.SetRunnerLaunchedForProject(ctx, dbgen.SetRunnerLaunchedForProjectParams{
					ProjectID: sql.NullInt64{Int64: a.Project, Valid: true}, ID: r.ID})
				delete(s.backoff, t.ID)
				launched[t.Name]++
			}
		}
	}
	if err := s.tickRemote(ctx, pdem, launched); err != nil {
		errs = append(errs, err)
	}
	return launched, errors.Join(errs...)
}

// driverDemands is the demand that remote runs put on a driver template: a run on a remote template
// needs a driver session, but only as many as its project's cap lets it have targets for (a project
// capped at 0 Macs does not get idle drivers). The driver's own ceiling is the template's max_runners.
func (s *Scheduler) driverDemands(ctx context.Context, pdem []dbgen.ProjectTemplateDemandRow, driver, driverMax int64) []ProjectDemand {
	rows, err := s.Fleet.DB.RQ.TargetDemand(ctx)
	if err != nil {
		return nil
	}
	var out []ProjectDemand
	for _, rt := range rows {
		if _, _, d, err := s.Fleet.Connection(ctx, rt.TemplateID); err != nil || d != driver {
			continue
		}
		for _, p := range pdem {
			if p.TemplateID != rt.TemplateID {
				continue
			}
			c := EffectiveCap(p.Stored, p.IsDefault == 1, rt.MaxRunners)
			if d := min(p.Demand, c*rt.MaxSessions); d > 0 {
				out = append(out, ProjectDemand{Project: p.ProjectID, Demand: d, Cap: driverMax})
			}
		}
	}
	return out
}

// tickRemote launches target machines for remote templates whose queued runs have no free target
// session, within each project's cap and the template's max_runners. Their drivers are counted as
// demand on the driver template by TemplateDemand.
func (s *Scheduler) tickRemote(ctx context.Context, pdem []dbgen.ProjectTemplateDemandRow, launched map[string]int) error {
	rows, err := s.Fleet.DB.RQ.TargetDemand(ctx)
	if err != nil {
		return err
	}
	now := s.Now()
	var errs []error
	for _, t := range rows {
		need := t.Demand - t.Supply
		if need <= 0 || t.Live >= t.MaxRunners {
			continue
		}
		if b := s.backoff[t.TemplateID]; b != nil && now.Before(b.until) {
			continue
		}
		var projs []ProjectDemand
		for _, p := range projectDemands(pdem, t.TemplateID, t.MaxRunners) {
			live, _ := s.Fleet.DB.RQ.CountProjectTargets(ctx, p.Project, t.TemplateID)
			p.Live = live
			projs = append(projs, p)
		}
		for _, a := range PlanLaunches(need, t.MaxSessions, t.MaxRunners-t.Live, projs) {
			for i := int64(0); i < a.Hosts; i++ {
				if _, err := s.Fleet.LaunchTarget(ctx, t.Name, a.Project); err != nil {
					next := MinBackoff
					if b := s.backoff[t.TemplateID]; b != nil {
						next = min(b.next*2, MaxBackoff)
					}
					s.backoff[t.TemplateID] = &backoff{until: now.Add(next), next: next}
					s.Log.Error("remote target launch failed; backing off", "template", t.Name, "retry_in", next, "err", err)
					errs = append(errs, err)
					break
				}
				delete(s.backoff, t.TemplateID)
				launched[t.Name]++
			}
		}
	}
	return errors.Join(errs...)
}

// ProjectDemand is one project's queued demand on a template, with its cap.
type ProjectDemand struct {
	Project int64
	Demand  int64 // queued runs wanting the template
	Live    int64 // non-terminated runners launched for this project on the template
	Cap     int64 // effective cap: min(project cap, template max_runners)
}

// Launch is a number of runners to start on behalf of one project.
type Launch struct{ Project, Hosts int64 }

// EffectiveCap is the number of instances a project's demand may cause on a template: the
// project's stored cap (when none is stored: 1 for its default template, else 0), but never
// more than the template's account-wide max_runners.
func EffectiveCap(stored sql.NullInt64, isDefault bool, templateMax int64) int64 {
	c := int64(0)
	if stored.Valid {
		c = stored.Int64
	} else if isDefault {
		c = 1
	}
	return max(min(c, templateMax), 0)
}

func projectDemands(rows []dbgen.ProjectTemplateDemandRow, template, templateMax int64) []ProjectDemand {
	var out []ProjectDemand
	for _, r := range rows {
		if r.TemplateID == template {
			out = append(out, ProjectDemand{Project: r.ProjectID, Demand: r.Demand, Live: r.Live,
				Cap: EffectiveCap(r.Stored, r.IsDefault == 1, templateMax)})
		}
	}
	return out
}

// PlanLaunches splits the sessions still without a slot (need) among the projects in the order
// given (highest priority first). Each project asks for the hosts its share needs, but never takes
// its cap past the instances already launched for it, and the total never passes maxHosts. A
// project at its cap does not push its demand onto the next one: its sessions wait.
func PlanLaunches(need, perHost, maxHosts int64, projs []ProjectDemand) []Launch {
	perHost = max(perHost, 1)
	var out []Launch
	for _, p := range projs {
		if need <= 0 || maxHosts <= 0 {
			break
		}
		share := min(p.Demand, need)
		need -= share
		hosts := min((share+perHost-1)/perHost, p.Cap-p.Live, maxHosts)
		if hosts <= 0 {
			continue
		}
		maxHosts -= hosts
		out = append(out, Launch{Project: p.Project, Hosts: hosts})
	}
	return out
}

// LaunchFor starts count runners of a template on behalf of a project (the controller agent's
// fleet.launch), but only within the project's cap for that template: min(project cap, template
// max_runners) less the runners already launched for the project. Otherwise nothing is started and
// the error states the cap. It returns the new runners' names.
func (s *Scheduler) LaunchFor(ctx context.Context, project, template string, count int) ([]string, error) {
	if count < 1 || count > 5 {
		return nil, fmt.Errorf("%w: count must be 1 to 5", errs.Invalid)
	}
	db := s.Fleet.DB
	p, err := db.RQ.GetProjectBySlug(ctx, project)
	if err != nil {
		return nil, fmt.Errorf("%w: project %q", errs.NotFound, project)
	}
	if p.ArchivedAt.Valid {
		return nil, fmt.Errorf("%w: project %q is archived", errs.Conflict, project)
	}
	t, err := s.Fleet.Template(ctx, template)
	if err != nil {
		return nil, err
	}
	limits, err := db.RQ.ListProjectTemplateLimits(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	var stored sql.NullInt64
	isDefault := false
	for _, l := range limits {
		if l.TemplateID == t.ID {
			stored, isDefault = l.Stored, l.IsDefault == 1
		}
	}
	projectCap := EffectiveCap(stored, isDefault, 1<<30)
	eff := EffectiveCap(stored, isDefault, t.MaxRunners)
	mode, _, _, err := s.Fleet.Connection(ctx, t.ID)
	if err != nil {
		return nil, err
	}
	remote := mode == "remote"
	var live int64
	if remote {
		live, err = db.RQ.CountProjectTargets(ctx, p.ID, t.ID)
	} else {
		live, err = db.RQ.CountProjectRunners(ctx, dbgen.CountProjectRunnersParams{ProjectID: sql.NullInt64{Int64: p.ID, Valid: true}, TemplateID: t.ID})
	}
	if err != nil {
		return nil, err
	}
	if live+int64(count) > eff {
		return nil, fmt.Errorf("%w: project %s may run at most %d instance(s) of template %s (project limit %d, template max_runners %d) and has %d, so %d more is not allowed; raise it with `wrangl project edit %s --limit %s=N`",
			errs.Conflict, project, eff, t.Name, projectCap, t.MaxRunners, live, count, project, t.Name)
	}
	var names []string
	warn := ""
	if remote {
		if w := fleet.CostWarning(t); w != "" {
			warn = " [COST WARNING: " + w + "]"
		}
	}
	for i := 0; i < count; i++ {
		if remote { // a target machine: no enrolling runner
			g, err := s.Fleet.LaunchTarget(ctx, t.Name, p.ID)
			if err != nil {
				return names, err
			}
			names = append(names, g.Name+warn)
			continue
		}
		r, err := s.Fleet.Launch(ctx, t.Name)
		if err != nil {
			return names, err
		}
		db.Q.SetRunnerLaunchedForProject(ctx, dbgen.SetRunnerLaunchedForProjectParams{ProjectID: sql.NullInt64{Int64: p.ID, Valid: true}, ID: r.ID})
		names = append(names, r.Name)
	}
	return names, nil
}

// Loop ticks every interval until ctx ends.
func (s *Scheduler) Loop(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if _, err := s.Tick(ctx); err != nil && ctx.Err() == nil {
			s.Log.Debug("scheduler tick", "err", err)
		}
		if s.Fleet.NC != nil {
			if _, err := s.Assign(ctx); err != nil && ctx.Err() == nil {
				s.Log.Warn("assign", "err", err)
			}
		}
	}
}
