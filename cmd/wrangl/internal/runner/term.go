package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/nats-io/nats.go"

	"github.com/nreid/devhost/cmd/wrangl/internal/controller/natsd"
	"github.com/nreid/devhost/cmd/wrangl/internal/proto"
)

// termSession is one PTY the owner can watch (and, once they take control, type into).
type termSession struct {
	info proto.TermInfo
	f    *os.File
	cmd  *exec.Cmd // shells only
}

// terms is the runner's table of terminal sessions: the run in progress and any shells.
type terms struct {
	mu sync.Mutex
	m  map[string]*termSession
}

func (t *terms) add(s *termSession) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.m == nil {
		t.m = map[string]*termSession{}
	}
	t.m[s.info.ID] = s
}

func (t *terms) remove(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.m, id)
}

func (t *terms) get(id string) *termSession {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.m[id]
}

// addRun registers the run's PTY; the returned func unregisters it.
func (w *Worker) addRunTerm(runID int64, f *os.File, size pty.Winsize) func() {
	id := "run-" + strconv.FormatInt(runID, 10)
	w.terms.add(&termSession{f: f, info: proto.TermInfo{ID: id, Kind: "run", Title: fmt.Sprintf("run #%d", runID), Cols: size.Cols, Rows: size.Rows}})
	return func() { w.terms.remove(id) }
}

