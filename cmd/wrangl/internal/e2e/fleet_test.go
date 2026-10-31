// Package e2e runs the controller and a runner together in one process: real NATS (TLS,
// operator JWTs), real HTTP API and enrolment, real runner bootstrap and daemon. Only
// EC2, IMDS and the clock are faked.
package e2e

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nreid/devhost/cmd/wrangl/internal/client"
	"github.com/nreid/devhost/cmd/wrangl/internal/config"
	"github.com/nreid/devhost/cmd/wrangl/internal/controller"
	"github.com/nreid/devhost/cmd/wrangl/internal/controller/provision"
	"github.com/nreid/devhost/cmd/wrangl/internal/runner"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Advance(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

func ec2Arch() string {
	if runtime.GOARCH == "arm64" {
		return "arm64"
	}
	return "x86_64"
}

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last := ""
	for time.Now().Before(deadline) {
		ok, s := cond()
		if ok {
			return
		}
		last = s
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s (last: %s)", what, last)
}

func TestLaunchEnrolHeartbeatIdleReap(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("runner binary fallback serves the controller executable only on linux")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()
	if _, err := controller.Init(ctx, filepath.Join(dir, "data"), filepath.Join(dir, "c.yaml")); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultController()
	cfg.DataDir = filepath.Join(dir, "data")
	cfg.Listen = config.Listen{HTTP: "127.0.0.1:0", Unix: filepath.Join(dir, "api.sock"), NATS: "127.0.0.1:-1", Enroll: "127.0.0.1:0"}

	fake := provision.NewFake()
	fake.FixedIP = "127.0.0.1" // the enrol request arrives from loopback
	clk := &clock{t: time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)}
	srv, err := controller.New(cfg, slog.New(slog.NewTextHandler(testLogWriter(t), nil)))
	if err != nil {
		t.Fatal(err)
	}
	srv.Now, srv.LoopInterval, srv.AllowLoopbackEnroll = clk.Now, 50*time.Millisecond, true
	srv.Provisioner = &provision.Provisioner{EC2: fake, SSM: fake} // controller id filled in at start
	srvDone := make(chan error, 1)
	go func() { srvDone <- srv.Run(ctx) }()
	<-srv.Ready()

	api, err := client.New("unix://"+cfg.Listen.Unix, nil)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := fmt.Sprintf("name: tiny\narch: %s\nmin: {vcpu: 1, ram: 256MiB, disk: 1GiB}\nmise_toml: \"\"\n", ec2Arch())
	if _, err := api.CreateTemplate(ctx, []byte(tmpl)); err != nil {
		t.Fatal(err)
	}
	launched, err := api.Launch(ctx, "tiny", 1)
	if err != nil {
		t.Fatal(err)
	}
	r := launched[0]
	if r.State != "launching" || r.InstanceID == "" {
		t.Fatalf("launched: %+v", r)
	}
	ud := userData(t, fake)
	token := between(ud, `--token "`, `"`)

	// Fake IMDSv2 answering with the launched instance's id.
	imds := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case req.Method == http.MethodPut && req.URL.Path == "/latest/api/token":
			io.WriteString(w, "imds-token")
		case req.URL.Path == "/latest/meta-data/instance-id" && req.Header.Get("X-aws-ec2-metadata-token") == "imds-token":
			io.WriteString(w, r.InstanceID)
		default:
			http.Error(w, "no", http.StatusUnauthorized)
		}
	}))
	defer imds.Close()

	root := filepath.Join(dir, "runner-root")
	rcfg, err := runner.Bootstrap(ctx, runner.BootstrapOptions{
		Controller: "http://" + srv.EnrollAddr(), Token: token, Root: root,
		IMDSURL: imds.URL, NoSystemd: true, NoUser: true,
	})
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if rcfg.Name != r.Name {
		t.Fatalf("enrolled as %q, launched %q", rcfg.Name, r.Name)
	}
	// The token is single use.
	if _, err := runner.Bootstrap(ctx, runner.BootstrapOptions{Controller: "http://" + srv.EnrollAddr(), Token: token,
		Root: filepath.Join(dir, "again"), IMDSURL: imds.URL, NoSystemd: true, NoUser: true}); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("token reuse: %v", err)
	}

	loaded, err := runner.LoadConfig(filepath.Join(root, "etc/wrangl/runner.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	daemonCtx, stopDaemon := context.WithCancel(ctx)
	defer stopDaemon()
	go runner.Run(daemonCtx, loaded, runner.DaemonOptions{
		HeartbeatInterval: 50 * time.Millisecond, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Poweroff: func() error { t.Error("daemon powered off while the controller was up"); return nil },
	})

	waitFor(t, "runner idle with metrics", 10*time.Second, func() (bool, string) {
		v, err := api.Runner(ctx, r.Name)
		return err == nil && v.State == "idle" && len(v.Metrics) > 0 && v.VCPU > 0, fmt.Sprintf("%+v %v", v, err)
	})
	if rtt, err := runner.Status(ctx, loaded); err != nil || rtt <= 0 {
		t.Fatalf("runner status: %v %v", rtt, err)
	}

	// 29 minutes idle: still there.
	clk.Advance(29 * time.Minute)
	time.Sleep(300 * time.Millisecond) // several reaper ticks
	if v, _ := api.Runner(ctx, r.Name); v.State != "idle" || fake.State(r.InstanceID) == "terminated" {
		t.Fatalf("reaped before 30 min: %s", v.State)
	}
	// Past 30 minutes: reaped, instance terminated after the tag check.
	clk.Advance(90 * time.Second)
	waitFor(t, "idle runner reaped", 5*time.Second, func() (bool, string) {
		v, _ := api.Runner(ctx, r.Name)
		return v.State == "terminated" && fake.State(r.InstanceID) == "terminated", v.State + "/" + fake.State(r.InstanceID)
	})
	if rs, _ := api.Runners(ctx, false); len(rs) != 0 {
		t.Fatalf("terminated runner still listed: %+v", rs)
	}

	stopDaemon()
	cancel()
	if err := <-srvDone; err != nil {
		t.Fatalf("controller: %v", err)
	}
}

func userData(t *testing.T, f *provision.Fake) string {
	t.Helper()
	b, err := decodeB64(*f.Runs[len(f.Runs)-1].UserData)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func between(s, a, b string) string {
	i := strings.Index(s, a)
	if i < 0 {
		return ""
	}
	s = s[i+len(a):]
	return s[:strings.Index(s, b)]
}
