package scheduler

import (
	"context"
	"database/sql"
	"errors"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/capability"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/placer"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/placement"
)

// claimRunner picks a runner of the template for the run and claims it. The candidates are the
// claimable runners the run fits (placer.Fits: the run's need snapshot against free capacity, and the
// template's session ceiling), best first (placer.Candidates); each claim is re-checked atomically
// (ClaimRunner), so one lost to a concurrent change moves on to the next. sql.ErrNoRows: none fits.
// With the 0/0/0 default needs this is the old ClaimIdleRunner: busy runners first, then the longest idle.
func (s *Scheduler) claimRunner(ctx context.Context, runID, templateID int64, driver ...bool) (dbgen.ClaimRunnerRow, error) {
	d := s.Fleet.DB
	run, err := d.RQ.GetRun(ctx, runID)
	if err != nil {
		return dbgen.ClaimRunnerRow{}, err
	}
	need := placer.Need{VCPU: run.NeedVcpu, RAMMiB: run.NeedRamMib, DiskGiB: run.NeedDiskGib}
	reqs, _ := placement.Requirements(ctx, d.RQ, placement.OwnerRun, runID)
	if len(driver) > 0 && driver[0] {
		reqs = capability.WithoutAgent(reqs) // a driver runner of another template hosts a remote run's agent
	}
	tags, _ := placement.RunnerTags(ctx, d.RQ)
	rows, err := d.RQ.ClaimableRunners(ctx, templateID)
	if err != nil {
		return dbgen.ClaimRunnerRow{}, err
	}
	pl := &placement.Service{DB: d, Now: s.Now}
	cutoff := pl.NeedsInputCutoff(ctx)
	var cands []placer.Runner
	for _, x := range rows {
		r, err := d.RQ.GetRunner(ctx, x.ID)
		if err != nil {
			continue
		}
		c, err := pl.RunnerCapacity(ctx, r)
		if err != nil {
			continue
		}
		active, _ := d.RQ.CountActiveRunsForRunner(ctx, sql.NullInt64{Int64: x.ID, Valid: true})
		ceiling := x.MaxSessions
		cands = append(cands, placer.Runner{ID: x.ID, Claimable: true, Busy: x.State == "busy", IdleSince: x.IdleSince.String, Tags: tags[x.ID],
			Cap: placer.Need{VCPU: c.VCPU, RAMMiB: c.RAMMiB, DiskGiB: c.DiskGiB}, Used: placer.Need{VCPU: c.UsedVCPU, RAMMiB: c.UsedRAMMiB, DiskGiB: c.UsedDiskGiB},
			Active: active, Ceiling: &ceiling})
	}
	for _, c := range placer.Candidates(need, cands, reqs...) {
		got, err := d.Q.ClaimRunner(ctx, dbgen.ClaimRunnerParams{ID: c.ID, NeedVcpu: need.VCPU, NeedRamMib: need.RAMMiB, NeedDiskGib: need.DiskGiB, NeedsInputCutoff: cutoff})
		if err == nil {
			return got, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return got, err
		}
	}
	return dbgen.ClaimRunnerRow{}, sql.ErrNoRows
}
