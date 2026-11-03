package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

// ApplyTimeout bounds one template apply (mise downloads can be slow).
const ApplyTimeout = 30 * time.Minute

const logTail = 8 << 10

// MiseInstaller installs the mise binary at dest. The default fetches the official
// installer; tests substitute a fake.
type MiseInstaller func(ctx context.Context, dest string, out io.Writer) error

// DefaultMiseInstaller downloads mise's official installer to a file and runs it with
// MISE_INSTALL_PATH=dest. It is wired in only by `wrangl runner run`: an Applier with no
// installer refuses to install, so tests can never reach the network by accident.
//
// The script is saved first rather than piped into sh: with a pipe, a failed download
// gives sh an empty script, which "succeeds" and hides the failure.
func DefaultMiseInstaller(ctx context.Context, dest string, out io.Writer) error {
	f, err := os.CreateTemp("", "mise-install-*.sh")
	if err != nil {
		return err
	}
	f.Close()
	defer os.Remove(f.Name())
	dl := exec.CommandContext(ctx, "curl", "-fsSL", "--retry", "3", "-o", f.Name(), "https://mise.run")
	dl.Stdout, dl.Stderr = out, out
	if err := dl.Run(); err != nil {
		return fmt.Errorf("download mise installer: %w", err)
	}
	run := exec.CommandContext(ctx, "/bin/sh", f.Name())
	run.Env = append(os.Environ(), "MISE_INSTALL_PATH="+dest)
	run.Stdout, run.Stderr = out, out
	if err := run.Run(); err != nil {
		return fmt.Errorf("run mise installer: %w", err)
	}
	if _, err := os.Stat(dest); err != nil {
		return fmt.Errorf("mise installer finished but %s is missing", dest)
	}
	return nil
}

// Applier applies RunnerTemplates on this machine. Every step is idempotent.
type Applier struct {
	Cfg         *Config
	InstallMise MiseInstaller
	Progress    func(line string) // optional; called per output line
}

// agentIdentity is who mise (and later the agent) runs as.
type agentIdentity struct {
	home     string
	uid, gid int
	drop     bool // run with these credentials (daemon is root, agent user configured)
	username string
}

func (a *Applier) agent() (agentIdentity, error) {
	id := agentIdentity{home: a.Cfg.AgentHome}
	if a.Cfg.AgentUser == "" {
		u, err := user.Current()
		if err != nil {
			return id, err
		}
		id.username = u.Username
		if id.home == "" {
			id.home = u.HomeDir
		}
		return id, nil
	}
	u, err := user.Lookup(a.Cfg.AgentUser)
	if err != nil {
		return id, fmt.Errorf("agent user %q: %w", a.Cfg.AgentUser, err)
	}
	id.username = u.Username
	id.uid, _ = strconv.Atoi(u.Uid)
	id.gid, _ = strconv.Atoi(u.Gid)
	id.drop = os.Geteuid() == 0
	if id.home == "" {
		id.home = u.HomeDir
	}
	return id, nil
}

// AppliedVersion is the template version last applied successfully (0 if none).
func AppliedVersion(cfg *Config) int64 {
	b, err := os.ReadFile(filepath.Join(cfg.StateDir, "template-version"))
	if err != nil {
		return 0
	}
	v, _ := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	return v
}

// tail keeps the last n bytes written and forwards complete lines to fn.
type tail struct {
	mu   sync.Mutex
	buf  []byte
	line []byte
	fn   func(string)
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > logTail {
		t.buf = t.buf[len(t.buf)-logTail:]
	}
	if t.fn != nil {
		t.line = append(t.line, p...)
		for {
			i := strings.IndexByte(string(t.line), '\n')
			if i < 0 {
				break
			}
			t.fn(string(t.line[:i]))
			t.line = t.line[i+1:]
		}
	}
	return len(p), nil
}

func (t *tail) String() string { t.mu.Lock(); defer t.mu.Unlock(); return string(t.buf) }

