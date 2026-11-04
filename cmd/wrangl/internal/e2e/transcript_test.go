package e2e

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
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

// transcriptAgent plays a Claude Code session: it copies the transcript fixture to a file, tells
// the runner where it is through a hook's transcript_path (as Claude Code does), and completes.
const transcriptAgent = `#!/bin/sh
tool() { curl -s --unix-socket "$WRANGL_SOCK" -d "$2" "http://x/tool/$1"; }
cp %q %q
curl -s --unix-socket "$WRANGL_SOCK" -d '{"event":"PostToolUse","payload":{"tool_name":"Bash","transcript_path":%q}}' http://x/hook >/dev/null
sleep 1
tool task_complete "{\"sqid\":\"$WRANGL_TASK\",\"summary\":\"done\"}" >/dev/null
`

// The runner tails the agent's transcript, the controller replays it from JetStream, and the
// expanded card renders it as Markdown: bold is <strong>, a fence is <pre><code>, no literal
// markers survive, and the fixture's <script> is inert.
func TestCardRendersTranscriptAsMarkdown(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs linux")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()
	bare := bareRepo(t, dir)
	fixture, err := filepath.Abs("../transcript/testdata/session.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	agentPath := filepath.Join(dir, "tx-agent")
	tpath := filepath.Join(dir, "session.jsonl")
	os.WriteFile(agentPath, []byte(fmt.Sprintf(transcriptAgent, fixture, tpath, tpath)), 0o755)

	data := filepath.Join(dir, "data")
	if _, err := controller.Init(ctx, data, filepath.Join(dir, "c.yaml")); err != nil {
		t.Fatal(err)
	}
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
	e := &workEnv{ctx: ctx, api: api, bare: bare, t: t, db: srv.Fleet.DB}

	tmpl := fmt.Sprintf("name: tiny\narch: %s\nmin: {vcpu: 1, ram: 256MiB, disk: 1GiB}\nmax_sessions: 1\nmise_toml: \"\"\nagent:\n  start: [%q, \"{{.Prompt}}\"]\n", ec2Arch(), agentPath)
	if _, err := api.CreateTemplate(ctx, []byte(tmpl)); err != nil {
		t.Fatal(err)
	}
	tok, _ := api.EnrollToken(ctx, "tiny", 10*time.Minute)
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
	if _, err := api.CreateProject(ctx, proto.ProjectRequest{Slug: "web", Template: ptr("tiny")}); err != nil {
		t.Fatal(err)
	}
	if _, err := api.LinkRepo(ctx, "web", "acme/app", ""); err != nil {
		t.Fatal(err)
	}

	sqid := e.mk(proto.TaskCreateRequest{Title: "explain the tests"})
	runID, _, err := api.QueueTask(ctx, sqid)
	if err != nil {
		t.Fatal(err)
	}
	e.waitRun(runID, "succeeded")

	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", cfg.Listen.Unix)
	}}}
	var card string
	waitFor(t, "the card to show the transcript", 20*time.Second, func() (bool, string) {
		resp, err := hc.Get("http://x/ui/detail?sqid=" + sqid)
		if err != nil {
			return false, err.Error()
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		card = string(b)
		return strings.Contains(card, `class="livetx"`), card
	})
	for _, want := range []string{"<strong>tests</strong>", "<pre><code", `class="txchip"`, "Bash: go test ./..."} {
		if !strings.Contains(card, want) {
			t.Errorf("card missing %q", want)
		}
	}
	for _, bad := range []string{"**tests**", "```", "<script>alert", "supersecretvalue123"} {
		if strings.Contains(card, bad) {
			t.Errorf("card must not contain %q", bad)
		}
	}
	// A finished run opens no live connection, and the old terminal is not used.
	if strings.Contains(card, "transcript-live") || strings.Contains(card, "liveterm") {
		t.Errorf("finished run should show stored entries only")
	}

	stopDaemon()
	<-ddone
	cancel()
	if err := <-srvDone; err != nil {
		t.Fatal(err)
	}
}
