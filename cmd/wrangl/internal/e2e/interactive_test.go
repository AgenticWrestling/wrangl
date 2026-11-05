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

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/client"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/config"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/gh"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/runner"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/runner/runnertest"
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

// agentHarness is a controller, a local runner and project "web" whose template runs a
// script agent: the shared setup for agent-integration tests.
type agentHarness struct {
	*workEnv
	srv       *controller.Server
	dir, bare string
	rcfg      *runner.Config
	stop      func()
}

// newAgentHarness writes script as the agent and creates template tmpl, formatted with
// (arch, agent path).
// harnessOpt adjusts a harness: beforeRunner runs with the controller's data dir before
// the runner starts; daemon edits the runner's options.
type harnessOpt struct {
	beforeRunner func(dataDir string)
	daemon       func(*runner.DaemonOptions)
	// controller adjusts the controller before it starts; its logger is cfgLog (nil: discarded).
	controller func(*controller.Server)
	cfgLog     io.Writer
}

func newAgentHarness(t *testing.T, script, tmplFmt string, secrets map[string]string, opts ...harnessOpt) *agentHarness {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	dir := t.TempDir()
	bare := bareRepo(t, dir)
	agentPath := filepath.Join(dir, "agent")
	os.WriteFile(agentPath, []byte(script), 0o755)
	data := filepath.Join(dir, "data")
	if _, err := controller.Init(ctx, data, filepath.Join(dir, "c.yaml")); err != nil {
		t.Fatal(err)
	}
	for k, v := range secrets {
		controller.SecretSet(data, k, strings.NewReader(v))
	}
	cfg := config.DefaultController()
	cfg.DataDir = data
	cfg.Listen = config.Listen{HTTP: "127.0.0.1:0", Unix: filepath.Join(dir, "api.sock"), NATS: "127.0.0.1:-1", Enroll: "127.0.0.1:0"}
	fake := gh.NewFake()
	fake.AddRepo(gh.Repo{Owner: "acme", Name: "app", DefaultBranch: "main", CloneURL: "file://" + bare})
	var ctlLog io.Writer = io.Discard
	for _, o := range opts {
		if o.cfgLog != nil {
			ctlLog = o.cfgLog
		}
	}
	srv, err := controller.New(cfg, slog.New(slog.NewTextHandler(ctlLog, &slog.HandlerOptions{Level: slog.LevelDebug})))
	if err != nil {
		t.Fatal(err)
	}
	srv.LoopInterval, srv.SchedulerInterval, srv.GH = 50*time.Millisecond, 50*time.Millisecond, fake
	for _, o := range opts {
		if o.controller != nil {
			o.controller(srv)
		}
	}
	srvDone := make(chan error, 1)
	go func() { srvDone <- srv.Run(ctx) }()
	<-srv.Ready()
	api, _ := client.New("unix://"+cfg.Listen.Unix, nil)
	tv, err := api.CreateTemplate(ctx, []byte(fmt.Sprintf(tmplFmt, ec2Arch(), agentPath)))
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := api.EnrollToken(ctx, tv.Name, 10*time.Minute)
	root := filepath.Join(dir, "rroot")
	if _, err := runner.Bootstrap(ctx, runner.BootstrapOptions{Controller: "http://" + srv.EnrollAddr(), Token: tok.Token,
		Root: root, IMDSURL: "http://127.0.0.1:1", NoSystemd: true, NoUser: true}); err != nil {
		t.Fatal(err)
	}
	rcfg, _ := runner.LoadConfig(filepath.Join(root, "etc/wrangl/runner.yaml"))
	dopts := runner.DaemonOptions{HeartbeatInterval: 50 * time.Millisecond,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), InstallMise: runnertest.MiseInstaller(nil)}
	for _, o := range opts {
		if o.beforeRunner != nil {
			o.beforeRunner(data)
		}
		if o.daemon != nil {
			o.daemon(&dopts)
		}
	}
	dctx, stopDaemon := context.WithCancel(ctx)
	ddone := make(chan struct{})
	go func() {
		runner.Run(dctx, rcfg, dopts)
		close(ddone)
	}()
	if _, err := api.CreateProject(ctx, proto.ProjectRequest{Slug: "web", Template: ptr(tv.Name)}); err != nil {
		t.Fatal(err)
	}
	if _, err := api.LinkRepo(ctx, "web", "acme/app", ""); err != nil {
		t.Fatal(err)
	}
	h := &agentHarness{workEnv: &workEnv{ctx: ctx, api: api, bare: bare, t: t, db: srv.Fleet.DB}, srv: srv, dir: dir, bare: bare, rcfg: rcfg}
	h.stop = func() {
		stopDaemon()
		<-ddone
		cancel()
		if err := <-srvDone; err != nil {
			t.Fatal(err)
		}
	}
	return h
}

// runLog is the run's whole log as text.
func (h *agentHarness) runLog(id int64) string {
	var b strings.Builder
	h.api.Logs(h.ctx, id, false, "", "", "", func(l proto.LogView) { b.WriteString(l.Message + "\n") })
	return b.String()
}

func TestInteractiveDrive(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs linux")
	}
	old := runner.InteractiveEndGrace
	runner.InteractiveEndGrace = 300 * time.Millisecond
	defer func() { runner.InteractiveEndGrace = old }()
	h := newAgentHarness(t, sessionAgent, `name: sess
arch: %s
min: {vcpu: 1, ram: 256MiB, disk: 1GiB}
max_sessions: 1
mise_toml: ""
agent:
  drive: interactive
  start: [%q, "{{.Prompt}}"]
  expect:
    - {match: "trust the files in this folder", send: "y\r"}
  secret_files:
    - {path: "{{.Home}}/.fake/creds.json", secret: fake_login}
`, map[string]string{"fake_login": `{"login":"full"}`})
	ctx, api, e, dir, bare, rcfg := h.ctx, h.api, h.workEnv, h.dir, h.bare, h.rcfg
	bad := "name: bad\narch: arm64\nmin: {vcpu: 1, ram: 1GiB, disk: 1GiB}\nmax_sessions: 1\nagent:\n  drive: sideways\n  start: [x]\n"
	if _, err := api.CreateTemplate(ctx, []byte(bad)); err == nil || !strings.Contains(err.Error(), "agent.drive") {
		t.Fatalf("bad drive accepted: %v", err)
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
	text := h.runLog(runID)
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
	h.stop()
}
