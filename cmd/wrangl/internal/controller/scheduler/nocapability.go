package scheduler

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/capability"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/placer"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
)

// capabilitySyncEvery is how often the scheduler re-derives template and runner tags (agent allow-lists
// change without touching the template).
const capabilitySyncEvery = 5 * time.Minute

func (s *Scheduler) syncCapabilitiesEvery(ctx context.Context) {
	if s.lastCapSync.IsZero() || s.Now().Sub(s.lastCapSync) >= capabilitySyncEvery {
		s.lastCapSync = s.Now()
		s.Fleet.SyncCapabilities(ctx)
	}
}

// raiseNoCapability puts a run whose requirements no template can provide in the Inbox at once: waiting
// cannot change a template's tags, only the owner can (change the requirement, or the template).
func (s *Scheduler) raiseNoCapability(ctx context.Context, plan placer.Plan, runs []placer.Run, entries []placer.Entry, full map[int64]dbgen.RunnerTemplate) {
	d := s.Fleet.DB
	byID := map[int64]placer.Run{}
	for _, r := range runs {
		byID[r.ID] = r
	}
	still := map[int64]bool{}
	defer func() { // items whose requirement was changed or is now provided resolve themselves
		rows, err := d.R.QueryContext(ctx, `SELECT id, run_id FROM question WHERE kind = 'unplaceable' AND state = 'open'
			AND json_extract(payload_json, '$.reason') = 'no_capability' AND json_extract(payload_json, '$.requires') IS NOT NULL`)
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
	}()
	for _, b := range plan.Unplaceable {
		if b.Code != placer.CodeNoCapability {
			continue
		}
		still[b.Run] = true
		r := byID[b.Run]
		run, err := d.RQ.GetRun(ctx, r.ID)
		if err != nil {
			continue
		}
		task, err := d.RQ.GetTask(ctx, run.TaskID)
		if err != nil {
			continue
		}
		var want []string
		for _, q := range r.Reqs {
			want = append(want, q.String())
		}
		sort.Strings(want)
		// Templates that would provide everything asked, as use_template options.
		var others []string
		for _, e := range entries {
			if e.Template != r.Template && capability.Match(r.Reqs, e.Tags) && placer.Fits(r.Need, placer.Runner{Claimable: true, Cap: e.Cap}) {
				if t, ok := full[e.Template]; ok && t.FailedVersion != t.Version {
					others = append(others, t.Name)
				}
			}
		}
		sort.Strings(others)
		others = dedupe(others)
		s.NoteUnplaceable(ctx, r.ID, Unplaceable{Code: ReasonNoCapability, Permanent: true, Templates: others,
			Prompt: fmt.Sprintf("Can't place %s: no runner template provides %s", task.Sqid.String, strings.Join(want, ", ")),
			Detail: fmt.Sprintf("`%s %s` requires **%s**, and no template it may launch on provides that. Change the requirement (`wrangl task edit --require/--unrequire`, or on the project), add the capability to the template (`provides:` in its file), or run it on another template.", task.Sqid.String, task.Title, strings.Join(want, ", ")),
			Extra:  map[string]any{"requires": want}})
	}
}

func dedupe(s []string) []string {
	var out []string
	for i, v := range s {
		if i == 0 || v != s[i-1] {
			out = append(out, v)
		}
	}
	return out
}
