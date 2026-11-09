package scheduler

import (
	"context"
	"fmt"

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
