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
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/runner"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/runner/runnertest"
)

// The agent runs on the driver with the repo checked out locally, builds on the target through the
// target_exec tool (served by the daemon), records what the target said, and completes its task.
// The key never reaches it: it only has the tool.
const remoteAgent = `#!/bin/sh
out=$(curl -s --unix-socket "$WRANGL_SOCK" -H "X-Wrangl-Run: $WRANGL_RUN_ID" \
  -d '{"cmd":"echo built on the target"}' http://x/tool/target_exec)
printf 'target said: %s\n' "$out" >> NOTES.md
env | grep -c PRIVATE-KEY > keycheck.txt
curl -s --unix-socket "$WRANGL_SOCK" -H "X-Wrangl-Run: $WRANGL_RUN_ID" \
  -d "{\"sqid\":\"$WRANGL_TASK\",\"summary\":\"did $WRANGL_TASK\"}" http://x/tool/task_complete >/dev/null
`

// A run on a remote template gets a ready target (no wrangl on it) and a driver slot together; the
// agent drives the target with target_exec; the run counts against the target's sessions.
func TestRemoteRunDrivesTarget(t *testing.T) {
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
	agentPath := filepath.Join(dir, "remote-agent")
	os.WriteFile(agentPath, []byte(remoteAgent), 0o755)

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
	srv.ProbeTarget = func(context.Context, dbgen.RemoteTarget, proto.Connection) error { return nil }
	go srv.Run(ctx)
	<-srv.Ready()
	api, _ := client.New("unix://"+cfg.Listen.Unix, nil)
	e := &workEnv{ctx: ctx, api: api, bare: bare, t: t}

	// the driver: an ordinary local runner template (its own agent is not used by remote runs)
	if _, err := api.CreateTemplate(ctx, []byte(fmt.Sprintf("name: drv\narch: %s\nmin: {vcpu: 1, ram: 256MiB, disk: 1GiB}\nmax_sessions: 2\nmise_toml: \"\"\nagent:\n  start: [\"true\"]\n", ec2Arch()))); err != nil {
		t.Fatal(err)
	}
	// the target: a remote template; its agent block names the agent that runs on the driver
	mac := fmt.Sprintf(`name: mac
arch: %s
min: {vcpu: 1, ram: 256MiB, disk: 1GiB}
max_runners: 1
max_sessions: 1
mode: remote
connection:
  driver_template: drv
  protocol: ssh
  user: ec2-user
  via: direct
  secret: mac_key
agent:
  start: [%q, "{{.Prompt}}"]
`, ec2Arch(), agentPath)
	if _, err := api.CreateTemplate(ctx, []byte(mac)); err != nil {
		t.Fatal(err)
	}
	if err := controller.SecretSet(cfg.DataDir, "mac_key", strings.NewReader("PRIVATE-KEY-MATERIAL")); err != nil {
		t.Fatal(err)
	}
	// a ready target (EC2 launching is covered by the fleet tests)
	if _, err := srv.Fleet.DB.W.Exec(`INSERT INTO remote_target (name, template_id, instance_id, host, state) VALUES ('mac-1', (SELECT id FROM runner_template WHERE name='mac'), 'i-fake', '10.0.0.9', 'ready')`); err != nil {
		t.Fatal(err)
	}

	tok, err := api.EnrollToken(ctx, "drv", 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "rroot")
	if _, err := runner.Bootstrap(ctx, runner.BootstrapOptions{Controller: "http://" + srv.EnrollAddr(), Token: tok.Token,
		Root: root, IMDSURL: "http://127.0.0.1:1", NoSystemd: true, NoUser: true}); err != nil {
		t.Fatal(err)
	}
	rcfg, _ := runner.LoadConfig(filepath.Join(root, "etc/wrangl/runner.yaml"))
	fake := &runner.FakeTarget{}
	go runner.Run(ctx, rcfg, runner.DaemonOptions{HeartbeatInterval: 50 * time.Millisecond,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), InstallMise: runnertest.MiseInstaller(nil), Targets: fake})

	if _, err := api.CreateProject(ctx, proto.ProjectRequest{Slug: "web", Template: ptr("mac")}); err != nil {
		t.Fatal(err)
	}
	if _, err := api.LinkRepo(ctx, "web", "acme/app", ""); err != nil {
		t.Fatal(err)
	}
	a := e.mk(proto.TaskCreateRequest{Title: "build the ios app", Prompt: "build it"})
	runID, _, err := api.QueueTask(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	v := e.waitRun(runID, "succeeded", "failed")
	if v.State != "succeeded" {
		t.Fatalf("run %s: %s", v.State, v.Error)
	}
	if len(fake.Cmds) != 1 || fake.Cmds[0] != "echo built on the target" {
		t.Fatalf("target commands: %v", fake.Cmds)
	}
	if !strings.HasPrefix(fake.LastKey, "PRIVATE-KEY-MATERIAL") {
		t.Fatalf("the daemon should hold the key for the ssh client, got %q", fake.LastKey)
	}
	notes := git(t, dir, "--git-dir", bare, "show", v.Branch+":NOTES.md")
	if !strings.Contains(notes, "echo built on the target") {
		t.Fatalf("NOTES.md on the branch should carry the target's answer:\n%s", notes)
	}
	if fake.Opened != 1 || fake.Closed != 1 {
		t.Fatalf("tunnel opened %d closed %d", fake.Opened, fake.Closed)
	}
	var bound int64
	srv.Fleet.DB.R.QueryRow(`SELECT target_id FROM run_target WHERE run_id = ?`, runID).Scan(&bound)
	if bound != 1 {
		t.Fatalf("run should be bound to the target, got %d", bound)
	}
}
