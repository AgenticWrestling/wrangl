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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"text/template"
	"time"

	"github.com/creack/pty"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/nreid/devhost/cmd/wrangl/internal/controller/natsd"
	"github.com/nreid/devhost/cmd/wrangl/internal/proto"
)

// Defaults for agent turns.
const (
	DefaultTurnTimeout = 2 * time.Hour
	killGrace          = 15 * time.Second
)

// Worker runs assignments on this runner, one at a time.
type Worker struct {
	Cfg *Config
	NC  *nats.Conn
	JS  jetstream.JetStream
	Log *slog.Logger

	mu         sync.Mutex
	runID      int64
	kind       string
	cancel     context.CancelFunc
	done       chan struct{}
	hookSeq    int64
	answers    waiters
	continueFn func(ctx context.Context) (string, bool) // interactive drive (M9)
}

// Kind is the kind of the run in progress.
func (w *Worker) Kind() string { w.mu.Lock(); defer w.mu.Unlock(); return w.kind }

// DeliverAnswer hands an owner's answer to the agent waiting on it.
func (w *Worker) DeliverAnswer(a proto.Answer) bool { return w.answers.deliver(a) }

// Busy reports the run in progress (0 = idle).
func (w *Worker) Busy() int64 { w.mu.Lock(); defer w.mu.Unlock(); return w.runID }

// Start begins an assignment in the background; it fails if one is already running.
func (w *Worker) Start(parent context.Context, a proto.Assignment) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.runID != 0 {
		return fmt.Errorf("busy with run %d", w.runID)
	}
	ctx, cancel := context.WithCancel(parent)
	w.runID, w.kind, w.cancel, w.done, w.hookSeq = a.RunID, a.Kind, cancel, make(chan struct{}), 0
	done := w.done
	go func() {
		defer close(done)
		defer cancel()
		w.run(ctx, a)
		w.answers.cancelAll()
		w.mu.Lock()
		w.runID, w.kind, w.cancel, w.continueFn = 0, "", nil, nil
		w.mu.Unlock()
	}()
	return nil
}

// Cancel stops run runID if it is the one in progress.
func (w *Worker) Cancel(runID int64) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.runID != runID || w.cancel == nil {
		return false
	}
	w.cancel()
	return true
}

