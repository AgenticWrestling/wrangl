package scheduler

import (
	"context"
	"fmt"
	"sort"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/placer"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/placement"
)

// raiseTooBig turns the plan's too_big runs into permanent Inbox items, at once: a run whose need
// exceeds every machine its template can launch (and every runner it may use) cannot become placeable
// by waiting. Items whose run is no longer too big (its needs were edited, or it is gone) are resolved.
func (s *Scheduler) raiseTooBig(ctx context.Context, plan placer.Plan, runs []placer.Run, runners []placer.RunnerIn, entries []placer.Entry, full map[int64]dbgen.RunnerTemplate) {
	d := s.Fleet.DB
	byID := map[int64]placer.Run{}
	for _, r := range runs {
		byID[r.ID] = r
	}
	still := map[int64]bool{}
	for _, b := range plan.Unplaceable {
		if b.Code != placer.CodeTooBig {
			continue
		}
		r := byID[b.Run]
		if existingRunnerCouldHold(r, runners) {
			continue // an existing runner is big enough: it only has to wait for room
		}
		still[r.ID] = true
		s.noteTooBig(ctx, r, entries, runners, full)
	}
	rows, err := d.R.QueryContext(ctx, `SELECT id, run_id FROM question WHERE kind = 'unplaceable' AND state = 'open' AND json_extract(payload_json, '$.reason') = 'too_big'`)
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
		if !still[it.run] {
			if n, err := d.Q.ResolveQuestion(ctx, dbgen.ResolveQuestionParams{Text: "resolved: placeable", ID: it.id}); err == nil && n > 0 {
				s.Fleet.Bus.Question(it.id)
			}
		}
	}
}

func existingRunnerCouldHold(r placer.Run, runners []placer.RunnerIn) bool {
	ok := map[int64]bool{r.Template: true}
	for _, a := range r.Allowed {
		ok[a] = true
	}
	for _, x := range runners {
		if ok[x.Template] && placer.Fits(r.Need, placer.Runner{Claimable: true, Cap: x.Cap}) {
			return true
		}
	}
	return false
}

func (s *Scheduler) noteTooBig(ctx context.Context, r placer.Run, entries []placer.Entry, runners []placer.RunnerIn, full map[int64]dbgen.RunnerTemplate) {
	d := s.Fleet.DB
	run, err := d.RQ.GetRun(ctx, r.ID)
	if err != nil {
		return
	}
	task, err := d.RQ.GetTask(ctx, run.TaskID)
	if err != nil {
		return
	}
	// The largest usable machine this run could get, per resource, and the templates that could hold it.
	var best placer.Need
	capable := map[int64]bool{}
	for _, e := range entries {
		if e.Template == r.Template {
			best = placer.Need{VCPU: max(best.VCPU, e.Cap.VCPU), RAMMiB: max(best.RAMMiB, e.Cap.RAMMiB), DiskGiB: max(best.DiskGiB, e.Cap.DiskGiB)}
		}
		if placer.Fits(r.Need, placer.Runner{Claimable: true, Cap: e.Cap}) {
			capable[e.Template] = true
		}
	}
	for _, x := range runners {
		if placer.Fits(r.Need, placer.Runner{Claimable: true, Cap: x.Cap}) {
			capable[x.Template] = true
		}
	}
	var others []string
	for id := range capable {
		if t, ok := full[id]; ok && id != r.Template && t.FailedVersion != t.Version {
			others = append(others, t.Name)
		}
	}
	sort.Strings(others)
	what := "its needs are too big"
	switch {
	case r.Need.RAMMiB > best.RAMMiB:
		what = "needs " + placement.Format("ram", r.Need.RAMMiB) + " RAM"
	case r.Need.VCPU > best.VCPU:
		what = fmt.Sprintf("needs %d vCPUs", r.Need.VCPU)
	case r.Need.DiskGiB > best.DiskGiB:
		what = "needs " + placement.Format("disk", r.Need.DiskGiB) + " disk"
	}
	set := fmt.Sprintf("**%s · %s RAM · %s disk**", placement.Format("vcpu", r.Need.VCPU), placement.Format("ram", r.Need.RAMMiB), placement.Format("disk", r.Need.DiskGiB))
	detail := fmt.Sprintf("`%s %s` needs %s. The largest machine its template can launch has %s usable (%s RAM, %s disk), and no runner it may use is big enough. This cannot be placed as asked: lower the task's needs, use another template, or cancel the run.",
		task.Sqid.String, task.Title, set, placement.Format("vcpu", best.VCPU), placement.Format("ram", best.RAMMiB), placement.Format("disk", best.DiskGiB))
	s.NoteUnplaceable(ctx, r.ID, Unplaceable{Code: ReasonTooBig, Permanent: true, EditNeeds: true, Templates: others,
		Prompt: fmt.Sprintf("Can't place %s: %s", task.Sqid.String, what), Detail: detail,
		Extra: map[string]any{"need": map[string]int64{"vcpu": r.Need.VCPU, "ram_mib": r.Need.RAMMiB, "disk_gib": r.Need.DiskGiB},
			"best": map[string]int64{"vcpu": best.VCPU, "ram_mib": best.RAMMiB, "disk_gib": best.DiskGiB}}})
}
