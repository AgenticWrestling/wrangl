package runner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/nats-io/jwt/v2"
	"github.com/nats-io/nkeys"
	"gopkg.in/yaml.v3"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

// BootstrapOptions configure a one-time enrolment. Root, IMDSURL, NoSystemd and NoUser
// exist so tests (and non-systemd boxes) can run it unprivileged. With NoUser the agent
// runs as the daemon's own user.
type BootstrapOptions struct {
	Controller string // http://<controller-ip>:4223
	Token      string
	SHA256     string // optional: expected sha256 of this executable
	Root       string // filesystem prefix; "" = /
	IMDSURL    string // default http://169.254.169.254
	NoSystemd  bool
	NoUser     bool
	// Host enrols this machine as the controller host's permanent runner: config and state
	// under the current user's home, a systemd --user unit, the agent is the current user.
	Host   bool
	Binary string // path written into the unit; default /usr/local/bin/wrangl
	// HostAgentUser (with Host) runs the host's agents as this dedicated user instead of the
	// current one (ADR 0011): the daemon runs as root from a system unit, drops to the user for
	// every agent, worktree and shell, and the owner's home is never touched. Empty = today's
	// behaviour. The user is created if missing (idempotent).
	HostAgentUser string
	// Slice limits for everything the host runner runs (ADR 0011 section 6B): a systemd slice so a
	// fan-out of agents cannot starve the controller. Empty = the Default* values below.
	SliceCPUWeight  int    // systemd CPUWeight, 1..10000 (default 100 is "equal share")
	SliceMemoryHigh string // MemoryHigh: throttle above this, e.g. "60%" or "8G"
	SliceMemoryMax  string // MemoryMax: hard ceiling
	Log             io.Writer
}

// The daemon runs as root (it holds the NATS credentials, installs mise, runs the
// template's setup script and powers off when orphaned). The agent runs as the
// unprivileged agent user, which cannot read /etc/wrangl/runner.creds (spec D26).
const unitTmpl = `[Unit]
Description=wrangl runner
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=%s --config /etc/wrangl/runner.yaml runner run
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
`

// hostUnitTmpl runs the host runner as root so it can drop to the agent user (ADR 0011). The
// owner's account is not involved.
const hostUnitTmpl = `[Unit]
Description=wrangl runner (controller host, agents as a dedicated user)
After=network-online.target
Wants=network-online.target

[Service]
Slice=wrangl-agent.slice
ExecStart=%s --config %s runner run
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
`

// Defaults for the host's agent slice: half the CPU weight of the controller's own services under
// contention, throttled at 60% of RAM and capped at 75%.
const (
	DefaultSliceCPUWeight  = 50
	DefaultSliceMemoryHigh = "60%"
	DefaultSliceMemoryMax  = "75%"
	HostSliceName          = "wrangl-agent.slice"
)

var sliceValue = regexp.MustCompile(`^(\d{1,3}%|\d+[KMGT]?|infinity)$`)

// HostSliceUnit renders the slice unit file for the host's agent user, validating its limits.
func HostSliceUnit(cpuWeight int, memHigh, memMax string) (string, error) {
	if cpuWeight == 0 {
		cpuWeight = DefaultSliceCPUWeight
	}
	if memHigh == "" {
		memHigh = DefaultSliceMemoryHigh
	}
	if memMax == "" {
		memMax = DefaultSliceMemoryMax
	}
	if cpuWeight < 1 || cpuWeight > 10000 {
		return "", fmt.Errorf("slice CPU weight %d out of range 1..10000", cpuWeight)
	}
	for _, v := range []string{memHigh, memMax} {
		if !sliceValue.MatchString(v) {
			return "", fmt.Errorf("invalid slice memory limit %q (use e.g. 60%% or 8G)", v)
		}
	}
	return fmt.Sprintf("[Unit]\nDescription=wrangl agent user: the host runner and everything it starts\n\n[Slice]\nCPUWeight=%d\nMemoryHigh=%s\nMemoryMax=%s\n", cpuWeight, memHigh, memMax), nil
}

