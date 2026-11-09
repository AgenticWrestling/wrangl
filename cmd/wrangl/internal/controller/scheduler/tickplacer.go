package scheduler

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/menu"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/placer"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/dbtime"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/placement"
)

// EnginePlacer selects the resource-based launch planner (Scheduler.Engine).
const EnginePlacer = "placer"

const menuTTL = 10 * time.Minute

type cachedMenu struct {
	version int64
	at      time.Time
	entries []placer.Entry
}

var unbounded = placer.Need{VCPU: 1 << 40, RAMMiB: 1 << 40, DiskGiB: 1 << 40}

// entriesFor is what a template can launch: its resolved menu (capacity from EC2, price), or a single
// entry for the template's own choice with session slots as the only limit.
func (s *Scheduler) entriesFor(ctx context.Context, t dbgen.RunnerTemplate) []placer.Entry {
	if c, ok := s.menus[t.ID]; ok && c.version == t.Version && s.Now().Sub(c.at) < menuTTL {
		return c.entries
	}
	legacy := placer.Entry{Template: t.ID, Cap: unbounded, Ceiling: t.MaxSessions, MinDiskGiB: t.MinDiskGib,
		ReserveDiskGiB: placement.ReserveDiskGiB, DiskUSDPerGiBHour: menu.DiskUSDPerGiBHour}
	out := []placer.Entry{legacy}
	if es := menu.Parse(t.InstanceTypes); len(es) > 0 && s.Menu != nil {
		rs, warns, err := s.Menu.Resolve(ctx, t.Arch, es, placement.ReserveRAMMiB)
		if err != nil {
			s.Log.Warn("instance menu lookup failed; using the template's own choice", "template", t.Name, "err", err)
		} else {
			for _, w := range warns {
				s.Log.Warn("instance menu", "template", t.Name, "warning", w)
			}
			var got []placer.Entry
			for _, r := range rs {
				if r.VCPU < t.MinVcpu || r.InstanceRAM < t.MinRamMib {
					continue // below the template's minimum
				}
				e := legacy
				e.Type = r.Type
				e.Cap = placer.Need{VCPU: r.VCPU, RAMMiB: r.RAMMiB, DiskGiB: menu.DefaultMaxDiskGiB - placement.ReserveDiskGiB}
				if r.PriceSource != menu.Unknown {
					p := r.HourlyUSD
					e.HourlyUSD = &p
				}
				got = append(got, e)
			}
			if len(got) > 0 {
				out = got
			}
		}
	}
	if s.menus == nil {
		s.menus = map[int64]cachedMenu{}
	}
	s.menus[t.ID] = cachedMenu{version: t.Version, at: s.Now(), entries: out}
	return out
}

