package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

// Remote runs (docs/wrangl/adr/0002). The agent runs here, on the driver, with the repo checked out
// locally. The target is a machine without a wrangl binary; this daemon holds the credential and
// the tunnel and offers the agent two tools, target_exec and target_put. The private key never
// reaches the agent's process: it sits in a daemon-owned 0700 directory and is used only by the
// ssh/scp/adb processes the daemon starts.

// Tunnel is an open path to the target: a loopback address the protocol client connects to.
type Tunnel interface {
	Addr() (host string, port int)
	Close() error
}

// TargetBackend is what the daemon needs to reach targets. Tests substitute FakeTarget.
type TargetBackend interface {
	// OpenTunnel opens (or joins) the path to the target; the caller closes it when done.
	OpenTunnel(ctx context.Context, t proto.TargetConn) (Tunnel, error)
	// Exec runs cmd on the target and returns its combined output and exit code.
	Exec(ctx context.Context, ep Endpoint, cmd string) (ExecResult, error)
	// Put copies a local file or directory to dest on the target.
	Put(ctx context.Context, ep Endpoint, local, dest string) error
}

// Endpoint is a target as a client sees it: the tunnel's address plus the credential.
type Endpoint struct {
	Conn    proto.TargetConn
	Host    string
	Port    int
	KeyFile string // daemon-owned
	Known   string // pinned host keys file, kept across runs
}

// ExecResult is a finished remote command.
type ExecResult struct {
	Output   string `json:"output"`
	ExitCode int    `json:"exit_code"`
	TimedOut bool   `json:"timed_out,omitempty"`
}

// ErrTargetLost is returned when the target or its tunnel cannot be reached.
var ErrTargetLost = errors.New("target lost: the remote machine is unreachable")

const (
	maxExecOutput  = 64 << 10
	defaultExecSec = 600
	maxExecSec     = 3600
)

// runTarget is one run's connection to its target.
type runTarget struct {
	ep     Endpoint
	tunnel Tunnel
	dir    string
	be     TargetBackend
	local  string // the run's worktree: target_put paths are confined to it
}

func (w *Worker) backend() TargetBackend {
	if w.Targets != nil {
		return w.Targets
	}
	return &realTargets{}
}

// openTarget connects a run to its target: writes the key where only the daemon can read it, opens
// the tunnel and pins host keys per target across runs.
func (w *Worker) openTarget(ctx context.Context, s *session) (*runTarget, error) {
	t := *s.a.Target
	dir, err := os.MkdirTemp("", "wrangl-target-")
	if err != nil {
		return nil, err
	}
	rt := &runTarget{dir: dir, be: w.backend(), local: s.dir}
	fail := func(err error) (*runTarget, error) { rt.close(); return nil, err }
	rt.ep = Endpoint{Conn: t}
	if t.Key != "" {
		rt.ep.KeyFile = filepath.Join(dir, "key")
		if err := os.WriteFile(rt.ep.KeyFile, []byte(ensureNL(t.Key)), 0o600); err != nil {
			return fail(err)
		}
	}
	state := w.Cfg.StateDir
	if state == "" {
		state = os.TempDir()
	}
	kd := filepath.Join(state, "targets", t.Name)
	if err := os.MkdirAll(kd, 0o700); err != nil {
		return fail(err)
	}
	rt.ep.Known = filepath.Join(kd, "known_hosts")
	tun, err := rt.be.OpenTunnel(ctx, t)
	if err != nil {
		return fail(fmt.Errorf("%w: %v", ErrTargetLost, err))
	}
	rt.tunnel = tun
	rt.ep.Host, rt.ep.Port = tun.Addr()
	return rt, nil
}

func ensureNL(s string) string {
	if s == "" || s[len(s)-1] == '\n' {
		return s
	}
	return s + "\n"
}

func (rt *runTarget) close() {
	if rt == nil {
		return
	}
	if rt.tunnel != nil {
		rt.tunnel.Close()
	}
	if rt.dir != "" {
		os.RemoveAll(rt.dir)
	}
}

