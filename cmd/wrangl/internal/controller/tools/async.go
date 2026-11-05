package tools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/dbtime"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/errs"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/toolkdl"
)

// Async tool calls (ADR 0003). The agent -> shim -> daemon -> NATS relay answers within 30 s, so a
// tool whose timeout is longer (toolkdl.AsyncAfter) is accepted instead: the call returns a call id
// at once, the executor keeps running on the controller, and the agent collects status, progress
// and the result with the ext_result tool (the shim serves tools only, with no notifications, so
// polling is the mechanism every MCP client supports). ext_result can wait up to maxWait seconds,
// which turns polling into a cheap long poll.

// ResultTool is the companion tool for async calls.
const ResultTool = Prefix + "result"

const (
	defaultMaxAsyncPerRun = 3
	defaultMaxAsync       = 16
	defaultResultTTL      = 30 * time.Minute
	maxRetainedPerRun     = 20
	maxWait               = 20 // seconds; below the relay bound
	maxProgressLines      = 20
	maxProgressLine       = 200
)

var resultDecl = proto.ToolDecl{
	Name: ResultTool,
	Description: "Get the status of a long-running project tool call by its call_id: running (with the latest progress lines), done (with the output), error or cancelled. " +
		"wait_s (up to 20) waits for completion before answering, so you need not poll in a loop. cancel=true stops the call. Results are kept for 30 minutes.",
	InputSchema: json.RawMessage(`{"type":"object","required":["call_id"],"additionalProperties":false,"properties":{"call_id":{"type":"string"},"wait_s":{"type":"integer","minimum":0,"maximum":20,"description":"seconds to wait for completion (default 0)"},"cancel":{"type":"boolean"}}}`),
	RunKinds:    []string{"work", "revise", "review", "plan", "resolve"},
}

type asyncCall struct {
	id      string
	runID   int64
	tool    string
	rowID   int64
	started time.Time
	cancel  context.CancelFunc
	done    chan struct{}

	mu        sync.Mutex
	status    string // running | done | error | cancelled
	output    string
	truncated bool
	errText   string
	progress  []string
	finished  time.Time
	duration  time.Duration
}

func (e *Engine) base() context.Context {
	if e.Base != nil {
		return e.Base
	}
	return context.Background()
}

func (e *Engine) limits() (perRun, total int, ttl time.Duration) {
	perRun, total, ttl = e.MaxAsyncPerRun, e.MaxAsync, e.ResultTTL
	if perRun <= 0 {
		perRun = defaultMaxAsyncPerRun
	}
	if total <= 0 {
		total = defaultMaxAsync
	}
	if ttl <= 0 {
		ttl = defaultResultTTL
	}
	return
}

// Recover marks audit rows left 'running' by a previous controller process as failed: their
// calls died with it. Call once at startup.
func (e *Engine) Recover(ctx context.Context) error {
	_, err := e.Svc.DB.Q.FailRunningToolCalls(ctx)
	return err
}

// CancelRun stops a run's long calls and forgets its finished ones. Called when the run ends.
func (e *Engine) CancelRun(runID int64) {
	e.amu.Lock()
	defer e.amu.Unlock()
	for id, c := range e.calls {
		if c.runID != runID {
			continue
		}
		c.mu.Lock()
		running := c.status == "running"
		c.mu.Unlock()
		if running {
			c.cancel() // the goroutine records 'cancelled'
		} else {
			delete(e.calls, id)
		}
	}
}

// Close cancels every running long call (controller shutdown).
func (e *Engine) Close() {
	e.amu.Lock()
	defer e.amu.Unlock()
	for _, c := range e.calls {
		c.cancel()
	}
}

func (e *Engine) startAsync(ctx context.Context, c Caller, name string, def *toolkdl.Definition, project string, in *toolkdl.Input, ex Executor) (any, error) {
	if _, err := e.Svc.Resolve(project, def); err != nil {
		return nil, err // a missing secret fails the call now, not minutes later
	}
	perRun, total, ttl := e.limits()
	e.amu.Lock()
	e.pruneLocked(ttl)
	runN, allN := 0, 0
	for _, x := range e.calls {
		x.mu.Lock()
		if x.status == "running" {
			allN++
			if x.runID == c.RunID {
				runN++
			}
		}
		x.mu.Unlock()
	}
	switch {
	case runN >= perRun:
		e.amu.Unlock()
		return nil, fmt.Errorf("this run already has %d long-running tool calls; wait for one to finish (%s) or cancel one", perRun, ResultTool)
	case allN >= total:
		e.amu.Unlock()
		return nil, errors.New("the controller is running too many long tool calls right now; try again shortly")
	}
	var rnd [8]byte
	rand.Read(rnd[:])
	start := e.now()
	rowID, err := e.Svc.DB.Q.InsertRunningToolCall(ctx, dbgen.InsertRunningToolCallParams{RunID: c.RunID, Tool: name, At: dbtime.Format(start)})
	if err != nil {
		e.amu.Unlock()
		return nil, err
	}
	cctx, cancel := context.WithCancel(e.base())
	call := &asyncCall{id: "call_" + hex.EncodeToString(rnd[:]), runID: c.RunID, tool: name, rowID: rowID, started: start,
		cancel: cancel, done: make(chan struct{}), status: "running"}
	if e.calls == nil {
		e.calls = map[string]*asyncCall{}
	}
	e.calls[call.id] = call
	e.amu.Unlock()

	go e.runAsync(cctx, cancel, call, ex, def, project, c.RunID, in, name)
	return map[string]any{"call_id": call.id, "status": "running", "tool": name,
		"note": fmt.Sprintf("Accepted: this tool can run up to %d s. Collect the result with %s {call_id, wait_s}.", def.TimeoutSeconds(), ResultTool)}, nil
}

