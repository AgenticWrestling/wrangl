package web

// Template data for the Runners list and a runner page.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/api"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/costs"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/errs"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

// runnerCard is what a runner's card and page show: the runner with its template, latest
// heartbeat, CPU history and active run.
type runnerCard struct {
	R       dbgen.Runner
	T       dbgen.RunnerTemplate
	M       proto.Heartbeat
	Spark   []float64
	Run     *dbgen.Run
	IdleFor string
	Used    int64 // agent sessions in use (active runs); the limit is T.MaxSessions
	Cost    *hostCost
}

// hostCost is the cost block on the controller host's card. It is derived from the cached
// cost snapshot only (no AWS calls on page load): compute is the hourly rate times the time
// the instance has run today / this month (an estimate); attached items are any per-resource
// costs Cost Explorer reported under the instance's own id.
type hostCost struct {
	HourlyUSD, TodayUSD, MonthUSD float64
	Indicative                    bool
	Attached                      []costs.TopCost
}

func hostCostFor(snap costs.Snapshot, instanceID string, now time.Time) *hostCost {
	for _, in := range snap.Instances {
		if in.ID != instanceID || in.State != "running" || !in.PriceKnown {
			continue
		}
		now = now.UTC()
		since := func(start time.Time) float64 {
			if in.LaunchedAt.After(start) {
				start = in.LaunchedAt
			}
			return now.Sub(start).Hours() * in.HourlyUSD
		}
		hc := &hostCost{HourlyUSD: in.HourlyUSD, Indicative: in.Indicative,
			TodayUSD: since(time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)),
			MonthUSD: since(time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC))}
		for _, t := range snap.Top.Items {
			if t.Name == instanceID {
				hc.Attached = append(hc.Attached, t)
			}
		}
		return hc
	}
	return nil
}

func (h *Handler) runnerCard(ctx context.Context, r dbgen.Runner) runnerCard {
	db := h.D.Fleet.DB
	c := runnerCard{R: r}
	c.T, _ = db.RQ.GetTemplate(ctx, r.TemplateID)
	json.Unmarshal([]byte(r.LastMetricsJson), &c.M)
	for _, s := range h.D.Fleet.History(r.ID) {
		c.Spark = append(c.Spark, s.CPU)
	}
	owner := sql.NullInt64{Int64: r.ID, Valid: true}
	if run, err := db.RQ.ActiveRunForRunner(ctx, owner); err == nil {
		c.Run = &run
	}
	c.Used, _ = db.RQ.CountActiveRunsForRunner(ctx, owner)
	if r.Launched == 0 && r.InstanceID.Valid && h.D.Costs != nil {
		c.Cost = hostCostFor(h.D.Costs.Cached(), r.InstanceID.String, time.Now())
	}
	return c
}

func (h *Handler) viewRunners(ctx context.Context, view string, r *http.Request, d map[string]any) error {
	db := h.D.Fleet.DB
	rows, err := db.RQ.ListRunners(ctx, 0)
	if err != nil {
		return err
	}
	var cards []runnerCard
	for _, x := range rows {
		cards = append(cards, h.runnerCard(ctx, x.Runner))
	}
	// Machines the fleet doesn't launch (the controller host) are permanent: pin them first.
	sort.SliceStable(cards, func(i, j int) bool {
		return cards[i].R.Launched == 0 && cards[j].R.Launched != 0
	})
	d["Cards"] = cards
	if h.D.Costs != nil {
		snap := h.D.Costs.Cached()
		known := map[string]bool{}
		for _, c := range cards {
			if c.R.InstanceID.Valid && c.R.State != "terminated" {
				known[c.R.InstanceID.String] = true
			}
		}
		d["Spend"], d["SpendAsOf"], d["SpendErrors"] = snap.Summary, snap.AsOf, snap.Errors
		others := api.OtherInstances(snap, known)
		d["Others"] = others
		var otherHourly float64
		for _, o := range others {
			if o.State == "running" && o.PriceKnown {
				otherHourly += o.HourlyUSD
			}
		}
		d["OtherHourly"] = otherHourly
		d["Top"] = snap.Top
		d["AdoptTemplates"], _ = db.RQ.ListTemplates(ctx)
	}
	demand, _ := db.RQ.TemplateDemand(ctx)
	d["Demand"], d["Title"] = demand, "Runners"
	return nil
}

func (h *Handler) viewRunner(ctx context.Context, view string, r *http.Request, d map[string]any) error {
	db := h.D.Fleet.DB
	r2, err := db.RQ.GetRunnerByName(ctx, reqValue(r, "name"))
	if err != nil {
		return fmt.Errorf("%w: runner", errs.NotFound)
	}
	c := h.runnerCard(ctx, r2)
	d["R"], d["Title"], d["T"], d["M"], d["Spark"] = r2, r2.Name, c.T, c.M, c.Spark
	if c.Run != nil {
		d["Run"] = *c.Run
		d["Progress"], _ = db.RQ.SubtreeOrdered(ctx, c.Run.TaskID)
	}
	d["Recent"], _ = db.RQ.ListRuns(ctx, dbgen.ListRunsParams{Runner: r2.Name, Lim: 10})
	return nil
}
