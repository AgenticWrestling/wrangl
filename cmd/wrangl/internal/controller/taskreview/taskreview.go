// Package taskreview raises a task_review Inbox item for a task an agent completed (state
// in_review) that has no open tracked pull request, for example a summary-only completion.
// The owner accepts it (the task is done) or reopens it with a note for the next run.
package taskreview

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/runs"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/events"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/stack"
)

// Kind is the question kind.
const Kind = "task_review"

// Service owns task_review questions.
type Service struct {
	DB    *db.DB
	Runs  *runs.Service
	Stack *stack.Service
	Log   *slog.Logger
}

// candidates are leaf tasks in review whose completing run has ended, with no open
// tracked PR for that run (or its root task), no open task_review item, and no open
// deliverable_review for the run.
const candidates = `
SELECT t.id, t.sqid, t.title, t.completion_summary, t.completed_run_id
FROM task t JOIN run r ON r.id = t.completed_run_id
WHERE t.state = 'in_review'
  AND r.state IN ('succeeded', 'failed', 'blocked', 'cancelled')
  AND NOT EXISTS (SELECT 1 FROM task k WHERE k.parent_id = t.id AND k.state <> 'cancelled')
  AND NOT EXISTS (SELECT 1 FROM pull_request pr WHERE pr.state = 'open' AND (pr.run_id = r.id OR pr.task_id = r.task_id))
  AND NOT EXISTS (SELECT 1 FROM question q WHERE q.kind = 'task_review' AND q.state = 'open' AND q.task_id = t.id)
  AND NOT EXISTS (SELECT 1 FROM question q WHERE q.kind = 'deliverable_review' AND q.state = 'open'
                  AND json_extract(q.payload_json, '$.run_id') = r.id)
ORDER BY t.id`

// Create raises an item for every task that needs one. It returns how many it made.
func (s *Service) Create(ctx context.Context) (int, error) {
	type cand struct {
		id, run         int64
		sqid, title, sm string
	}
	rows, err := s.DB.R.QueryContext(ctx, candidates)
	if err != nil {
		return 0, err
	}
	var cs []cand
	for rows.Next() {
		var c cand
		var sq sql.NullString
		if err := rows.Scan(&c.id, &sq, &c.title, &c.sm, &c.run); err != nil {
			rows.Close()
			return 0, err
		}
		c.sqid = sq.String
		cs = append(cs, c)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	n := 0
	for _, c := range cs {
		var d strings.Builder
		if strings.TrimSpace(c.sm) != "" {
			fmt.Fprintf(&d, "**Agent's summary**\n\n%s\n\n", c.sm)
		} else {
			d.WriteString("The agent gave no summary.\n\n")
		}
		fmt.Fprintf(&d, "Run: [run #%d](/run/%d). No pull request is tracked for this task.\n", c.run, c.run)
		payload, _ := json.Marshal(map[string]int64{"run_id": c.run})
		if _, err := s.Runs.NewQuestion(ctx, dbgen.CreateQuestionParams{TaskID: c.id, Kind: Kind,
			Prompt: fmt.Sprintf("Review %s %q", c.sqid, c.title), DetailMd: d.String(),
			AllowFreeText: 1, PayloadJson: string(payload)}, []proto.QuestionOption{
			{Label: "Accept", Value: "accept", Description: "mark the task done"},
			{Label: "Reopen", Value: "reopen", Description: "say what is missing in the text; the next run sees it"},
		}); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// Resolve cancels open items whose task is no longer in review (completed, reopened or
// cancelled by another path) or now has an open tracked PR. It returns how many it closed.
func (s *Service) Resolve(ctx context.Context) (int, error) {
	res, err := s.DB.W.ExecContext(ctx, `
UPDATE question SET state = 'cancelled'
WHERE kind = 'task_review' AND state = 'open' AND (
  NOT EXISTS (SELECT 1 FROM task t WHERE t.id = question.task_id AND t.state = 'in_review')
  OR EXISTS (SELECT 1 FROM task t JOIN run r ON r.id = t.completed_run_id
             JOIN pull_request pr ON pr.state = 'open' AND (pr.run_id = r.id OR pr.task_id = r.task_id)
             WHERE t.id = question.task_id))`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		s.Runs.Bus.Publish("question", "")
	}
	return int(n), nil
}

// Sync resolves stale items and raises missing ones.
func (s *Service) Sync(ctx context.Context) {
	if _, err := s.Resolve(ctx); err != nil {
		s.Log.Error("task review resolve", "err", err)
	}
	if _, err := s.Create(ctx); err != nil {
		s.Log.Error("task review create", "err", err)
	}
}

// Watch resolves items as tasks and PRs change, until ctx ends.
func (s *Service) Watch(ctx context.Context, bus *events.Bus) {
	ch, unsub := bus.Subscribe()
	defer unsub()
	for {
		select {
		case <-ctx.Done():
			return
		case e := <-ch:
			if e.Kind == "task" || e.Kind == "pr" {
				if _, err := s.Resolve(ctx); err != nil {
					s.Log.Error("task review resolve", "err", err)
				}
			}
		}
	}
}

// Answer handles an answered task_review question.
func (s *Service) Answer(ctx context.Context, q dbgen.Question, value, text string) error {
	t, err := s.DB.RQ.GetTask(ctx, q.TaskID)
	if err != nil {
		return err
	}
	if t.State != "in_review" {
		return nil // settled another way meanwhile
	}
	sqid := t.Sqid.String
	switch value {
	case "accept":
		return s.Stack.Complete(ctx, sqid, t.CompletionSummary, stack.User)
	case "reopen":
		if err := s.Stack.Reopen(ctx, sqid, stack.User); err != nil {
			return err
		}
		prompt := strings.TrimRight(t.Prompt, "\n") + "\n\n---\n**Owner feedback on the previous attempt:**\n\n" + strings.TrimSpace(text) + "\n"
		_, err := s.Stack.Edit(ctx, sqid, nil, &prompt, stack.User)
		return err
	}
	return nil
}
