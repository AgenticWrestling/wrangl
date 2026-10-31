package e2e

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/nreid/devhost/cmd/wrangl/internal/client"
	"github.com/nreid/devhost/cmd/wrangl/internal/config"
	"github.com/nreid/devhost/cmd/wrangl/internal/controller"
	"github.com/nreid/devhost/cmd/wrangl/internal/controller/gh"
	"github.com/nreid/devhost/cmd/wrangl/internal/proto"
	"github.com/nreid/devhost/cmd/wrangl/internal/runner"
	"github.com/nreid/devhost/cmd/wrangl/internal/runner/runnertest"
)

// sessionAgent stands in for an interactive agent session (a TUI that never exits by
// itself): it asks a first-run trust question, checks its login file, then works task
// after task, asking the Stop hook for the next prompt each time it finishes.
const sessionAgent = `#!/bin/sh
tool() { curl -s --unix-socket "$WRANGL_SOCK" -d "$2" "http://x/tool/$1"; }
echo "fake session start"
printf 'Do you trust the files in this folder? (y/n) '
read ans
[ "$ans" = "y" ] || { echo "untrusted: $ans"; exit 4; }
if [ "$(cat "$HOME/.fake/creds.json" 2>/dev/null)" = '{"login":"full"}' ]; then echo "creds ok"; else echo "creds missing"; exit 5; fi
prompt="$1"
while :; do
  first=$(printf '%s' "$prompt" | head -1)
  sqid=$(printf '%s' "$first" | sed -n 's/^You are working on task \([A-Za-z0-9]*\):.*/\1/p; s/.*for task \([A-Za-z0-9]*\) .*/\1/p')
  echo "turn on $sqid: $first"
  printf '%s\n' "$first" >> NOTES.md
  case "$first" in
    "You ended"*) tool task_complete "{\"sqid\":\"$sqid\",\"summary\":\"after nudge\"}" >/dev/null;;
    *FORGET*) echo "forgetting";;
    *) tool task_complete "{\"sqid\":\"$sqid\",\"summary\":\"did $sqid\"}" >/dev/null;;
  esac
  out=$(curl -s --unix-socket "$WRANGL_SOCK" -d '{"event":"Stop","payload":{"stop_hook_active":true}}' 'http://x/hook?continue=claude')
  case "$out" in *'"continue":true'*) ;; *) echo "session idle"; sleep 600;; esac
  prompt=$(printf '%s' "$out" | sed 's/.*"prompt":"//; s/\\n.*//; s/"}$//')
done
`