// Wait blocks until the current run (if any) has finished.
func (w *Worker) Wait() {
	w.mu.Lock()
	d := w.done
	w.mu.Unlock()
	if d != nil {
		<-d
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
}

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
		s.lines = append(s.lines, msg)
		if len(s.lines) > 50 {
			s.lines = s.lines[len(s.lines)-50:]
		}
	}
	s.mu.Unlock()
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
func (w *Worker) run(ctx context.Context, a proto.Assignment) {
	// Final states must be published even after a cancel.
	pubCtx := context.WithoutCancel(ctx)
	dir, id, err := w.workdir(a.RunID)
	s := &session{w: w, a: a, id: id, dir: dir}
	fail := func(err error) {
		s.logf("runner", "run failed: %v", err)
		s.flush()
		st := proto.RunState{State: "failed", Error: err.Error()}
		if ctx.Err() != nil {
			st = proto.RunState{State: "cancelled", Error: "cancelled"}
		}
		s.state(pubCtx, st)
	}
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
	s.state(pubCtx, proto.RunState{State: "running", BaseSHA: base})

	turnTimeout := DefaultTurnTimeout
	if a.Agent.TurnTimeout != "" {
		if d, err := time.ParseDuration(a.Agent.TurnTimeout); err == nil && d > 0 {
			turnTimeout = d
		}
	}
	turns := 0
	blocked := false
	for {
		r, err := w.stack(ctx, "next", proto.StackRequest{RunID: a.RunID})
		if err != nil {
			fail(fmt.Errorf("ask controller for next task: %w", err))
			return
		}
		n := r.Next
		if n == nil || n.Done || n.Blocked {
			blocked = n != nil && n.Blocked
			break
		}
		s.logf("runner", "==> %s %s", n.Sqid, n.Title)
		s.state(pubCtx, proto.RunState{State: "running", Status: n.Sqid + " " + n.Title})
		argv := a.Agent.Start
		if turns > 0 && len(a.Agent.Resume) > 0 {
			argv = a.Agent.Resume
		}
		s.resetSummary()
		code, err := w.turn(ctx, s, argv, n, turnTimeout)
		turns++
		if err != nil {
			fail(fmt.Errorf("task %s: %w", n.Sqid, err))
			return
		}
		if code != 0 {
			c := code
			s.flush()
			s.state(pubCtx, proto.RunState{State: "failed", ExitCode: &c, Error: fmt.Sprintf("agent exited %d on task %s", code, n.Sqid)})
			return
		}
		if err := w.checkpoint(ctx, s, n); err != nil {
			fail(err)
			return
		}
		if err := w.ensureComplete(ctx, s, n, &turns, turnTimeout); err != nil {
			if errors.Is(err, errFailRun) {
				s.flush()
				s.state(pubCtx, proto.RunState{State: "failed", Error: "the owner chose to fail the run: " + n.Sqid + " was not completed"})
				return
			}
			fail(err)
			return
		}
	}

	head, _ := gitAsAgent(ctx, id, dir, "rev-parse", "HEAD")
	ahead, _ := gitAsAgent(ctx, id, dir, "rev-list", "--count", base+"..HEAD")
	pushed := false
	if ahead != "" && ahead != "0" {
		s.state(pubCtx, proto.RunState{State: "pushing", Status: fmt.Sprintf("pushing %s commits to %s", ahead, a.Branch)})
		if out, err := gitAsDaemon(ctx, dir, a.GitToken, "push", "origin", "HEAD:refs/heads/"+a.Branch); err != nil {
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
	os.RemoveAll(dir) // a retried run starts clean
	s.logf("runner", "cloning %s (%s)", a.Repo, a.BaseBranch)
	if out, err := gitAsDaemon(ctx, filepath.Dir(dir), a.GitToken, "clone", "--branch", a.BaseBranch, a.CloneURL, dir); err != nil {
		return "", fmt.Errorf("clone: %w: %s", err, out)
	}
	if err := chownTree(dir, id); err != nil {
		return "", err
	}
	for _, args := range [][]string{
		{"checkout", "-b", a.Branch},
		{"config", "user.name", "wrangl agent (" + a.SessionName + ")"},
		{"config", "user.email", "agent@wrangl.invalid"},
	} {
		if out, err := gitAsAgent(ctx, id, dir, args...); err != nil {
			return "", fmt.Errorf("git %s: %w: %s", args[0], err, out)
		}
	}
	base, err := gitAsAgent(ctx, id, dir, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	os.WriteFile(filepath.Join(dir, ".git", "wrangl-base"), []byte(base+"\n"), 0o644)
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
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			return "", err
		}
		if id.drop {
			os.Lchown(filepath.Dir(path), id.uid, id.gid)
			os.Lchown(path, id.uid, id.gid)
		}
	}
	return base, nil
}

func (s *session) vars(prompt string) map[string]string {
	return map[string]string{
		"Prompt": prompt, "Workdir": s.dir, "RunID": strconv.FormatInt(s.a.RunID, 10), "TaskSqid": s.a.TaskSqid,
		"SessionName": s.a.SessionName, "SessionUUID": s.a.SessionUUID, "Branch": s.a.Branch, "Repo": s.a.Repo,
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
	for k, v := range s.a.Agent.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	for k, v := range s.a.Env { // resolved secrets
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	attrs := &syscall.SysProcAttr{Setsid: true, Setctty: true}
	if s.id.drop {
		attrs.Credential = &syscall.Credential{Uid: uint32(s.id.uid), Gid: uint32(s.id.gid)}
	}
	cmd.SysProcAttr = attrs
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 50, Cols: 200})
	if err != nil {
		return 0, fmt.Errorf("start agent %s: %w", argv[0], err)
	}
	defer f.Close()

	ptySubj := natsd.RunPTYSubject(w.Cfg.RunnerID, s.a.RunID)
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		buf := make([]byte, 32<<10)
		var partial []byte
		for {
			nr, err := f.Read(buf)
			if nr > 0 {
				chunk := append([]byte(nil), buf[:nr]...)
				w.JS.PublishAsync(ptySubj, chunk)
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
		base, _ := os.ReadFile(filepath.Join(dir, ".git", "wrangl-base"))
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

// nudgePrompt is sent once when the agent ends a turn without completing its task.
func nudgePrompt(n *proto.StackNext) string {
	return fmt.Sprintf("You ended your turn without calling the task_complete tool for task %s (%q). "+
		"If the task is finished, call task_complete with sqid %q and a short summary now. "+
		"Otherwise, continue working on it until it is done.", n.Sqid, n.Title, n.Sqid)
}

// leafState asks the controller for a task's state.
func (w *Worker) leafState(ctx context.Context, sqid string) (string, error) {
	args, _ := json.Marshal(map[string]string{"sqid": sqid})
	out, err := w.Tool(ctx, "task_get", args)
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
		_, err := w.stack(ctx, "complete", proto.StackRequest{RunID: s.a.RunID, Sqid: n.Sqid, Summary: s.summary()})
		return err
	}
	resume := s.a.Agent.Resume
	if len(resume) == 0 {
		resume = s.a.Agent.Start
	}
	nudged := false
	for {
		st, err := w.leafState(ctx, n.Sqid)
		if err != nil {
			return err
		}
		if st == "in_review" || st == "done" || st == "cancelled" {
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
			"prompt":    fmt.Sprintf("The agent stopped twice without completing %s %q. What now?", n.Sqid, n.Title),
			"detail_md": "Last output:\n\n```\n" + s.summary() + "\n```",
			"options": []map[string]string{
				{"label": "Retry the task", "value": "retry"},
				{"label": "Mark it complete", "value": "complete"},
				{"label": "Fail the run", "value": "fail"},
			},
		})
		out, err := w.Tool(ctx, "_system_question", args)
		if err != nil {
			return err
		}
		var ans struct {
			Answer string `json:"answer"`
		}
		json.Unmarshal(out, &ans)
		switch ans.Answer {
		case "complete":
			_, err := w.stack(ctx, "complete", proto.StackRequest{RunID: s.a.RunID, Sqid: n.Sqid, Summary: "marked complete by the owner"})
			return err
		case "fail":
			return errFailRun
		default: // retry: a fresh turn on the same task, then the nudge rule again
			code, err := w.turn(ctx, s, resume, n, timeout)
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
