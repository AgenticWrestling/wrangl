// Package runs ingests run events from runners, answers their work-stack requests, and
// cancels and inspects runs.
package runs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/dbnull"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/dbtime"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/natsd"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/errs"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/events"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/stack"
)

// LostAfter: an active run fails once its runner has been offline this long (spec §13).
const LostAfter = 10 * time.Minute

// StuckAfter: a running run that has logged nothing, fired no hook and not started anything for
// this long is reported to the owner.
const StuckAfter = 30 * time.Minute

// Service handles runs on the controller side.
type Service struct {
	DB    *db.DB
	Stack *stack.Service
	NC    *nats.Conn
	Now   func() time.Time
	Log   *slog.Logger

	Bus *events.Bus // change notices
	// OnQuestion is called after a question is created (web push in M8).
	OnQuestion func(q dbgen.Question)
	// AnswerHandlers handle answers for kinds other than agent/system/approval.
	AnswerHandlers map[string]AnswerHandler
	// ToolHandlers implement submit_review (M6) and propose_split (M8).
	ToolHandlers map[string]ToolHandler
	// ApprovalHandlers run the held operation of an approval question whose op is not one the
	// runs service knows (issue_close). They get the question's payload JSON.
	ApprovalHandlers map[string]func(ctx context.Context, payload json.RawMessage) error
	// OnRunFinished is called after a run reaches a terminal state (PRs in M6).
	OnRunFinished func(ctx context.Context, runID int64)
	// OnAgentNotice is called when an agent's Notification hook fires (Claude Code: it needs
	// permission, or has been idle waiting): the owner should look. message is the agent's text.
	OnAgentNotice func(run dbgen.Run, message string)
}

func (s *Service) ts() string { return dbtime.Format(s.Now()) }

// parseEvt splits wr.evt.<runner>.<run>.<kind>.
func parseEvt(subject, kind string) (runner, run int64, ok bool) {
	p := strings.Split(subject, ".")
	if len(p) != 5 || p[0] != "wr" || p[1] != "evt" || p[4] != kind {
		return 0, 0, false
	}
	a, err1 := strconv.ParseInt(p[2], 10, 64)
	b, err2 := strconv.ParseInt(p[3], 10, 64)
	return a, b, err1 == nil && err2 == nil && a > 0 && b > 0
}

// owned loads the run and checks it belongs to the runner the subject names. The subject
// is bound to the sender by NATS permissions, so this stops a runner touching another
// runner's runs.
func (s *Service) owned(ctx context.Context, runnerID, runID int64) (dbgen.Run, error) {
	r, err := s.DB.RQ.GetRun(ctx, runID)
	if errors.Is(err, sql.ErrNoRows) {
		return r, fmt.Errorf("%w: run %d", errs.NotFound, runID)
	} else if err != nil {
		return r, err
	}
	if !r.RunnerID.Valid || r.RunnerID.Int64 != runnerID {
		return r, fmt.Errorf("%w: run %d does not belong to runner %d", errs.Forbidden, runID, runnerID)
	}
	return r, nil
}

