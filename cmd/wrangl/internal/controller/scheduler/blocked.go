package scheduler

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/dbtime"
)

// UnplaceableAfter is how long a temporary block must last before the owner is asked (ADR 0009
// decision 4). Permanent ones (the pinned template is gone) are raised at once.
const UnplaceableAfter = 15 * time.Minute

// checkUnplaceable raises the Inbox item for a queued run that Assign could not place, when only the
// owner can fix it. It writes nothing while the run merely waits for a busy or starting runner.
func (s *Scheduler) checkUnplaceable(ctx context.Context, r dbgen.QueuedRunsForAssignmentRow) {
	d := s.Fleet.DB
	task, err := d.RQ.GetTask(ctx, r.TaskID)
	if err != nil {
		return
	}
	label := task.Sqid.String
	others := func(skip int64) []string {
		var out []string
		ts, _ := d.RQ.ListTemplates(ctx)
		for _, t := range ts {
			if t.ID != skip && t.FailedVersion != t.Version {
				out = append(out, t.Name)
			}
		}
		return out
	}
	if !r.TemplateID.Valid {
		s.NoteUnplaceable(ctx, r.ID, Unplaceable{Code: ReasonNoCapability, Permanent: true, Templates: others(0),
			Prompt: fmt.Sprintf("Can't place %s: its runner template is gone", label),
			Detail: fmt.Sprintf("`%s %s` was queued for a runner template that no longer exists, so no runner can take it. Pick another template or cancel the run.", label, task.Title)})
		return
	}
	run, err := d.RQ.GetRun(ctx, r.ID)
	if err != nil || !run.BlockedSince.Valid {
		return
	}
	since, err := dbtime.Parse(run.BlockedSince.String)
	if err != nil || s.Now().Sub(since) < UnplaceableAfter {
		return
	}
	t, err := d.RQ.GetTemplate(ctx, r.TemplateID.Int64)
	if errors.Is(err, sql.ErrNoRows) {
		s.NoteUnplaceable(ctx, r.ID, Unplaceable{Code: ReasonNoCapability, Permanent: true, Templates: others(0),
			Prompt: fmt.Sprintf("Can't place %s: its runner template is gone", label),
			Detail: fmt.Sprintf("`%s %s` was queued for a runner template that no longer exists.", label, task.Title)})
		return
	} else if err != nil {
		return
	}
	c, err := d.RQ.TemplateRunnerCounts(ctx, t.ID)
	if err != nil || c.Usable > 0 || c.Launching > 0 {
		return // something is live or on its way: it will free up
	}
	code, why := "", ""
	failed, _ := d.RQ.CountFailedEnrolments(ctx, dbgen.CountFailedEnrolmentsParams{TemplateID: t.ID, TerminatedAt: dbtime.Format(s.Now().Add(-EnrolFailWindow))})
	live, _ := d.RQ.CountLiveRunners(ctx, t.ID)
	var cap int64 = -1
	if lims, err := d.RQ.ListProjectTemplateLimits(ctx, task.ProjectID); err == nil {
		for _, l := range lims {
			if l.TemplateID == t.ID {
				cap = EffectiveCap(l.Stored, l.IsDefault == 1, l.MaxRunners)
			}
		}
	}
	switch {
	case t.FailedVersion == t.Version:
		code, why = ReasonLaunchBlocked, fmt.Sprintf("version %d of template `%s` failed to apply, so no runner is launched; fix it with `wrangl template edit`", t.Version, t.Name)
	case failed >= MaxEnrolFailures:
		code, why = ReasonLaunchBlocked, fmt.Sprintf("the last %d runners of template `%s` never enrolled, so launching is paused; check the template's image, user-data and network", failed, t.Name)
	case cap == 0:
		code, why = ReasonCapacity, fmt.Sprintf("this project may not launch runners of template `%s` (its limit is 0); raise it with `wrangl project edit --limit %s=N`", t.Name, t.Name)
	case live >= t.MaxRunners:
		code, why = ReasonCapacity, fmt.Sprintf("template `%s` is at its max_runners (%d) with no usable runner", t.Name, t.MaxRunners)
	default:
		return
	}
	s.NoteUnplaceable(ctx, r.ID, Unplaceable{Code: code, EditNeeds: true, Templates: others(t.ID),
		Prompt: fmt.Sprintf("Can't place %s: %s", label, why),
		Detail: fmt.Sprintf("`%s %s` has been waiting since %s: %s.", label, task.Title, run.BlockedSince.String, why)})
}
