package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/client"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/config"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/sessionhost"
)

// TestPrimarySessionButtonBrowser drives the Session button and pill on the Projects and project
// pages and the terminal tab it opens (ADR 0011 section 3, Access/Web), at desktop and phone width,
// against a real session host whose child is `cat`. Needs node and Playwright (PLAYWRIGHT_DIR).
func TestPrimarySessionButtonBrowser(t *testing.T) {
	pw := os.Getenv("PLAYWRIGHT_DIR")
	if pw == "" {
		t.Skip("set PLAYWRIGHT_DIR to run the browser test")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	base, ctx := primaryEnv(t)
	cmd := exec.CommandContext(ctx, node, "browser/primary.js", pw, base)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			t.Fatalf("browser script: %v %s %s", err, out, ee.Stderr)
		}
		t.Fatalf("browser script: %v %s", err, out)
	}
	var res struct{ Errors []string }
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("bad script output %q: %v", out, err)
	}
	for _, e := range res.Errors {
		t.Error(e)
	}
}

// primaryEnv starts a controller with the primary-session reconciler, a real session host (child:
// cat), a host runner "host-test" and projects alpha (with a repo, so a session) and gamma (none).
// It returns the controller's http base URL and a context that ends with the test.
func primaryEnv(t *testing.T) (string, context.Context) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	dir := t.TempDir()
	if _, err := controller.Init(ctx, filepath.Join(dir, "data"), filepath.Join(dir, "c.yaml")); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultController()
	cfg.DataDir = filepath.Join(dir, "data")
	cfg.Listen = config.Listen{HTTP: "127.0.0.1:0", Unix: filepath.Join(dir, "api.sock"), NATS: "127.0.0.1:-1", Enroll: "127.0.0.1:0"}
	// A real session host with `cat` as the child, and the controller's reconciler pointed at it.
	hostSock := filepath.Join(dir, "sessions.sock")
	sh := sessionhost.NewHost(sessionhost.Config{
		Argv:      func(sp sessionhost.Spec, resume bool) []string { return []string{"/bin/cat"} },
		StopGrace: time.Second,
	})
	shDone := make(chan struct{})
	go func() {
		(&sessionhost.Server{Host: sh, Path: hostSock, Group: "no-such-group", Log: slog.New(slog.NewTextHandler(io.Discard, nil))}).Serve(ctx)
		close(shDone)
	}()
	t.Cleanup(func() { sh.StopAll(); cancel(); <-shDone })
	for i := 0; i < 200; i++ {
		if _, err := os.Stat(hostSock); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	src := filepath.Join(dir, "src")
	os.MkdirAll(src, 0o755)
	for _, args := range [][]string{{"init", "-q", "-b", "trunk"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "one"}} {
		c := exec.Command("git", args...)
		c.Dir = src
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	cfg.Sessions = config.Sessions{Socket: hostSock, Dir: filepath.Join(dir, "primary")}
	srv, err := controller.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	srv.LoopInterval, srv.SchedulerInterval = 400*time.Millisecond, time.Hour
	go func() {
		if err := srv.Run(ctx); err != nil && ctx.Err() == nil {
			t.Errorf("controller: %v", err)
		}
	}()
	<-srv.Ready()
	api, _ := client.New("unix://"+cfg.Listen.Unix, nil)
	for _, slug := range []string{"alpha", "gamma"} {
		if _, err := api.CreateProject(ctx, proto.ProjectRequest{Slug: slug}); err != nil {
			t.Fatal(err)
		}
	}
	w := srv.Fleet.DB.W
	w.Exec(`INSERT INTO runner_template (name, mise_toml, agent_json, min_vcpu, min_ram_mib, min_disk_gib) VALUES ('tpl','','{}',1,1,1)`)
	w.Exec(`INSERT INTO runner (name, template_id, state, last_heartbeat_at) VALUES ('host-test', 1, 'idle', strftime('%Y-%m-%dT%H:%M:%fZ','now'))`)
	// alpha has a repo (so it gets a session); gamma has none (pill "none", no button).
	w.Exec(`INSERT INTO repo (project_id, owner, name, default_branch, clone_url) SELECT id, 'acme', 'alpha', 'trunk', ? FROM project WHERE slug = 'alpha'`, src)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if l := sh.List(); len(l) == 1 && l[0].State == "running" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if l := sh.List(); len(l) != 1 {
		t.Fatalf("the reconciler did not start alpha's session: %+v", l)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				u, err := net.Dial("unix", cfg.Listen.Unix)
				if err != nil {
					c.Close()
					return
				}
				go io.Copy(u, c)
				io.Copy(c, u)
				u.Close()
				c.Close()
			}()
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return "http://" + ln.Addr().String(), ctx
}
