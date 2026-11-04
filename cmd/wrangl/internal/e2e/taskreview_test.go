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

// reviewAgent changes no files: it completes its task (no push, so no PR) or fails.
const reviewAgent = `#!/bin/sh
case "$1" in *EXPLODE*) echo "boom"; exit 3;; esac
curl -s --unix-socket "$WRANGL_SOCK" -d "{\"sqid\":\"$WRANGL_TASK\",\"summary\":\"report for $WRANGL_TASK\"}" http://x/tool/task_complete >/dev/null
echo "no changes needed"
`

func TestTaskReviewInbox(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs linux")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()
	bare := bareRepo(t, dir)
	agentPath := filepath.Join(dir, "review-agent")
	os.WriteFile(agentPath, []byte(reviewAgent), 0o755)
	if _, err := controller.Init(ctx, filepath.Join(dir, "data"), filepath.Join(dir, "c.yaml")); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultController()
	cfg.DataDir = filepath.Join(dir, "data")
	cfg.Listen = config.Listen{HTTP: "127.0.0.1:0", Unix: filepath.Join(dir, "api.sock"), NATS: "127.0.0.1:-1", Enroll: "127.0.0.1:0"}
	fake := gh.NewFake()
	fake.AddRepo(gh.Repo{Owner: "acme", Name: "app", DefaultBranch: "main", CloneURL: "file://" + bare})
	srv, err := controller.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	srv.LoopInterval, srv.SchedulerInterval, srv.GH = 50*time.Millisecond, 50*time.Millisecond, fake
	go srv.Run(ctx)
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
	dctx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		runner.Run(dctx, rcfg, runner.DaemonOptions{HeartbeatInterval: 50 * time.Millisecond,
			Log: slog.New(slog.NewTextHandler(io.Discard, nil)), InstallMise: runnertest.MiseInstaller(nil)})
		close(done)
	}()
	defer func() { stop(); <-done }()
	if _, err := api.CreateProject(ctx, proto.ProjectRequest{Slug: "web", Template: ptr("tiny")}); err != nil {
		t.Fatal(err)
	}
	if _, err := api.LinkRepo(ctx, "web", "acme/app", ""); err != nil {
		t.Fatal(err)
	}

	review := func(sqid string, open bool) proto.QuestionView {
		t.Helper()
		var q proto.QuestionView
		waitFor(t, "task_review for "+sqid, 30*time.Second, func() (bool, string) {
			qs, _ := api.Questions(ctx, false)
			for _, x := range qs {
				if x.Kind == "task_review" && x.Task == sqid {
					q = x
					return open, ""
				}
			}
			return !open, fmt.Sprint(len(qs), " open questions")
		})
		return q
	}

	// 1. A run that changed nothing leaves the task in review with no PR: it is in the Inbox.
	a := e.mk(proto.TaskCreateRequest{Title: "audit the config", Prompt: "Report on the config."})
	run, _, err := api.QueueTask(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if v := e.waitRun(run, "succeeded", "failed", "blocked"); v.Pushed {
		t.Fatalf("run pushed: %+v", v)
	}
	q := review(a, true)
	if q.RunID != run || !strings.Contains(q.DetailMD, "report for "+a) || !strings.Contains(q.DetailMD, fmt.Sprintf("/run/%d", run)) {
		t.Fatalf("question: %+v", q)
	}
	if e.task(a).State != "in_review" {
		t.Fatalf("state %s", e.task(a).State)
	}
	// Reopen needs a note; with one, the task reopens and the note lands in its prompt.
	if err := api.Answer(ctx, q.ID, "reopen", ""); err == nil {
		t.Fatal("reopen without a note accepted")
	}
	q = review(a, true)
	if err := api.Answer(ctx, q.ID, "reopen", "Also cover the TLS settings."); err != nil {
		t.Fatal(err)
	}
	if d := e.task(a); d.State != "open" || !strings.Contains(d.Prompt, "Also cover the TLS settings.") || !strings.Contains(d.Prompt, "Report on the config.") {
		t.Fatalf("after reopen: %s %q", d.State, d.Prompt)
	}
	// Run it again, then accept: the task is done and the item is gone.
	run, _, err = api.QueueTask(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	e.waitRun(run, "succeeded")
	q = review(a, true)
	if err := api.Answer(ctx, q.ID, "accept", ""); err != nil {
		t.Fatal(err)
	}
	if got := e.task(a).State; got != "done" {
		t.Fatalf("after accept: %s", got)
	}

	// 2. The item closes by itself when the task leaves review some other way.
	b := e.mk(proto.TaskCreateRequest{Title: "second audit", Prompt: "Report again."})
	run, _, _ = api.QueueTask(ctx, b)
	e.waitRun(run, "succeeded")
	review(b, true)
	if err := api.TaskAction(ctx, b, "complete", ""); err != nil {
		t.Fatal(err)
	}
	review(b, false)

	// 3. A failed run needs the owner too.
	c := e.mk(proto.TaskCreateRequest{Title: "doomed", Prompt: "EXPLODE"})
	run, _, _ = api.QueueTask(ctx, c)
	e.waitRun(run, "failed", "blocked")
	q = review(c, true)
	if !strings.Contains(q.Prompt, "Run ") || q.RunID != run {
		t.Fatalf("failed-run question: %+v", q)
	}
	if err := api.Answer(ctx, q.ID, "cancel", ""); err != nil {
		t.Fatal(err)
	}
	if got := e.task(c).State; got != "cancelled" {
		t.Fatalf("after cancel: %s", got)
	}
	review(c, false)
}