// reset runs the template's reset command on the target at the end of a session.
func (rt *runTarget) reset(ctx context.Context) {
	if rt.ep.Conn.Reset != "" {
		rt.be.Exec(ctx, rt.ep, rt.ep.Conn.Reset)
	}
}

// targetTool serves target_exec / target_put for a run.
func (w *Worker) targetTool(ctx context.Context, rs *runState, name string, args json.RawMessage) (json.RawMessage, error) {
	w.mu.Lock()
	s := rs.sess
	var rt *runTarget
	if s != nil {
		rt = s.target
	}
	w.mu.Unlock()
	if rt == nil {
		return nil, errors.New("this run has no remote target")
	}
	switch name {
	case "target_exec":
		var a struct {
			Cmd      string `json:"cmd"`
			TimeoutS int    `json:"timeout_s"`
		}
		if err := json.Unmarshal(args, &a); err != nil || a.Cmd == "" {
			return nil, errors.New("cmd is required")
		}
		sec := a.TimeoutS
		if sec <= 0 {
			sec = defaultExecSec
		}
		sec = min(sec, maxExecSec)
		cctx, cancel := context.WithTimeout(ctx, time.Duration(sec)*time.Second)
		defer cancel()
		res, err := rt.be.Exec(cctx, rt.ep, a.Cmd)
		if err != nil {
			return nil, err
		}
		if cctx.Err() == context.DeadlineExceeded {
			res.TimedOut = true
		}
		if len(res.Output) > maxExecOutput { // keep the tail: that is where errors are
			res.Output = "…(truncated)…\n" + res.Output[len(res.Output)-maxExecOutput:]
		}
		s.logf("target", "$ %s -> exit %d", a.Cmd, res.ExitCode)
		return json.Marshal(res)
	case "target_put":
		var a struct{ Path, Dest string }
		if err := json.Unmarshal(args, &a); err != nil || a.Path == "" || a.Dest == "" {
			return nil, errors.New("path and dest are required")
		}
		root, err := os.OpenRoot(rt.local) // confine to the checkout: no ../ and no escaping symlinks
		if err != nil {
			return nil, err
		}
		defer root.Close()
		if _, err := root.Stat(a.Path); err != nil {
			return nil, fmt.Errorf("path %q is not inside the checkout: %w", a.Path, err)
		}
		if err := rt.be.Put(ctx, rt.ep, filepath.Join(rt.local, filepath.Clean("/"+a.Path)), a.Dest); err != nil {
			return nil, err
		}
		s.logf("target", "put %s -> %s", a.Path, a.Dest)
		return json.Marshal(map[string]any{"ok": true})
	}
	return nil, fmt.Errorf("unknown tool %q", name)
}

// ---- real backend: ssh / scp / adb through an SSM port-forward --------------------------------

type realTargets struct {
	mu      sync.Mutex
	tunnels map[string]*sharedTunnel
}

type sharedTunnel struct {
	n      int
	host   string
	port   int
	cancel context.CancelFunc
	done   chan struct{}
}

type tunnelHandle struct {
	r    *realTargets
	name string
	st   *sharedTunnel
	once sync.Once
}

func (h *tunnelHandle) Addr() (string, int) { return h.st.host, h.st.port }
func (h *tunnelHandle) Close() error {
	h.once.Do(func() {
		h.r.mu.Lock()
		defer h.r.mu.Unlock()
		if h.st.n--; h.st.n == 0 {
			if h.st.cancel != nil {
				h.st.cancel()
				<-h.st.done
			}
			delete(h.r.tunnels, h.name)
		}
	})
	return nil
}