// Apply installs mise if needed, runs the setup script (as the daemon, i.e. root) when it
// changed, writes the agent's global mise config and runs `mise install` as the agent.
// It returns the output tail either way.
func (a *Applier) Apply(ctx context.Context, t proto.TemplateApply) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, ApplyTimeout)
	defer cancel()
	out := &tail{fn: a.Progress}
	step := func(f string, args ...any) { fmt.Fprintf(out, "==> "+f+"\n", args...) }
	if err := os.MkdirAll(a.Cfg.StateDir, 0o700); err != nil {
		return out.String(), err
	}

	if a.Cfg.Host {
		step("host runner: template left unapplied (the machine is managed by hand)")
		if err := os.WriteFile(filepath.Join(a.Cfg.StateDir, "template-version"), []byte(strconv.FormatInt(t.Version, 10)+"\n"), 0o600); err != nil {
			return out.String(), err
		}
		return out.String(), nil
	}

	// 1. mise binary.
	if _, err := os.Stat(a.Cfg.MiseBin); errors.Is(err, os.ErrNotExist) {
		step("installing mise to %s", a.Cfg.MiseBin)
		if a.InstallMise == nil {
			return out.String(), fmt.Errorf("%s is missing and no mise installer is configured", a.Cfg.MiseBin)
		}
		if err := a.InstallMise(ctx, a.Cfg.MiseBin, out); err != nil {
			return out.String(), fmt.Errorf("install mise: %w", err)
		}
	}

	// 2. setup script, only when its content changed.
	sum := sha256.Sum256([]byte(t.SetupScript))
	hash := hex.EncodeToString(sum[:])
	hashFile := filepath.Join(a.Cfg.StateDir, "setup.sha256")
	if prev, _ := os.ReadFile(hashFile); strings.TrimSpace(string(prev)) != hash {
		if strings.TrimSpace(t.SetupScript) != "" {
			step("running setup script")
			cmd := exec.CommandContext(ctx, "/bin/bash", "-euo", "pipefail", "-c", t.SetupScript)
			cmd.Stdout, cmd.Stderr, cmd.Dir = out, out, "/"
			if err := cmd.Run(); err != nil {
				return out.String(), fmt.Errorf("setup script: %w", err)
			}
		}
		if err := os.WriteFile(hashFile, []byte(hash+"\n"), 0o600); err != nil {
			return out.String(), err
		}
	} else {
		step("setup script unchanged; skipping")
	}

	// 3. agent's mise config.
	id, err := a.agent()
	if err != nil {
		return out.String(), err
	}
	cfgDir := filepath.Join(id.home, ".config", "mise")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		return out.String(), err
	}
	cfgFile := filepath.Join(cfgDir, "config.toml")
	if err := os.WriteFile(cfgFile, []byte(t.MiseTOML), 0o644); err != nil {
		return out.String(), err
	}
	if id.drop {
		for _, p := range []string{filepath.Join(id.home, ".config"), cfgDir, cfgFile} {
			if err := os.Chown(p, id.uid, id.gid); err != nil {
				return out.String(), err
			}
		}
	}

	// 4. mise install as the agent.
	step("mise install (as %s)", id.username)
	cmd := exec.CommandContext(ctx, a.Cfg.MiseBin, "install", "--yes")
	cmd.Dir = id.home
	cmd.Env = []string{
		"HOME=" + id.home, "USER=" + id.username, "LOGNAME=" + id.username,
		"PATH=" + filepath.Dir(a.Cfg.MiseBin) + ":/usr/local/bin:/usr/bin:/bin",
		"MISE_YES=1",
	}
	if v := os.Getenv("HTTPS_PROXY"); v != "" { // honour a configured egress proxy
		cmd.Env = append(cmd.Env, "HTTPS_PROXY="+v, "https_proxy="+v)
	}
	if id.drop {
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(id.uid), Gid: uint32(id.gid)}}
	}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("mise install: %w", err)
	}

	if err := os.WriteFile(filepath.Join(a.Cfg.StateDir, "template-version"), []byte(strconv.FormatInt(t.Version, 10)+"\n"), 0o600); err != nil {
		return out.String(), err
	}
	step("template version %d applied", t.Version)
	return out.String(), nil
}
