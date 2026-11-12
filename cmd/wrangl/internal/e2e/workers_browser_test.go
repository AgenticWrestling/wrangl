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
)

// TestWorkersPageBrowser loads /workers at desktop and phone width with a runner whose latest
// heartbeat lists three Claude sessions (ADR 0011 section 4). Needs node and Playwright
// (PLAYWRIGHT_DIR); skipped otherwise.
func TestWorkersPageBrowser(t *testing.T) {
	pw := os.Getenv("PLAYWRIGHT_DIR")
	if pw == "" {
		t.Skip("set PLAYWRIGHT_DIR to run the browser test")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
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
	srv, err := controller.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	srv.LoopInterval, srv.SchedulerInterval = time.Hour, time.Hour
	go func() {
		if err := srv.Run(ctx); err != nil && ctx.Err() == nil {
			t.Errorf("controller: %v", err)
		}
	}()
	<-srv.Ready()
	api, _ := client.New("unix://"+cfg.Listen.Unix, nil)
	if _, err := api.CreateProject(ctx, proto.ProjectRequest{Slug: "wk"}); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.Fleet.DB.W.Exec(`INSERT INTO runner_template (name, mise_toml, agent_json, min_vcpu, min_ram_mib, min_disk_gib) VALUES ('tpl','','{}',1,1,1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.Fleet.DB.W.Exec(`INSERT INTO github_account (login, kind) VALUES ('acme', 'org')`); err != nil {
		t.Fatal(err)
	}
	if _, err := api.CreateTask(ctx, proto.TaskCreateRequest{Project: "wk", Title: "first", Prompt: "p"}); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
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
	hb, _ := json.Marshal(proto.Heartbeat{Time: time.Now(), CPUPercent: 12, Claude: []proto.ClaudeProc{
		{PID: 11, Kind: proto.ClaudeRun, RunID: 7, Task: "abc123", Cwd: "/work/some/long/worktree/path/for/the/task", RSS: 300 << 20, Started: time.Now().Add(-time.Hour)},
		{PID: 12, Kind: proto.ClaudePrimary, RCName: "wk", Cwd: "/work/wk"},
		{PID: 13, Kind: proto.ClaudeOther, RCName: "hand", Mux: "zellij:z1", RSS: 1 << 30},
	}})
	if _, err := srv.Fleet.DB.W.Exec(`INSERT INTO runner (name, template_id, state, last_heartbeat_at, last_metrics_json) VALUES ('w-test', 1, 'idle', strftime('%Y-%m-%dT%H:%M:%fZ','now'), ?)`, string(hb)); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, node, "browser/workers.js", pw, "http://"+ln.Addr().String())
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
