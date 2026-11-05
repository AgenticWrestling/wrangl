package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"text/template"
	"time"

	"github.com/creack/pty"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/natsd"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/transcript"
)

// Defaults for agent turns.
const (
	DefaultTurnTimeout = 2 * time.Hour
	killGrace          = 15 * time.Second
)

// Worker runs assignments on this runner: up to the template's max_sessions at once, each in
// its own workspace, with its own hook sequence, interactive drive and pending questions.
type Worker struct {
	Cfg *Config
	NC  *nats.Conn
	JS  jetstream.JetStream
	Log *slog.Logger

	mu      sync.Mutex
	runs    map[int64]*runState
	answers waiters
	terms   terms // PTYs the owner can watch from the web terminal
	// secretRefs counts the sessions using each secret file written to a shared path, so the
	// last one out removes it (a template's secrets are the same for every session).
	secretRefs map[string]int
	// repoLocks serialises git operations that touch a repo's shared object store (fetch,
	// worktree add/remove): one lock per mirror path.
	repoLocks sync.Map // mirror path -> *sync.Mutex
	// Targets reaches remote targets for remote runs (nil = real ssh/adb over SSM; tests use FakeTarget).
	Targets TargetBackend
}

// runState is one run in progress on this runner.
type runState struct {
	id         int64
	kind       string
	cancel     context.CancelFunc
	done       chan struct{}
	hookSeq    int64
	continueFn func(ctx context.Context) (string, bool) // interactive drive (M9)
	sess       *session                                 // hooks report its agent session id
}

// Active lists the runs in progress, lowest id first.
func (w *Worker) Active() []int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	ids := make([]int64, 0, len(w.runs))
	for id := range w.runs {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// errNoRun: no run matches.
var errNoRun = errors.New("no run in progress")

// target picks the run a request is about: the one named (the agent's WRANGL_RUN_ID), else
// the only one running. With several running and none named it refuses rather than guess.
func (w *Worker) target(runID int64) (*runState, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if runID != 0 {
		if r := w.runs[runID]; r != nil {
			return r, nil
		}
		return nil, fmt.Errorf("run %d is not running on this runner", runID)
	}
	switch len(w.runs) {
	case 0:
		return nil, errNoRun
	case 1:
		for _, r := range w.runs {
			return r, nil
		}
	}
	return nil, errors.New("several runs are active: the caller must say which (WRANGL_RUN_ID)")
}

// DeliverAnswer hands an owner's answer to the agent waiting on it.
func (w *Worker) DeliverAnswer(a proto.Answer) bool { return w.answers.deliver(a) }

// Start begins an assignment in the background; it fails if the runner has no free session
// (a.MaxSessions, default 1) or already has that run.
func (w *Worker) Start(parent context.Context, a proto.Assignment) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.runs[a.RunID] != nil {
		return fmt.Errorf("run %d is already running here", a.RunID)
	}
	limit := a.MaxSessions
	if limit < 1 {
		limit = 1
	}
	if len(w.runs) >= limit {
		return fmt.Errorf("busy: %d of %d sessions in use", len(w.runs), limit)
	}
	ctx, cancel := context.WithCancel(parent)
	rs := &runState{id: a.RunID, kind: a.Kind, cancel: cancel, done: make(chan struct{})}
	if w.runs == nil {
		w.runs = map[int64]*runState{}
	}
	w.runs[a.RunID] = rs
	go func() {
		defer close(rs.done)
		defer cancel()
		w.run(ctx, a, rs)
		w.answers.cancelRun(a.RunID)
		w.mu.Lock()
		delete(w.runs, a.RunID)
		w.mu.Unlock()
	}()
	return nil
}

// Cancel stops run runID if it is in progress here.
func (w *Worker) Cancel(runID int64) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	r := w.runs[runID]
	if r == nil || r.cancel == nil {
		return false
	}
	r.cancel()
	return true
}

// Wait blocks until every run in progress has finished.
func (w *Worker) Wait() {
	w.mu.Lock()
	var ds []chan struct{}
	for _, r := range w.runs {
		ds = append(ds, r.done)
	}
	w.mu.Unlock()
	for _, d := range ds {
		<-d
	}
}

// holdSecret records that a session uses the secret file at path; it reports whether the file
// is already in place (another session wrote it), in which case the caller must not rewrite it.
func (w *Worker) holdSecret(path string) (already bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.secretRefs == nil {
		w.secretRefs = map[string]int{}
	}
	already = w.secretRefs[path] > 0
	w.secretRefs[path]++
	return already
}

// dropSecret releases a session's hold; the file is removed with the last one.
func (w *Worker) dropSecret(path string) {
	w.mu.Lock()
	w.secretRefs[path]--
	last := w.secretRefs[path] <= 0
	if last {
		delete(w.secretRefs, path)
	}
	w.mu.Unlock()
	if last {
		os.Remove(path)
	}
}

func (w *Worker) workdir(runID int64) (string, agentIdentity, error) {
	id, err := (&Applier{Cfg: w.Cfg}).agent()
	if err != nil {
		return "", id, err
	}
	return filepath.Join(id.home, "work", strconv.FormatInt(runID, 10)), id, nil
}

