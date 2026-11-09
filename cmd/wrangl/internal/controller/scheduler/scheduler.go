// Package scheduler decides when to launch runners. M3 does scale-up from queued runs;
// assigning runs to runners arrives with M4.
package scheduler

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/menu"
	"log/slog"
	"sync"
	"time"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/fleet"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/gh"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/errs"
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
	// OnNeedApp is called (at most hourly per owner) when queued work cannot start because the
	// repository owner has no GitHub App, or it is not installed on the repository.
	OnNeedApp func(owner, repo string, cause error)

	// OnLaunchStuck is called (at most hourly per template) when launching is paused because
	// recent runners of the template never enrolled.
	OnLaunchStuck func(template string, failed int64)

	// Menu resolves instance menus (capacity and price); nil leaves every template with a single
	// legacy entry (the template's own instance choice, session slots only).
	Menu *menu.Lookup

	menus       map[int64]cachedMenu
	lastCapSync time.Time

	// OnUnplaceable is called when a run first lands in the Inbox as unplaceable.
	OnUnplaceable func(runID int64, prompt string)
	// CancelRun cancels a run (the "cancel the task's run" answer).
	CancelRun func(ctx context.Context, runID int64) error

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
	defer s.checkBudget(ctx)
	return s.tick(ctx)
}

func (s *Scheduler) tick(ctx context.Context) (map[string]int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.backoff == nil {
		s.backoff, s.warned = map[int64]*backoff{}, map[int64]int64{}
	}
	return s.tickPlacer(ctx)
}

// tickRemote launches target machines for remote templates whose queued runs have no free target
// session, within the template's max_runners. Their drivers are counted as demand on the driver template by
// the placer. Each launch is attributed to the project whose queued runs asked for it, highest priority first.
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
		perHost := max(t.MaxSessions, 1)
		room := t.MaxRunners - t.Live
		for _, p := range pdem {
			if p.TemplateID != t.TemplateID || need <= 0 || room <= 0 {
				continue
			}
			share := min(p.Demand, need)
			need -= share
			hosts := min((share+perHost-1)/perHost, room)
			room -= hosts
			for i := int64(0); i < hosts; i++ {
				if _, err := s.Fleet.LaunchTarget(ctx, t.Name, p.ProjectID); err != nil {
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

// LaunchFor starts count runners of a template on behalf of a project (the controller agent's
// fleet.launch), but only within the template's max_runners: if the template already has that many
// instances (or would with these) nothing is started and the error says so. It returns the new runners'
// names.
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
	mode, _, _, err := s.Fleet.Connection(ctx, t.ID)
	if err != nil {
		return nil, err
	}
	remote := mode == "remote"
	var live int64
	if remote {
		targets, derr := db.RQ.TargetDemand(ctx)
		if derr != nil {
			return nil, derr
		}
		for _, x := range targets {
			if x.TemplateID == t.ID {
				live = x.Live
			}
		}
	} else if live, err = db.RQ.CountLiveRunners(ctx, t.ID); err != nil {
		return nil, err
	}
	if live+int64(count) > t.MaxRunners {
		return nil, fmt.Errorf("%w: template %s may have at most %d instance(s) (max_runners) and has %d, so %d more is not allowed; raise max_runners with `wrangl template edit`",
			errs.Conflict, t.Name, t.MaxRunners, live, count)
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
	s.syncCapabilitiesEvery(ctx)
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