// tickPlacer is Tick with placer.Place: it plans which runners to launch from the queued runs' needs
// (virtual packing onto existing and arriving runners first), then launches them under the same gates as
// the legacy Tick: max_runners room, a failed template version, backoff, the enrol brake and project caps.
func (s *Scheduler) tickPlacer(ctx context.Context) (map[string]int, error) {
	d := s.Fleet.DB
	if s.backoff == nil {
		s.backoff, s.warned = map[int64]*backoff{}, map[int64]int64{}
	}
	trows, err := d.RQ.TemplateDemand(ctx)
	if err != nil {
		return nil, err
	}
	pdem, err := d.RQ.ProjectTemplateDemand(ctx)
	if err != nil {
		return nil, err
	}
	now := s.Now()
	launched := map[string]int{}
	var errs []error

	remoteIDs, _ := d.RQ.ListRemoteTemplateIDs(ctx)
	remote := map[int64]bool{}
	for _, id := range remoteIDs {
		remote[id] = true
	}
	tmpl := map[int64]dbgen.TemplateDemandRow{}
	full := map[int64]dbgen.RunnerTemplate{}
	lim := placer.Limits{Room: map[int64]int64{}, ProjectRoom: map[[2]int64]int64{}}
	var entries []placer.Entry
	for _, t := range trows {
		tmpl[t.ID] = t
		if remote[t.ID] {
			continue // remote templates launch targets, in tickRemote
		}
		ft, err := d.RQ.GetTemplate(ctx, t.ID)
		if err != nil {
			continue
		}
		full[t.ID] = ft
		lim.Room[t.ID] = max(t.MaxRunners-t.Live, 0)
		if t.Demand > 0 && s.launchBlocked(ctx, t, now) {
			lim.Room[t.ID] = 0
		}
		entries = append(entries, s.entriesFor(ctx, ft)...)
	}
	budget, _ := (&placement.Service{DB: d}).Budget(ctx)
	if budget.LimitUSD != nil {
		// With a budget, it is the guard: per-project limits are not consulted (max_runners stays).
		lim.Budget = &placer.Budget{Limit: *budget.LimitUSD, Committed: budget.Committed}
	}
	for _, p := range pdem {
		if lim.Budget != nil {
			break
		}
		if tt, ok := tmpl[p.TemplateID]; ok {
			lim.ProjectRoom[[2]int64{p.ProjectID, p.TemplateID}] = EffectiveCap(p.Stored, p.IsDefault == 1, tt.MaxRunners) - p.Live
		}
	}

	// The queued runs, each on the template that hosts its agent session (a remote run's driver).
	qrows, err := d.RQ.QueuedRunsForPlacement(ctx)
	if err != nil {
		return nil, err
	}
	allowed := map[int64][]int64{}
	var runs []placer.Run
	for _, q := range qrows {
		if !q.TemplateID.Valid {
			continue
		}
		tid := q.TemplateID.Int64
		if remote[tid] {
			mode, _, driver, err := s.Fleet.Connection(ctx, tid)
			if err != nil || mode != "remote" {
				continue
			}
			capacity := int64(0)
			for _, p := range pdem {
				if p.ProjectID == q.ProjectID && p.TemplateID == tid {
					capacity = EffectiveCap(p.Stored, p.IsDefault == 1, 1<<30)
				}
			}
			if capacity == 0 {
				continue // a project capped at 0 targets does not get idle drivers
			}
			tid = driver
		}
		al, ok := allowed[q.ProjectID]
		if !ok {
			rows, _ := d.RQ.ListAllowedTemplates(ctx, q.ProjectID)
			for _, r := range rows {
				al = append(al, r.ID)
			}
			allowed[q.ProjectID] = al
		}
		var others []int64
		for _, id := range al {
			if id != tid {
				others = append(others, id)
			}
		}
		runs = append(runs, placer.Run{ID: q.ID, Project: q.ProjectID, Template: tid, Allowed: others,
			Need: placer.Need{VCPU: q.NeedVcpu, RAMMiB: q.NeedRamMib, DiskGiB: q.NeedDiskGib}})
	}

	runners, err := s.placerRunners(ctx, tmpl, full)
	if err != nil {
		return nil, err
	}
	plan := placer.Place(runs, runners, entries, lim)

	s.raiseTooBig(ctx, plan, runs, runners, entries, full)
	s.budgetBlocks(ctx, plan, runs, runners)

	failed := map[int64]bool{}
	for _, l := range plan.Launch {
		t := tmpl[l.Entry.Template]
		if failed[t.ID] {
			continue
		}
		r, err := s.Fleet.LaunchSized(ctx, t.Name, l.Entry.Type, l.DiskGiB)
		if err != nil {
			next := MinBackoff
			if b := s.backoff[t.ID]; b != nil {
				next = min(b.next*2, MaxBackoff)
			}
			s.backoff[t.ID] = &backoff{until: now.Add(next), next: next}
			s.Log.Error("launch failed; backing off", "template", t.Name, "retry_in", next, "err", err)
			errs = append(errs, err)
			failed[t.ID] = true
			continue
		}
		if p, ok := l.Entry.Price(l.DiskGiB - l.Entry.ReserveDiskGiB); ok {
			d.Q.SetRunnerPrice(ctx, dbgen.SetRunnerPriceParams{RunnerID: r.ID, HourlyUsd: sql.NullFloat64{Float64: p, Valid: true}})
		}
		d.Q.SetRunnerLaunchedForProject(ctx, dbgen.SetRunnerLaunchedForProjectParams{ProjectID: sql.NullInt64{Int64: l.Project, Valid: true}, ID: r.ID})
		delete(s.backoff, t.ID)
		launched[t.Name]++
	}
	if err := s.tickRemote(ctx, pdem, launched); err != nil {
		errs = append(errs, err)
	}
	return launched, errors.Join(errs...)
}

