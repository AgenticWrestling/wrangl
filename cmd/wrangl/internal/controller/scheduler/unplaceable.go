package scheduler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/placement"
	"strconv"
	"strings"
	"time"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/errs"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

// Reason codes of an unplaceable run (ADR 0009 section 9.1) that stage 1 raises.
const (
	ReasonNoCapability  = "no_capability"  // the pinned template is missing: permanent
	ReasonLaunchBlocked = "launch_blocked" // failed template version, enrol brake, caps full: temporary
	ReasonCapacity      = "capacity"       // every runner full and no launch allowed: temporary
	ReasonBudget        = "budget"         // launching would exceed the hourly budget: temporary
	ReasonPrice         = "price_unknown"  // a budget is set and nothing that could host it has a known price
	ReasonTooBig        = "too_big"        // the need exceeds every machine any template can launch and every runner: permanent
)

// WaitAgain is how long a "keep waiting" answer silences the same reason for a run.
const WaitAgain = 24 * time.Hour

// Unplaceable is an Inbox item about a queued run only the owner can get placed.
type Unplaceable struct {
	Code      string
	Prompt    string   // one line
	Detail    string   // markdown
	Templates []string // other templates that could host it ("use template X" options, at most 3)
	Permanent bool     // no "keep waiting" option
	RaiseTo   float64  // offer "raise the budget to $X/h" (the smallest budget that places the run); 0 = no
	EditNeeds bool     // offer "edit this task's needs" (a link to the task; the item stays open)
	Extra     map[string]any
}

func (u Unplaceable) options() []proto.QuestionOption {
	var o []proto.QuestionOption
	if u.EditNeeds {
		o = append(o, proto.QuestionOption{Label: "Edit this task's needs", Value: "edit_needs", Description: "Open the task to change what it needs; the item resolves itself once the run can be placed."})
	}
	if u.RaiseTo > 0 {
		o = append(o, proto.QuestionOption{Label: fmt.Sprintf("Raise budget to $%.2f/h", u.RaiseTo), Value: "raise_budget", Description: "Sets the hourly budget to this; the next tick launches. Nothing raises it on its own."})
	}
	if !u.Permanent {
		o = append(o, proto.QuestionOption{Label: "Keep waiting", Value: "wait", Description: "The run stays queued with its reason; this is raised again if the reason changes or after 24 hours."})
	}
	for i, t := range u.Templates {
		if i == 3 {
			break
		}
		o = append(o, proto.QuestionOption{Label: "Use template " + t, Value: "use_template:" + t, Description: "Run this task on " + t + " instead."})
	}
	return append(o, proto.QuestionOption{Label: "Cancel the task's run", Value: "cancel_run"})
}

