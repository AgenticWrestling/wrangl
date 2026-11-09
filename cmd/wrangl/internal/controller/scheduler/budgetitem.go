package scheduler

import (
	"context"
	"fmt"
	"math"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/placer"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/dbtime"
)

// raiseBudgetItem puts a run the budget has held back for UnplaceableAfter in the Inbox. The owner decides:
// *Raise budget to $X/h* is an option, never automatic (ADR 0009 decision 10).
func (s *Scheduler) raiseBudgetItem(ctx context.Context, b placer.Blocked) {
	d := s.Fleet.DB
	run, err := d.RQ.GetRun(ctx, b.Run)
	if err != nil || !run.BlockedSince.Valid {
		return
	}
	since, err := dbtime.Parse(run.BlockedSince.String)
	if err != nil || s.Now().Sub(since) < UnplaceableAfter {
		return
	}
	task, err := d.RQ.GetTask(ctx, run.TaskID)
	if err != nil {
		return
	}
	label := task.Sqid.String
	if b.Code == placer.CodePrice {
		s.NoteUnplaceable(ctx, b.Run, Unplaceable{Code: ReasonPrice, EditNeeds: true,
			Prompt: fmt.Sprintf("Can't place %s: no known price for any machine that could host it", label),
			Detail: fmt.Sprintf("`%s %s` has been waiting since %s. An hourly budget is set, and a budget cannot be enforced against an unknown price, so none of the machines that could host it is launched. Give the template an explicit `price:` for its instance types (`{ type: c7g.xlarge, price: 0.145 }`).", label, task.Title, run.BlockedSince.String),
			Extra:  map[string]any{"budget": map[string]float64{"limit": b.Limit, "committed": b.Committed}}})
		return
	}
	needed := math.Ceil((b.Committed+b.Adds)*100-1e-9) / 100
	s.NoteUnplaceable(ctx, b.Run, Unplaceable{Code: ReasonBudget, EditNeeds: true, RaiseTo: needed,
		Prompt: fmt.Sprintf("Hourly budget reached: $%.2f of $%.2f in use", b.Committed, b.Limit),
		Detail: fmt.Sprintf("`%s %s` has been waiting since %s. Hourly budget reached: $%.2f of $%.2f in use; the cheapest machine that fits (%s, $%.3f/h) needs $%.2f/h in total.", label, task.Title, run.BlockedSince.String, b.Committed, b.Limit, b.Type, b.Adds, needed),
		Extra:  map[string]any{"raise_to": needed, "budget": map[string]float64{"limit": b.Limit, "committed": b.Committed, "needed": needed}}})
}

// resolveBudgetItems cancels open budget / price_unknown items whose run is no longer held back by the budget.
func (s *Scheduler) resolveBudgetItems(ctx context.Context, blocked map[int64]bool) {
	d := s.Fleet.DB
	rows, err := d.R.QueryContext(ctx, `SELECT id, run_id FROM question WHERE kind = 'unplaceable' AND state = 'open' AND json_extract(payload_json, '$.reason') IN ('budget', 'price_unknown')`)
	if err != nil {
		return
	}
	type item struct{ id, run int64 }
	var open []item
	for rows.Next() {
		var it item
		if rows.Scan(&it.id, &it.run) == nil {
			open = append(open, it)
		}
	}
	rows.Close()
	for _, it := range open {
		if !blocked[it.run] {
			if n, err := d.Q.ResolveQuestion(ctx, dbgen.ResolveQuestionParams{Text: "resolved: placeable", ID: it.id}); err == nil && n > 0 {
				s.Fleet.Bus.Question(it.id)
			}
		}
	}
}