func TestInteractiveDrive(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs linux")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	old := runner.InteractiveEndGrace
	runner.InteractiveEndGrace = 300 * time.Millisecond
	defer func() { runner.InteractiveEndGrace = old }()
	dir := t.TempDir()
	bare := bareRepo(t, dir)
	agentPath := filepath.Join(dir, "session-agent")
	os.WriteFile(agentPath, []byte(sessionAgent), 0o755)
	data := filepath.Join(dir, "data")
	if _, err := controller.Init(ctx, data, filepath.Join(dir, "c.yaml")); err != nil {
		t.Fatal(err)
	}
	controller.SecretSet(data, "fake_login", strings.NewReader(`{"login":"full"}`))
	cfg := config.DefaultController()
	cfg.DataDir = data
	cfg.Listen = config.Listen{HTTP: "127.0.0.1:0", Unix: filepath.Join(dir, "api.sock"), NATS: "127.0.0.1:-1", Enroll: "127.0.0.1:0"}
	fake := gh.NewFake()
	fake.AddRepo(gh.Repo{Owner: "acme", Name: "app", DefaultBranch: "main", CloneURL: "file://" + bare})
	srv, err := controller.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	srv.LoopInterval, srv.SchedulerInterval, srv.GH = 50*time.Millisecond, 50*time.Millisecond, fake
	srvDone := make(chan error, 1)
	go func() { srvDone <- srv.Run(ctx) }()
	<-srv.Ready()
	api, _ := client.New("unix://"+cfg.Listen.Unix, nil)
	e := &workEnv{ctx: ctx, api: api, bare: bare, t: t}

	bad := "name: bad\narch: arm64\nmin: {vcpu: 1, ram: 1GiB, disk: 1GiB}\nagent:\n  drive: sideways\n  start: [x]\n"
	if _, err := api.CreateTemplate(ctx, []byte(bad)); err == nil || !strings.Contains(err.Error(), "agent.drive") {
		t.Fatalf("bad drive accepted: %v", err)
	}
	tmpl := fmt.Sprintf(`name: sess
arch: %s
min: {vcpu: 1, ram: 256MiB, disk: 1GiB}
mise_toml: ""
agent:
  drive: interactive
  start: [%q, "{{.Prompt}}"]
  expect:
    - {match: "trust the files in this folder", send: "y\r"}
  secret_files:
    - {path: "{{.Home}}/.fake/creds.json", secret: fake_login}
`, ec2Arch(), agentPath)
	if _, err := api.CreateTemplate(ctx, []byte(tmpl)); err != nil {
		t.Fatal(err)
	}
	tok, _ := api.EnrollToken(ctx, "sess", 10*time.Minute)
	root := filepath.Join(dir, "rroot")
	if _, err := runner.Bootstrap(ctx, runner.BootstrapOptions{Controller: "http://" + srv.EnrollAddr(), Token: tok.Token,
		Root: root, IMDSURL: "http://127.0.0.1:1", NoSystemd: true, NoUser: true}); err != nil {
		t.Fatal(err)
	}
	rcfg, _ := runner.LoadConfig(filepath.Join(root, "etc/wrangl/runner.yaml"))
	dctx, stopDaemon := context.WithCancel(ctx)
	ddone := make(chan struct{})
	go func() {
		runner.Run(dctx, rcfg, runner.DaemonOptions{HeartbeatInterval: 50 * time.Millisecond,
			Log: slog.New(slog.NewTextHandler(io.Discard, nil)), InstallMise: runnertest.MiseInstaller(nil)})
		close(ddone)
	}()
	if _, err := api.CreateProject(ctx, proto.ProjectRequest{Slug: "web", Template: ptr("sess")}); err != nil {
		t.Fatal(err)
	}
	if _, err := api.LinkRepo(ctx, "web", "acme/app", ""); err != nil {
		t.Fatal(err)
	}

	parent := e.mk(proto.TaskCreateRequest{Title: "three steps"})
	for _, title := range []string{"step one", "step FORGET two", "step three"} {
		e.mk(proto.TaskCreateRequest{Title: title, Parent: parent})
	}
	runID, _, err := api.QueueTask(ctx, parent)
	if err != nil {
		t.Fatal(err)
	}
	v := e.waitRun(runID, "succeeded", "failed", "blocked")
	var all strings.Builder
	api.Logs(ctx, runID, false, "", "", "", func(l proto.LogView) { all.WriteString(l.Message + "\n") })
	text := all.String()
	if v.State != "succeeded" {
		t.Fatalf("run: %+v\n%s", v, text)
	}
	for _, want := range []string{"creds ok", "expect \"trust the files in this folder\" matched", "(same session)", "nudging in the session", "ending the interactive session", "wrote secret file"} {
		if !strings.Contains(text, want) {
			t.Errorf("log missing %q:\n%s", want, text)
		}
	}
	if n := strings.Count(text, "fake session start"); n != 1 {
		t.Errorf("want one agent session for the whole assignment, got %d:\n%s", n, text)
	}
	for _, c := range e.task(parent).Children {
		if c.State != "in_review" && c.State != "done" {
			t.Errorf("child %s %q: %s", c.Sqid, c.Title, c.State)
		}
	}
	notes := git(t, dir, "--git-dir", bare, "show", v.Branch+":NOTES.md")
	if strings.Count(notes, "You are working on task") != 3 || !strings.Contains(notes, "You ended your turn") {
		t.Errorf("NOTES.md on the branch:\n%s", notes)
	}
	commits := git(t, dir, "--git-dir", bare, "log", "--format=%s", v.Branch)
	for _, title := range []string{"] step one", "] step FORGET two", "] step three"} {
		if !strings.Contains(commits, title) {
			t.Errorf("no checkpoint commit for %q:\n%s", title, commits)
		}
	}
	if _, err := os.Stat(filepath.Join(rcfg.AgentHome, ".fake/creds.json")); !os.IsNotExist(err) {
		t.Errorf("secret file left behind after the run (%v)", err)
	}
	stopDaemon()
	<-ddone
	cancel()
	if err := <-srvDone; err != nil {
		t.Fatal(err)
	}
}