// TermList reports the live terminal sessions, runs first.
func (w *Worker) TermList() []proto.TermInfo {
	w.terms.mu.Lock()
	out := make([]proto.TermInfo, 0, len(w.terms.m))
	for _, s := range w.terms.m {
		out = append(out, s.info)
	}
	w.terms.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind == "run"
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// TermInput types into a session (the controller only forwards it while the owner is
// in control).
func (w *Worker) TermInput(id string, data []byte) error {
	s := w.terms.get(id)
	if s == nil {
		return fmt.Errorf("no terminal %s", id)
	}
	_, err := s.f.Write(data)
	return err
}

// TermResize sets a session's PTY size.
func (w *Worker) TermResize(id string, sz proto.TermSize) error {
	s := w.terms.get(id)
	if s == nil {
		return fmt.Errorf("no terminal %s", id)
	}
	if sz.Cols < 20 || sz.Cols > 500 || sz.Rows < 5 || sz.Rows > 300 {
		return fmt.Errorf("size %dx%d out of range", sz.Cols, sz.Rows)
	}
	if err := pty.Setsize(s.f, &pty.Winsize{Cols: sz.Cols, Rows: sz.Rows}); err != nil {
		return err
	}
	w.terms.mu.Lock()
	s.info.Cols, s.info.Rows = sz.Cols, sz.Rows
	w.terms.mu.Unlock()
	return nil
}

// shellTools are installed via mise the first time a shell opens on a runner, so basic
// runners that never open one don't pay for them. They live in conf.d, which Apply leaves
// alone (it owns config.toml).
const shellTools = `[tools]
bat = "latest"
lsd = "latest"
dust = "latest"
duf = "latest"
fd = "latest"
ripgrep = "latest"
tealdeer = "latest"
`

const shellPrelude = `
marker="$HOME/.local/state/wrangl/shell-tools-v1"
if [ ! -e "$marker" ] && [ -x "$WRANGL_MISE" ]; then
  printf '\033[1mSetting up shell environment…\033[0m (bat lsd dust duf fd ripgrep tldr; first shell only)\r\n'
  mkdir -p "$HOME/.config/mise/conf.d" "$HOME/.local/state/wrangl"
  cat > "$HOME/.config/mise/conf.d/wrangl-shell.toml" <<'EOF'
` + shellTools + `EOF
  if "$WRANGL_MISE" install --yes; then
    touch "$marker"
    printf '\033[32mShell environment ready.\033[0m\r\n'
  else
    printf '\033[31mShell tool install failed; continuing without it (it retries on the next shell).\033[0m\r\n'
  fi
fi
exec "$1" -l
`

// shellMise is the mise the first-shell prelude installs tools with; empty on the host
// runner, whose user's own tool setup is left alone.
func (w *Worker) shellMise() string {
	if w.Cfg.Host {
		return ""
	}
	return w.Cfg.MiseBin
}

// OpenShell starts a login shell as the agent user and streams its PTY like a run's.
func (w *Worker) OpenShell(ctx context.Context) (string, error) {
	id, err := (&Applier{Cfg: w.Cfg}).agent()
	if err != nil {
		return "", err
	}
	// fish when the runner has it (provisioning installs it best-effort), else bash, else sh.
	shell := "/bin/sh"
	for _, c := range []string{"/usr/bin/fish", "/usr/local/bin/fish", "/bin/bash"} {
		if _, err := os.Stat(c); err == nil {
			shell = c
			break
		}
	}
	n := time.Now().UnixMilli() // unique across daemon restarts: old output stays on the stream
	tid := "sh-" + strconv.FormatInt(n, 10)
	// A /bin/sh prelude installs the optional shell tools the first time any shell opens
	// (output streams into the terminal), then execs the login shell.
	cmd := exec.Command("/bin/sh", "-c", shellPrelude, "wrangl-shell", shell)
	cmd.Dir = id.home
	cmd.Env = []string{
		"HOME=" + id.home, "USER=" + id.username, "LOGNAME=" + id.username, "TERM=xterm-256color", "SHELL=" + shell,
		"PATH=" + strings.Join([]string{id.home + "/.local/share/mise/shims", id.home + "/.local/bin", filepath.Dir(w.Cfg.MiseBin), "/usr/local/bin", "/usr/bin", "/bin"}, ":"),
		"WRANGL_SOCK=" + w.Cfg.Socket(), "WRANGL_MISE=" + w.shellMise(),
	}
	if filepath.Base(shell) == "fish" {
		// Nobody is attached to answer fish's terminal queries when the shell starts, and it
		// would wait 10s for them.
		cmd.Env = append(cmd.Env, "fish_features=no-query-term")
	}
	if v := os.Getenv("HTTPS_PROXY"); v != "" {
		cmd.Env = append(cmd.Env, "HTTPS_PROXY="+v, "https_proxy="+v)
	}
	attrs := &syscall.SysProcAttr{Setsid: true, Setctty: true}
	if id.drop {
		attrs.Credential = &syscall.Credential{Uid: uint32(id.uid), Gid: uint32(id.gid)}
	}
	cmd.SysProcAttr = attrs
	size := pty.Winsize{Rows: 40, Cols: 120}
	f, err := pty.StartWithSize(cmd, &size)
	if err != nil {
		return "", fmt.Errorf("start shell: %w", err)
	}
	s := &termSession{f: f, cmd: cmd, info: proto.TermInfo{ID: tid, Kind: "shell", Title: "shell", Cols: size.Cols, Rows: size.Rows}}
	w.terms.add(s)
	subj := natsd.TermPTYSubject(w.Cfg.RunnerID, tid)
	go func() {
		defer f.Close()
		buf := make([]byte, 32<<10)
		for {
			nr, err := f.Read(buf)
			if nr > 0 {
				w.JS.PublishAsync(subj, append([]byte(nil), buf[:nr]...))
			}
			if err != nil {
				break
			}
		}
		cmd.Wait()
		w.terms.remove(tid)
		msg := nats.NewMsg(subj)
		msg.Header.Set(proto.TermEndHeader, "1")
		w.JS.PublishMsgAsync(msg)
	}()
	return tid, nil
}

// CloseTerm ends a shell. Runs end when their agent does.
func (w *Worker) CloseTerm(id string) error {
	s := w.terms.get(id)
	if s == nil {
		return fmt.Errorf("no terminal %s", id)
	}
	if s.cmd == nil {
		return errors.New("a run's terminal closes when the run ends")
	}
	syscall.Kill(-s.cmd.Process.Pid, syscall.SIGHUP)
	time.AfterFunc(5*time.Second, func() {
		if w.terms.get(id) != nil {
			syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
		}
	})
	return nil
}