func (e *Engine) runAsync(ctx context.Context, cancel context.CancelFunc, call *asyncCall, ex Executor, def *toolkdl.Definition, project string, runID int64, in *toolkdl.Input, name string) {
	defer close(call.done)
	defer cancel()
	progress := func(msg string) {
		msg = strings.TrimSpace(strings.ReplaceAll(msg, "\n", " "))
		if len(msg) > maxProgressLine {
			msg = msg[:maxProgressLine] + "…"
		}
		if msg == "" {
			return
		}
		call.mu.Lock()
		call.progress = append(call.progress, msg)
		if len(call.progress) > maxProgressLines {
			call.progress = call.progress[len(call.progress)-maxProgressLines:]
		}
		call.mu.Unlock()
	}
	text, truncated, err := e.execute(ctx, ex, def, project, runID, in, name, time.Duration(def.TimeoutSeconds())*time.Second, progress)
	dur := e.now().Sub(call.started)
	status := "done"
	auditStatus := "ok"
	if err != nil {
		status, auditStatus = "error", "error"
		if errors.Is(ctx.Err(), context.Canceled) {
			status, auditStatus = "cancelled", "cancelled"
		}
	}
	call.mu.Lock()
	call.status, call.finished, call.duration = status, e.now(), dur
	if err != nil {
		call.errText = err.Error()
		if status == "cancelled" {
			call.errText = "cancelled"
		}
	} else {
		call.output, call.truncated = text, truncated
	}
	call.mu.Unlock()
	// One row per call: the one written at accept time is updated here, exactly once.
	if werr := e.Svc.DB.Q.FinishToolCall(context.Background(), dbgen.FinishToolCallParams{
		Status: auditStatus, DurationMs: dur.Milliseconds(), Bytes: int64(len(text)), ID: call.rowID}); werr != nil && e.Log != nil {
		e.Log.Warn("tool audit update failed", "run", runID, "tool", name, "err", werr)
	}
}

// pruneLocked drops finished calls past their retention and keeps at most maxRetainedPerRun
// finished calls per run (the oldest go first).
func (e *Engine) pruneLocked(ttl time.Duration) {
	now := e.now()
	perRun := map[int64][]*asyncCall{}
	for id, c := range e.calls {
		c.mu.Lock()
		fin, st := c.finished, c.status
		c.mu.Unlock()
		if st == "running" {
			continue
		}
		if now.Sub(fin) > ttl {
			delete(e.calls, id)
			continue
		}
		perRun[c.runID] = append(perRun[c.runID], c)
	}
	for _, cs := range perRun {
		if len(cs) <= maxRetainedPerRun {
			continue
		}
		sort.Slice(cs, func(i, j int) bool { return cs[i].finished.Before(cs[j].finished) })
		for _, c := range cs[:len(cs)-maxRetainedPerRun] {
			delete(e.calls, c.id)
		}
	}
}

// result implements ext_result. A run can only see its own calls.
func (e *Engine) result(ctx context.Context, c Caller, raw json.RawMessage) (any, error) {
	if c.RunID == 0 {
		return nil, fmt.Errorf("%w: project tools are only available to runs", errs.Forbidden)
	}
	var a struct {
		CallID string `json:"call_id"`
		WaitS  int    `json:"wait_s"`
		Cancel bool   `json:"cancel"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &a); err != nil {
			return nil, fmt.Errorf("%w: arguments: %v", errs.Invalid, err)
		}
	}
	if a.WaitS < 0 || a.WaitS > maxWait {
		return nil, fmt.Errorf("%w: wait_s must be 0..%d", errs.Invalid, maxWait)
	}
	_, _, ttl := e.limits()
	e.amu.Lock()
	e.pruneLocked(ttl)
	call := e.calls[a.CallID]
	e.amu.Unlock()
	if call == nil || call.runID != c.RunID { // another run's call looks the same as an unknown one
		return nil, fmt.Errorf("%w: no long-running call %q for this run (finished results are kept for %s)", errs.NotFound, a.CallID, ttl)
	}
	if a.Cancel {
		call.cancel()
		a.WaitS = 2 // give it a moment to record the cancellation
	}
	if a.WaitS > 0 {
		t := time.NewTimer(time.Duration(a.WaitS) * time.Second)
		defer t.Stop()
		select {
		case <-call.done:
		case <-t.C:
		case <-ctx.Done():
		}
	}
	return call.view(e.now()), nil
}

func (c *asyncCall) view(now time.Time) map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	v := map[string]any{"call_id": c.id, "tool": c.tool, "status": c.status}
	if len(c.progress) > 0 {
		v["progress"] = append([]string(nil), c.progress...)
	}
	switch c.status {
	case "running":
		v["elapsed_s"] = int(now.Sub(c.started).Seconds())
		v["note"] = "Still running. Call " + ResultTool + " again (wait_s up to 20) or carry on with other work."
	case "done":
		v["output"], v["truncated"], v["duration_ms"] = c.output, c.truncated, c.duration.Milliseconds()
	default:
		v["error"], v["duration_ms"] = c.errText, c.duration.Milliseconds()
	}
	return v
}
