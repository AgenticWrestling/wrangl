package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/natsd"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/stacktools"
)

type peerKey struct{}

// waiters holds questions the agent is blocked on.
type waiters struct {
	mu  sync.Mutex
	m   map[int64]chan proto.Answer
	run map[int64]int64 // question id -> the run that asked it
}

func (w *waiters) add(id, run int64) chan proto.Answer {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.m == nil {
		w.m, w.run = map[int64]chan proto.Answer{}, map[int64]int64{}
	}
	ch := make(chan proto.Answer, 1)
	w.m[id], w.run[id] = ch, run
	return ch
}

func (w *waiters) deliver(a proto.Answer) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	ch, ok := w.m[a.QuestionID]
	if ok {
		ch <- a
		delete(w.m, a.QuestionID)
		delete(w.run, a.QuestionID)
	}
	return ok
}

// cancelRun cancels the questions one run is blocked on (it ended); other runs' stay.
func (w *waiters) cancelRun(run int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for id, ch := range w.m {
		if w.run[id] != run {
			continue
		}
		ch <- proto.Answer{QuestionID: id, Cancelled: true}
		delete(w.m, id)
		delete(w.run, id)
	}
}

// Tool relays a work-stack tool call for the run in progress. ask_question (and the
// runner's own system question) block until the owner answers.
func (w *Worker) Tool(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	return w.ToolFor(ctx, 0, name, args)
}