// HandleState records a run state event.
func (s *Service) HandleState(ctx context.Context, subject string, data []byte) error {
	runnerID, runID, ok := parseEvt(subject, "state")
	if !ok {
		return fmt.Errorf("%w: subject %q", errs.Invalid, subject)
	}
	var st proto.RunState
	if err := json.Unmarshal(data, &st); err != nil {
		return fmt.Errorf("%w: %v", errs.Invalid, err)
	}
	run, err := s.owned(ctx, runnerID, runID)
	if err != nil {
		return err
	}
	defer s.Bus.Run(runID)
	if !proto.Terminal(st.State) {
		started := sql.NullString{}
		if st.State == "preparing" || st.State == "running" {
			started = dbnull.Str(s.ts())
		}
		return s.DB.Tx(ctx, func(q *dbgen.Queries) error {
			if err := q.UpdateRunProgress(ctx, dbgen.UpdateRunProgressParams{
				State: st.State, StartedAt: started, BaseSha: st.BaseSHA, HeadSha: st.HeadSHA, StatusText: st.Status, ID: runID,
			}); err != nil {
				return err
			}
			return q.ReassertNeedsInput(ctx, runID)
		})
	}
	var code sql.NullInt64
	if st.ExitCode != nil {
		code = sql.NullInt64{Int64: int64(*st.ExitCode), Valid: true}
	}
	pushed := int64(0)
	if st.Pushed {
		pushed = 1
	}
	if err := s.DB.Tx(ctx, func(q *dbgen.Queries) error {
		if err := q.FinishRun(ctx, dbgen.FinishRunParams{
			State: st.State, Now: dbnull.Str(s.ts()), ExitCode: code, Error: st.Error, StatusText: st.Status,
			Pushed: pushed, HeadSha: st.HeadSHA, ID: runID,
		}); err != nil {
			return err
		}
		if run.CurrentTaskID.Valid {
			if err := q.RevertInProgress(ctx, run.CurrentTaskID.Int64); err != nil {
				return err
			}
		}
		return q.ReleaseRunner(ctx, dbgen.ReleaseRunnerParams{Now: dbnull.Str(s.ts()), ID: runnerID})
	}); err != nil {
		return err
	}
	if run.CurrentTaskID.Valid {
		s.recomputeTask(ctx, run.CurrentTaskID.Int64)
	}
	s.DB.Q.CancelRunQuestions(ctx, sql.NullInt64{Int64: runID, Valid: true})
	s.Log.Info("run finished", "run", runID, "state", st.State, "pushed", st.Pushed, "error", st.Error)
	if s.OnRunFinished != nil {
		s.OnRunFinished(ctx, runID)
	}
	return nil
}

// recomputeTask refreshes container states above a leaf that was reverted to open.
func (s *Service) recomputeTask(ctx context.Context, taskID int64) {
	t, err := s.DB.RQ.GetTask(ctx, taskID)
	if err == nil && t.Sqid.Valid {
		_ = s.Stack.Recompute(ctx, t.Sqid.String)
	}
}

