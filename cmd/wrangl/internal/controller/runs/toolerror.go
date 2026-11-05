package runs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/errs"
)

// noteToolError puts a failed tool call in the Inbox right away, so the owner learns that GitHub
// (or another service the controller calls for a runner) is broken while the run is still going,
// not when the task finally fails. Mistakes by the agent (bad arguments, a task that is not there,
// a refused move) are the agent's to handle and are not surfaced. It never blocks the run: the
// agent still gets the error, and the item only asks to be dismissed.
func (s *Service) noteToolError(ctx context.Context, runID int64, tool string, err error) {
	if runID == 0 || errors.Is(err, errs.Invalid) || errors.Is(err, errs.NotFound) || errors.Is(err, errs.Conflict) || errors.Is(err, errs.Forbidden) {
		return
	}
	run, e := s.DB.RQ.GetRun(ctx, runID)
	if e != nil {
		return
	}
	var open int
	s.DB.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM question WHERE kind = 'tool_error' AND state = 'open' AND run_id = ? AND json_extract(payload_json, '$.tool') = ?`, runID, tool).Scan(&open)
	if open > 0 {
		return // one item per run and tool; the first failure is the news
	}
	pl := fmt.Sprintf(`{"tool":%q}`, tool)
	q, e := s.DB.Q.CreateQuestion(ctx, dbgen.CreateQuestionParams{TaskID: run.TaskID, RunID: sql.NullInt64{Int64: runID, Valid: true},
		Kind: "tool_error", Prompt: fmt.Sprintf("Tool call failed: %s", tool),
		DetailMd: fmt.Sprintf("`%s` returned:\n\n> %s\n\nThe agent has the same error and carries on without it. Fix the cause (for example the GitHub App's permissions), then dismiss this.", tool, err.Error()), PayloadJson: pl})
	if e != nil {
		s.Log.Warn("tool error item", "err", e)
		return
	}
	s.DB.Q.CreateQuestionOption(ctx, dbgen.CreateQuestionOptionParams{QuestionID: q.ID, Position: 0, Label: "Dismiss", Value: "dismiss"})
	s.Log.Info("tool call failed", "run", runID, "tool", tool, "err", err.Error())
	s.Bus.Question(q.ID)
}