var validUsername = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,30}$`)

// Bootstrap enrols this machine and installs the daemon. It returns the config written.
func Bootstrap(ctx context.Context, o BootstrapOptions) (*Config, error) {
	logf := func(f string, a ...any) {
		if o.Log != nil {
			fmt.Fprintf(o.Log, f+"\n", a...)
		}
	}
	if o.IMDSURL == "" {
		o.IMDSURL = "http://169.254.169.254"
	}
	if o.Binary == "" {
		o.Binary = "/usr/local/bin/wrangl"
	}
	if o.SHA256 != "" {
		if err := verifySelf(o.SHA256); err != nil {
			return nil, err
		}
	}
	etc := filepath.Join(o.Root, "etc", "wrangl")
	state := filepath.Join(o.Root, "var", "lib", "wrangl")
	var uhome string
	hostAgent := o.Host && o.HostAgentUser != ""
	if o.HostAgentUser != "" && !o.Host {
		return nil, fmt.Errorf("--agent-user only applies with --host")
	}
	if hostAgent {
		if !validUsername.MatchString(o.HostAgentUser) {
			return nil, fmt.Errorf("invalid agent user name %q", o.HostAgentUser)
		}
		if _, err := HostSliceUnit(o.SliceCPUWeight, o.SliceMemoryHigh, o.SliceMemoryMax); err != nil {
			return nil, err
		}
		if os.Geteuid() != 0 && o.Root == "" {
			return nil, fmt.Errorf("--agent-user needs root: run `sudo wrangl runner bootstrap --host --agent-user %s ...`", o.HostAgentUser)
		}
		etc = filepath.Join(o.Root, "etc", "wrangl-host-runner")
		state = filepath.Join(o.Root, "var", "lib", "wrangl-host-runner")
		if exe, err := os.Executable(); err == nil && o.Binary == "/usr/local/bin/wrangl" {
			o.Binary = exe
		}
	} else if o.Host {
		u, err := user.Current()
		if err != nil {
			return nil, err
		}
		uhome = u.HomeDir
		etc = filepath.Join(uhome, ".config", "wrangl-host-runner")
		state = filepath.Join(uhome, ".local", "state", "wrangl-host-runner")
		o.NoUser = true
		if exe, err := os.Executable(); err == nil && o.Binary == "/usr/local/bin/wrangl" {
			o.Binary = exe
		}
	}
	for _, d := range []string{etc, state} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	if !o.NoUser {
		name := DefaultAgentUser
		if hostAgent {
			name = o.HostAgentUser
		}
		if err := ensureAgentUser(name); err != nil {
			return nil, err
		}
	}

	kp, err := nkeys.CreateUser()
	if err != nil {
		return nil, err
	}
	pub, _ := kp.PublicKey()
	seed, _ := kp.Seed()
	facts, err := MeasureFacts(ctx)
	if err != nil {
		return nil, fmt.Errorf("measure machine: %w", err)
	}
	req := proto.EnrollRequest{Token: o.Token, NKeyPublic: pub, InstanceID: InstanceID(ctx, o.IMDSURL), Facts: facts}
	logf("enrolling with %s (instance %q, %s, %d vCPU, %d MiB, %d GiB)", o.Controller, req.InstanceID, facts.Arch, facts.VCPU, facts.RAMMiB, facts.DiskGiB)
	resp, err := enroll(ctx, o.Controller, req)
	if err != nil {
		return nil, err
	}

	creds, err := jwt.FormatUserConfig(resp.UserJWT, seed)
	if err != nil {
		return nil, err
	}
	cfg := &Config{
		RunnerID: resp.RunnerID, Name: resp.Name, Controller: o.Controller, NATSURLs: resp.NATSURLs,
		CAFile: "/etc/wrangl/ca.pem", CredsFile: "/etc/wrangl/runner.creds",
		PoweroffCommand: []string{"/usr/bin/systemctl", "poweroff"},
		StateDir:        DefaultStateDir, AgentUser: DefaultAgentUser, MiseBin: DefaultMiseBin,
	}
	if o.NoUser {
		cfg.AgentUser = ""
	}
	if hostAgent {
		cfg.Host = true
		cfg.PoweroffCommand = nil
		cfg.AgentUser = o.HostAgentUser
		cfg.CAFile, cfg.CredsFile = filepath.Join(etc, "ca.pem"), filepath.Join(etc, "runner.creds")
		cfg.StateDir = state
		cfg.SocketPath = filepath.Join(o.Root, "run", "wrangl-host", "runner.sock")
		cfg.MiseBin = DefaultMiseBin
		if p, err := exec.LookPath("mise"); err == nil {
			cfg.MiseBin = p
		}
	} else if o.Host {
		cfg.Host = true
		cfg.PoweroffCommand = nil
		cfg.CAFile, cfg.CredsFile = filepath.Join(etc, "ca.pem"), filepath.Join(etc, "runner.creds")
		cfg.StateDir = state
		cfg.SocketPath = filepath.Join(state, "runner.sock")
		cfg.MiseBin = filepath.Join(uhome, ".local", "bin", "mise")
		if p, err := exec.LookPath("mise"); err == nil {
			cfg.MiseBin = p
		}
	}
	if o.Root != "" { // tests: keep paths inside the prefix
		if hostAgent {
			cfg.AgentHome = filepath.Join(o.Root, "home", o.HostAgentUser)
		}
		cfg.CAFile, cfg.CredsFile = filepath.Join(etc, "ca.pem"), filepath.Join(etc, "runner.creds")
		cfg.StateDir = state
		if !hostAgent {
			cfg.AgentHome = filepath.Join(o.Root, "home", "agent")
		}
		cfg.MiseBin = filepath.Join(o.Root, "usr", "local", "bin", "mise")
		if !hostAgent {
			cfg.SocketPath = filepath.Join(o.Root, "run", "wrangl", "runner.sock")
		}
	}
	cfgYAML, _ := yaml.Marshal(cfg)
	files := []struct {
		path string
		data []byte
		mode os.FileMode
	}{
		{filepath.Join(etc, "runner.creds"), creds, 0o600},
		{filepath.Join(etc, "ca.pem"), []byte(resp.NATSCA), 0o644},
		{filepath.Join(etc, "runner.yaml"), cfgYAML, 0o640},
	}
	for _, f := range files {
		if err := writeFile(f.path, f.data, f.mode); err != nil {
			return nil, err
		}
	}
	logf("enrolled as %s (runner %d)", resp.Name, resp.RunnerID)

	if hostAgent && !o.NoSystemd {
		sl, err := HostSliceUnit(o.SliceCPUWeight, o.SliceMemoryHigh, o.SliceMemoryMax)
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(o.Root, "etc/systemd/system", HostSliceName), []byte(sl), 0o644); err != nil {
			return nil, err
		}
		unit := filepath.Join(o.Root, "etc/systemd/system/wrangl-host-runner.service")
		body := fmt.Sprintf(hostUnitTmpl, o.Binary, filepath.Join(etc, "runner.yaml"))
		if err := os.WriteFile(unit, []byte(body), 0o644); err != nil {
			return nil, err
		}
		for _, args := range [][]string{{"daemon-reload"}, {"enable", "--now", "wrangl-host-runner.service"}} {
			if out, err := exec.CommandContext(ctx, "systemctl", args...).CombinedOutput(); err != nil {
				return nil, fmt.Errorf("systemctl %s: %v: %s", strings.Join(args, " "), err, out)
			}
		}
		logf("started wrangl-host-runner.service (system unit; agents run as %s)", o.HostAgentUser)
		return cfg, nil
	}
	if o.Host && !hostAgent && !o.NoSystemd {
		dir := filepath.Join(uhome, ".config", "systemd", "user")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
		unit := fmt.Sprintf("[Unit]\nDescription=wrangl runner (controller host)\nAfter=network-online.target\n\n[Service]\nExecStart=%s --config %s runner run\nRestart=always\nRestartSec=5\n\n[Install]\nWantedBy=default.target\n",
			o.Binary, filepath.Join(etc, "runner.yaml"))
		if err := os.WriteFile(filepath.Join(dir, "wrangl-host-runner.service"), []byte(unit), 0o644); err != nil {
			return nil, err
		}
		for _, args := range [][]string{{"--user", "daemon-reload"}, {"--user", "enable", "--now", "wrangl-host-runner.service"}} {
			if out, err := exec.CommandContext(ctx, "systemctl", args...).CombinedOutput(); err != nil {
				return nil, fmt.Errorf("systemctl %s: %v: %s", strings.Join(args, " "), err, out)
			}
		}
		logf("started wrangl-host-runner.service (user unit)")
		return cfg, nil
	}
	if !o.NoSystemd {
		unit := filepath.Join(o.Root, "etc/systemd/system/wrangl-runner.service")
		if err := os.WriteFile(unit, []byte(fmt.Sprintf(unitTmpl, o.Binary)), 0o644); err != nil {
			return nil, err
		}
		for _, args := range [][]string{{"daemon-reload"}, {"enable", "--now", "wrangl-runner.service"}} {
			if out, err := exec.CommandContext(ctx, "systemctl", args...).CombinedOutput(); err != nil {
				return nil, fmt.Errorf("systemctl %s: %v: %s", strings.Join(args, " "), err, out)
			}
		}
		logf("started wrangl-runner.service")
	}
	return cfg, nil
}

func enroll(ctx context.Context, controller string, req proto.EnrollRequest) (*proto.EnrollResponse, error) {
	body, _ := json.Marshal(req)
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ { // the controller may be restarting
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt*2) * time.Second):
			}
		}
		hreq, _ := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(controller, "/")+proto.EnrollPath, bytes.NewReader(body))
		hreq.Header.Set("Content-Type", "application/json")
		resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(hreq)
		if err != nil {
			lastErr = err
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			var out proto.EnrollResponse
			if err := json.Unmarshal(b, &out); err != nil {
				return nil, err
			}
			return &out, nil
		}
		var e proto.APIError
		json.Unmarshal(b, &e)
		err = fmt.Errorf("enrolment refused (HTTP %d): %s", resp.StatusCode, e.Error)
		if resp.StatusCode < 500 {
			return nil, err // a 4xx will not succeed on retry (and the token may be burned)
		}
		lastErr = err
	}
	return nil, lastErr
}

func verifySelf(want string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	f, err := os.Open(exe)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != strings.ToLower(want) {
		return fmt.Errorf("binary sha256 %s does not match expected %s", got, want)
	}
	return nil
}

// ensureAgentUser creates the unprivileged user the agent and its toolchain run as.
// It gets a home directory and a login shell but no sudo.
func ensureAgentUser(name string) error {
	if _, err := user.Lookup(name); err == nil {
		return nil
	}
	if out, err := exec.Command("useradd", "--create-home", "--shell", "/bin/bash", name).CombinedOutput(); err != nil {
		return fmt.Errorf("useradd %s: %v: %s", name, err, out)
	}
	return nil
}

// writeFile writes atomically with the given mode. Files are owned by the daemon's
// user (root in production), never by the agent.
func writeFile(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
