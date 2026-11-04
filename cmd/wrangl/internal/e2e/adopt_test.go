package e2e

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
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

func TestAdoptAgentOpenedPRs(t *testing.T) {
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
	srv.LoopInterval, srv.SchedulerInterval, srv.GH, srv.ReconcileInterval = 50*time.Millisecond, 50*time.Millisecond, fake, 100*time.Millisecond
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

	mkBranch := func(name string) { git(t, dir, "--git-dir", bare, "branch", name, "main") }

	// A run that fails; the agent had opened its own PR meanwhile.
	c := e.mk(proto.TaskCreateRequest{Title: "doomed", Prompt: "EXPLODE"})
	run, _, _ := api.QueueTask(ctx, c)
	e.waitRun(run, "failed", "blocked")
	br := "wrangl/" + c + "-1"
	mkBranch(br)
	own, err := fake.CreatePR(ctx, gh.Repo{Owner: "acme", Name: "app"}, br, "main", "agent work", "")
	if err != nil {
		t.Fatal(err)
	}
	// Titled PR for a second task, a fork PR, and one naming no known task: only the first two
	// count, and the fork is never adopted.
	d := e.mk(proto.TaskCreateRequest{Title: "titled", Prompt: "EXPLODE"})
	run, _, _ = api.QueueTask(ctx, d)
	e.waitRun(run, "failed", "blocked")
	mkBranch("feature-d")
	titled, _ := fake.CreatePR(ctx, gh.Repo{Owner: "acme", Name: "app"}, "feature-d", "main", "["+d+"] by title", "")
	e2 := e.mk(proto.TaskCreateRequest{Title: "forked", Prompt: "EXPLODE"})
	run, _, _ = api.QueueTask(ctx, e2)
	e.waitRun(run, "failed", "blocked")
	mkBranch("wrangl/" + e2 + "-1")
	fork, _ := fake.CreatePR(ctx, gh.Repo{Owner: "acme", Name: "app"}, "wrangl/"+e2+"-1", "main", "fork", "")
	fake.SetFork(fork.Number)
	mkBranch("stranger")
	fake.CreatePR(ctx, gh.Repo{Owner: "acme", Name: "app"}, "stranger", "main", "[zzzzzz] unknown", "")

	waitFor(t, "pr_review items for adopted PRs", 30*time.Second, func() (bool, string) {
		qs, _ := api.Questions(ctx, false)
		got := map[string]bool{}
		for _, x := range qs {
			if x.Kind == "pr_review" {
				got[x.Task] = true
			}
		}
		return got[c] && got[d], fmt.Sprint(got)
	})
	time.Sleep(500 * time.Millisecond) // several more reconcile passes: adoption is idempotent
	prs, _ := api.PRs(ctx, "all")
	byNum := map[int64]proto.PRView{}
	for _, p := range prs {
		byNum[p.Number] = p
	}
	if len(prs) != 2 || byNum[own.Number].Task != c || byNum[titled.Number].Task != d {
		t.Fatalf("adopted PRs: %+v", prs)
	}
	// The failed run no longer needs its own item: the PR is what awaits the owner.
	qs, _ := api.Questions(ctx, false)
	for _, x := range qs {
		if x.Kind == "task_review" && (x.Task == c || x.Task == d) {
			t.Errorf("task_review still open for %s", x.Task)
		}
	}
}
