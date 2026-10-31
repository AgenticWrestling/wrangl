// Package plans runs "split with agent" (spec §8, D21): a plan run reads the task and the
// repo, proposes subtasks with propose_split, and the owner accepts (applied through the
// same atomic split), edits, or rejects with feedback (another plan run).
package plans

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/nreid/devhost/cmd/wrangl/internal/controller/runs"
	"github.com/nreid/devhost/cmd/wrangl/internal/db"
	"github.com/nreid/devhost/cmd/wrangl/internal/db/dbgen"
	"github.com/nreid/devhost/cmd/wrangl/internal/errs"
	"github.com/nreid/devhost/cmd/wrangl/internal/proto"
	"github.com/nreid/devhost/cmd/wrangl/internal/stack"
)

// Service queues plan runs and applies their proposals.
type Service struct {
	DB    *db.DB
	Stack *stack.Service
	Runs  *runs.Service
	Log   *slog.Logger
}

// Proposal is what propose_split submits (stored in the plan_review question's payload).
type Proposal struct {
	Children  []proto.SplitNode `json:"children"`
	Chain     bool              `json:"chain"`
	Rationale string            `json:"rationale_md,omitempty"`
	TaskSqid  string            `json:"task"`
	Guidance  string            `json:"guidance,omitempty"`
}

func nint(v int64) sql.NullInt64 { return sql.NullInt64{Int64: v, Valid: v != 0} }

