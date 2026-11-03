package e2e

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
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

const prAgent = `#!/bin/sh
tool() { curl -s --unix-socket "$WRANGL_SOCK" -d "$2" "http://x/tool/$1"; }
case "$1" in
  "Review pull request"*)
    risk=low; case "$(printf '%s' "$1" | head -1)" in *RISKY*) risk=high;; esac
    tool submit_review "{\"risk\":\"$risk\",\"summary_md\":\"Reviewed by the fake agent.\",\"concerns\":[\"naming\"]}"
    echo "reviewed: $risk"; exit 0;;
  "Plan task"*)
    tool propose_split '{"chain":true,"rationale_md":"two steps","children":[{"title":"design it","prompt":"write the design"},{"title":"build it","children":[{"title":"backend"}]}]}'
    echo "planned"; exit 0;;
  "The owner requested changes"*)
    printf 'revised per review\n' >> NOTES.md; echo "revised"; exit 0;;
esac
printf '%s\n' "$(printf '%s' "$1" | head -1)" >> NOTES.md
tool task_complete "{\"sqid\":\"$WRANGL_TASK\",\"summary\":\"did $WRANGL_TASK\"}" >/dev/null
echo "worked $WRANGL_TASK"
`

func TestPRFlowEndToEnd(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs linux")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()
	bare := bareRepo(t, dir)
	agentPath := filepath.Join(dir, "pr-agent")
	os.WriteFile(agentPath, []byte(prAgent), 0o755)

	data := filepath.Join(dir, "data")
	if _, err := controller.Init(ctx, data, filepath.Join(dir, "c.yaml")); err != nil {
		t.Fatal(err)
	}
	secret := "whsec-test"
	controller.SecretSet(data, "gh-webhook", strings.NewReader(secret))
	cfg := config.DefaultController()
	cfg.Web.PublicURL = "https://ctrl.example.com"
	cfg.DataDir = data
	cfg.Listen = config.Listen{HTTP: "127.0.0.1:0", Unix: filepath.Join(dir, "api.sock"), NATS: "127.0.0.1:-1", Enroll: "127.0.0.1:0"}
	cfg.GitHub.ReviewerApp.WebhookSecret = "secrets/gh-webhook"
	fake := gh.NewFake()
	fake.AddRepo(gh.Repo{Owner: "acme", Name: "app", DefaultBranch: "main", CloneURL: "file://" + bare})
	srv, err := controller.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	srv.LoopInterval, srv.SchedulerInterval, srv.GH, srv.Reviewer = 50*time.Millisecond, 50*time.Millisecond, fake, fake
	srvDone := make(chan error, 1)
	go func() { srvDone <- srv.Run(ctx) }()
	<-srv.Ready()
	api, _ := client.New("unix://"+cfg.Listen.Unix, nil)
	e := &workEnv{ctx: ctx, api: api, bare: bare, t: t}
	var kindsMu sync.Mutex
	kinds := map[string]int{}
	go api.Events(ctx, func(ev client.Event) { kindsMu.Lock(); kinds[ev.Kind]++; kindsMu.Unlock() })

	tmpl := fmt.Sprintf("name: tiny\narch: %s\nmin: {vcpu: 1, ram: 256MiB, disk: 1GiB}\nmise_toml: \"\"\nagent:\n  start: [%q, \"{{.Prompt}}\"]\n", ec2Arch(), agentPath)
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
	autoMerge := true
	if _, err := api.CreateProject(ctx, proto.ProjectRequest{Slug: "web", Template: ptr("tiny"), AutoMerge: &autoMerge}); err != nil {
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
	waitPR := func(sqid, state string) proto.PRView {
		t.Helper()
		var p proto.PRView
		waitFor(t, "PR for "+sqid+" "+state, 30*time.Second, func() (bool, string) {
			p = prFor(sqid)
			return p.State == state, fmt.Sprintf("%+v", p)
		})
		return p
	}
	prQuestion := func(sqid string, after int64) proto.QuestionView {
		t.Helper()
		var q proto.QuestionView
		waitFor(t, "pr_review question for "+sqid, 30*time.Second, func() (bool, string) {
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

	// A. Low risk, gates pass, auto_merge on: reviewed, approved by the reviewer App, merged.
	small := e.mk(proto.TaskCreateRequest{Title: "small feature"})
	api.QueueTask(ctx, small)
	p := waitPR(small, "merged")
	if p.Decision != "auto_merged" || p.Risk != "low" || p.Checks != "success" {
		t.Fatalf("auto-merged PR: %+v", p)
	}
	if got := e.task(small).State; got != "done" {
		t.Fatalf("task after merge: %s", got)
	}
	if rv := fake.Reviews(p.Number); len(rv) != 1 || !strings.HasPrefix(rv[0], "APPROVE:") {
		t.Fatalf("reviews: %v", rv)
	}
	if notes := git(t, dir, "--git-dir", bare, "show", "main:NOTES.md"); !strings.Contains(notes, small) {
		t.Fatalf("main after merge: %q", notes)
	}

	// B. High risk: the owner is asked, requests changes, a revise run commits on the same
	// branch, it is re-reviewed, and the owner approves.
	risky := e.mk(proto.TaskCreateRequest{Title: "RISKY change"})
	api.QueueTask(ctx, risky)
	q1 := prQuestion(risky, 0)
	if !strings.Contains(q1.DetailMD, "**Risk:** high") || len(q1.Options) != 3 {
		t.Fatalf("question: %+v", q1)
	}
	if err := api.Answer(ctx, q1.ID, "request_changes", "rename the function"); err != nil {
		t.Fatal(err)
	}
	q2 := prQuestion(risky, q1.ID) // after the revise and a fresh review
	pb := prFor(risky)
	if notes := git(t, dir, "--git-dir", bare, "show", pb.Branch+":NOTES.md"); !strings.Contains(notes, "revised per review") {
		t.Fatalf("revise did not land on the PR branch: %q", notes)
	}
	if rv := fake.Reviews(pb.Number); len(rv) == 0 || !strings.Contains(rv[0], "REQUEST_CHANGES: rename the function") {
		t.Fatalf("change request not posted: %v", rv)
	}
	if err := api.Answer(ctx, q2.ID, "approve", ""); err != nil {
		t.Fatal(err)
	}
	if p := waitPR(risky, "merged"); p.Decision != "user_approved" {
		t.Fatalf("approved PR: %+v", p)
	}
	if got := e.task(risky).State; got != "done" {
		t.Fatalf("risky task: %s", got)
	}

	// C. CI pending: no review yet. A signed check_suite webhook moves it on.
	fake.SetDefaultChecks(gh.ChecksPending)
	gated := e.mk(proto.TaskCreateRequest{Title: "ci gated"})
	api.QueueTask(ctx, gated)
	var pc proto.PRView
	waitFor(t, "PR with pending checks", 30*time.Second, func() (bool, string) {
		pc = prFor(gated)
		return pc.State == "open" && pc.Checks == "pending", fmt.Sprintf("%+v", pc)
	})
	time.Sleep(300 * time.Millisecond)
	if rs, _ := api.Runs(ctx, "", "", "web", 100); func() bool {
		for _, r := range rs {
			if r.Kind == "review" && r.Task == gated {
				return true
			}
		}
		return false
	}() {
		t.Fatal("review queued while CI was pending")
	}
	fake.SetDefaultChecks(gh.ChecksSuccess)
	live, _ := fake.FakePR(pc.Number)
	body := fmt.Sprintf(`{"action":"completed","check_suite":{"head_sha":%q},"repository":{"name":"app","owner":{"login":"acme"}}}`, live.HeadSHA)
	httpURL := "http://" + srv.HTTPAddr() + "/gh/webhook"
	post := func(sig, delivery string) int {
		req, _ := http.NewRequest(http.MethodPost, httpURL, strings.NewReader(body))
		req.Header.Set("X-GitHub-Event", "check_suite")
		req.Header.Set("X-GitHub-Delivery", delivery)
		req.Header.Set("X-Hub-Signature-256", sig)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	good := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if code := post("sha256=deadbeef", "d-1"); code != http.StatusUnauthorized {
		t.Fatalf("forged webhook: %d", code)
	}
	if code := post(good, "d-2"); code != http.StatusAccepted {
		t.Fatalf("webhook: %d", code)
	}
	if code := post(good, "d-2"); code != http.StatusOK {
		t.Fatalf("duplicate delivery: %d", code)
	}
	if p := waitPR(gated, "merged"); p.Decision != "auto_merged" {
		t.Fatalf("gated PR: %+v", p)
	}

	ps, _ := api.PRs(ctx, "merged")
	if len(ps) != 3 {
		t.Fatalf("merged PRs: %+v", ps)
	}
	// M8: split with agent. The proposal arrives as a plan_review question; accept applies
	// it through the atomic split; reject queues another plan run with the feedback.
	big := e.mk(proto.TaskCreateRequest{Title: "big job", Prompt: "do the whole thing"})
	if _, err := api.PlanTask(ctx, big, "keep it small"); err != nil {
		t.Fatal(err)
	}
	var pq proto.QuestionView
	waitFor(t, "plan_review question", 30*time.Second, func() (bool, string) {
		qs, _ := api.Questions(ctx, false)
		for _, x := range qs {
			if x.Kind == "plan_review" && x.Task == big {
				pq = x
				return true, ""
			}
		}
		return false, ""
	})
	if !strings.Contains(pq.DetailMD, "two steps") || !strings.Contains(pq.DetailMD, "  - backend") || !strings.Contains(string(pq.Payload), "keep it small") {
		t.Fatalf("plan question: %+v %s", pq, pq.Payload)
	}
	if err := api.Answer(ctx, pq.ID, "accept", ""); err != nil {
		t.Fatal(err)
	}
	d := e.task(big)
	if len(d.Children) != 2 || d.Children[0].Title != "design it" {
		t.Fatalf("split not applied: %+v", d.Children)
	}
	if built := e.task(d.Children[1].Sqid); len(built.Precursors) != 1 || len(built.Children) != 1 {
		t.Fatalf("chain/nesting: %+v", built)
	}
	other := e.mk(proto.TaskCreateRequest{Title: "another big job"})
	api.PlanTask(ctx, other, "")
	waitFor(t, "second plan question", 30*time.Second, func() (bool, string) {
		qs, _ := api.Questions(ctx, false)
		for _, x := range qs {
			if x.Kind == "plan_review" && x.Task == other {
				pq = x
				return true, ""
			}
		}
		return false, ""
	})
	if err := api.Answer(ctx, pq.ID, "reject", "fewer steps please"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "re-plan with feedback", 30*time.Second, func() (bool, string) {
		qs, _ := api.Questions(ctx, false)
		for _, x := range qs {
			if x.Kind == "plan_review" && x.Task == other && x.ID != pq.ID && strings.Contains(string(x.Payload), "fewer steps please") {
				return true, ""
			}
		}
		return false, ""
	})
	if d := e.task(other); len(d.Children) != 0 {
		t.Fatalf("rejected proposal was applied: %+v", d.Children)
	}

	// A feature with no subtasks queues a plan run (its work is its subtasks), not a work run.
	feat := e.mk(proto.TaskCreateRequest{Title: "childless feature", Type: "feature"})
	fr, _, err := api.QueueTask(ctx, feat)
	if err != nil {
		t.Fatal(err)
	}
	if v, err := api.Run(ctx, fr); err != nil || v.Kind != "plan" {
		t.Fatalf("queueing a childless feature: %+v %v", v, err)
	}
	api.CancelRun(ctx, fr)

	// M7: the event stream carried every kind of change.
	kindsMu.Lock()
	for _, k := range []string{"run", "task", "question", "pr", "runner"} {
		if kinds[k] == 0 {
			t.Errorf("no %q events (got %v)", k, kinds)
		}
	}
	kindsMu.Unlock()
	// M7: terminal output as SSE (base64 chunks with stream-sequence ids, then "end").
	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", cfg.Listen.Unix)
	}}}
	resp, err := hc.Get("http://x/api/v1/runs/1/pty?format=sse")
	if err != nil {
		t.Fatal(err)
	}
	sseBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(sseBody), "event: pty") || !strings.Contains(string(sseBody), "event: end") || !strings.Contains(string(sseBody), "\nid: ") {
		t.Fatalf("pty sse: %.300s", sseBody)
	}

	runners := mustRunners(t, api, ctx)
	checkPWA(t, e, hc, runners[0].Name, big, other, pq.ID)
	checkTerm(t, ctx, hc, runners[0].Name)

	stopDaemon()
	<-ddone
	cancel()
	if err := <-srvDone; err != nil {
		t.Fatal(err)
	}
}
