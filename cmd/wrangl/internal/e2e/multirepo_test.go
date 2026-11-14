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

// multiAgent is fakeAgent plus a look at the sibling repo: it records WRANGL_READ_REPOS and a listing of the
// first read-only checkout in NOTES.md, which the runner commits on the run's branch.
const multiAgent = `#!/bin/sh
printf '%s\n' "$1" | grep 'You are working on task' >> NOTES.md
if [ -n "$WRANGL_READ_REPOS" ]; then
  printf 'READ_REPOS %s\n' "$WRANGL_READ_REPOS" >> NOTES.md
  p=$(printf '%s' "$WRANGL_READ_REPOS" | sed 's/.*"path":"\([^"]*\)".*/\1/')
  printf 'SIBLING_FILES %s\n' "$(ls "$p" | tr '\n' ' ')" >> NOTES.md
fi
curl -s --unix-socket "$WRANGL_SOCK" -d "{\"sqid\":\"$WRANGL_TASK\",\"summary\":\"did $WRANGL_TASK\"}" "http://x/tool/task_complete" >/dev/null
`

// A two-repo project end to end: a milestone with a leaf in each repo and one with no repo is queued; it
// becomes one run per repo, each run reads the other repo, each pushes and opens its own PR, and the
// repo-less leaf waits with a needs_repo item until the owner picks a repo.
func TestMultiRepoProjectEndToEnd(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs linux (PTY, controller-served runner binary)")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "a"), 0o755)
	os.MkdirAll(filepath.Join(dir, "b"), 0o755)
	bareA, bareB := bareRepo(t, filepath.Join(dir, "a")), bareRepo(t, filepath.Join(dir, "b"))
	agentPath := filepath.Join(dir, "fake-agent")
	os.WriteFile(agentPath, []byte(multiAgent), 0o755)

	if _, err := controller.Init(ctx, filepath.Join(dir, "data"), filepath.Join(dir, "c.yaml")); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultController()
	cfg.DataDir = filepath.Join(dir, "data")
	cfg.Listen = config.Listen{HTTP: "127.0.0.1:0", Unix: filepath.Join(dir, "api.sock"), NATS: "127.0.0.1:-1", Enroll: "127.0.0.1:0"}
	fakeGH := gh.NewFake()
	fakeGH.AddRepo(gh.Repo{Owner: "acme", Name: "api", DefaultBranch: "main", CloneURL: "file://" + bareA})
	fakeGH.AddRepo(gh.Repo{Owner: "acme", Name: "web", DefaultBranch: "main", CloneURL: "file://" + bareB})
	srv, err := controller.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	srv.LoopInterval, srv.SchedulerInterval, srv.GH = 50*time.Millisecond, 50*time.Millisecond, fakeGH
	go srv.Run(ctx)
	<-srv.Ready()
	api, _ := client.New("unix://"+cfg.Listen.Unix, nil)
	e := &workEnv{ctx: ctx, api: api, t: t, db: srv.Fleet.DB}

	tmpl := fmt.Sprintf(`name: tiny
arch: %s
min: {vcpu: 1, ram: 256MiB, disk: 1GiB}
max_sessions: 1
mise_toml: ""
agent:
  start: [%q, "{{.Prompt}}"]
`, ec2Arch(), agentPath)
	if _, err := api.CreateTemplate(ctx, []byte(tmpl)); err != nil {
		t.Fatal(err)
	}
	tok, err := api.EnrollToken(ctx, "tiny", 10*time.Minute)
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

	if _, err := api.CreateProject(ctx, proto.ProjectRequest{Slug: "web", Template: ptr("tiny")}); err != nil {
		t.Fatal(err)
	}
	for _, r := range []string{"acme/api", "acme/web"} {
		if _, err := api.LinkRepo(ctx, "web", r, ""); err != nil {
			t.Fatal(err)
		}
	}

	ms := e.mk(proto.TaskCreateRequest{Title: "ship the feature", Type: "milestone"})
	apiLeaf := e.mk(proto.TaskCreateRequest{Parent: ms, Title: "api part", Repo: "acme/api"})
	webLeaf := e.mk(proto.TaskCreateRequest{Parent: ms, Title: "web part", Repo: "acme/web"})
	loose := e.mk(proto.TaskCreateRequest{Parent: ms, Title: "docs part"})
	if d := e.task(loose); d.Repo != "" || d.EffectiveRepo != "" {
		t.Fatalf("the loose leaf already resolves: %+v", d)
	}

	// Queueing the mixed milestone makes one run per repo, plus one waiting for a repo.
	if _, _, err := api.QueueTask(ctx, ms); err != nil {
		t.Fatal(err)
	}
	type runRow struct {
		id     int64
		repo   string
		state  string
		status string
	}
	runs := func() []runRow {
		rows, err := srv.Fleet.DB.R.QueryContext(ctx, `SELECT r.id, COALESCE(p.owner || '/' || p.name, ''), r.state, r.status_text FROM run r JOIN task t ON t.id = r.task_id LEFT JOIN repo p ON p.id = r.repo_id WHERE t.sqid = ? AND r.kind = 'work' ORDER BY r.id`, ms)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []runRow
		for rows.Next() {
			var x runRow
			rows.Scan(&x.id, &x.repo, &x.state, &x.status)
			out = append(out, x)
		}
		return out
	}
	if rs := runs(); len(rs) != 3 {
		t.Fatalf("want one run per repo plus one without: %+v", rs)
	}
	waitFor(t, "the two repo runs to finish and the third to wait", 40*time.Second, func() (bool, string) {
		n, waiting := 0, false
		for _, r := range runs() {
			switch {
			case r.repo != "" && r.state == "succeeded":
				n++
			case r.repo == "" && r.state == "queued" && r.status == "waiting: needs a repo":
				waiting = true
			}
		}
		return n == 2 && waiting, fmt.Sprintf("%+v", runs())
	})

	// One PR per repo, each on its own repo's branch; each branch carries the notes of the sibling read.
	var prs []proto.PRView
	waitFor(t, "a PR per repo", 20*time.Second, func() (bool, string) {
		prs, _ = api.PRs(ctx, "")
		return len(prs) >= 2, fmt.Sprintf("%d PRs", len(prs))
	})
	byRepo := map[string]int{}
	for _, p := range prs {
		byRepo[p.Repo]++
	}
	if len(prs) != 2 || byRepo["acme/api"] != 1 || byRepo["acme/web"] != 1 {
		t.Fatalf("PRs: %+v", prs)
	}
	for _, rr := range runs() {
		if rr.repo == "" {
			continue
		}
		bare, sibling := bareA, "web"
		if rr.repo == "acme/web" {
			bare, sibling = bareB, "api"
		}
		v := e.run(rr.id)
		notes := git(t, dir, "--git-dir", bare, "show", v.Branch+":NOTES.md")
		if !strings.Contains(notes, "READ_REPOS") || !strings.Contains(notes, "acme/"+sibling) || !strings.Contains(notes, "refs/"+fmt.Sprint(rr.id)+"/"+sibling) || !strings.Contains(notes, "SIBLING_FILES README.md") {
			t.Fatalf("run %d (%s) did not read the sibling repo %s:\n%s", rr.id, rr.repo, sibling, notes)
		}
		want := map[string]string{"acme/api": apiLeaf, "acme/web": webLeaf}[rr.repo]
		if !strings.Contains(notes, "You are working on task "+want) || strings.Contains(notes, "task "+loose) {
			t.Fatalf("run %d took the wrong leaves:\n%s", rr.id, notes)
		}
	}

	// The needs_repo item is in the Inbox; answering with a repo schedules the waiting run.
	qs, err := api.Questions(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	var item *proto.QuestionView
	for i, q := range qs {
		if q.Kind == "needs_repo" {
			item = &qs[i]
		}
	}
	if item == nil {
		t.Fatalf("no needs_repo item in the Inbox: %+v", qs)
	}
	if err := api.Answer(ctx, item.ID, "repo:acme/web", ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the repo-less run to finish in the chosen repo", 40*time.Second, func() (bool, string) {
		for _, r := range runs() {
			if r.repo == "acme/web" && r.state == "succeeded" && r.id == runs()[2].id {
				return true, ""
			}
		}
		return false, fmt.Sprintf("%+v", runs())
	})
	if d := e.task(ms); d.State == "" {
		t.Fatal("milestone vanished")
	}
	waitFor(t, "a third PR", 20*time.Second, func() (bool, string) {
		prs, _ = api.PRs(ctx, "")
		return len(prs) == 3, fmt.Sprintf("%d PRs", len(prs))
	})
}
