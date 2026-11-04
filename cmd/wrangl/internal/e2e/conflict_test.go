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

// The fake agent edits the line of CONFLICT.txt that main also edits; for a Smart Merge it merges
// origin/main, combines both sides and commits the merge (the runner pushes it).
const conflictAgent = `#!/bin/sh
tool() { curl -s --unix-socket "$WRANGL_SOCK" -d "$2" "http://x/tool/$1"; }
case "$1" in
  "Review pull request"*)
    tool submit_review '{"risk":"low","summary_md":"Reviewed by the fake agent.","concerns":[]}'
    echo reviewed; exit 0;;
  "Smart Merge:"*)
    git fetch -q origin main
    git merge origin/main >/dev/null 2>&1 || true
    printf 'greeting: feature and main\n' > CONFLICT.txt
    git add CONFLICT.txt
    git commit -q --no-edit
    echo "resolved CONFLICT.txt"; exit 0;;
esac
printf 'greeting: feature\n' > CONFLICT.txt
tool task_complete "{\"sqid\":\"$WRANGL_TASK\",\"summary\":\"did $WRANGL_TASK\"}" >/dev/null
echo "worked $WRANGL_TASK"
`

// TestSmartMergeResolvesConflictsEndToEnd: the PR branch and main edit the same line, GitHub
// (the fake) reports the PR as conflicted, the owner clicks Smart Merge, a resolve run merges
// main into the branch (a merge commit pushed as a fast-forward, no force) and the PR is
// re-evaluated at the new head.
func TestSmartMergeResolvesConflictsEndToEnd(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs linux")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()
	bare := bareRepo(t, dir)
	seed := filepath.Join(dir, "seed")
	os.WriteFile(filepath.Join(seed, "CONFLICT.txt"), []byte("greeting: hello\n"), 0o644)
	git(t, seed, "add", ".")
	git(t, seed, "commit", "-q", "-m", "add CONFLICT.txt")
	git(t, seed, "push", "-q", "origin", "HEAD:main")
	agentPath := filepath.Join(dir, "conflict-agent")
	os.WriteFile(agentPath, []byte(conflictAgent), 0o755)

	data := filepath.Join(dir, "data")
	if _, err := controller.Init(ctx, data, filepath.Join(dir, "c.yaml")); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultController()
	cfg.Web.PublicURL = "https://ctrl.example.com"
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
	prFor := func(sqid string) proto.PRView {
		ps, _ := api.PRs(ctx, "all")
		for _, p := range ps {
			if p.Task == sqid {
				return p
			}
		}
		return proto.PRView{}
	}
	prQuestion := func(sqid string, after int64) proto.QuestionView {
		t.Helper()
		var q proto.QuestionView
		waitFor(t, "pr_review question", 30*time.Second, func() (bool, string) {
			qs, _ := api.Questions(ctx, false)
			for _, x := range qs {
				if x.Kind == "pr_review" && x.Task == sqid && x.ID > after {
					q = x
					return true, ""
				}
			}
			return false, fmt.Sprintf("pr=%+v", prFor(sqid))
		})
		return q
	}
	revParse := func(ref string) string { return git(t, dir, "--git-dir", bare, "rev-parse", ref) }
	isAncestor := func(a, b string) bool {
		return exec.Command("git", "--git-dir", bare, "merge-base", "--is-ancestor", a, b).Run() == nil
	}

	// The feature edits the greeting; its PR is opened and reviewed, and waits for the owner.
	sqid := e.mk(proto.TaskCreateRequest{Title: "change the greeting"})
	api.QueueTask(ctx, sqid)
	q1 := prQuestion(sqid, 0)
	pr := prFor(sqid)
	oldHead := revParse(pr.Branch)

	// Meanwhile main edits the same line, and GitHub reports the PR as conflicted.
	git(t, seed, "pull", "-q", "origin", "main")
	os.WriteFile(filepath.Join(seed, "CONFLICT.txt"), []byte("greeting: hi there\n"), 0o644)
	git(t, seed, "commit", "-q", "-am", "change the greeting on main")
	git(t, seed, "push", "-q", "origin", "HEAD:main")
	mainHead := revParse("main")
	fake.SetMergeable(pr.Number, gh.MergeConflicted)

	if err := api.PRAction(ctx, pr.ID, "resolve", ""); err != nil {
		t.Fatal(err)
	}
	fake.SetMergeable(pr.Number, gh.MergeClean) // what GitHub reports once the merge is pushed

	var rr proto.RunView
	waitFor(t, "resolve run", 60*time.Second, func() (bool, string) {
		rs, _ := api.Runs(ctx, "", "", "web", 100)
		for _, r := range rs {
			if r.Kind == "resolve" && r.Task == sqid {
				rr = r
				return r.State == "succeeded" || r.State == "failed", r.State + " " + r.Error
			}
		}
		return false, "no resolve run"
	})
	if rr.State != "succeeded" || !rr.Pushed {
		t.Fatalf("resolve run: %+v", rr)
	}

	// A merge commit on the PR branch: two parents, a plain fast-forward of the old head (so no
	// force-push), containing main, with both sides' intent in the file.
	newHead := revParse(pr.Branch)
	if newHead == oldHead {
		t.Fatal("PR branch did not move")
	}
	if parents := strings.Fields(git(t, dir, "--git-dir", bare, "rev-list", "--parents", "-n1", newHead)); len(parents) != 3 {
		t.Fatalf("want a merge commit, got %v", parents)
	}
	if !isAncestor(oldHead, newHead) || !isAncestor(mainHead, newHead) {
		t.Fatalf("new head %s must contain the old head %s and main %s", newHead, oldHead, mainHead)
	}
	if got := git(t, dir, "--git-dir", bare, "show", pr.Branch+":CONFLICT.txt"); got != "greeting: feature and main" {
		t.Fatalf("resolved file: %q", got)
	}
	if got := revParse("main"); got != mainHead {
		t.Fatalf("main moved: %s", got)
	}

	// The PR is re-evaluated at the new head: the old review card is gone and a fresh review
	// produced a new one; the state is clean again.
	q2 := prQuestion(sqid, q1.ID)
	if q2.ID == q1.ID {
		t.Fatal("no fresh review card")
	}
	waitFor(t, "PR head re-evaluated", 30*time.Second, func() (bool, string) {
		var sha, mergeable string
		e.db.R.QueryRow(`SELECT head_sha, mergeable FROM pull_request WHERE gh_number = ?`, pr.Number).Scan(&sha, &mergeable)
		return sha == newHead && mergeable == "clean", sha + " " + mergeable
	})
	var title string
	e.db.R.QueryRow(`SELECT title FROM log_entry WHERE kind = 'pr' AND title LIKE 'Smart Merge resolved%'`).Scan(&title)
	if title == "" {
		t.Fatal("no outcome on the PR timeline")
	}

	stopDaemon()
	<-ddone
	cancel()
	if err := <-srvDone; err != nil {
		t.Fatal(err)
	}
}
