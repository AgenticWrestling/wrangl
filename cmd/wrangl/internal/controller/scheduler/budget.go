package scheduler

import (
	"context"
	"fmt"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/placer"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/placement"
)

// checkBudget notes, once per crossing, that committed hourly spend is approaching the budget (nothing
// is blocked by this; ADR 0009 section 8.3). It re-arms when spend falls below 70% of the budget.
func (s *Scheduler) checkBudget(ctx context.Context) {
	note, b, err := (&placement.Service{DB: s.Fleet.DB}).CheckApproaching(ctx)
	if err != nil || !note {
		return
	}
	title := fmt.Sprintf("Hourly spend $%.2f of $%.2f", b.Committed, *b.LimitUSD)
	s.Log.Warn("approaching the hourly budget", "committed", b.Committed, "budget", *b.LimitUSD)
	s.Fleet.Note(ctx, "runner", "warn", "budget", title, fmt.Sprintf("%.0f%% of the hourly budget is committed by running and starting runners.", b.Pct()))
}

// budgetBlocks says on each run the budget holds back why it waits, and reclaims what it can: when
// the budget is what stops a launch, an idle launched runner with no active run that cannot host any queued
// run is terminated now rather than after idle_timeout, because it is what consumes the budget. Runners that
// could host a queued run are never reaped. The freed spend lets the next tick launch.
func (s *Scheduler) budgetBlocks(ctx context.Context, plan placer.Plan, runs []placer.Run, runners []placer.RunnerIn) {
	d := s.Fleet.DB
	blocked := map[int64]bool{}
	reclaim := false
	for _, b := range plan.Unplaceable {
		switch b.Code {
		case placer.CodeBudget:
			blocked[b.Run] = true
			reclaim = true
			s.block(ctx, b.Run, fmt.Sprintf("waiting: hourly budget $%.2f reached ($%.2f in use; %s would add $%.3f)", b.Limit, b.Committed, b.Type, b.Adds))
		case placer.CodePrice:
			blocked[b.Run] = true
			s.block(ctx, b.Run, "waiting: hourly budget set, but no machine that could host this run has a known price; give the template an explicit `price:`")
		}
	}
	for _, r := range runs {
		if !blocked[r.ID] {
			d.Q.ClearBudgetBlock(ctx, r.ID)
		}
	}
	if !reclaim {
		return
	}
	for _, x := range runners {
		if x.Arriving || x.Busy || x.Active > 0 {
			continue
		}
		could := false
		for _, r := range runs {
			if existingRunnerCouldHold(r, []placer.RunnerIn{x}) {
				could = true
				break
			}
		}
		if could {
			continue
		}
		row, err := d.RQ.GetRunner(ctx, x.ID)
		if err != nil || row.Launched == 0 || row.State != "idle" {
			continue
		}
		if err := s.Fleet.Terminate(ctx, row.Name); err == nil {
			s.Log.Info("reclaimed an idle runner that cannot host any queued run (hourly budget reached)", "runner", row.Name)
		}
	}
}
