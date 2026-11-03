package e2e

import (
	"bytes"
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

	wcli "github.com/agenticwrestling/wrangl/cmd/wrangl/internal/cli"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/client"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/config"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/gh"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/runner"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/runner/runnertest"
)

// fakeAgent stands in for a coding agent: it records its prompt in NOTES.md (which the
// runner checkpoint-commits), prints progress, and misbehaves on request.
const fakeAgent = `#!/bin/sh
first=$(printf '%s' "$1" | head -1)
echo "fake agent starting: $first"
printf '%s\n' "$first" >> NOTES.md
tool() { curl -s --unix-socket "$WRANGL_SOCK" -d "$2" "http://x/tool/$1"; }
# M6 review runs: submit a review and stop (never act on the reviewed task's markers).
case "$first" in "Review pull request"*) tool submit_review '{"risk":"low","summary_md":"ok"}'; exit 0;; esac
case "$1" in *SLOW*) echo "going slow"; sleep 60;; esac
case "$1" in *EXPLODE*) echo "boom"; exit 3;; esac
case "$1" in *ASKME*)
  ans=$(tool ask_question '{"prompt":"Which colour?","options":[{"label":"Red","value":"red"},{"label":"Blue","value":"blue"}]}')
  echo "owner said: $ans";;
esac
case "$1" in *CANCELOTHER*)
  for target in $(printf '%s' "$1" | sed -n 's/.*CANCELOTHER \([A-Za-z0-9]*\).*/\1/p'); do
    echo "cancel attempt: $(tool task_cancel "{\"sqid\":\"$target\",\"reason\":\"redundant\"}")"
  done;;
esac
case "$1" in *FORGET*) echo "forgot to complete"; exit 0;; esac
curl -s --unix-socket "$WRANGL_SOCK" -d '{"event":"PostToolUse","payload":{"tool_name":"Edit"}}' http://x/hook >/dev/null
tool task_complete "{\"sqid\":\"$WRANGL_TASK\",\"summary\":\"did $WRANGL_TASK\"}" >/dev/null
echo "fake agent finished needle-$WRANGL_TASK"
`

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "init.defaultBranch=main"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// bareRepo makes file://<dir>/app.git with one commit on main.
func bareRepo(t *testing.T, dir string) string {
	t.Helper()
	bare := filepath.Join(dir, "app.git")
	git(t, dir, "init", "-q", "--bare", bare)
	seed := filepath.Join(dir, "seed")
	git(t, dir, "clone", "-q", bare, seed)
	os.WriteFile(filepath.Join(seed, "README.md"), []byte("# app\n"), 0o644)
	git(t, seed, "add", ".")
	git(t, seed, "commit", "-q", "-m", "initial")
	git(t, seed, "push", "-q", "origin", "HEAD:main")
	return bare
}

type workEnv struct {
	ctx  context.Context
	api  *client.Client
	bare string
	t    *testing.T
}