func (u Unplaceable) payload() string {
	m := map[string]any{"reason": u.Code}
	for k, v := range u.Extra {
		m[k] = v
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// NoteUnplaceable puts the run's problem in the Inbox. One open item per run: when it is open and the
// reason changed it is reworded in place; when the owner answered "keep waiting" the same reason is not
// raised again for WaitAgain.
func (s *Scheduler) NoteUnplaceable(ctx context.Context, runID int64, u Unplaceable) error {
	d := s.Fleet.DB
	run, err := d.RQ.GetRun(ctx, runID)
	if err != nil {
		return err
	}
	if run.State != "queued" {
		return nil
	}
	var id int64
	var oldPayload string
	err = d.R.QueryRowContext(ctx, `SELECT id, payload_json FROM question WHERE kind = 'unplaceable' AND state = 'open' AND run_id = ?`, runID).Scan(&id, &oldPayload)
	switch {
	case err == nil:
		var old struct{ Reason string }
		json.Unmarshal([]byte(oldPayload), &old)
		if old.Reason == u.Code {
			return nil
		}
		if err := d.Q.UpdateOpenQuestion(ctx, dbgen.UpdateOpenQuestionParams{Prompt: u.Prompt, DetailMd: u.Detail, PayloadJson: u.payload(), ID: id}); err != nil {
			return err
		}
		d.Q.DeleteQuestionOptions(ctx, id)
		s.addOptions(ctx, id, u)
		s.Fleet.Bus.Question(id)
		return nil
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}
	// Answered "keep waiting" for this reason recently?
	var payload string
	var at sql.NullString
	var value sql.NullString
	err = d.R.QueryRowContext(ctx, `SELECT q.payload_json, q.answered_at, o.value FROM question q LEFT JOIN question_option o ON o.id = q.answer_option_id
		WHERE q.kind = 'unplaceable' AND q.state = 'answered' AND q.run_id = ? ORDER BY q.id DESC LIMIT 1`, runID).Scan(&payload, &at, &value)
	if err == nil && value.String == "wait" {
		var old struct{ Reason string }
		json.Unmarshal([]byte(payload), &old)
		if t, perr := time.Parse("2006-01-02T15:04:05.000Z", at.String); old.Reason == u.Code && perr == nil && s.Now().Sub(t) < WaitAgain {
			return nil
		}
	}
	q, err := d.Q.CreateQuestion(ctx, dbgen.CreateQuestionParams{TaskID: run.TaskID, RunID: sql.NullInt64{Int64: runID, Valid: true},
		Kind: "unplaceable", Prompt: u.Prompt, DetailMd: u.Detail, PayloadJson: u.payload()})
	if err != nil {
		return err
	}
	s.addOptions(ctx, q.ID, u)
	s.Log.Info("run cannot be placed", "run", runID, "reason", u.Code)
	s.Fleet.Bus.Question(q.ID)
	if s.OnUnplaceable != nil {
		s.OnUnplaceable(runID, u.Prompt)
	}
	return nil
}

func (s *Scheduler) addOptions(ctx context.Context, id int64, u Unplaceable) {
	for i, o := range u.options() {
		s.Fleet.DB.Q.CreateQuestionOption(ctx, dbgen.CreateQuestionOptionParams{QuestionID: id, Position: int64(i), Label: o.Label, Value: o.Value, Description: o.Description})
	}
}

// resolveUnplaceable cancels open items whose run is no longer queued or was assigned this pass.
func (s *Scheduler) resolveUnplaceable(ctx context.Context, assigned []int64) {
	d := s.Fleet.DB
	rows, err := d.RQ.ListOpenUnplaceable(ctx)
	if err != nil {
		return
	}
	got := map[int64]bool{}
	for _, id := range assigned {
		got[id] = true
	}
	for _, r := range rows {
		if r.RunState.String == "queued" && !got[r.RunID.Int64] {
			continue
		}
		if n, err := d.Q.ResolveQuestion(ctx, dbgen.ResolveQuestionParams{Text: "resolved: placeable", ID: r.ID}); err == nil && n > 0 {
			s.Fleet.Bus.Question(r.ID)
		}
	}
}

// Answer handles an answered unplaceable item: wait changes nothing, cancel_run cancels the queued
// run, use_template:NAME moves the task and its queued run to that template.
func (s *Scheduler) Answer(ctx context.Context, q dbgen.Question, value, _ string) error {
	switch {
	case value == "wait", value == "edit_needs":
		return nil
	case value == "raise_budget":
		var pl struct {
			RaiseTo float64 `json:"raise_to"`
		}
		json.Unmarshal([]byte(q.PayloadJson), &pl)
		if pl.RaiseTo <= 0 {
			return fmt.Errorf("%w: this item has no budget to raise to", errs.Invalid)
		}
		if err := (&placement.Service{DB: s.Fleet.DB}).SetBudget(ctx, strconv.FormatFloat(pl.RaiseTo, 'f', 2, 64)); err != nil {
			return err
		}
		s.Fleet.Note(ctx, "runner", "info", "budget", "Hourly budget raised", fmt.Sprintf("raised to $%.2f/h from the \"can't be placed\" item", pl.RaiseTo))
		return nil
	case value == "cancel_run":
		if s.CancelRun == nil || !q.RunID.Valid {
			return nil
		}
		return s.CancelRun(ctx, q.RunID.Int64)
	case strings.HasPrefix(value, "use_template:"):
		d := s.Fleet.DB
		name := strings.TrimPrefix(value, "use_template:")
		t, err := d.RQ.GetTemplateByName(ctx, name)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: template %q", errs.NotFound, name)
		} else if err != nil {
			return err
		}
		id := sql.NullInt64{Int64: t.ID, Valid: true}
		if err := d.Q.SetTaskTemplate(ctx, dbgen.SetTaskTemplateParams{TemplateID: id, ID: q.TaskID}); err != nil {
			return err
		}
		if q.RunID.Valid {
			return d.Q.SetQueuedRunTemplate(ctx, dbgen.SetQueuedRunTemplateParams{TemplateID: id, ID: q.RunID.Int64})
		}
	}
	return nil
}
