package runs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/nreid/devhost/cmd/wrangl/internal/controller/natsd"
	"github.com/nreid/devhost/cmd/wrangl/internal/db/dbgen"
	"github.com/nreid/devhost/cmd/wrangl/internal/errs"
	"github.com/nreid/devhost/cmd/wrangl/internal/proto"
)

// AnswerHandler reacts to an answered question of a given kind (M6 pr_review, M8
// plan_review). value/text are the answer.
type AnswerHandler func(ctx context.Context, q dbgen.Question, value, text string) error

// NewQuestion creates a question with options.
func (s *Service) NewQuestion(ctx context.Context, q dbgen.CreateQuestionParams, opts []proto.QuestionOption) (dbgen.Question, error) {
	var out dbgen.Question
	if q.PayloadJson == "" {
		q.PayloadJson = "{}"
	}
	err := s.DB.Tx(ctx, func(tx *dbgen.Queries) error {
		var err error
		if out, err = tx.CreateQuestion(ctx, q); err != nil {
			return err
		}
		seen := map[string]bool{}
		for i, o := range opts {
			if o.Label == "" || o.Value == "" || seen[o.Value] {
				return fmt.Errorf("%w: each option needs a unique value and a label", errs.Invalid)
			}
			seen[o.Value] = true
			if _, err := tx.CreateQuestionOption(ctx, dbgen.CreateQuestionOptionParams{QuestionID: out.ID, Position: int64(i), Label: o.Label, Value: o.Value, Description: o.Description}); err != nil {
				return err
			}
		}
		if q.RunID.Valid {
			return tx.SetRunNeedsInput(ctx, q.RunID.Int64)
		}
		return nil
	})
	if err == nil {
		s.Log.Info("question asked", "id", out.ID, "kind", out.Kind, "prompt", out.Prompt)
		s.Bus.Publish("question", fmt.Sprint(out.ID))
		if s.OnQuestion != nil {
			s.OnQuestion(out)
		}
	}
	return out, err
}

// Answer records an answer and routes it: agent and system questions go back to the
// runner that asked; approval questions execute the held operation; other kinds go to
// their registered handler.
func (s *Service) Answer(ctx context.Context, id int64, value, text string) error {
	q, err := s.DB.RQ.GetQuestion(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: question %d", errs.NotFound, id)
	} else if err != nil {
		return err
	}
	if q.State != "open" {
		return fmt.Errorf("%w: question %d is %s", errs.Conflict, id, q.State)
	}
	opts, err := s.DB.RQ.ListQuestionOptions(ctx, id)
	if err != nil {
		return err
	}
	var optID sql.NullInt64
	if value != "" {
		for _, o := range opts {
			if o.Value == value {
				optID = sql.NullInt64{Int64: o.ID, Valid: true}
			}
		}
		if !optID.Valid {
			var vals []string
			for _, o := range opts {
				vals = append(vals, o.Value)
			}
			return fmt.Errorf("%w: %q is not an option (choose one of %s)", errs.Invalid, value, strings.Join(vals, ", "))
		}
	} else if text == "" || q.AllowFreeText == 0 {
		if q.AllowFreeText == 1 {
			return fmt.Errorf("%w: give an option or --text", errs.Invalid)
		}
		return fmt.Errorf("%w: choose one of the options", errs.Invalid)
	}
	n, err := s.DB.Q.AnswerQuestion(ctx, dbgen.AnswerQuestionParams{OptionID: optID, Text: text, Now: nstr(s.ts()), ID: id})
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: question %d was answered meanwhile", errs.Conflict, id)
	}
	if q.RunID.Valid {
		s.DB.Q.SetRunState(ctx, dbgen.SetRunStateParams{State: "running", ID: q.RunID.Int64, FromState: "needs_input"})
	}
	s.Log.Info("question answered", "id", id, "kind", q.Kind, "value", value)
	s.Bus.Publish("question", fmt.Sprint(id))
	switch q.Kind {
	case "agent", "system":
		return s.sendAnswer(ctx, q, proto.Answer{QuestionID: id, Value: value, Text: text})
	case "approval":
		return s.resolveApproval(ctx, q, value)
	}
	if h := s.AnswerHandlers[q.Kind]; h != nil {
		return h(ctx, q, value, text)
	}
	return nil
}

