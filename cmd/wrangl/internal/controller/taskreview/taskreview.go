// Package taskreview keeps the Inbox complete for tasks that wait on the owner without a
// pull request to review: a leaf in review whose run pushed nothing (a no-change or
// report-only run), and a task whose last run failed or was blocked. Each gets one open
// task_review question, closed when the task moves on.
package taskreview

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/runs"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/errs"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/events"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/stack"
)

// PRGrace is how long after a pushing run the PR flow has to open its pull request before
// the task is treated as having none.
const PRGrace = 2 * time.Minute

// Service syncs task_review questions with task and run state.
type Service struct {
	DB    *db.DB
	Runs  *runs.Service
	Stack *stack.Service
	Bus   *events.Bus
	Log   *slog.Logger
	Now   func() time.Time

	mu sync.Mutex // one sync at a time
}

type payload struct {
	Sqid   string `json:"sqid"`
	Reason string `json:"reason"` // review | failed | blocked
	RunID  int64  `json:"run_id"`
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// Sync opens questions for tasks that need the owner and cancels those that no longer do.
func (s *Service) Sync(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.closeStale(ctx); err != nil {
		s.Log.Error("task review: close", "err", err)
	}
	if err := s.openReviews(ctx); err != nil {
		s.Log.Error("task review: open reviews", "err", err)
	}
	if err := s.openFailures(ctx); err != nil {
		s.Log.Error("task review: open failures", "err", err)
	}
}

// Loop syncs on task/run changes and every interval until ctx ends.
func (s *Service) Loop(ctx context.Context, interval time.Duration) {
	ch, unsub := s.Bus.Subscribe()
	defer unsub()
	t := time.NewTicker(interval)
	defer t.Stop()
	s.Sync(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.Sync(ctx)
		case ev := <-ch:
			if ev.Kind == "task" || ev.Kind == "run" {
				s.Sync(ctx)
			}
		}
	}
}

