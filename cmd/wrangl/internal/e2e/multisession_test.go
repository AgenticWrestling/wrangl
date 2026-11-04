package e2e

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
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
i=0; while [ ! -e "$BARRIER/release" ] && [ $i -lt 150 ]; do sleep 0.1; i=$((i+1)); done
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
	// While both are mid-run (held by the agent), the Tasks page lists them under In progress with
	// their runner, not under Queue or Backlog.
	waitFor(t, "both sessions running", 20*time.Second, func() (bool, string) {
		ents, _ := os.ReadDir(barrier)
		n := 0
		for _, f := range ents {
			if strings.HasPrefix(f.Name(), "dir.") {
				n++
			}
		}
		return n == 2, fmt.Sprintf("%d of 2 started", n)
	})
	hc := runner.SocketClient(cfg.Listen.Unix)
	resp, err := hc.Get("http://x/projects/web")
	if err != nil {
		t.Fatal(err)
	}
	pb, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	page := string(pb)
	if !strings.Contains(page, `id="in-progress"`) {
		t.Fatalf("running runs must be listed under In progress; page had sections: queue=%v backlog=%v",
			strings.Contains(page, `id="queue"`), strings.Contains(page, `id="backlog"`))
	}
	if strings.Count(page, `class="task run `) != 2 || strings.Contains(page, `id="queue"`) || !strings.Contains(page, "▶ ") {
		t.Fatalf("want two run entries with a runner tag and nothing queued:\n%.1500s", page)
	}
	// A third task has no free slot (one runner at its max): it waits in Queue.
	c := e.mk(proto.TaskCreateRequest{Title: "third independent task", Prompt: "do c"})
	runC, _, err := api.QueueTask(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "task C in Queue", 10*time.Second, func() (bool, string) {
		r, _ := hc.Get("http://x/projects/web")
		b, _ := io.ReadAll(r.Body)
		r.Body.Close()
		return strings.Contains(string(b), `id="queue"`), "not queued yet"
	})
	// An expanded run entry's card survives live re-renders, for a running one (A) and a queued
	// one (C) alike.
	for _, sq := range []string{a, c} {
		lctx, lcancel := context.WithTimeout(ctx, 10*time.Second)
		req, _ := http.NewRequestWithContext(lctx, "GET", "http://x/ui/live?view=tasks&project=web&sid=runsid-"+sq, nil)
		lresp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		rd := bufio.NewReader(lresp.Body)
		frame := func() string {
			var b strings.Builder
			for {
				line, err := rd.ReadString('\n')
				if err != nil || (line == "\n" && b.Len() > 0) {
					return b.String()
				}
				b.WriteString(line)
			}
		}
		if first := frame(); !strings.Contains(first, `id="in-progress"`) { // what an open page is sent
			t.Fatalf("the live stream's render lacks the In progress section: %.600s", first)
		}
		xr, err := hc.Post("http://x/ui/expand?sid=runsid-"+sq+"&sqid="+sq, "text/plain", nil)
		if err != nil {
			t.Fatal(err)
		}
		xr.Body.Close()
		var got string
		for i := 0; i < 4 && !strings.Contains(got, `id="subtasks"`); i++ {
			got = frame()
		}
		if !strings.Contains(got, `id="subtasks"`) || !strings.Contains(got, `id="d-`+sq+`"`) {
			t.Fatalf("the live re-render drops the expanded run entry's card for %s: %.600s", sq, got)
		}
		lcancel()
		lresp.Body.Close()
	}
	os.WriteFile(filepath.Join(barrier, "release"), nil, 0o644)
	va := e.waitRun(runA, "succeeded", "failed", "blocked")
	vb := e.waitRun(runB, "succeeded", "failed", "blocked")
	if vc := e.waitRun(runC, "succeeded", "failed", "blocked"); vc.State != "succeeded" { // C ran once a slot freed
		t.Fatalf("run C: %+v", vc)
	}
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