func uuid4() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6], b[8] = (b[6]&0x0f)|0x40, (b[8]&0x3f)|0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// Queue starts a plan run for an open task.
func (s *Service) Queue(ctx context.Context, sqid, guidance string) (dbgen.Run, error) {
	t, err := s.Stack.Task(ctx, s.DB.RQ, sqid)
	if err != nil {
		return dbgen.Run{}, err
	}
	if t.State != "open" {
		return dbgen.Run{}, fmt.Errorf("%w: %s is %s; only open tasks can be planned", errs.Conflict, sqid, t.State)
	}
	locked, err := s.DB.RQ.SubtreeLocked(ctx, t.ID)
	if err != nil {
		return dbgen.Run{}, err
	}
	if locked == 1 {
		return dbgen.Run{}, fmt.Errorf("%w: %s is part of a subtree a run is working on", errs.Conflict, sqid)
	}
	proj, err := s.DB.RQ.GetProject(ctx, t.ProjectID)
	if err != nil {
		return dbgen.Run{}, err
	}
	chain, _ := s.DB.RQ.AncestorChain(ctx, t.ID)
	var templateID, repoID int64
	for _, a := range chain {
		if templateID == 0 && a.TemplateID.Valid {
			templateID = a.TemplateID.Int64
		}
		if repoID == 0 && a.RepoID.Valid {
			repoID = a.RepoID.Int64
		}
	}
	if proj.PlanTemplateID.Valid {
		templateID = proj.PlanTemplateID.Int64
	}
	if templateID == 0 {
		templateID = proj.DefaultTemplateID.Int64
	}
	if repoID == 0 {
		rc, _ := s.DB.RQ.ListProjectRepoCount(ctx, proj.ID)
		if rc.N == 1 {
			repoID = rc.OnlyID
		}
	}
	if templateID == 0 || repoID == 0 {
		return dbgen.Run{}, fmt.Errorf("%w: planning needs a runner template and a repo for %s", errs.Invalid, sqid)
	}
	repo, err := s.DB.RQ.GetRepo(ctx, repoID)
	if err != nil {
		return dbgen.Run{}, err
	}
	var siblings []string
	if t.ParentID.Valid {
		kids, _ := s.DB.RQ.ListChildren(ctx, t.ParentID)
		for _, k := range kids {
			if k.ID != t.ID {
				siblings = append(siblings, k.Title)
			}
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Plan task %s %q: break it into subtasks.\n\n", t.Sqid.String, t.Title)
	if p := strings.TrimSpace(t.Prompt); p != "" {
		fmt.Fprintf(&b, "Task description:\n%s\n\n", p)
	}
	for i := len(chain) - 1; i >= 1; i-- {
		if i == len(chain)-1 {
			b.WriteString("It sits under (outermost first):\n")
		}
		fmt.Fprintf(&b, "- %s %s\n", chain[i].Sqid.String, chain[i].Title)
	}
	if len(siblings) > 0 {
		fmt.Fprintf(&b, "\nSibling tasks already planned: %s\n", strings.Join(siblings, "; "))
	}
	if guidance != "" {
		fmt.Fprintf(&b, "\nGuidance from the owner:\n%s\n", guidance)
	}
	b.WriteString("\nThe repository is checked out read-only for context. Do not change files. " +
		"Call the propose_split tool exactly once with concrete, independently reviewable subtasks " +
		"(each with a title and a prompt), nested where useful, in the order they should be done; " +
		"set chain if each depends on the previous. Explain your reasoning in rationale_md.\n")
	n, _ := s.DB.RQ.CountRunsForTask(ctx, t.ID)
	run, err := s.DB.Q.QueueSpecialRun(ctx, dbgen.QueueSpecialRunParams{
		TaskID: t.ID, Kind: "plan", Attempt: n + 1, TemplateID: nint(templateID), RepoID: nint(repoID),
		Branch: repo.DefaultBranch, SessionUuid: uuid4(), PromptOverride: b.String(), ContinueBranch: 1,
	})
	if err != nil {
		return run, err
	}
	if guidance != "" {
		if err := s.DB.Q.SetRunGuidance(ctx, dbgen.SetRunGuidanceParams{Guidance: guidance, ID: run.ID}); err != nil {
			return run, err
		}
	}
	s.Log.Info("plan run queued", "task", sqid, "run", run.ID)
	return run, nil
}

// ProposeSplit implements the propose_split tool for plan runs.
func (s *Service) ProposeSplit(ctx context.Context, sc runs.Scope, raw json.RawMessage) (any, error) {
	var p Proposal
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("%w: %v", errs.Invalid, err)
	}
	if len(p.Children) == 0 {
		return nil, fmt.Errorf("%w: propose at least one subtask", errs.Invalid)
	}
	var check func([]proto.SplitNode) error
	check = func(ns []proto.SplitNode) error {
		for _, n := range ns {
			if strings.TrimSpace(n.Title) == "" {
				return fmt.Errorf("%w: every subtask needs a title", errs.Invalid)
			}
			if err := check(n.Children); err != nil {
				return err
			}
		}
		return nil
	}
	if err := check(p.Children); err != nil {
		return nil, err
	}
	run, err := s.DB.RQ.GetRun(ctx, sc.RunID)
	if err != nil {
		return nil, err
	}
	t, err := s.DB.RQ.GetTask(ctx, run.TaskID)
	if err != nil {
		return nil, err
	}
	p.TaskSqid, p.Guidance = t.Sqid.String, run.Guidance
	payload, _ := json.Marshal(p)
	var d strings.Builder
	if p.Rationale != "" {
		d.WriteString(p.Rationale + "\n\n")
	}
	var list func([]proto.SplitNode, string)
	list = func(ns []proto.SplitNode, indent string) {
		for _, n := range ns {
			fmt.Fprintf(&d, "%s- %s\n", indent, n.Title)
			list(n.Children, indent+"  ")
		}
	}
	list(p.Children, "")
	if p.Chain {
		d.WriteString("\n_Each top-level subtask needs the previous one._\n")
	}
	q, err := s.Runs.NewQuestion(ctx, dbgen.CreateQuestionParams{TaskID: t.ID, RunID: nint(sc.RunID), Kind: "plan_review",
		Prompt: fmt.Sprintf("Accept the proposed split of %s %q?", t.Sqid.String, t.Title), DetailMd: d.String(),
		AllowFreeText: 1, PayloadJson: string(payload)}, []proto.QuestionOption{
		{Label: "Accept", Value: "accept"},
		{Label: "Edit", Value: "edit", Description: "open the split sheet with this proposal"},
		{Label: "Reject", Value: "reject", Description: "give feedback in the text to plan again"},
	})
	if err != nil {
		return nil, err
	}
	// The plan run is done once it proposed: don't hold it on needs_input.
	s.DB.Q.SetRunState(ctx, dbgen.SetRunStateParams{State: "running", ID: sc.RunID, FromState: "needs_input"})
	return map[string]any{"ok": true, "question_id": q.ID, "note": "Proposal sent to the owner. You are done; end your turn."}, nil
}

// Answer handles a plan_review answer.
func (s *Service) Answer(ctx context.Context, q dbgen.Question, value, text string) error {
	var p Proposal
	if err := json.Unmarshal([]byte(q.PayloadJson), &p); err != nil {
		return err
	}
	switch value {
	case "accept":
		_, err := s.Stack.Split(ctx, p.TaskSqid, toStack(p.Children), p.Chain, false, stack.User)
		return err
	case "reject":
		g := p.Guidance
		if strings.TrimSpace(text) != "" {
			g = strings.TrimSpace(g + "\nFeedback on the previous proposal: " + text)
		}
		// The plan run that proposed may still be finishing (its subtree stays locked until
		// it ends), so re-plan once it has.
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			for {
				if _, err := s.Queue(ctx, p.TaskSqid, g); err == nil || !errors.Is(err, errs.Conflict) {
					if err != nil {
						s.Log.Warn("re-plan", "task", p.TaskSqid, "err", err)
					}
					return
				}
				select {
				case <-ctx.Done():
					s.Log.Warn("re-plan timed out waiting for the previous plan run", "task", p.TaskSqid)
					return
				case <-time.After(250 * time.Millisecond):
				}
			}
		}()
		return nil
	}
	return nil // edit: the owner applies their edited version through the split sheet
}

func toStack(in []proto.SplitNode) []stack.SplitNode {
	out := make([]stack.SplitNode, len(in))
	for i, n := range in {
		out[i] = stack.SplitNode{Title: n.Title, Prompt: n.Prompt, Type: n.Type, Children: toStack(n.Children)}
	}
	return out
}