// OpenTunnel shares one tunnel per target among the driver's sessions (reference counted). Via
// "direct" uses host:port as is; otherwise an SSM port-forward session is supervised and restarted
// with backoff if it drops.
func (r *realTargets) OpenTunnel(ctx context.Context, t proto.TargetConn) (Tunnel, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.tunnels == nil {
		r.tunnels = map[string]*sharedTunnel{}
	}
	if st := r.tunnels[t.Name]; st != nil {
		st.n++
		return &tunnelHandle{r: r, name: t.Name, st: st}, nil
	}
	st := &sharedTunnel{n: 1}
	if t.Via == "direct" {
		st.host, st.port = t.Host, t.Port
	} else {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		st.host, st.port = "127.0.0.1", l.Addr().(*net.TCPAddr).Port
		l.Close()
		sctx, cancel := context.WithCancel(context.Background())
		st.cancel, st.done = cancel, make(chan struct{})
		go func() {
			defer close(st.done)
			backoff := time.Second
			for sctx.Err() == nil {
				cmd := exec.CommandContext(sctx, "aws", "ssm", "start-session", "--target", t.InstanceID,
					"--document-name", "AWS-StartPortForwardingSession",
					"--parameters", fmt.Sprintf("portNumber=%d,localPortNumber=%d", t.Port, st.port))
				start := time.Now()
				cmd.Run()
				if time.Since(start) > time.Minute {
					backoff = time.Second
				}
				select {
				case <-sctx.Done():
				case <-time.After(backoff):
				}
				backoff = min(backoff*2, time.Minute)
			}
		}()
	}
	// wait for the path to answer
	deadline := time.Now().Add(45 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", net.JoinHostPort(st.host, strconv.Itoa(st.port)), 2*time.Second)
		if err == nil {
			c.Close()
			break
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			if st.cancel != nil {
				st.cancel()
				<-st.done
			}
			return nil, fmt.Errorf("tunnel to %s did not come up: %v", t.Name, err)
		}
		time.Sleep(500 * time.Millisecond)
	}
	r.tunnels[t.Name] = st
	return &tunnelHandle{r: r, name: t.Name, st: st}, nil
}

func sshArgs(ep Endpoint) []string {
	return []string{"-i", ep.KeyFile, "-p", strconv.Itoa(ep.Port), "-o", "BatchMode=yes", "-o", "IdentitiesOnly=yes",
		"-o", "StrictHostKeyChecking=accept-new", "-o", "UserKnownHostsFile=" + ep.Known,
		"-o", "ServerAliveInterval=15", "-o", "ConnectTimeout=15"}
}

func run(ctx context.Context, name string, args ...string) (ExecResult, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	res := ExecResult{Output: out.String()}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		res.ExitCode = ee.ExitCode()
		return res, nil
	}
	return res, err
}

func (r *realTargets) Exec(ctx context.Context, ep Endpoint, cmd string) (ExecResult, error) {
	if ep.Conn.Protocol == "adb" {
		serial := net.JoinHostPort(ep.Host, strconv.Itoa(ep.Port))
		run(ctx, "adb", "connect", serial)
		return run(ctx, "adb", "-s", serial, "shell", cmd)
	}
	res, err := run(ctx, "ssh", append(sshArgs(ep), ep.Conn.User+"@"+ep.Host, "--", cmd)...)
	if err == nil && res.ExitCode == 255 { // ssh's own failure: the path is down
		return res, fmt.Errorf("%w: %s", ErrTargetLost, bytes.TrimSpace([]byte(res.Output)))
	}
	return res, err
}

func (r *realTargets) Put(ctx context.Context, ep Endpoint, local, dest string) error {
	if ep.Conn.Protocol == "adb" {
		serial := net.JoinHostPort(ep.Host, strconv.Itoa(ep.Port))
		run(ctx, "adb", "connect", serial)
		res, err := run(ctx, "adb", "-s", serial, "push", local, dest)
		if err == nil && res.ExitCode != 0 {
			err = fmt.Errorf("adb push: %s", res.Output)
		}
		return err
	}
	args := []string{"-r", "-i", ep.KeyFile, "-P", strconv.Itoa(ep.Port), "-o", "BatchMode=yes", "-o", "IdentitiesOnly=yes",
		"-o", "StrictHostKeyChecking=accept-new", "-o", "UserKnownHostsFile=" + ep.Known, local, ep.Conn.User + "@" + ep.Host + ":" + dest}
	res, err := run(ctx, "scp", args...)
	if err == nil && res.ExitCode != 0 {
		err = fmt.Errorf("scp: %s", res.Output)
	}
	return err
}

func readFile(p string) ([]byte, error) { return os.ReadFile(p) }