func (e *workEnv) run(id int64) proto.RunView {
	v, err := e.api.Run(e.ctx, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return v
}

func (e *workEnv) waitRun(id int64, states ...string) proto.RunView {
	e.t.Helper()
	var v proto.RunView
	waitFor(e.t, fmt.Sprintf("run %d in %v", id, states), 20*time.Second, func() (bool, string) {
		v = e.run(id)
		for _, s := range states {
			if v.State == s {
				return true, ""
			}
		}
		return false, fmt.Sprintf("state=%s status=%q error=%q", v.State, v.Status, v.Error)
	})
	return v
}

func (e *workEnv) task(sqid string) proto.TaskDetail {
	d, err := e.api.Task(e.ctx, sqid)
	if err != nil {
		e.t.Fatal(err)
	}
	return d
}

func (e *workEnv) mk(req proto.TaskCreateRequest) string {
	e.t.Helper()
	req.Project = "web"
	t, err := e.api.CreateTask(e.ctx, req)
	if err != nil {
		e.t.Fatalf("create %s: %v", req.Title, err)
	}
	return t.Sqid
}

func TestWorkLoopEndToEnd(t *testing.T) {
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
	agentPath := filepath.Join(dir, "fake-agent")
	os.WriteFile(agentPath, []byte(fakeAgent), 0o755)

	// Controller with a fake GitHub whose repo clones from the local bare repo.
	if _, err := controller.Init(ctx, filepath.Join(dir, "data"), filepath.Join(dir, "c.yaml")); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultController()
	cfg.DataDir = filepath.Join(dir, "data")
	cfg.Listen = config.Listen{HTTP: "127.0.0.1:0", Unix: filepath.Join(dir, "api.sock"), NATS: "127.0.0.1:-1", Enroll: "127.0.0.1:0"}
	fakeGH := gh.NewFake()
	fakeGH.AddRepo(gh.Repo{Owner: "acme", Name: "app", DefaultBranch: "main", CloneURL: "file://" + bare},
		gh.Issue{Number: 12, Title: "add a changelog", Body: "please", State: "open", URL: "https://github.com/acme/app/issues/12"})
	srv, err := controller.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	srv.LoopInterval, srv.SchedulerInterval, srv.GH = 50*time.Millisecond, 50*time.Millisecond, fakeGH
	srvDone := make(chan error, 1)
	go func() { srvDone <- srv.Run(ctx) }()
	<-srv.Ready()
	api, _ := client.New("unix://"+cfg.Listen.Unix, nil)
	e := &workEnv{ctx: ctx, api: api, bare: bare, t: t}

	// Template whose "agent" is the fake script; secrets resolved from the controller.
	if err := controller.SecretSet(cfg.DataDir, "fake_token", strings.NewReader("s3cret")); err != nil {
		t.Fatal(err)
	}
	tmpl := fmt.Sprintf(`name: tiny
arch: %s
min: {vcpu: 1, ram: 256MiB, disk: 1GiB}
mise_toml: ""
agent:
  start: [%q, "{{.Prompt}}"]
  secrets: {FAKE_TOKEN: fake_token}
  files:
    - path: AGENT_SESSION
      content: "{{.SessionName}} {{.SessionUUID}}"
`, ec2Arch(), agentPath)
	if _, err := api.CreateTemplate(ctx, []byte(tmpl)); err != nil {
		t.Fatal(err)
	}
	// Adopt a runner on this machine (no EC2 needed for the work loop).
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
	daemonCtx, stopDaemon := context.WithCancel(ctx)
	daemonDone := make(chan struct{})
	go func() {
		runner.Run(daemonCtx, rcfg, runner.DaemonOptions{HeartbeatInterval: 50 * time.Millisecond,
			Log: slog.New(slog.NewTextHandler(io.Discard, nil)), InstallMise: runnertest.MiseInstaller(nil)})
		close(daemonDone)
	}()

	// Project, repo, issues.
	if _, err := api.CreateProject(ctx, proto.ProjectRequest{Slug: "web", Template: ptr("tiny")}); err != nil {
		t.Fatal(err)
	}
	if _, err := api.LinkRepo(ctx, "web", "acme/app", ""); err != nil {
		t.Fatal(err)
	}
	if n, err := api.SyncIssues(ctx, "web"); err != nil || n != 1 {
		t.Fatalf("issue sync: %d %v", n, err)
	}

	// epic{a, b, c}: a waits on an outside task, c needs b (same run).
	ext := e.mk(proto.TaskCreateRequest{Title: "external prerequisite"})
	epic := e.mk(proto.TaskCreateRequest{Title: "ship the feature", Prompt: "Overall goal: ship it."})
	a := e.mk(proto.TaskCreateRequest{Parent: epic, Title: "task a", Needs: []string{ext}})
	b := e.mk(proto.TaskCreateRequest{Parent: epic, Title: "task b"})
	c := e.mk(proto.TaskCreateRequest{Parent: epic, Title: "task c", Needs: []string{b}})

	runID, branch, err := api.QueueTask(ctx, epic)
	if err != nil {
		t.Fatal(err)
	}
	v := e.waitRun(runID, "blocked", "failed", "succeeded")
	if v.State != "blocked" || !v.Pushed || v.Runner == "" || !strings.HasPrefix(v.Session, v.Runner+":"+epic) {
		t.Fatalf("run 1: %+v", v)
	}
	// The branch landed in the bare repo with one checkpoint commit per leaf, b before c.
	logOut := git(t, dir, "--git-dir", bare, "log", "--format=%s", branch)
	if want := fmt.Sprintf("[%s] task c\n[%s] task b\ninitial", c, b); logOut != want {
		t.Fatalf("commits on %s:\n%s\nwant:\n%s", branch, logOut, want)
	}
	notes := git(t, dir, "--git-dir", bare, "show", branch+":NOTES.md")
	if !strings.Contains(notes, "You are working on task "+b) || !strings.Contains(notes, "You are working on task "+c) {
		t.Fatalf("NOTES.md: %q", notes)
	}
	// Agent files are written but the template's secret never lands on disk in the repo.
	if s := git(t, dir, "--git-dir", bare, "show", branch+":AGENT_SESSION"); !strings.HasPrefix(s, v.Runner+":"+epic) {
		t.Fatalf("AGENT_SESSION: %q", s)
	}
	// git grep exits 1 when nothing matches, which is what we want.
	if out, err := exec.Command("git", "--git-dir", bare, "grep", "-l", "s3cret", branch).CombinedOutput(); err == nil {
		t.Fatalf("secret committed: %s", out)
	}
	for sqid, want := range map[string]string{b: "in_review", c: "in_review", a: "open", epic: "open"} {
		if got := e.task(sqid).State; got != want {
			t.Errorf("task %s: %s, want %s", sqid, got, want)
		}
	}
	if evs := e.task(b).Events; evs[len(evs)-1].Actor != fmt.Sprintf("agent:%d", runID) {
		t.Errorf("completion actor: %+v", evs[len(evs)-1])
	}

	// Logs: stored, filterable, searchable.
	var lines []string
	if err := api.Logs(ctx, runID, false, "fake agent starting", "agent", "", func(l proto.LogView) { lines = append(lines, l.Message) }); err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 {
		t.Fatalf("grep'd agent log lines: %q", lines)
	}
	hits, err := api.SearchLogs(ctx, "needle", "web", "", "", 10)
	if err != nil || len(hits) != 2 || hits[0].Task != epic {
		t.Fatalf("search: %+v %v", hits, err)
	}
	// PTY replay.
	var pty bytes.Buffer
	actx, acancel := context.WithTimeout(ctx, 10*time.Second)
	if err := api.AttachPTY(actx, runID, &pty); err != nil {
		t.Fatal(err)
	}
	acancel()
	if !strings.Contains(pty.String(), "fake agent starting: You are working on task "+b) {
		t.Fatalf("pty replay: %q", pty.String())
	}
	// Live inspection of the workspace on the runner.
	diff, err := api.Inspect(ctx, runID, "diff", "", true)
	if err != nil || !strings.Contains(diff.Data, "NOTES.md") {
		t.Fatalf("diff --stat: %+v %v", diff, err)
	}
	files, err := api.Inspect(ctx, runID, "files", "", false)
	if err != nil || !strings.Contains(files.Data, "README.md") || strings.Contains(files.Data, ".git/") {
		t.Fatalf("files: %+v %v", files, err)
	}
	if _, err := api.Inspect(ctx, runID, "file", "../../../etc/passwd", false); err == nil {
		// Cleaned to "etc/passwd" inside the workspace, which does not exist.
		t.Fatal("path escaped the workspace")
	}

	// Finish the outside prerequisite and re-queue: a is now ready.
	if err := api.TaskAction(ctx, ext, "complete", "done by hand"); err != nil {
		t.Fatal(err)
	}
	run2, _, err := api.QueueTask(ctx, epic)
	if err != nil {
		t.Fatal(err)
	}
	if v := e.waitRun(run2, "succeeded", "failed", "blocked"); v.State != "succeeded" {
		t.Fatalf("run 2: %+v", v)
	}
	if got := e.task(a).State; got != "in_review" {
		t.Fatalf("task a after run 2: %s", got)
	}

	// Issue -> task -> run.
	issueTask, err := api.IssueToTask(ctx, "web", "acme/app#12", "", "")
	if err != nil {
		t.Fatal(err)
	}
	run3, _, _ := api.QueueTask(ctx, issueTask.Sqid)
	if v := e.waitRun(run3, "succeeded", "failed"); v.State != "succeeded" {
		t.Fatalf("issue run: %+v", v)
	}

	// Cancel a slow run: the agent is killed and the leaf goes back to open.
	slow := e.mk(proto.TaskCreateRequest{Title: "slow one", Prompt: "SLOW"})
	run4, _, _ := api.QueueTask(ctx, slow)
	waitFor(t, "slow agent running", 20*time.Second, func() (bool, string) {
		var got []string
		api.Logs(ctx, run4, false, "going slow", "", "", func(l proto.LogView) { got = append(got, l.Message) })
		return len(got) > 0, e.run(run4).State
	})
	if err := api.CancelRun(ctx, run4); err != nil {
		t.Fatal(err)
	}
	if v := e.waitRun(run4, "cancelled", "failed", "succeeded"); v.State != "cancelled" {
		t.Fatalf("cancelled run: %+v", v)
	}
	if got := e.task(slow).State; got != "open" {
		t.Fatalf("cancelled leaf: %s", got)
	}

	// M5: the agent asks a question; the run waits (needs_input) until it is answered.
	askT := e.mk(proto.TaskCreateRequest{Title: "needs input", Prompt: "ASKME"})
	run6, _, _ := api.QueueTask(ctx, askT)
	var qv proto.QuestionView
	waitFor(t, "agent question", 20*time.Second, func() (bool, string) {
		qs, _ := api.Questions(ctx, false)
		for _, q := range qs {
			if q.RunID == run6 {
				qv = q
				return true, ""
			}
		}
		return false, e.run(run6).State
	})
	// The question is stored before the run flips to needs_input: wait for the state, don't sample it.
	waitFor(t, "run needs_input", 10*time.Second, func() (bool, string) {
		st := e.run(run6).State
		return st == "needs_input", st
	})
	if qv.Kind != "agent" || len(qv.Options) != 2 {
		t.Fatalf("question %+v, run %s", qv, e.run(run6).State)
	}
	if err := api.Answer(ctx, qv.ID, "purple", ""); err == nil {
		t.Fatal("accepted an answer that is not an option")
	}
	if err := api.Answer(ctx, qv.ID, "blue", ""); err != nil {
		t.Fatal(err)
	}
	if v := e.waitRun(run6, "succeeded", "failed"); v.State != "succeeded" {
		t.Fatalf("ask run: %+v", v)
	}
	var said []string
	api.Logs(ctx, run6, false, "owner said", "", "", func(l proto.LogView) { said = append(said, l.Message) })
	if len(said) != 1 || !strings.Contains(said[0], `"answer":"blue"`) {
		t.Fatalf("agent did not get the answer: %q", said)
	}
	if hooks, _ := api.RunHooks(ctx, run6); len(hooks) == 0 || hooks[0]["event"] != "PostToolUse" {
		t.Fatalf("hooks: %v", hooks)
	}

	// M5: an agent that forgets task_complete is nudged once; the nudge turn completes it.
	forgetful := e.mk(proto.TaskCreateRequest{Title: "forgetful", Prompt: "FORGET once"})
	run7, _, _ := api.QueueTask(ctx, forgetful)
	if v := e.waitRun(run7, "succeeded", "failed"); v.State != "succeeded" {
		t.Fatalf("nudged run: %+v", v)
	}
	if got := e.task(forgetful).State; got != "in_review" {
		t.Fatalf("forgetful after nudge: %s", got)
	}
	// ... and one that keeps forgetting (FORGET in the title reaches the nudge prompt) is
	// put to the owner, who marks it complete.
	stubborn := e.mk(proto.TaskCreateRequest{Title: "FORGET always", Prompt: "FORGET"})
	run8, _, _ := api.QueueTask(ctx, stubborn)
	waitFor(t, "system question", 20*time.Second, func() (bool, string) {
		qs, _ := api.Questions(ctx, false)
		for _, q := range qs {
			if q.RunID == run8 && q.Kind == "system" {
				qv = q
				return true, ""
			}
		}
		return false, e.run(run8).State
	})
	if err := api.Answer(ctx, qv.ID, "complete", ""); err != nil {
		t.Fatal(err)
	}
	if v := e.waitRun(run8, "succeeded", "failed"); v.State != "succeeded" || e.task(stubborn).State != "in_review" {
		t.Fatalf("owner-completed run: %+v task %s", v, e.task(stubborn).State)
	}

	// M5: cancelling a task the agent did not create needs approval; approving executes it.
	// Outside its assignment the agent may not touch a task at all.
	outsider := e.mk(proto.TaskCreateRequest{Title: "not in the assignment"})
	tidy := e.mk(proto.TaskCreateRequest{Title: "tidy up"})
	gate := e.mk(proto.TaskCreateRequest{Title: "gate (outside, open)"})
	victim := e.mk(proto.TaskCreateRequest{Parent: tidy, Title: "someone else's task", Needs: []string{gate}})
	e.mk(proto.TaskCreateRequest{Parent: tidy, Title: "worker", Prompt: "CANCELOTHER " + victim + "\nCANCELOTHER " + outsider, First: true})
	run9, _, _ := api.QueueTask(ctx, tidy)
	if v := e.waitRun(run9, "blocked", "succeeded", "failed"); v.State != "blocked" {
		t.Fatalf("canceller run: %+v", v)
	}
	if got := e.task(victim).State; got != "open" {
		t.Fatalf("victim cancelled without approval: %s", got)
	}
	var approval proto.QuestionView
	qs, _ := api.Questions(ctx, false)
	for _, q := range qs {
		if q.Kind == "approval" && q.Task == victim {
			approval = q
		}
	}
	if approval.ID == 0 {
		var lines []string
		api.Logs(ctx, run9, false, "cancel attempt", "", "", func(l proto.LogView) { lines = append(lines, l.Message) })
		t.Fatalf("no approval question: %+v\nagent: %q", qs, lines)
	}
	if err := api.Answer(ctx, approval.ID, "approve", ""); err != nil {
		t.Fatal(err)
	}
	if got := e.task(victim).State; got != "cancelled" {
		t.Fatalf("victim after approval: %s", got)
	}
	var attempts []string
	api.Logs(ctx, run9, false, "cancel attempt", "", "", func(l proto.LogView) { attempts = append(attempts, l.Message) })
	if len(attempts) != 2 || !strings.Contains(attempts[0], "pending_approval") || !strings.Contains(attempts[1], "outside your assignment") {
		t.Fatalf("cancel attempts: %q", attempts)
	}
	if got := e.task(outsider).State; got != "open" {
		t.Fatalf("outsider changed: %s", got)
	}
	if evs := e.task(victim).Events; evs[len(evs)-1].Actor != fmt.Sprintf("agent:%d", run9) {
		t.Fatalf("approved cancel actor: %+v", evs[len(evs)-1])
	}

	// A failing agent fails the run with its exit code.
	boom := e.mk(proto.TaskCreateRequest{Title: "explodes", Prompt: "EXPLODE"})
	run5, _, _ := api.QueueTask(ctx, boom)
	if v := e.waitRun(run5, "failed", "succeeded"); v.State != "failed" || !strings.Contains(v.Error, "exited 3") {
		t.Fatalf("failing agent: %+v", v)
	}
	// The runner is idle again after every run.
	waitFor(t, "runner idle", 5*time.Second, func() (bool, string) {
		rs, _ := api.Runners(ctx, false)
		return len(rs) == 1 && rs[0].State == "idle", fmt.Sprintf("%+v", rs)
	})

	// The CLI, end to end against this controller.
	sock := "unix://" + cfg.Listen.Unix
	cli := func(args ...string) string {
		t.Helper()
		root := wcli.NewRoot()
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs(append([]string{"--controller", sock}, args...))
		if err := root.ExecuteContext(ctx); err != nil {
			t.Fatalf("wrangl %v: %v\n%s", args, err, out.String())
		}
		return out.String()
	}
	expect := func(out string, want ...string) {
		t.Helper()
		for _, w := range want {
			if !strings.Contains(out, w) {
				t.Fatalf("output missing %q:\n%s", w, out)
			}
		}
	}
	expect(cli("task", "tree", "web", "--all"), epic+" ◐ ship the feature", "  "+b+" ◐ task b", ext+" ✓ external prerequisite")
	expect(cli("task", "show", b), "agent:"+fmt.Sprint(runID), "needed by", c)
	expect(cli("task", "split", slow, "--title", "part one", "--title", "part two", "--chain"), "created", "part two")
	parts := cli("task", "tree", slow)
	expect(parts, "part one", "part two")
	expect(cli("task", "create", "web", "--title", "cli made", "--first"), "created")
	expect(cli("task", "tree", "web"), "cli made")
	expect(cli("run", "list", "--project", "web"), fmt.Sprint(runID), "blocked", "cancelled", "failed")
	expect(cli("run", "show", fmt.Sprint(runID)), "branch", branch, "pushed: true")
	expect(cli("run", "diff", fmt.Sprint(runID), "--stat"), "NOTES.md")
	expect(cli("run", "cat", fmt.Sprint(runID), "README.md"), "# app")
	expect(cli("logs", fmt.Sprint(runID), "--grep", "needle"), "fake agent finished needle-"+b)
	expect(cli("logs", "search", "needle", "--project", "web"), epic)
	expect(cli("issue", "list", "--project", "web"), "acme/app#12", issueTask.Sqid)
	expect(cli("project", "show", "web"), "tiny")
	expect(cli("repo", "list"), "acme/app")
	if out := cli("task", "tree", "web", "-o", "json"); !strings.Contains(out, `"sqid": "`+epic+`"`) {
		t.Fatalf("json tree: %s", out)
	}

	stopDaemon()
	<-daemonDone
	cancel()
	if err := <-srvDone; err != nil {
		t.Fatal(err)
	}
}

func ptr(s string) *string { return &s }