func (s *Service) openReviews(ctx context.Context) error {
	cutoff := s.now().Add(-PRGrace).Format("2006-01-02T15:04:05.000Z")
	rows, err := s.DB.R.QueryContext(ctx, `
SELECT t.id, t.sqid, t.title, t.completion_summary, r.id, r.status_text
FROM task t JOIN run r ON r.id = t.completed_run_id
WHERE t.state = 'in_review'
  AND r.finished_at IS NOT NULL
  AND NOT EXISTS (SELECT 1 FROM task c WHERE c.parent_id = t.id)
  AND NOT EXISTS (SELECT 1 FROM question q WHERE q.kind = 'task_review' AND q.state = 'open' AND q.task_id = t.id)
  AND NOT EXISTS (SELECT 1 FROM question q WHERE q.kind = 'deliverable_review' AND q.state = 'open'
                  AND json_extract(q.payload_json, '$.run_id') = r.id)
  AND NOT EXISTS (SELECT 1 FROM pull_request pr WHERE pr.state = 'open' AND r.branch <> ''
                  AND pr.repo_id = r.repo_id AND pr.head_branch = r.branch)
  AND NOT (r.pushed = 1 AND r.finished_at > ?)`, cutoff)
	if err != nil {
		return err
	}
	type cand struct {
		id, runID                  int64
		sqid, title, summary, text string
	}
	var cs []cand
	for rows.Next() {
		var c cand
		var sqid sql.NullString
		if err := rows.Scan(&c.id, &sqid, &c.title, &c.summary, &c.runID, &c.text); err != nil {
			rows.Close()
			return err
		}
		c.sqid = sqid.String
		cs = append(cs, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, c := range cs {
		var detail []string
		if c.summary != "" {
			detail = append(detail, c.summary)
		}
		if c.text != "" && c.text != c.summary {
			detail = append(detail, "_Run status:_ "+c.text)
		}
		detail = append(detail, fmt.Sprintf("[Run #%d](/run/%d) · no pull request to review", c.runID, c.runID))
		pl, _ := json.Marshal(payload{Sqid: c.sqid, Reason: "review", RunID: c.runID})
		if _, err := s.Runs.NewQuestion(ctx, dbgen.CreateQuestionParams{TaskID: c.id, RunID: sql.NullInt64{Int64: c.runID, Valid: true},
			Kind: "task_review", Prompt: fmt.Sprintf("Review %s %q", c.sqid, c.title), DetailMd: strings.Join(detail, "\n\n"),
			AllowFreeText: 1, PayloadJson: string(pl)}, []proto.QuestionOption{
			{Label: "Accept", Value: "accept", Description: "mark the task done"},
			{Label: "Reopen", Value: "reopen", Description: "say what to change in the text; it is appended to the task's prompt"},
		}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) openFailures(ctx context.Context) error {
	rows, err := s.DB.R.QueryContext(ctx, `
SELECT t.id, t.sqid, t.title, r.id, r.state, r.error, r.status_text
FROM run r JOIN task t ON t.id = r.task_id
WHERE r.state IN ('failed', 'blocked') AND r.kind IN ('work', 'revise') AND r.error <> 'runner lost'
  AND r.id = (SELECT MAX(x.id) FROM run x WHERE x.task_id = r.task_id)
  AND t.state IN ('open', 'in_progress')
  AND NOT EXISTS (SELECT 1 FROM pull_request pr WHERE pr.task_id = t.id AND pr.state = 'open')
  AND NOT EXISTS (SELECT 1 FROM question q WHERE q.kind = 'task_review' AND q.state = 'open' AND q.task_id = t.id)`)
	if err != nil {
		return err
	}
	type cand struct {
		id, runID                    int64
		sqid, title, state, err, txt string
	}
	var cs []cand
	for rows.Next() {
		var c cand
		var sqid sql.NullString
		if err := rows.Scan(&c.id, &sqid, &c.title, &c.runID, &c.state, &c.err, &c.txt); err != nil {
			rows.Close()
			return err
		}
		c.sqid = sqid.String
		cs = append(cs, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, c := range cs {
		var detail []string
		if c.err != "" {
			detail = append(detail, "_Error:_ "+c.err)
		}
		if c.txt != "" {
			detail = append(detail, "_Run status:_ "+c.txt)
		}
		detail = append(detail, fmt.Sprintf("[Run #%d](/run/%d)", c.runID, c.runID))
		pl, _ := json.Marshal(payload{Sqid: c.sqid, Reason: c.state, RunID: c.runID})
		if _, err := s.Runs.NewQuestion(ctx, dbgen.CreateQuestionParams{TaskID: c.id, RunID: sql.NullInt64{Int64: c.runID, Valid: true},
			Kind: "task_review", Prompt: fmt.Sprintf("Run %s for %s %q", c.state, c.sqid, c.title), DetailMd: strings.Join(detail, "\n\n"),
			AllowFreeText: 1, PayloadJson: string(pl)}, []proto.QuestionOption{
			{Label: "Retry", Value: "reopen", Description: "say what to change in the text (optional); it is appended to the prompt and the task is queued again"},
			{Label: "Cancel task", Value: "cancel", Description: "give up on the task"},
		}); err != nil {
			return err
		}
	}
	return nil
}

// closeStale cancels open task_review questions whose task has moved on.
func (s *Service) closeStale(ctx context.Context) error {
	rows, err := s.DB.R.QueryContext(ctx, `SELECT id, task_id, payload_json FROM question WHERE kind = 'task_review' AND state = 'open'`)
	if err != nil {
		return err
	}
	type open struct {
		id, task int64
		pl       string
	}
	var qs []open
	for rows.Next() {
		var o open
		if err := rows.Scan(&o.id, &o.task, &o.pl); err != nil {
			rows.Close()
			return err
		}
		qs = append(qs, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, o := range qs {
		var p payload
		json.Unmarshal([]byte(o.pl), &p)
		t, err := s.DB.RQ.GetTask(ctx, o.task)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		live := false
		if err == nil {
			if p.Reason == "review" {
				live = t.State == "in_review" && t.CompletedRunID.Int64 == p.RunID
			} else {
				var latest int64
				s.DB.R.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM run WHERE task_id = ?`, o.task).Scan(&latest)
				var prs int
				s.DB.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM pull_request WHERE task_id = ? AND state = 'open'`, o.task).Scan(&prs)
				live = latest == p.RunID && prs == 0 && (t.State == "open" || t.State == "in_progress")
			}
		}
		if live {
			continue
		}
		res, err := s.DB.W.ExecContext(ctx, `UPDATE question SET state = 'cancelled' WHERE id = ? AND state = 'open'`, o.id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			s.Bus.Publish("question", fmt.Sprint(o.id))
		}
	}
	return nil
}

// Answer handles an answered task_review question.
func (s *Service) Answer(ctx context.Context, q dbgen.Question, value, text string) error {
	var p payload
	json.Unmarshal([]byte(q.PayloadJson), &p)
	t, err := s.DB.RQ.GetTask(ctx, q.TaskID)
	if err != nil {
		return err
	}
	sqid := t.Sqid.String
	text = strings.TrimSpace(text)
	switch value {
	case "accept":
		if t.State != "in_review" {
			return fmt.Errorf("%w: %s is %s", errs.Conflict, sqid, t.State)
		}
		return s.Stack.Complete(ctx, sqid, t.CompletionSummary, stack.User)
	case "cancel":
		return s.Stack.Cancel(ctx, sqid, "owner cancelled after run "+p.Reason, stack.User)
	case "reopen":
		failed := p.Reason != "review"
		if text == "" && !failed {
			return fmt.Errorf("%w: say what to change", errs.Invalid)
		}
		if !failed {
			if err := s.Stack.Reopen(ctx, sqid, stack.User); err != nil {
				return err
			}
		}
		if text != "" {
			prompt := strings.TrimRight(t.Prompt, "\n") + "\n\n---\n**Owner feedback:**\n\n" + text + "\n"
			if _, err := s.Stack.Edit(ctx, sqid, nil, &prompt, stack.User); err != nil {
				return err
			}
		}
		if failed {
			if _, err := s.Stack.Queue(ctx, sqid); err != nil {
				return fmt.Errorf("queue the retry: %w", err)
			}
		}
	}
	return nil
}
