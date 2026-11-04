// Package runs ingests run events from runners, answers their work-stack requests, and
// cancels and inspects runs.
package runs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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
	// OnRunFinished is called after a run reaches a terminal state (PRs in M6).
	OnRunFinished func(ctx context.Context, runID int64)
	// OnAgentNotice is called when an agent's Notification hook fires (Claude Code: it needs
	// permission, or has been idle waiting): the owner should look. message is the agent's text.
	OnAgentNotice func(run dbgen.Run, message string)
}

func (s *Service) ts() string { return s.Now().UTC().Format("2006-01-02T15:04:05.000Z") }

func nstr(v string) sql.NullString { return sql.NullString{String: v, Valid: v != ""} }

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
	defer s.Bus.Publish("run", strconv.FormatInt(runID, 10))
	if !proto.Terminal(st.State) {
		started := sql.NullString{}
		if st.State == "preparing" || st.State == "running" {
			started = nstr(s.ts())
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
			State: st.State, Now: nstr(s.ts()), ExitCode: code, Error: st.Error, StatusText: st.Status,
			Pushed: pushed, HeadSha: st.HeadSHA, ID: runID,
		}); err != nil {
			return err
		}
		if run.CurrentTaskID.Valid {
			if err := q.RevertInProgress(ctx, run.CurrentTaskID.Int64); err != nil {
				return err
			}
		}
		return q.ReleaseRunner(ctx, dbgen.ReleaseRunnerParams{Now: nstr(s.ts()), ID: runnerID})
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
		Ts: h.TS.UTC().Format("2006-01-02T15:04:05.000Z"), Name: h.Event, PayloadJson: payload}); err != nil {
		return err
	}
	if h.Event == "Notification" {
		var n struct {
			Message string `json:"message"`
		}
		json.Unmarshal(h.Payload, &n)
		if s.Bus != nil {
			s.Bus.Publish("run", strconv.FormatInt(runID, 10))
		}
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
		RunID: runID, Seq: l.Seq, Ts: l.TS.UTC().Format("2006-01-02T15:04:05.000Z"), Stream: l.Stream, Level: "info", Message: l.Message,
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

// Cancel cancels a run: a queued run directly, an active one via its runner.
func (s *Service) Cancel(ctx context.Context, runID int64) error {
	n, err := s.DB.Q.CancelQueuedRun(ctx, dbgen.CancelQueuedRunParams{Now: nstr(s.ts()), ID: runID})
	if err != nil {
		return err
	}
	if n == 1 {
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
	cutoff := s.Now().Add(-LostAfter).UTC().Format("2006-01-02T15:04:05.000Z")
	lost, err := s.DB.RQ.RunsOnLostRunners(ctx, nstr(cutoff))
	if err != nil {
		return err
	}
	for _, r := range lost {
		err := s.DB.Tx(ctx, func(q *dbgen.Queries) error {
			if err := q.FinishRun(ctx, dbgen.FinishRunParams{State: "failed", Now: nstr(s.ts()), Error: "runner lost", ID: r.ID}); err != nil {
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
	}
	return nil
}