// session tracks one run's output and publishing.
type session struct {
	w     *Worker
	a     proto.Assignment
	id    agentIdentity
	dir   string
	seq   int64
	mu    sync.Mutex
	lines []string // recent agent output lines (for summaries)
	// continuedFrom is the existing branch head a revise/continuation run started from:
	// "ahead" counts commits past it.
	continuedFrom string
	// lastMsg is the agent's final message of its latest turn (from the Stop hook).
	lastMsg string
	// interactive drive (M9): one agent session; its Stop hook asks onStop for the next
	// prompt, and when there is none the daemon ends the session.
	interactive bool
	onStop      func(ctx context.Context) (string, bool)
	secretPaths []string   // secret files to remove when the run ends
	mirror      string     // the shared bare clone this run's worktree belongs to
	target      *runTarget // remote runs: the connection to the target
	cleanup     bool       // remove the worktree when the run ends (set once its work is safely pushed)
	// agentSession is the agent's own session id, for agents that cannot be given one up
	// front (Codex, agy, Gemini): read from its JSON output, else from a hook payload.
	agentSession      string
	agentSessionFixed bool // taken from the agent's own output: hooks don't override it
	// transcript tailing: entries keep one increasing seq across agent invocations,
	// and a resumed session's file is read on from where the last tail stopped.
	tsNorm    transcript.Normaliser
	tsOffsets map[string]int64
	tsPath    string // transcript_path from the first hook event
}

var sessionIDOut = regexp.MustCompile(`"(?:thread_id|session_id|sessionId|conversation_id|conversationId)"\s*:\s*"([A-Za-z0-9_.:-]+)"`)

// noteSession records the agent's session id (the first seen; stdout beats hooks).
func (s *session) noteSession(id string, fromOutput bool) {
	if id == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.agentSessionFixed || (s.agentSession != "" && !fromOutput) {
		return
	}
	s.agentSession, s.agentSessionFixed = id, fromOutput
}

func (s *session) agentSessionID() string { s.mu.Lock(); defer s.mu.Unlock(); return s.agentSession }

// resumeArgv is the command for a follow-up turn: resume, unless it needs the agent's
// session id and none has been seen yet (then a fresh start).
func (s *session) resumeArgv() []string {
	r := s.a.Agent.Resume
	if len(r) == 0 {
		return s.a.Agent.Start
	}
	if strings.Contains(strings.Join(r, " "), "AgentSessionID") && s.agentSessionID() == "" {
		s.logf("runner", "no agent session id seen yet; starting a new session instead of resuming")
		return s.a.Agent.Start
	}
	return r
}

// InteractiveEndGrace is how long an interactive session may take to exit by itself after
// its Stop hook was told to stop, before the daemon terminates it (a TUI never exits).
var InteractiveEndGrace = 5 * time.Second