// ToolFor is Tool for a named run (0 = the only one running).
func (w *Worker) ToolFor(ctx context.Context, runID int64, name string, args json.RawMessage) (json.RawMessage, error) {
	rs, err := w.target(runID)
	if err != nil {
		return nil, err
	}
	runID = rs.id
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	if name == "target_exec" || name == "target_put" {
		return w.targetTool(ctx, rs, name, args) // served here: the credential never leaves the daemon
	}
	b, _ := json.Marshal(proto.ToolRequest{RunID: runID, Tool: name, Args: args})
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	msg, err := w.NC.RequestWithContext(rctx, natsd.StackSubject(w.Cfg.RunnerID, "tool"), b)
	if err != nil {
		return nil, fmt.Errorf("controller unreachable: %w", err)
	}
	var r proto.ToolReply
	if err := json.Unmarshal(msg.Data, &r); err != nil {
		return nil, err
	}
	if !r.OK {
		return nil, errors.New(r.Error)
	}
	if name != "ask_question" && name != "_system_question" {
		return r.Result, nil
	}
	var q struct {
		QuestionID int64 `json:"question_id"`
	}
	json.Unmarshal(r.Result, &q)
	ch := w.answers.add(q.QuestionID, runID)
	select {
	case a := <-ch:
		if a.Cancelled {
			return nil, errors.New("the question was cancelled (the run ended)")
		}
		return json.Marshal(map[string]any{"question_id": a.QuestionID, "answer": a.Value, "text": a.Text})
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Hook publishes an agent hook event for the run in progress (fire and forget).
func (w *Worker) Hook(event string, payload json.RawMessage) error {
	return w.HookFor(0, event, payload)
}

// HookFor is Hook for a named run (0 = the only one running).
func (w *Worker) HookFor(runID int64, event string, payload json.RawMessage) error {
	rs, err := w.target(runID)
	if err != nil {
		return nil // hooks outside a run (e.g. a manual session) are ignored
	}
	runID = rs.id
	w.mu.Lock()
	rs.hookSeq++
	seq := rs.hookSeq
	w.mu.Unlock()
	if !json.Valid(payload) {
		payload, _ = json.Marshal(string(payload))
	}
	var ids struct {
		SessionID      string `json:"session_id"`     // Claude Code, Codex, Gemini CLI
		ConversationID string `json:"conversationId"` // Antigravity (agy)
	}
	if json.Unmarshal(payload, &ids) == nil {
		w.mu.Lock()
		sess := rs.sess
		w.mu.Unlock()
		if sess != nil {
			sess.noteSession(ids.SessionID+ids.ConversationID, false)
		}
	}
	b, _ := json.Marshal(map[string]any{"seq": seq, "ts": time.Now().UTC(), "event": event, "payload": payload})
	_, err = w.JS.PublishAsync(natsd.RunHookSubject(w.Cfg.RunnerID, runID), b)
	return err
}

// ServeAgentSocket serves the agent's local socket until ctx ends. Only the agent user
// (and the daemon's own user) may connect: checked with SO_PEERCRED.
func (w *Worker) ServeAgentSocket(ctx context.Context, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	os.Remove(path)
	l, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	id, err := (&Applier{Cfg: w.Cfg}).agent()
	if err != nil {
		l.Close()
		return err
	}
	allowed := map[uint32]bool{uint32(os.Getuid()): true}
	if id.drop {
		allowed[uint32(id.uid)] = true
		os.Chown(path, id.uid, id.gid)
	}
	os.Chmod(path, 0o660)

	mux := http.NewServeMux()
	reply := func(rw http.ResponseWriter, code int, v any) {
		rw.Header().Set("Content-Type", "application/json")
		rw.WriteHeader(code)
		json.NewEncoder(rw).Encode(v)
	}
	mux.HandleFunc("POST /hook", func(rw http.ResponseWriter, r *http.Request) {
		var h struct {
			Event   string          `json:"event"`
			Payload json.RawMessage `json:"payload"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&h); err != nil || h.Event == "" {
			reply(rw, 400, map[string]string{"error": "want {event, payload}"})
			return
		}
		runID := runOf(r)
		if err := w.HookFor(runID, h.Event, h.Payload); err != nil {
			reply(rw, 500, map[string]string{"error": err.Error()})
			return
		}
		if strings.EqualFold(h.Event, "Stop") && r.URL.Query().Get("continue") != "" {
			reply(rw, 200, w.continuation(r.Context(), runID))
			return
		}
		reply(rw, 200, map[string]bool{"ok": true})
	})
	mux.HandleFunc("GET /tools", func(rw http.ResponseWriter, r *http.Request) {
		kind := w.Kind()
		if rs, err := w.target(runOf(r)); err == nil {
			kind = rs.kind
		}
		reply(rw, 200, stacktools.For(kind))
	})
	known := map[string]bool{}
	for _, t := range stacktools.All {
		known[t.Name] = true
	}
	mux.HandleFunc("POST /tool/{name}", func(rw http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if !known[name] { // the daemon-only system question is not reachable from here
			reply(rw, 404, map[string]string{"error": "unknown tool " + name})
			return
		}
		args, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		out, err := w.ToolFor(r.Context(), runOf(r), name, args)
		if err != nil {
			reply(rw, 422, map[string]string{"error": err.Error()})
			return
		}
		rw.Header().Set("Content-Type", "application/json")
		rw.Write(out)
	})
	srv := &http.Server{
		Handler: http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
			uid, ok := r.Context().Value(peerKey{}).(uint32)
			if !ok || !allowed[uid] {
				reply(rw, 403, map[string]string{"error": "only the agent user may use this socket"})
				return
			}
			mux.ServeHTTP(rw, r)
		}),
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			if uc, ok := c.(*net.UnixConn); ok {
				if raw, err := uc.SyscallConn(); err == nil {
					var cred *syscall.Ucred
					raw.Control(func(fd uintptr) {
						cred, _ = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
					})
					if cred != nil {
						return context.WithValue(ctx, peerKey{}, cred.Uid)
					}
				}
			}
			return ctx
		},
	}
	go func() { <-ctx.Done(); srv.Close() }()
	if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// continuation answers a Stop hook in interactive drive (M9): the agent-specific
// "keep going" response with the next task's prompt, or permission to stop.
func (w *Worker) continuation(ctx context.Context, runID int64) map[string]any {
	var next func(ctx context.Context) (string, bool)
	if rs, err := w.target(runID); err == nil {
		w.mu.Lock()
		next = rs.continueFn
		w.mu.Unlock()
	}
	if next == nil {
		return map[string]any{"continue": false}
	}
	prompt, ok := next(ctx)
	if !ok {
		return map[string]any{"continue": false}
	}
	return map[string]any{"continue": true, "prompt": prompt}
}

// SocketClient talks to the daemon's agent socket (used by `wrangl runner hook|tool|mcp`).
func SocketClient(path string) *http.Client {
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	}}}
}

// RunHeader is how an agent process tells the daemon which run it belongs to (its WRANGL_RUN_ID).
const RunHeader = "X-Wrangl-Run"

// runOf reads the run a request is for: 0 when the agent did not say (single-session runners).
func runOf(r *http.Request) int64 {
	id, _ := strconv.ParseInt(r.Header.Get(RunHeader), 10, 64)
	return id
}