func (s *Service) sendAnswer(ctx context.Context, q dbgen.Question, a proto.Answer) error {
	if !q.RunID.Valid {
		return nil
	}
	run, err := s.DB.RQ.GetRun(ctx, q.RunID.Int64)
	if err != nil || !run.RunnerID.Valid || proto.Terminal(run.State) {
		return nil // the run is over; nothing is waiting
	}
	b, _ := json.Marshal(a)
	if err := s.NC.Publish(natsd.RunnerCmdSubject(run.RunnerID.Int64, "answer"), b); err != nil {
		return fmt.Errorf("deliver answer to runner: %w", err)
	}
	return nil
}

// approvalPayload is what an approval question holds until the owner decides.
type approvalPayload struct {
	Op     string `json:"op"` // task_cancel | task_delete
	Sqid   string `json:"sqid"`
	Reason string `json:"reason,omitempty"`
	RunID  int64  `json:"run_id"`
}

func (s *Service) resolveApproval(ctx context.Context, q dbgen.Question, value string) error {
	if value != "approve" {
		return nil
	}
	var p approvalPayload
	if err := json.Unmarshal([]byte(q.PayloadJson), &p); err != nil {
		return err
	}
	actor := stackActor(p.RunID)
	switch p.Op {
	case "task_cancel":
		return s.Stack.Cancel(ctx, p.Sqid, p.Reason, actor)
	case "task_delete":
		return s.Stack.Delete(ctx, p.Sqid, actor)
	}
	return fmt.Errorf("unknown approval op %q", p.Op)
}

// Questions lists questions (open only unless all).
func (s *Service) Questions(ctx context.Context, all bool) ([]proto.QuestionView, error) {
	open := int64(1)
	if all {
		open = 0
	}
	rows, err := s.DB.RQ.ListQuestions(ctx, open)
	if err != nil {
		return nil, err
	}
	out := []proto.QuestionView{}
	for _, r := range rows {
		v, err := s.view(ctx, r.Question)
		if err != nil {
			return nil, err
		}
		v.Task, v.TaskTitle, v.Project = r.TaskSqid, r.TaskTitle, r.ProjectSlug
		out = append(out, v)
	}
	return out, nil
}

// Question returns one question.
func (s *Service) Question(ctx context.Context, id int64) (proto.QuestionView, error) {
	q, err := s.DB.RQ.GetQuestion(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return proto.QuestionView{}, fmt.Errorf("%w: question %d", errs.NotFound, id)
	} else if err != nil {
		return proto.QuestionView{}, err
	}
	v, err := s.view(ctx, q)
	if t, terr := s.DB.RQ.GetTask(ctx, q.TaskID); terr == nil {
		v.Task, v.TaskTitle = t.Sqid.String, t.Title
		if p, perr := s.DB.RQ.GetProject(ctx, t.ProjectID); perr == nil {
			v.Project = p.Slug
		}
	}
	return v, err
}

func (s *Service) view(ctx context.Context, q dbgen.Question) (proto.QuestionView, error) {
	v := proto.QuestionView{ID: q.ID, Kind: q.Kind, State: q.State, RunID: q.RunID.Int64, Prompt: q.Prompt,
		DetailMD: q.DetailMd, AllowFreeText: q.AllowFreeText == 1, Options: []proto.QuestionOption{}, CreatedAt: q.CreatedAt}
	if q.PayloadJson != "" && q.PayloadJson != "{}" {
		v.Payload = json.RawMessage(q.PayloadJson)
	}
	opts, err := s.DB.RQ.ListQuestionOptions(ctx, q.ID)
	if err != nil {
		return v, err
	}
	for _, o := range opts {
		v.Options = append(v.Options, proto.QuestionOption{Label: o.Label, Value: o.Value, Description: o.Description})
		if q.AnswerOptionID.Valid && q.AnswerOptionID.Int64 == o.ID {
			v.Answer = o.Value
		}
	}
	if v.Answer == "" {
		v.Answer = q.AnswerText
	} else if q.AnswerText != "" {
		v.Answer += ": " + q.AnswerText
	}
	return v, nil
}
