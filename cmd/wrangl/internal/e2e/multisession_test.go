package e2e

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/client"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/config"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/gh"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/runner"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/runner/runnertest"
)

// barrierAgent proves concurrency: each run announces itself in $BARRIER, waits until the other
// has too, records that it saw both, then completes its own task. It names its run to the daemon
// the way the real shim does (X-Wrangl-Run), which a runner with several sessions requires.
const barrierAgent = `#!/bin/sh
touch "$BARRIER/started.$WRANGL_RUN_ID"
i=0
while [ "$(ls "$BARRIER" | grep -c '^started\.')" -lt 2 ] && [ $i -lt 150 ]; do sleep 0.1; i=$((i+1)); done
[ "$(ls "$BARRIER" | grep -c '^started\.')" -ge 2 ] && touch "$BARRIER/saw-both.$WRANGL_RUN_ID"
pwd > "$BARRIER/dir.$WRANGL_RUN_ID"
printf 'work for %s\n' "$WRANGL_TASK" >> NOTES.md
curl -s --unix-socket "$WRANGL_SOCK" -H "X-Wrangl-Run: $WRANGL_RUN_ID" \
  -d '{"event":"PostToolUse","payload":{"tool_name":"Edit"}}' http://x/hook >/dev/null
curl -s --unix-socket "$WRANGL_SOCK" -H "X-Wrangl-Run: $WRANGL_RUN_ID" \
  -d "{\"sqid\":\"$WRANGL_TASK\",\"summary\":\"did $WRANGL_TASK\"}" http://x/tool/task_complete >/dev/null
`

// Two runs of one template share one runner when its template allows two sessions: both are
// placed on it at once, run in separate workspaces, and each finishes its own task.
func TestTwoSessionsShareOneRunner(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs linux (PTY, controller-served runner binary)")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()
	bare := bareRepo(t, dir)
	barrier := filepath.Join(dir, "barrier")
	os.MkdirAll(barrier, 0o755)
	agentPath := filepath.Join(dir, "session-agent")
	os.WriteFile(agentPath, []byte(barrierAgent), 0o755)

	if _, err := controller.Init(ctx, filepath.Join(dir, "data"), filepath.Join(dir, "c.yaml")); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultController()
	cfg.DataDir = filepath.Join(dir, "data")
	cfg.Listen = config.Listen{HTTP: "127.0.0.1:0", Unix: filepath.Join(dir, "api.sock"), NATS: "127.0.0.1:-1", Enroll: "127.0.0.1:0"}
	fakeGH := gh.NewFake()
	fakeGH.AddRepo(gh.Repo{Owner: "acme", Name: "app", DefaultBranch: "main", CloneURL: "file://" + bare})
	srv, err := controller.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	srv.LoopInterval, srv.SchedulerInterval, srv.GH = 50*time.Millisecond, 50*time.Millisecond, fakeGH
	go srv.Run(ctx)
	<-srv.Ready()
	api, _ := client.New("unix://"+cfg.Listen.Unix, nil)
	e := &workEnv{ctx: ctx, api: api, bare: bare, t: t}

	tmpl := fmt.Sprintf(`name: duo
arch: %s
min: {vcpu: 1, ram: 256MiB, disk: 1GiB}
max_sessions: 2
mise_toml: ""
agent:
  start: [%q, "{{.Prompt}}"]
  env: {BARRIER: %q}
`, ec2Arch(), agentPath, barrier)
	if _, err := api.CreateTemplate(ctx, []byte(tmpl)); err != nil {
		t.Fatal(err)
	}
	tok, err := api.EnrollToken(ctx, "duo", 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "rroot")
	if _, err := runner.Bootstrap(ctx, runner.BootstrapOptions{Controller: "http://" + srv.EnrollAddr(), Token: tok.Token,
		Root: root, IMDSURL: "http://127.0.0.1:1", NoSystemd: true, NoUser: true}); err != nil {
		t.Fatal(err)
	}
	rcfg, _ := runner.LoadConfig(filepath.Join(root, "etc/wrangl/runner.yaml"))
	go runner.Run(ctx, rcfg, runner.DaemonOptions{HeartbeatInterval: 50 * time.Millisecond,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), InstallMise: runnertest.MiseInstaller(nil)})

	if _, err := api.CreateProject(ctx, proto.ProjectRequest{Slug: "web", Template: ptr("duo")}); err != nil {
		t.Fatal(err)
	}
	if _, err := api.LinkRepo(ctx, "web", "acme/app", ""); err != nil {
		t.Fatal(err)
	}
	a := e.mk(proto.TaskCreateRequest{Title: "first independent task", Prompt: "do a"})
	b := e.mk(proto.TaskCreateRequest{Title: "second independent task", Prompt: "do b"})
	runA, _, err := api.QueueTask(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	runB, _, err := api.QueueTask(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	va := e.waitRun(runA, "succeeded", "failed", "blocked")
	vb := e.waitRun(runB, "succeeded", "failed", "blocked")
	if va.State != "succeeded" || vb.State != "succeeded" {
		t.Fatalf("runs: %+v / %+v", va, vb)
	}
	if va.Runner == "" || va.Runner != vb.Runner {
		t.Fatalf("both runs belong on the one runner: %q vs %q", va.Runner, vb.Runner)
	}
	for _, id := range []int64{runA, runB} {
		if _, err := os.Stat(filepath.Join(barrier, fmt.Sprintf("saw-both.%d", id))); err != nil {
			t.Fatalf("run %d never overlapped with the other: the runner ran them one at a time", id)
		}
	}
	da, _ := os.ReadFile(filepath.Join(barrier, fmt.Sprintf("dir.%d", runA)))
	db, _ := os.ReadFile(filepath.Join(barrier, fmt.Sprintf("dir.%d", runB)))
	if strings.TrimSpace(string(da)) == strings.TrimSpace(string(db)) || len(da) == 0 {
		t.Fatalf("each session needs its own workspace: %q vs %q", da, db)
	}
	for sq, want := range map[string]string{a: "in_review", b: "in_review"} {
		if got := e.task(sq).State; got != want {
			t.Fatalf("task %s is %s, want %s", sq, got, want)
		}
	}
}