// HandleHook stores one agent hook event.
func (s *Service) HandleHook(ctx context.Context, subject string, data []byte) error {
	runnerID, runID, ok := parseEvt(subject, "hook")
	if !ok {
		return fmt.Errorf("%w: subject %q", errs.Invalid, subject)
	}
	var h struct {
		Seq     int64           `json:"seq"`
		TS      time.Time       `json:"ts"`
		Event   string          `json:"event"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(data, &h); err != nil || h.Event == "" {
		return fmt.Errorf("%w: hook event", errs.Invalid)
	}
	run, err := s.owned(ctx, runnerID, runID)
	if err != nil {
		return err
	}
	payload := string(h.Payload)
	if payload == "" || !json.Valid(h.Payload) {
		payload = "{}"
	}
	if len(payload) > 64<<10 {
		payload = `{"truncated":true}`
	}
	if err := s.DB.Q.InsertHookEvent(ctx, dbgen.InsertHookEventParams{RunID: runID, Seq: h.Seq,
		Ts: dbtime.Format(h.TS), Name: h.Event, PayloadJson: payload}); err != nil {
		return err
	}
	if h.Event == "Notification" {
		var n struct {
			Message string `json:"message"`
		}
		json.Unmarshal(h.Payload, &n)
		s.Bus.Run(runID)
		if s.OnAgentNotice != nil {
			s.OnAgentNotice(run, n.Message)
		}
	}
	return nil
}

// HandleLog stores one log line.
func (s *Service) HandleLog(ctx context.Context, subject string, data []byte) error {
	runnerID, runID, ok := parseEvt(subject, "log")
	if !ok {
		return fmt.Errorf("%w: subject %q", errs.Invalid, subject)
	}
	var l proto.LogLine
	if err := json.Unmarshal(data, &l); err != nil {
		return fmt.Errorf("%w: %v", errs.Invalid, err)
	}
	if _, err := s.owned(ctx, runnerID, runID); err != nil {
		return err
	}
	if l.Stream != "agent" && l.Stream != "runner" {
		l.Stream = "runner"
	}
	return s.DB.Q.InsertLogLine(ctx, dbgen.InsertLogLineParams{
		RunID: runID, Seq: l.Seq, Ts: dbtime.Format(l.TS), Stream: l.Stream, Level: "info", Message: l.Message,
	})
}

// HandleStack answers wr.stack.<runner>.<op> requests from a runner's daemon.
func (s *Service) HandleStack(ctx context.Context, m *nats.Msg) {
	reply := func(r proto.StackReply) {
		b, _ := json.Marshal(r)
		m.Respond(b)
	}
	fail := func(err error) { reply(proto.StackReply{Error: err.Error()}) }
	p := strings.Split(m.Subject, ".")
	if len(p) != 4 {
		fail(fmt.Errorf("bad subject"))
		return
	}
	runnerID, err := strconv.ParseInt(p[2], 10, 64)
	if err != nil {
		fail(err)
		return
	}
	var req proto.StackRequest
	if err := json.Unmarshal(m.Data, &req); err != nil {
		fail(err)
		return
	}
	if _, err := s.owned(ctx, runnerID, req.RunID); err != nil {
		fail(err)
		return
	}
	switch p[3] {
	case "next":
		n, err := s.Stack.NextForRun(ctx, req.RunID)
		if err != nil {
			fail(err)
			return
		}
		reply(proto.StackReply{OK: true, Next: &proto.StackNext{Done: n.Done, Blocked: n.Blocked, Sqid: n.Sqid, Title: n.Title, Prompt: n.Prompt}})
	case "complete":
		if err := s.Stack.AgentComplete(ctx, req.RunID, req.Sqid, req.Summary); err != nil {
			fail(err)
			return
		}
		reply(proto.StackReply{OK: true})
	case "checkpoint":
		if err := s.Stack.RecordCheckpoint(ctx, req.RunID, req.Sqid, req.Commit); err != nil {
			fail(err)
			return
		}
		reply(proto.StackReply{OK: true})
	case "tool":
		var tr proto.ToolRequest
		json.Unmarshal(m.Data, &tr)
		sc, err := s.ScopeForRun(ctx, req.RunID)
		if err != nil {
			fail(err)
			return
		}
		out, err := s.ExecTool(ctx, sc, tr.Tool, tr.Args)
		if err != nil {
			s.noteToolError(ctx, req.RunID, tr.Tool, err)
			b, _ := json.Marshal(proto.ToolReply{Error: err.Error()})
			m.Respond(b)
			return
		}
		res, _ := json.Marshal(out)
		b, _ := json.Marshal(proto.ToolReply{OK: true, Result: res})
		m.Respond(b)
	default:
		fail(fmt.Errorf("unknown stack op %q", p[3]))
	}
}

// Dequeue takes a task's queued run(s) back out of the queue (the task stays open, in the backlog).
// It only touches runs that have not been assigned; a running run is stopped with Cancel instead.
func (s *Service) Dequeue(ctx context.Context, sqid string) (int64, error) {
	t, err := s.Stack.Task(ctx, s.DB.RQ, sqid)
	if err != nil {
		return 0, err
	}
	rs, err := s.DB.RQ.RunsForTask(ctx, t.ID)
	if err != nil {
		return 0, err
	}
	var n int64
	for _, r := range rs {
		if r.State != "queued" {
			continue
		}
		c, err := s.DB.Q.CancelQueuedRun(ctx, dbgen.CancelQueuedRunParams{Now: dbnull.Str(s.ts()), ID: r.ID})
		if err != nil {
			return n, err
		}
		n += c
		s.Bus.Publish("run", strconv.FormatInt(r.ID, 10))
	}
	if n == 0 {
		return 0, fmt.Errorf("%w: %s has no queued run to remove (already started, or never queued)", errs.Conflict, sqid)
	}
	return n, nil
}

// Cancel cancels a run: a queued run directly, an active one via its runner.
func (s *Service) Cancel(ctx context.Context, runID int64) error {
	if run, err := s.DB.RQ.GetRun(ctx, runID); err == nil {
		if t, err := s.DB.RQ.GetTask(ctx, run.TaskID); err == nil {
			if err := stack.ActiveProject(ctx, s.DB.RQ, t.ProjectID); err != nil {
				return err
			}
		}
	}
	n, err := s.DB.Q.CancelQueuedRun(ctx, dbgen.CancelQueuedRunParams{Now: dbnull.Str(s.ts()), ID: runID})
	if err != nil {
		return err
	}
	if n == 1 {
		s.DB.Q.CancelRunQuestions(ctx, sql.NullInt64{Int64: runID, Valid: true})
		return nil
	}
	run, err := s.DB.RQ.GetRun(ctx, runID)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: run %d", errs.NotFound, runID)
	} else if err != nil {
		return err
	}
	if proto.Terminal(run.State) {
		return fmt.Errorf("%w: run %d is already %s", errs.Conflict, runID, run.State)
	}
	if _, err := s.DB.Q.MarkRunCancelling(ctx, runID); err != nil {
		return err
	}
	b, _ := json.Marshal(proto.RunRef{RunID: runID})
	msg, err := s.NC.RequestWithContext(ctx, natsd.RunnerCmdSubject(run.RunnerID.Int64, "cancel"), b)
	if err != nil {
		return fmt.Errorf("runner did not answer the cancel (it will be failed if it stays offline): %w", err)
	}
	var ack proto.Ack
	if json.Unmarshal(msg.Data, &ack) != nil || !ack.Accepted {
		return fmt.Errorf("runner declined cancel: %s", ack.Reason)
	}
	return nil
}

// Inspect asks the run's runner for its diff, a directory listing or a file.
func (s *Service) Inspect(ctx context.Context, runID int64, kind, path string, stat bool) (proto.InspectReply, error) {
	run, err := s.DB.RQ.GetRun(ctx, runID)
	if errors.Is(err, sql.ErrNoRows) {
		return proto.InspectReply{}, fmt.Errorf("%w: run %d", errs.NotFound, runID)
	} else if err != nil {
		return proto.InspectReply{}, err
	}
	if !run.RunnerID.Valid {
		return proto.InspectReply{}, fmt.Errorf("%w: run %d has not been assigned to a runner", errs.Conflict, runID)
	}
	b, _ := json.Marshal(proto.RunRef{RunID: runID, Path: path, Stat: stat})
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	msg, err := s.NC.RequestWithContext(ctx, natsd.RunnerCmdSubject(run.RunnerID.Int64, kind), b)
	if err != nil {
		return proto.InspectReply{}, fmt.Errorf("%w: runner unavailable (the workspace is only on the runner; M6 adds the PR fallback): %v", errs.Conflict, err)
	}
	var out proto.InspectReply
	if err := json.Unmarshal(msg.Data, &out); err != nil {
		return out, err
	}
	if out.Error != "" {
		return out, fmt.Errorf("%w: %s", errs.Invalid, out.Error)
	}
	return out, nil
}

// SweepLost fails active runs whose runner is gone.
func (s *Service) SweepLost(ctx context.Context) error {
	cutoff := dbtime.Format(s.Now().Add(-LostAfter))
	lost, err := s.DB.RQ.RunsOnLostRunners(ctx, dbnull.Str(cutoff))
	if err != nil {
		return err
	}
	for _, r := range lost {
		err := s.DB.Tx(ctx, func(q *dbgen.Queries) error {
			if err := q.FinishRun(ctx, dbgen.FinishRunParams{State: "failed", Now: dbnull.Str(s.ts()), Error: "runner lost", ID: r.ID}); err != nil {
				return err
			}
			if r.CurrentTaskID.Valid {
				return q.RevertInProgress(ctx, r.CurrentTaskID.Int64)
			}
			return nil
		})
		if err != nil {
			return err
		}
		if r.CurrentTaskID.Valid {
			s.recomputeTask(ctx, r.CurrentTaskID.Int64)
		}
		s.Log.Warn("run failed: runner lost", "run", r.ID)
		s.afterLost(ctx, r.ID)
	}
	return nil
}

// afterLost recovers a work run whose runner died: the first loss for a task queues a fresh run
// (its workspace is gone, so it starts over); a second loss means something is wrong with the
// task or the fleet, so the owner is asked instead of looping.
func (s *Service) afterLost(ctx context.Context, runID int64) {
	run, err := s.DB.RQ.GetRun(ctx, runID)
	if err != nil || run.Kind != "work" || s.Stack == nil {
		return
	}
	t, err := s.DB.RQ.GetTask(ctx, run.TaskID)
	if err != nil || !t.Sqid.Valid {
		return
	}
	lost, err := s.DB.RQ.CountLostRunsForTask(ctx, run.TaskID)
	if err != nil {
		return
	}
	if lost <= 1 {
		if nr, err := s.Stack.Queue(ctx, t.Sqid.String); err != nil {
			s.Log.Warn("retry after lost runner", "run", runID, "task", t.Sqid.String, "err", err)
		} else {
			s.DB.Q.SetRunStatus(ctx, dbgen.SetRunStatusParams{StatusText: fmt.Sprintf("runner lost; retrying as run %d", nr.ID), ID: runID})
			s.Log.Info("runner lost; run requeued", "run", runID, "new_run", nr.ID)
			s.Bus.Run(runID)
		}
		return
	}
	s.askOwner(ctx, t, runID, "requeue", "Try again", "Leave it",
		fmt.Sprintf("Run %d on %q lost its runner, and so did an earlier run of this task. Try again?", runID, t.Title))
}

// SweepStuck asks the owner about running runs that have been silent for StuckAfter: they may be
// hung (waiting on a prompt, a dead process, a deadlocked tool). The owner can cancel the run or
// keep waiting; a kept-waiting run is raised again after another StuckAfter of silence.
func (s *Service) SweepStuck(ctx context.Context) error {
	cutoff := dbtime.Format(s.Now().Add(-StuckAfter))
	stuck, err := s.DB.RQ.StuckRuns(ctx, cutoff)
	if err != nil {
		return err
	}
	for _, r := range stuck {
		taskID := r.TaskID
		if r.CurrentTaskID.Valid {
			taskID = r.CurrentTaskID.Int64
		}
		t, err := s.DB.RQ.GetTask(ctx, taskID)
		if err != nil {
			continue
		}
		s.askOwner(ctx, t, r.ID, "cancel_run", "Cancel the run", "Keep waiting",
			fmt.Sprintf("Run %d on %q has shown no activity for %s. Cancel it?", r.ID, t.Title, StuckAfter))
	}
	return nil
}

// askOwner raises an approval question about a run; approving performs op (see resolveApproval).
func (s *Service) askOwner(ctx context.Context, t dbgen.Task, runID int64, op, yes, no, prompt string) {
	payload, _ := json.Marshal(approvalPayload{Op: op, Sqid: t.Sqid.String, RunID: runID})
	_, err := s.NewQuestion(ctx, dbgen.CreateQuestionParams{
		TaskID: t.ID, RunID: sql.NullInt64{Int64: runID, Valid: true}, Kind: "approval",
		Prompt: prompt, PayloadJson: string(payload),
	}, []proto.QuestionOption{{Label: yes, Value: "approve"}, {Label: no, Value: "deny"}})
	if err != nil {
		s.Log.Error("raise question", "run", runID, "op", op, "err", err)
	}
}

// FailRun fails an active run for a reason that is not the agent's fault (e.g. its remote target
// vanished): the run is finished as failed, its driver slot is released, the driver is told to stop
// the session, and the task is handled as for any lost run (re-queued when appropriate).
func (s *Service) FailRun(ctx context.Context, runID int64, reason string) error {
	run, err := s.DB.RQ.GetRun(ctx, runID)
	if err != nil {
		return err
	}
	if proto.Terminal(run.State) {
		return nil
	}
	err = s.DB.Tx(ctx, func(q *dbgen.Queries) error {
		if err := q.FinishRun(ctx, dbgen.FinishRunParams{State: "failed", Now: dbnull.Str(s.ts()), Error: reason, StatusText: reason, ID: runID}); err != nil {
			return err
		}
		if run.CurrentTaskID.Valid {
			if err := q.RevertInProgress(ctx, run.CurrentTaskID.Int64); err != nil {
				return err
			}
		}
		if run.RunnerID.Valid {
			return q.ReleaseRunner(ctx, dbgen.ReleaseRunnerParams{Now: dbnull.Str(s.ts()), ID: run.RunnerID.Int64})
		}
		return nil
	})
	if err != nil {
		return err
	}
	if run.RunnerID.Valid && s.NC != nil { // best effort: stop the session on the driver
		b, _ := json.Marshal(proto.RunRef{RunID: runID})
		rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		s.NC.RequestWithContext(rctx, natsd.RunnerCmdSubject(run.RunnerID.Int64, "cancel"), b)
	}
	if run.CurrentTaskID.Valid {
		s.recomputeTask(ctx, run.CurrentTaskID.Int64)
	}
	s.Log.Warn("run failed", "run", runID, "reason", reason)
	s.afterLost(ctx, runID)
	return nil
}