var ansi = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]|\x1b\][^\x07]*(\x07|\x1b\\)|\x1b[@-Z\\-_]|\r`)

// state publishes a run state durably. Bounded, so a cancelled run with no controller
// cannot hang daemon shutdown; the controller's lost-run sweep covers what never arrives.
func (s *session) state(ctx context.Context, st proto.RunState) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	publishDurable(ctx, s.w.JS, natsd.RunStateSubject(s.w.Cfg.RunnerID, s.a.RunID), st, s.w.Log)
}

func (s *session) logf(stream, f string, a ...any) {
	s.line(stream, fmt.Sprintf(f, a...))
}

func (s *session) line(stream, msg string) {
	msg = strings.TrimRight(ansi.ReplaceAllString(msg, ""), " \t")
	if strings.TrimSpace(msg) == "" {
		return
	}
	s.mu.Lock()
	s.seq++
	l := proto.LogLine{Seq: s.seq, TS: time.Now().UTC(), Stream: stream, Message: msg}
	if stream == "agent" {
		if !s.agentSessionFixed {
			if m := sessionIDOut.FindStringSubmatch(msg); m != nil {
				s.agentSession, s.agentSessionFixed = m[1], true
			}
		}
		s.lines = append(s.lines, msg)
		if len(s.lines) > 50 {
			s.lines = s.lines[len(s.lines)-50:]
		}
	}
	s.mu.Unlock()
	if s.w.JS == nil { // unit tests without a bus
		return
	}
	b, _ := json.Marshal(l)
	if _, err := s.w.JS.PublishAsync(natsd.RunLogSubject(s.w.Cfg.RunnerID, s.a.RunID), b); err != nil {
		s.w.Log.Warn("log publish", "err", err)
	}
}

func (s *session) summary() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.lines)
	start := max(0, n-5)
	out := strings.Join(s.lines[start:], "\n")
	if len(out) > 500 {
		out = out[len(out)-500:]
	}
	return out
}

func (s *session) resetSummary() { s.mu.Lock(); s.lines = nil; s.mu.Unlock() }

// flush waits for queued async publishes (logs, PTY) to be acknowledged.
func (s *session) flush() {
	select {
	case <-s.w.JS.PublishAsyncComplete():
	case <-time.After(10 * time.Second):
		s.w.Log.Warn("timed out flushing run output")
	}
}

func (w *Worker) stack(ctx context.Context, op string, req proto.StackRequest) (proto.StackReply, error) {
	b, _ := json.Marshal(req)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	msg, err := w.NC.RequestWithContext(ctx, natsd.StackSubject(w.Cfg.RunnerID, op), b)
	if err != nil {
		return proto.StackReply{}, err
	}
	var r proto.StackReply
	if err := json.Unmarshal(msg.Data, &r); err != nil {
		return r, err
	}
	if !r.OK {
		return r, errors.New(r.Error)
	}
	return r, nil
}

// run executes one assignment end to end and always reports a terminal state.
func (w *Worker) run(ctx context.Context, a proto.Assignment, rs *runState) {
	// Final states must be published even after a cancel.
	pubCtx := context.WithoutCancel(ctx)
	dir, id, err := w.workdir(a.RunID)
	s := &session{w: w, a: a, id: id, dir: dir, interactive: a.Agent.Drive == proto.DriveInteractive && a.Kind == "work"}
	defer s.removeWorktree(pubCtx) // runs after removeSecrets (LIFO); only acts once cleanup is set: failed runs keep theirs
	defer s.removeSecrets()
	w.mu.Lock()
	rs.sess = s
	w.mu.Unlock()
	defer func() { w.mu.Lock(); rs.sess = nil; w.mu.Unlock() }()
	fail := func(err error) { s.fail(ctx, pubCtx, err) }
	if err != nil {
		fail(err)
		return
	}
	s.state(pubCtx, proto.RunState{State: "preparing", Status: "cloning " + a.Repo})
	base, err := w.prepare(ctx, s)
	if err != nil {
		fail(err)
		return
	}
	if a.Target != nil {
		rt, err := w.openTarget(ctx, s)
		if err != nil {
			fail(err)
			return
		}
		w.mu.Lock()
		s.target = rt
		w.mu.Unlock()
		defer func() {
			rt.reset(context.WithoutCancel(ctx))
			rt.close()
		}()
		s.logf("runner", "remote target %s ready (%s)", a.Target.Name, a.Target.Protocol)
	}
	s.state(pubCtx, proto.RunState{State: "running", BaseSHA: base})

	turns, blocked, ok := w.runTasks(ctx, pubCtx, s, fail)
	if !ok {
		return
	}
	w.finishRun(ctx, pubCtx, s, fail, base, turns, blocked)
}

// fail reports a failed (or, after a cancel, cancelled) run.
func (s *session) fail(ctx, pubCtx context.Context, err error) {
	s.logf("runner", "run failed: %v", err)
	s.flush()
	st := proto.RunState{State: "failed", Error: err.Error()}
	if ctx.Err() != nil {
		st = proto.RunState{State: "cancelled", Error: "cancelled"}
	}
	s.state(pubCtx, st)
}

// runTasks asks the controller for the next task until none is ready, running one turn (and its
// checkpoint) for each. ok is false when the run has already been reported as failed.
func (w *Worker) runTasks(ctx, pubCtx context.Context, s *session, fail func(error)) (turns int, blocked, ok bool) {
	a := s.a
	turnTimeout := DefaultTurnTimeout
	if a.Agent.TurnTimeout != "" {
		if d, err := time.ParseDuration(a.Agent.TurnTimeout); err == nil && d > 0 {
			turnTimeout = d
		}
	}
	for {
		r, err := w.stack(ctx, "next", proto.StackRequest{RunID: a.RunID})
		if err != nil {
			fail(fmt.Errorf("ask controller for next task: %w", err))
			return
		}
		n := r.Next
		if n == nil || n.Done || n.Blocked {
			blocked = n != nil && n.Blocked
			return turns, blocked, true
		}
		if !w.turnAndCheckpoint(ctx, pubCtx, s, n, &turns, turnTimeout, fail) {
			return
		}
	}
}

// turnAndCheckpoint runs the agent on one task, commits its work and, for work runs, makes sure
// the task was completed. It reports false when the run ended (failed) instead.
func (w *Worker) turnAndCheckpoint(ctx, pubCtx context.Context, s *session, n *proto.StackNext, turns *int, turnTimeout time.Duration, fail func(error)) bool {
	a := s.a
	s.logf("runner", "==> %s %s", n.Sqid, n.Title)
	s.state(pubCtx, proto.RunState{State: "running", Status: n.Sqid + " " + n.Title})
	argv := a.Agent.Start
	if *turns > 0 {
		argv = s.resumeArgv()
	}
	s.resetSummary()
	var ch *chain
	if s.interactive {
		ch = &chain{cur: n}
		s.onStop = func(hctx context.Context) (string, bool) { return w.chainNext(hctx, s, ch) }
	}
	code, err := w.turn(ctx, s, argv, n, turnTimeout)
	*turns++
	if ch != nil {
		s.onStop = nil
		ch.mu.Lock()
		n, *turns = ch.cur, *turns+ch.extra // the session may have moved on to later tasks
		ch.mu.Unlock()
	}
	if err != nil {
		fail(fmt.Errorf("task %s: %w", n.Sqid, err))
		return false
	}
	if code != 0 {
		c := code
		s.flush()
		s.state(pubCtx, proto.RunState{State: "failed", ExitCode: &c, Error: fmt.Sprintf("agent exited %d on task %s", code, n.Sqid)})
		return false
	}
	if a.Kind == "review" || a.Kind == "plan" {
		return true // read-only runs: no commits; the controller records what they submitted
	}
	if err := w.checkpoint(ctx, s, n); err != nil {
		fail(err)
		return false
	}
	if a.Kind != "work" {
		return true // revise: one composed prompt, no leaf to complete
	}
	if err := w.ensureComplete(ctx, s, n, turns, turnTimeout); err != nil {
		if errors.Is(err, errFailRun) {
			s.flush()
			s.state(pubCtx, proto.RunState{State: "failed", Error: "the owner chose to fail the run: " + n.Sqid + " was not completed"})
			return false
		}
		fail(err)
		return false
	}
	return true
}

// finishRun pushes the run branch when it has commits and reports the final state.
func (w *Worker) finishRun(ctx, pubCtx context.Context, s *session, fail func(error), base string, turns int, blocked bool) {
	a, id, dir := s.a, s.id, s.dir
	head, _ := gitAsAgent(ctx, id, dir, "rev-parse", "HEAD")
	from := base
	if s.continuedFrom != "" {
		from = s.continuedFrom
	}
	ahead, _ := gitAsAgent(ctx, id, dir, "rev-list", "--count", from+"..HEAD")
	pushed := false
	if a.Kind == "review" || a.Kind == "plan" {
		ahead = "0" // never push from a read-only run
	}
	if ahead != "" && ahead != "0" {
		s.state(pubCtx, proto.RunState{State: "pushing", Status: fmt.Sprintf("pushing %s commits to %s", ahead, a.Branch)})
		if out, err := w.pushBranch(ctx, id, dir, a.GitToken, a.Branch); err != nil {
			fail(fmt.Errorf("push %s: %w: %s", a.Branch, err, out))
			return
		}
		s.logf("runner", "pushed %s commits to %s", ahead, a.Branch)
		pushed = true
	} else {
		s.logf("runner", "no commits to push")
	}
	s.flush()
	final := proto.RunState{State: "succeeded", HeadSHA: head, Pushed: pushed, Status: fmt.Sprintf("%d task(s) completed", turns)}
	if blocked {
		final.State, final.Status = "blocked", "remaining tasks wait on precursors outside this run"
	}
	s.cleanup = !blocked
	s.state(pubCtx, final)
}

// prepare clones the repo (as the daemon, with the token), hands the tree to the agent,
// creates the run branch and writes the agent's files. It returns the base commit.
func (w *Worker) prepare(ctx context.Context, s *session) (string, error) {
	a, id, dir := s.a, s.id, s.dir
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return "", err
	}
	if err := chownTree(filepath.Dir(dir), id); err != nil {
		return "", err
	}
	cloneBranch := a.BaseBranch
	if a.Continue {
		cloneBranch = a.Branch // revise / continuation: build on the existing branch
	}
	s.logf("runner", "preparing worktree of %s (%s)", a.Repo, cloneBranch)
	if err := w.addWorktree(ctx, s, cloneBranch); err != nil {
		return "", err
	}
	if err := chownTree(dir, id); err != nil {
		return "", err
	}
	if err := w.setIdentity(ctx, s); err != nil {
		return "", err
	}
	if a.Continue {
		// Base = where the branch left the base branch, so diffs and "ahead" count only
		// this run's commits plus the earlier ones on the branch.
		mb, err := gitAsAgent(ctx, id, dir, "merge-base", "HEAD", "origin/"+a.BaseBranch)
		if err != nil {
			return "", fmt.Errorf("merge-base: %w: %s", err, mb)
		}
		head, _ := gitAsAgent(ctx, id, dir, "rev-parse", "HEAD")
		os.WriteFile(filepath.Join(gitDir(dir), "wrangl-base"), []byte(mb+"\n"), 0o644)
		s.continuedFrom = head
	}
	base, err := gitAsAgent(ctx, id, dir, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	if !a.Continue {
		os.WriteFile(filepath.Join(gitDir(dir), "wrangl-base"), []byte(base+"\n"), 0o644)
	}
	vars := s.vars("")
	for _, f := range a.Agent.Files {
		path, err := render(f.Path, vars)
		if err != nil {
			return "", fmt.Errorf("agent file path %q: %w", f.Path, err)
		}
		content, err := render(f.Content, vars)
		if err != nil {
			return "", fmt.Errorf("agent file %s: %w", path, err)
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(dir, path)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return "", err
		}
		if f.Merge == "json" { // deep-merge into the agent's existing settings, e.g. ~/.claude.json
			if content, err = mergeJSONFile(path, content); err != nil {
				return "", fmt.Errorf("agent file %s: %w", path, err)
			}
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			return "", err
		}
		if id.drop {
			os.Lchown(filepath.Dir(path), id.uid, id.gid)
			os.Lchown(path, id.uid, id.gid)
		}
	}
	for _, f := range a.SecretFiles {
		if err := s.writeSecret(f, vars); err != nil {
			return "", err
		}
	}
	return base, nil
}

// mergeJSONFile deep-merges the JSON object patch into the file at path (objects merge,
// everything else is replaced) and returns the result.
func mergeJSONFile(path, patch string) (string, error) {
	var add map[string]any
	if err := json.Unmarshal([]byte(patch), &add); err != nil {
		return "", fmt.Errorf("merge: content is not a JSON object: %w", err)
	}
	cur := map[string]any{}
	if b, err := os.ReadFile(path); err == nil && len(bytes.TrimSpace(b)) > 0 {
		if err := json.Unmarshal(b, &cur); err != nil {
			return "", fmt.Errorf("merge: existing file is not a JSON object: %w", err)
		}
	}
	var merge func(dst, src map[string]any)
	merge = func(dst, src map[string]any) {
		for k, v := range src {
			if sm, ok := v.(map[string]any); ok {
				if dm, ok := dst[k].(map[string]any); ok {
					merge(dm, sm)
					continue
				}
			}
			dst[k] = v
		}
	}
	merge(cur, add)
	out, err := json.MarshalIndent(cur, "", "  ")
	return string(out) + "\n", err
}

// writeSecret materialises one secret file for the agent (never logged).
func (s *session) writeSecret(f proto.SecretFile, vars map[string]string) error {
	path, err := render(f.Path, vars)
	if err != nil {
		return fmt.Errorf("secret file path %q: %w", f.Path, err)
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(s.dir, path)
	}
	mode := os.FileMode(0o600)
	if f.Mode != "" {
		if m, err := strconv.ParseUint(f.Mode, 8, 32); err == nil {
			mode = os.FileMode(m)
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if s.id.drop {
		os.Lchown(filepath.Dir(path), s.id.uid, s.id.gid)
	}
	if s.w.holdSecret(path) { // another session on this runner already wrote it (same template, same content)
		s.secretPaths = append(s.secretPaths, path)
		return nil
	}
	os.Remove(path) // never write through a symlink the agent planted
	fh, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		s.w.dropSecret(path)
		return fmt.Errorf("secret file %s: %w", path, err)
	}
	if s.id.drop {
		fh.Chown(s.id.uid, s.id.gid)
	}
	_, err = fh.WriteString(f.Content)
	if cerr := fh.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		s.w.dropSecret(path)
		return fmt.Errorf("secret file %s: %w", path, err)
	}
	s.secretPaths = append(s.secretPaths, path)
	s.logf("runner", "wrote secret file %s", path)
	return nil
}

func (s *session) removeSecrets() {
	for _, p := range s.secretPaths {
		s.w.dropSecret(p)
	}
}

func (s *session) vars(prompt string) map[string]string {
	return map[string]string{
		"Prompt": prompt, "Workdir": s.dir, "Home": s.id.home, "RunID": strconv.FormatInt(s.a.RunID, 10), "TaskSqid": s.a.TaskSqid,
		"SessionName": s.a.SessionName, "SessionUUID": s.a.SessionUUID, "AgentSessionID": s.agentSessionID(), "Branch": s.a.Branch, "Repo": s.a.Repo,
	}
}

func render(text string, vars map[string]string) (string, error) {
	t, err := template.New("").Option("missingkey=error").Parse(text)
	if err != nil {
		return "", err
	}
	var b bytes.Buffer
	if err := t.Execute(&b, vars); err != nil {
		return "", err
	}
	return b.String(), nil
}

// turn runs one agent invocation in a PTY as the agent user. It returns the exit code.
func (w *Worker) turn(ctx context.Context, s *session, argvTmpl []string, n *proto.StackNext, timeout time.Duration) (int, error) {
	if len(argvTmpl) == 0 {
		return 0, errors.New("template has no agent command")
	}
	vars := s.vars(n.Prompt)
	vars["TaskSqid"] = n.Sqid
	argv := make([]string, len(argvTmpl))
	for i, a := range argvTmpl {
		v, err := render(a, vars)
		if err != nil {
			return 0, fmt.Errorf("agent argv %q: %w", a, err)
		}
		argv[i] = v
	}
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = s.dir
	home := s.id.home
	cmd.Env = []string{
		"HOME=" + home, "USER=" + s.id.username, "LOGNAME=" + s.id.username, "TERM=xterm-256color",
		"PATH=" + strings.Join([]string{home + "/.local/share/mise/shims", home + "/.local/bin", filepath.Dir(w.Cfg.MiseBin), "/usr/local/bin", "/usr/bin", "/bin"}, ":"),
		"WRANGL_RUN_ID=" + strconv.FormatInt(s.a.RunID, 10), "WRANGL_TASK=" + n.Sqid,
		"WRANGL_SOCK=" + w.Cfg.Socket(),
	}
	if v := os.Getenv("HTTPS_PROXY"); v != "" {
		cmd.Env = append(cmd.Env, "HTTPS_PROXY="+v, "https_proxy="+v)
	}
	for k, v := range s.a.Agent.Env { // templated, e.g. {{.Home}}/.config/gcp/cred.json
		if rv, err := render(v, vars); err == nil {
			v = rv
		}
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	for k, v := range s.a.Env { // resolved secrets
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	// exec.Command looked argv[0] up in the daemon's PATH; the agent's tools (mise shims)
	// are only on the agent's PATH.
	if !strings.Contains(argv[0], "/") {
		if p, ok := lookPathIn(argv[0], envValue(cmd.Env, "PATH")); ok {
			cmd.Path, cmd.Err = p, nil
		}
	}
	attrs := &syscall.SysProcAttr{Setsid: true, Setctty: true}
	if s.id.drop {
		attrs.Credential = &syscall.Credential{Uid: uint32(s.id.uid), Gid: uint32(s.id.gid)}
	}
	cmd.SysProcAttr = attrs
	// Interactive drive: the Stop hook asks for the next prompt; when there is none the
	// session gets InteractiveEndGrace to exit by itself, then is terminated.
	end := make(chan struct{})
	if s.interactive {
		var once sync.Once
		onStop := s.onStop
		w.mu.Lock()
		rs := w.runs[s.a.RunID]
		if rs == nil { // not started through Start (tests)
			rs = &runState{id: s.a.RunID}
			if w.runs == nil {
				w.runs = map[int64]*runState{}
			}
			w.runs[s.a.RunID] = rs
		}
		rs.continueFn = func(hctx context.Context) (string, bool) {
			if onStop != nil {
				if p, ok := onStop(hctx); ok {
					return p, true
				}
			}
			time.AfterFunc(InteractiveEndGrace, func() { once.Do(func() { close(end) }) })
			return "", false
		}
		w.mu.Unlock()
		defer func() { w.mu.Lock(); rs.continueFn = nil; w.mu.Unlock() }()
	}
	rules := make([]expectRule, 0, len(s.a.Agent.Expect))
	for _, e := range s.a.Agent.Expect {
		if re, err := regexp.Compile(e.Match); err == nil {
			rules = append(rules, expectRule{re: re, send: e.Send, repeat: e.Repeat})
		}
	}
	size := pty.Winsize{Rows: 50, Cols: 200}
	f, err := pty.StartWithSize(cmd, &size)
	if err != nil {
		return 0, fmt.Errorf("start agent %s: %w", argv[0], err)
	}
	defer f.Close()
	defer w.addRunTerm(s.a.RunID, f, size)()

	tailCtx, stopTail := context.WithCancel(ctx)
	tailDone := s.startTranscriptTail(tailCtx)
	defer func() { stopTail(); <-tailDone }()

	ptySubj := natsd.RunPTYSubject(w.Cfg.RunnerID, s.a.RunID)
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		buf := make([]byte, 32<<10)
		var partial []byte
		var screen string // ANSI-stripped recent output, for expect rules
		for {
			nr, err := f.Read(buf)
			if nr > 0 {
				chunk := append([]byte(nil), buf[:nr]...)
				w.JS.PublishAsync(ptySubj, chunk)
				if len(rules) > 0 {
					screen += ansi.ReplaceAllString(string(chunk), "")
					if len(screen) > 8<<10 {
						screen = screen[len(screen)-(8<<10):]
					}
					for i := range rules {
						r := &rules[i]
						if r.done || !r.re.MatchString(screen) {
							continue
						}
						s.logf("runner", "expect %q matched; answering", r.re.String())
						f.WriteString(r.send)
						screen, r.done = "", !r.repeat
						break
					}
				}
				partial = append(partial, chunk...)
				for {
					i := bytes.IndexByte(partial, '\n')
					if i < 0 {
						break
					}
					s.line("agent", string(partial[:i]))
					partial = partial[i+1:]
				}
			}
			if err != nil { // EIO once the child exits
				if len(partial) > 0 {
					s.line("agent", string(partial))
				}
				return
			}
		}
	}()

	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()
	var werr error
	select {
	case werr = <-waitErr:
	case <-end: // interactive session told to stop but still open
		s.logf("runner", "ending the interactive session")
		syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		select {
		case <-waitErr:
		case <-time.After(killGrace):
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-waitErr
		}
		<-readDone
		return 0, nil
	case <-tctx.Done():
		reason := "cancelled"
		if ctx.Err() == nil {
			reason = fmt.Sprintf("turn exceeded %s", timeout)
		}
		s.logf("runner", "stopping agent: %s", reason)
		syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		select {
		case werr = <-waitErr:
		case <-time.After(killGrace):
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			werr = <-waitErr
		}
		<-readDone
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return 0, errors.New(reason)
	}
	<-readDone
	var ee *exec.ExitError
	if errors.As(werr, &ee) {
		return ee.ExitCode(), nil
	}
	if werr != nil {
		return 0, werr
	}
	return 0, nil
}

// checkpoint commits anything the agent left uncommitted, as the agent.
func (w *Worker) checkpoint(ctx context.Context, s *session, n *proto.StackNext) error {
	if err := w.checkpointCommit(ctx, s, n); err != nil {
		return err
	}
	w.reportCheckpoint(ctx, s, n)
	return nil
}

func (w *Worker) checkpointCommit(ctx context.Context, s *session, n *proto.StackNext) error {
	if out, err := gitAsAgent(ctx, s.id, s.dir, "add", "-A"); err != nil {
		return fmt.Errorf("git add: %w: %s", err, out)
	}
	if _, err := gitAsAgent(ctx, s.id, s.dir, "diff", "--cached", "--quiet"); err == nil {
		return nil // nothing staged
	}
	msg := fmt.Sprintf("[%s] %s", n.Sqid, n.Title)
	if out, err := gitAsAgent(ctx, s.id, s.dir, "commit", "-q", "-m", msg); err != nil {
		return fmt.Errorf("checkpoint commit: %w: %s", err, out)
	}
	s.logf("runner", "checkpoint commit: %s", msg)
	return nil
}

// reportCheckpoint pushes the run branch and tells the controller which commit the task ended on
// (whether the runner or the agent made it), so a done task can link to its changes. Best effort: a failure is logged,
// never fatal to the run.
func (w *Worker) reportCheckpoint(ctx context.Context, s *session, n *proto.StackNext) {
	if s.a.Kind == "review" || s.a.Kind == "plan" {
		return // read-only runs never push, so a link to their HEAD would 404
	}
	out, err := gitAsAgent(ctx, s.id, s.dir, "rev-parse", "HEAD")
	sha := strings.TrimSpace(out)
	if err != nil || sha == "" {
		s.logf("runner", "checkpoint: no HEAD to report for %s", n.Sqid)
		return
	}
	// Push the run branch now so the commit exists on GitHub for the "what changed" link while
	// the run is still going (and the work is backed up). The final push in finishRun repeats it.
	if out, err := w.pushBranch(ctx, s.id, s.dir, s.a.GitToken, s.a.Branch); err != nil {
		s.logf("runner", "checkpoint: push %s: %v: %s", s.a.Branch, err, strings.TrimSpace(out))
		return // not on GitHub, so a "what changed" link to it would 404
	}
	if _, err := w.stack(ctx, "checkpoint", proto.StackRequest{RunID: s.a.RunID, Sqid: n.Sqid, Commit: sha}); err != nil {
		s.logf("runner", "checkpoint: report %s: %v", n.Sqid, err)
	}
}

// Inspect answers cmd.diff / cmd.tree / cmd.file for a run's workspace.
func (w *Worker) Inspect(ctx context.Context, kind string, ref proto.RunRef) proto.InspectReply {
	dir, id, err := w.workdir(ref.RunID)
	if err != nil {
		return proto.InspectReply{Error: err.Error()}
	}
	if _, err := os.Stat(dir); err != nil {
		return proto.InspectReply{Error: fmt.Sprintf("no workspace for run %d on this runner", ref.RunID)}
	}
	clip := func(s string) proto.InspectReply {
		if len(s) > proto.InspectLimit {
			return proto.InspectReply{Data: s[:proto.InspectLimit], Truncated: true}
		}
		return proto.InspectReply{Data: s}
	}
	switch kind {
	case "diff":
		base, _ := os.ReadFile(filepath.Join(gitDir(dir), "wrangl-base"))
		args := []string{"diff", strings.TrimSpace(string(base))}
		if ref.Stat {
			args = append(args, "--stat")
		}
		gitAsAgent(ctx, id, dir, "add", "-N", ".") // include new, untracked files
		out, err := gitAsAgent(ctx, id, dir, args...)
		if err != nil {
			return proto.InspectReply{Error: out}
		}
		return clip(out)
	case "tree", "file":
		root, err := os.OpenRoot(dir) // confines paths to the workspace (no ../, no escaping symlinks)
		if err != nil {
			return proto.InspectReply{Error: err.Error()}
		}
		defer root.Close()
		p := filepath.Clean("/" + ref.Path)[1:]
		if p == "" {
			p = "."
		}
		if kind == "file" {
			f, err := root.Open(p)
			if err != nil {
				return proto.InspectReply{Error: err.Error()}
			}
			defer f.Close()
			b, err := io.ReadAll(io.LimitReader(f, proto.InspectLimit+1))
			if err != nil {
				return proto.InspectReply{Error: err.Error()}
			}
			return clip(string(b))
		}
		d, err := root.Open(p)
		if err != nil {
			return proto.InspectReply{Error: err.Error()}
		}
		defer d.Close()
		ents, err := d.ReadDir(-1)
		if err != nil {
			return proto.InspectReply{Error: err.Error()}
		}
		var b strings.Builder
		for _, e := range ents {
			if e.Name() == ".git" {
				continue
			}
			name := e.Name()
			if e.IsDir() {
				name += "/"
			}
			b.WriteString(name + "\n")
		}
		return clip(b.String())
	}
	return proto.InspectReply{Error: "unknown inspect kind " + kind}
}

var errFailRun = errors.New("owner chose to fail the run")

type expectRule struct {
	re     *regexp.Regexp
	send   string
	repeat bool
	done   bool
}

func finished(state string) bool {
	return state == "in_review" || state == "done" || state == "cancelled"
}

// chain is the interactive session's position in the work stack.
type chain struct {
	mu     sync.Mutex
	cur    *proto.StackNext
	extra  int    // tasks taken on within the session after the first
	nudged string // sqid already nudged in this session
}

// chainNext runs at the session's Stop hook: checkpoint the task, nudge once if it was not
// completed, else hand the agent the next ready task in the same session.
func (w *Worker) chainNext(ctx context.Context, s *session, ch *chain) (string, bool) {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	n := ch.cur
	if err := w.checkpoint(ctx, s, n); err != nil {
		s.logf("runner", "stop hook: %v", err)
		return "", false
	}
	if s.a.Agent.Completion == "exit" {
		if _, err := w.stack(ctx, "complete", proto.StackRequest{RunID: s.a.RunID, Sqid: n.Sqid, Summary: s.summary()}); err != nil {
			s.logf("runner", "stop hook: complete %s: %v", n.Sqid, err)
			return "", false
		}
	}
	st, err := w.leafState(ctx, s.a.RunID, n.Sqid)
	if err != nil {
		s.logf("runner", "stop hook: %v", err)
		return "", false
	}
	if !finished(st) {
		if ch.nudged == n.Sqid {
			return "", false // the run loop asks the owner
		}
		ch.nudged = n.Sqid
		s.logf("runner", "agent stopped without completing %s; nudging in the session", n.Sqid)
		return nudgePrompt(n), true
	}
	r, err := w.stack(ctx, "next", proto.StackRequest{RunID: s.a.RunID})
	if err != nil || r.Next == nil || r.Next.Done || r.Next.Blocked {
		return "", false
	}
	ch.cur, ch.extra = r.Next, ch.extra+1
	s.logf("runner", "==> %s %s (same session)", r.Next.Sqid, r.Next.Title)
	s.state(context.WithoutCancel(ctx), proto.RunState{State: "running", Status: r.Next.Sqid + " " + r.Next.Title})
	s.resetSummary()
	return r.Next.Prompt, true
}

// nudgePrompt is sent once when the agent ends a turn without completing its task.
func nudgePrompt(n *proto.StackNext) string {
	return fmt.Sprintf("You ended your turn without calling the task_complete tool for task %s (%q). "+
		"If the task is finished, call task_complete with sqid %q and a short summary now. "+
		"Otherwise, continue working on it until it is done. "+
		"If you are blocked on a decision only the owner can make, do not end your turn: call the ask_question tool "+
		"with your question and, where you can, options. The owner's answer comes back to you in this session.", n.Sqid, n.Title, n.Sqid)
}

// ownerReplyPrompt resumes the agent with what the owner typed in answer to the "what now?" card.
func ownerReplyPrompt(n *proto.StackNext, text string) string {
	return fmt.Sprintf("The owner replied about task %s (%q):\n\n%s\n\nContinue the task with this in mind. "+
		"Call task_complete when it is finished, or ask_question if you need a further decision.", n.Sqid, n.Title, strings.TrimSpace(text))
}

// stoppedDetail is the Markdown body of the "what now?" question: the agent's own last message,
// rendered as Markdown (falling back to the raw output lines for agents without a Stop message).
func stoppedDetail(msg, rawTail string) string {
	if strings.TrimSpace(msg) == "" {
		return "Last output:\n\n```\n" + rawTail + "\n```"
	}
	if len(msg) > 6000 {
		msg = msg[:6000] + "\n\n(truncated)"
	}
	return "**The agent's last message:**\n\n" + msg
}

// leafState asks the controller for a task's state.
func (w *Worker) leafState(ctx context.Context, runID int64, sqid string) (string, error) {
	args, _ := json.Marshal(map[string]string{"sqid": sqid})
	out, err := w.ToolFor(ctx, runID, "task_get", args)
	if err != nil {
		return "", err
	}
	var t struct {
		State string `json:"state"`
	}
	json.Unmarshal(out, &t)
	return t.State, nil
}

// ensureComplete makes sure the leaf was completed. With completion "exit" the runner
// completes it; with "tool" (default) the agent must have called task_complete: nudge
// once, then ask the owner (retry / mark complete / fail).
func (w *Worker) ensureComplete(ctx context.Context, s *session, n *proto.StackNext, turns *int, timeout time.Duration) error {
	if s.a.Agent.Completion == "exit" {
		if st, err := w.leafState(ctx, s.a.RunID, n.Sqid); err == nil && finished(st) {
			return nil // an interactive session's Stop already completed it
		}
		_, err := w.stack(ctx, "complete", proto.StackRequest{RunID: s.a.RunID, Sqid: n.Sqid, Summary: s.summary()})
		return err
	}
	resume := s.resumeArgv()
	nudged := false
	for {
		st, err := w.leafState(ctx, s.a.RunID, n.Sqid)
		if err != nil {
			return err
		}
		if finished(st) {
			return nil
		}
		if !nudged {
			nudged = true
			s.logf("runner", "agent did not call task_complete for %s; nudging once", n.Sqid)
			nn := *n
			nn.Prompt = nudgePrompt(n)
			code, err := w.turn(ctx, s, resume, &nn, timeout)
			*turns++
			if err != nil {
				return err
			}
			if code != 0 {
				return fmt.Errorf("agent exited %d on the nudge for %s", code, n.Sqid)
			}
			if err := w.checkpoint(ctx, s, n); err != nil {
				return err
			}
			continue
		}
		s.logf("runner", "agent still has not completed %s; asking the owner", n.Sqid)
		args, _ := json.Marshal(map[string]any{
			"prompt":          fmt.Sprintf("The agent stopped twice without completing %s %q. What now?", n.Sqid, n.Title),
			"detail_md":       stoppedDetail(s.lastMessage(), s.summary()),
			"allow_free_text": true, // type a reply and the agent resumes with it
			"options": []map[string]string{
				{"label": "Retry the task", "value": "retry"},
				{"label": "Mark it complete", "value": "complete"},
				{"label": "Fail the run", "value": "fail"},
			},
		})
		out, err := w.ToolFor(ctx, s.a.RunID, "_system_question", args)
		if err != nil {
			return err
		}
		var ans struct {
			Answer string `json:"answer"`
			Text   string `json:"text"`
		}
		json.Unmarshal(out, &ans)
		switch ans.Answer {
		case "complete":
			_, err := w.stack(ctx, "complete", proto.StackRequest{RunID: s.a.RunID, Sqid: n.Sqid, Summary: "marked complete by the owner"})
			return err
		case "fail":
			return errFailRun
		default: // retry: a fresh turn on the same task (with the owner's reply, if any), then the nudge rule again
			retry := n
			if strings.TrimSpace(ans.Text) != "" {
				nn := *n
				nn.Prompt = ownerReplyPrompt(n, ans.Text)
				retry = &nn
			}
			code, err := w.turn(ctx, s, resume, retry, timeout)
			*turns++
			if err != nil {
				return err
			}
			if code != 0 {
				return fmt.Errorf("agent exited %d retrying %s", code, n.Sqid)
			}
			if err := w.checkpoint(ctx, s, n); err != nil {
				return err
			}
			nudged = false
		}
	}
}

// envValue returns key's value in a KEY=VALUE list ("" if absent).
func envValue(env []string, key string) string {
	for i := len(env) - 1; i >= 0; i-- {
		if v, ok := strings.CutPrefix(env[i], key+"="); ok {
			return v
		}
	}
	return ""
}

// lookPathIn finds an executable file called name in the given PATH-style list.
func lookPathIn(name, path string) (string, bool) {
	for _, d := range filepath.SplitList(path) {
		p := filepath.Join(d, name)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return p, true
		}
	}
	return "", false
}

// pushBranch pushes HEAD to the run branch. An agent that opened its own PR with gh has
// usually pushed the branch already, so a rejected push is not a failure: if the remote
// already contains HEAD there is nothing to push; if it holds commits we lack, replace it
// only if it is still what we just fetched (force-with-lease), since wrangl owns the branch.
func (w *Worker) pushBranch(ctx context.Context, id agentIdentity, dir, token, branch string) (string, error) {
	ref := "HEAD:refs/heads/" + branch
	out, err := gitAsDaemon(ctx, dir, token, "push", "origin", ref)
	if err == nil {
		return out, nil
	}
	if _, ferr := gitAsDaemon(ctx, dir, token, "fetch", "origin", "refs/heads/"+branch); ferr != nil {
		return out, err
	}
	tip, terr := gitAsAgent(ctx, id, dir, "rev-parse", "FETCH_HEAD")
	if terr != nil || tip == "" {
		return out, err
	}
	if _, aerr := gitAsAgent(ctx, id, dir, "merge-base", "--is-ancestor", "HEAD", tip); aerr == nil {
		return "remote already has HEAD", nil // the agent pushed it
	}
	return gitAsDaemon(ctx, dir, token, "push", "--force-with-lease=refs/heads/"+branch+":"+tip, "origin", ref)
}
