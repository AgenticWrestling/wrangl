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
	"strconv"
	"strings"
	"time"

	"github.com/nats-io/jwt/v2"
	"github.com/nats-io/nkeys"
	"gopkg.in/yaml.v3"

	"github.com/nreid/devhost/cmd/wrangl/internal/proto"
)

// BootstrapOptions configure a one-time enrolment. Root, IMDSURL, NoSystemd and NoUser
// exist so tests (and non-systemd boxes) can run it unprivileged.
type BootstrapOptions struct {
	Controller string // http://<controller-ip>:4223
	Token      string
	SHA256     string // optional: expected sha256 of this executable
	Root       string // filesystem prefix; "" = /
	IMDSURL    string // default http://169.254.169.254
	NoSystemd  bool
	NoUser     bool
	Binary     string // path written into the unit; default /usr/local/bin/wrangl
	Log        io.Writer
}

const serviceUser = "wrangl"

const unitTmpl = `[Unit]
Description=wrangl runner
After=network-online.target
Wants=network-online.target

[Service]
User=wrangl
ExecStart=%s runner run --config /etc/wrangl/runner.yaml
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
`

// The daemon runs as the unprivileged wrangl user but must be able to power the box off
// when orphaned; this is the only privileged command it may run.
const sudoersRule = "wrangl ALL=(root) NOPASSWD: /usr/bin/systemctl poweroff\n"

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
	if err := os.MkdirAll(etc, 0o750); err != nil {
		return nil, err
	}
	if !o.NoUser {
		if err := ensureUser(); err != nil {
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
		PoweroffCommand: []string{"sudo", "-n", "/usr/bin/systemctl", "poweroff"},
	}
	if o.Root != "" { // tests: keep paths inside the prefix
		cfg.CAFile, cfg.CredsFile = filepath.Join(etc, "ca.pem"), filepath.Join(etc, "runner.creds")
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
		if err := writeFile(f.path, f.data, f.mode, !o.NoUser); err != nil {
			return nil, err
		}
	}
	logf("enrolled as %s (runner %d)", resp.Name, resp.RunnerID)

	if !o.NoSystemd {
		if err := os.WriteFile(filepath.Join(o.Root, "etc/sudoers.d/wrangl-poweroff"), []byte(sudoersRule), 0o440); err != nil {
			return nil, err
		}
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

func ensureUser() error {
	if _, err := user.Lookup(serviceUser); err == nil {
		return nil
	}
	if out, err := exec.Command("useradd", "--system", "--create-home", "--shell", "/bin/bash", serviceUser).CombinedOutput(); err != nil {
		return fmt.Errorf("useradd %s: %v: %s", serviceUser, err, out)
	}
	return nil
}

// writeFile writes atomically and, when chown is set, hands the file to the wrangl user.
func writeFile(path string, data []byte, mode os.FileMode, chown bool) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		return err
	}
	if chown {
		u, err := user.Lookup(serviceUser)
		if err != nil {
			return err
		}
		uid, _ := strconv.Atoi(u.Uid)
		gid, _ := strconv.Atoi(u.Gid)
		if err := os.Chown(tmp, uid, gid); err != nil {
			return err
		}
	}
	return os.Rename(tmp, path)
}