// launchBlocked is the legacy Tick's gates on launching a template: a failed current version, a
// backoff after a failed launch, and the enrol brake.
func (s *Scheduler) launchBlocked(ctx context.Context, t dbgen.TemplateDemandRow, now time.Time) bool {
	if t.FailedVersion == t.Version {
		if s.warned[t.ID] != t.Version {
			s.Log.Warn("not launching: template's current version failed to apply; fix it with `wrangl template edit`",
				"template", t.Name, "version", t.Version, "queued", t.Demand)
			s.warned[t.ID] = t.Version
		}
		return true
	}
	if b := s.backoff[t.ID]; b != nil && now.Before(b.until) {
		return true
	}
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
		return true
	}
	return false
}

// placerRunners snapshots the runners that count as supply: claimable idle and busy ones, and ones
// still coming up. Runners behind on the template version, cordoned, draining, upgrading or offline are not.
func (s *Scheduler) placerRunners(ctx context.Context, tmpl map[int64]dbgen.TemplateDemandRow, full map[int64]dbgen.RunnerTemplate) ([]placer.RunnerIn, error) {
	d := s.Fleet.DB
	rows, err := d.RQ.ListRunners(ctx, 0)
	if err != nil {
		return nil, err
	}
	pl := &placement.Service{DB: d}
	var out []placer.RunnerIn
	for _, row := range rows {
		r := row.Runner
		t, ok := tmpl[r.TemplateID]
		if !ok || r.Cordoned == 1 || r.DrainRequested == 1 || r.UpgradeSha != "" {
			continue
		}
		ceiling := t.MaxSessions
		switch r.State {
		case "idle", "busy":
			if r.TemplateVersion != t.Version {
				continue
			}
			c, err := pl.RunnerCapacity(ctx, r)
			if err != nil {
				return nil, err
			}
			active, _ := d.RQ.CountActiveRunsForRunner(ctx, sql.NullInt64{Int64: r.ID, Valid: true})
			out = append(out, placer.RunnerIn{Template: r.TemplateID, Runner: placer.Runner{ID: r.ID, Claimable: true, Busy: r.State == "busy",
				IdleSince: r.IdleSince.String, Active: active, Ceiling: &ceiling,
				Cap:  placer.Need{VCPU: c.VCPU, RAMMiB: c.RAMMiB, DiskGiB: c.DiskGiB},
				Used: placer.Need{VCPU: c.UsedVCPU, RAMMiB: c.UsedRAMMiB, DiskGiB: c.UsedDiskGiB}}})
		case "launching", "enrolling", "provisioning":
			capacity := unbounded
			for _, e := range s.entriesFor(ctx, full[r.TemplateID]) {
				if e.Type != "" && e.Type == r.InstanceType {
					capacity = e.Cap
				}
			}
			out = append(out, placer.RunnerIn{Template: r.TemplateID, Arriving: true, Runner: placer.Runner{ID: r.ID, Claimable: true,
				Ceiling: &ceiling, Cap: capacity}})
		}
	}
	return out, nil
}
