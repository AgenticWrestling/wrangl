package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/errs"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/toolkdl"
)

// slow is a fake executor that blocks until released, reporting progress while it waits.
type slow struct {
	mu        sync.Mutex
	started   chan int64    // run ids, as calls start
	release   chan struct{} // close or send to finish a call
	out       []byte
	err       error
	cancelled chan struct{}
}

func newSlow() *slow {
	return &slow{started: make(chan int64, 16), release: make(chan struct{}, 16), cancelled: make(chan struct{}, 16)}
}

func (s *slow) Execute(ctx context.Context, c *Call) ([]byte, error) {
	c.Progress("fetched 1 of 3 pages")
	c.Progress("token is " + secretValue + " (must be scrubbed)")
	s.started <- c.RunID
	select {
	case <-s.release:
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.out, s.err
	case <-ctx.Done():
		s.cancelled <- struct{}{}
		return nil, ctx.Err()
	}
}

const secretValue = "KUBE-SECRET-VALUE"

func asyncEnv(t *testing.T) (*env, *slow) {
	t.Helper()
	v := newEnv(t)
	sl := newSlow()
	v.e.Executors[toolkdl.KindExec] = sl
	ctx := context.Background()
	for _, name := range []string{"slow_report", "slow_two"} {
		src := strings.Replace(toolSrc(name, "read", `"work" "revise" "review"`, `secret "KUBECONFIG" name="kube.prod" as="file"`), "timeout 5", "timeout 60", 1)
		if _, err := v.svc.Add(ctx, src); err != nil {
			t.Fatal(err)
		}
		v.svc.Enable(ctx, "alpha", name)
	}
	return v, sl
}

func (v *env) poll(runID int64, args string) (map[string]any, error) {
	out, err := v.e.Call(context.Background(), Caller{RunID: runID, Kind: "work", ProjectID: 1}, ResultTool, json.RawMessage(args))
	m, _ := out.(map[string]any)
	return m, err
}

func (v *env) callAs(runID int64, name string) (map[string]any, error) {
	out, err := v.e.Call(context.Background(), Caller{RunID: runID, Kind: "work", ProjectID: 1}, Prefix+name, json.RawMessage(`{"env":"prod"}`))
	m, _ := out.(map[string]any)
	return m, err
}

func (v *env) rows(t *testing.T, runID int64) []string {
	t.Helper()
	rs, err := v.svc.DB.RQ.ListToolCallsForRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range rs {
		out = append(out, r.Tool+":"+r.Status)
	}
	return out
}

func waitStarted(t *testing.T, sl *slow) int64 {
	t.Helper()
	select {
	case id := <-sl.started:
		return id
	case <-time.After(3 * time.Second):
		t.Fatal("the executor never started")
		return 0
	}
}

func TestAsyncAcceptProgressAndResult(t *testing.T) {
	v, sl := asyncEnv(t)
	sl.out = []byte("report: " + secretValue + " " + strings.Repeat("x", 3000))

	start := time.Now()
	acc, err := v.callAs(7, "slow_report")
	if err != nil || acc["status"] != "running" || acc["call_id"] == nil || time.Since(start) > time.Second {
		t.Fatalf("accept: %v %v", acc, err)
	}
	id := acc["call_id"].(string)
	waitStarted(t, sl)
	if got := v.rows(t, 7); len(got) != 1 || got[0] != "ext_slow_report:running" {
		t.Fatalf("audit at accept: %v", got)
	}

	// while it runs: status, elapsed, scrubbed progress
	p, err := v.poll(7, fmt.Sprintf(`{"call_id":%q}`, id))
	if err != nil || p["status"] != "running" {
		t.Fatalf("poll: %v %v", p, err)
	}
	lines := p["progress"].([]string)
	if len(lines) != 2 || lines[0] != "fetched 1 of 3 pages" || strings.Contains(strings.Join(lines, "|"), secretValue) {
		t.Fatalf("progress: %v", lines)
	}
	// a short wait that expires still answers running
	if p, _ := v.poll(7, fmt.Sprintf(`{"call_id":%q,"wait_s":1}`, id)); p["status"] != "running" {
		t.Fatalf("wait timeout: %v", p)
	}

	// finishing: a waiting poll returns the result as soon as it is ready
	go func() { time.Sleep(100 * time.Millisecond); sl.release <- struct{}{} }()
	p, err = v.poll(7, fmt.Sprintf(`{"call_id":%q,"wait_s":10}`, id))
	if err != nil || p["status"] != "done" {
		t.Fatalf("result: %v %v", p, err)
	}
	text := p["output"].(string)
	if strings.Contains(text, secretValue) || !strings.Contains(text, Redacted) || p["truncated"] != true || len(text) > 1024+200 {
		t.Fatalf("output must be scrubbed and capped: len=%d truncated=%v", len(text), p["truncated"])
	}
	// the result stays available
	if p2, _ := v.poll(7, fmt.Sprintf(`{"call_id":%q}`, id)); p2["status"] != "done" || p2["output"] != text {
		t.Fatalf("second read: %v", p2)
	}
	// one audit row, updated in place with the outcome
	rows, _ := v.svc.DB.RQ.ListToolCallsForRun(context.Background(), 7)
	if len(rows) != 1 || rows[0].Status != "ok" || rows[0].Bytes != int64(len(text)) {
		t.Fatalf("audit after completion: %+v", rows)
	}
}

func TestAsyncFailureIsScrubbedAndAudited(t *testing.T) {
	v, sl := asyncEnv(t)
	sl.err = errors.New("auth failed for " + secretValue)
	acc, _ := v.callAs(7, "slow_report")
	waitStarted(t, sl)
	sl.release <- struct{}{}
	p, _ := v.poll(7, fmt.Sprintf(`{"call_id":%q,"wait_s":5}`, acc["call_id"]))
	if p["status"] != "error" || strings.Contains(fmt.Sprint(p["error"]), secretValue) || !strings.Contains(fmt.Sprint(p["error"]), "auth failed") {
		t.Fatalf("failure: %v", p)
	}
	if got := v.rows(t, 7); len(got) != 1 || got[0] != "ext_slow_report:error" {
		t.Fatalf("audit: %v", got)
	}
}

func TestAsyncLimitsAndMissingSecret(t *testing.T) {
	v, sl := asyncEnv(t)
	for i := 0; i < 3; i++ {
		if _, err := v.callAs(7, "slow_report"); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		waitStarted(t, sl)
	}
	if _, err := v.callAs(7, "slow_two"); err == nil || !strings.Contains(err.Error(), "already has 3") {
		t.Fatalf("per-run limit: %v", err)
	}
	// another run is unaffected until the global cap
	v.e.MaxAsync = 4
	if _, err := v.callAs(8, "slow_report"); err != nil {
		t.Fatalf("other run: %v", err)
	}
	waitStarted(t, sl)
	if _, err := v.callAs(8, "slow_two"); err == nil || !strings.Contains(err.Error(), "too many") {
		t.Fatalf("global limit: %v", err)
	}
	// a rejected call leaves no row behind beyond its own audit line
	if got := v.rows(t, 7); len(got) != 4 || got[3] != "ext_slow_two:error" {
		t.Fatalf("audit: %v", got)
	}
	// freeing a slot lets the next one in
	for i := 0; i < 4; i++ {
		sl.release <- struct{}{}
	}
	time.Sleep(200 * time.Millisecond)
	if _, err := v.callAs(7, "slow_two"); err != nil {
		t.Fatalf("after completion: %v", err)
	}
	waitStarted(t, sl)

	// a missing secret fails at accept, not minutes later
	v.svc.GetSecret = func(string) ([]byte, error) { return nil, errors.New("gone") }
	if _, err := v.callAs(8, "slow_report"); err == nil || !strings.Contains(err.Error(), "kube.prod") {
		t.Fatalf("missing secret: %v", err)
	}
}

func TestAsyncCancelByAgentAndWhenTheRunEnds(t *testing.T) {
	v, sl := asyncEnv(t)
	a, _ := v.callAs(7, "slow_report")
	waitStarted(t, sl)
	p, err := v.poll(7, fmt.Sprintf(`{"call_id":%q,"cancel":true}`, a["call_id"]))
	if err != nil || p["status"] != "cancelled" {
		t.Fatalf("cancel: %v %v", p, err)
	}
	select {
	case <-sl.cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("the executor's context was not cancelled")
	}

	b, _ := v.callAs(7, "slow_report")
	waitStarted(t, sl)
	v.e.CancelRun(7) // the run ended
	select {
	case <-sl.cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("ending the run did not cancel its call")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		rows, _ := v.svc.DB.RQ.ListToolCallsForRun(context.Background(), 7)
		if len(rows) == 2 && rows[0].Status == "cancelled" && rows[1].Status == "cancelled" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("audit rows after cancellation: %+v", rows)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if p, _ := v.poll(7, fmt.Sprintf(`{"call_id":%q}`, b["call_id"])); p["status"] != "cancelled" {
		t.Fatalf("a call cancelled by the run end still reports it: %v", p)
	}
	// once the run's finished calls are dropped, nothing is left to read
	v.e.CancelRun(7)
	if _, err := v.poll(7, fmt.Sprintf(`{"call_id":%q}`, a["call_id"])); !errors.Is(err, errs.NotFound) {
		t.Fatalf("after the run is gone: %v", err)
	}
}

func TestAsyncScope(t *testing.T) {
	v, sl := asyncEnv(t)
	a, _ := v.callAs(7, "slow_report")
	waitStarted(t, sl)
	id := fmt.Sprintf(`{"call_id":%q}`, a["call_id"])
	if _, err := v.poll(8, id); !errors.Is(err, errs.NotFound) { // another run cannot see or cancel it
		t.Fatalf("other run: %v", err)
	}
	if _, err := v.poll(8, fmt.Sprintf(`{"call_id":%q,"cancel":true}`, a["call_id"])); !errors.Is(err, errs.NotFound) {
		t.Fatalf("other run cancel: %v", err)
	}
	if p, _ := v.poll(7, id); p["status"] != "running" {
		t.Fatalf("it must still be running: %v", p)
	}
	if _, err := v.poll(7, `{"call_id":"call_nope"}`); !errors.Is(err, errs.NotFound) {
		t.Fatalf("unknown id: %v", err)
	}
	if _, err := v.poll(7, `{"call_id":"x","wait_s":99}`); !errors.Is(err, errs.Invalid) {
		t.Fatalf("wait bound: %v", err)
	}
	if _, err := v.e.Call(context.Background(), Caller{Kind: "user"}, ResultTool, json.RawMessage(id)); !errors.Is(err, errs.Forbidden) {
		t.Fatalf("no run: %v", err)
	}
}

func TestAsyncResultsExpire(t *testing.T) {
	v, sl := asyncEnv(t)
	now := time.Now()
	v.e.Now = func() time.Time { return now }
	a, _ := v.callAs(7, "slow_report")
	waitStarted(t, sl)
	sl.release <- struct{}{}
	id := fmt.Sprintf(`{"call_id":%q,"wait_s":5}`, a["call_id"])
	if p, _ := v.poll(7, id); p["status"] != "done" {
		t.Fatalf("%v", p)
	}
	now = now.Add(31 * time.Minute)
	if _, err := v.poll(7, id); !errors.Is(err, errs.NotFound) {
		t.Fatalf("expired result: %v", err)
	}
}

func TestAsyncDestructiveStillAsksFirst(t *testing.T) {
	v, sl := asyncEnv(t)
	src := strings.Replace(toolSrc("slow_wipe", "destructive", `"work"`, ``), "timeout 5", "timeout 60", 1)
	if _, err := v.svc.Add(context.Background(), src); err != nil {
		t.Fatal(err)
	}
	v.svc.Enable(context.Background(), "alpha", "slow_wipe")
	out, err := v.callAs(7, "slow_wipe")
	if err != nil || out["pending_approval"] != true || out["call_id"] != nil {
		t.Fatalf("destructive async call must wait for approval: %v %v", out, err)
	}
	select {
	case <-sl.started:
		t.Fatal("ran before approval")
	case <-time.After(200 * time.Millisecond):
	}
}

func TestRecoverMarksOrphanedRowsFailed(t *testing.T) {
	v, _ := asyncEnv(t)
	if _, err := v.svc.DB.W.Exec(`INSERT INTO tool_call (run_id, tool, status) VALUES (7, 'ext_slow_report', 'running'), (7, 'ext_x', 'ok')`); err != nil {
		t.Fatal(err)
	}
	if err := v.e.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(v.rows(t, 7), ","); got != "ext_slow_report:error,ext_x:ok" {
		t.Fatalf("recover: %s", got)
	}
}

func TestDeclarationsAddTheResultToolOnlyWhenNeeded(t *testing.T) {
	v := newEnv(t) // only synchronous tools
	ctx := context.Background()
	has := func(kind string) (bool, string) {
		ds, err := v.svc.Declarations(ctx, 1, kind)
		if err != nil {
			t.Fatal(err)
		}
		var desc string
		found := false
		for _, d := range ds {
			if d.Name == ResultTool {
				found = true
			}
			if strings.Contains(d.Description, ResultTool) && d.Name != ResultTool {
				desc = d.Name
			}
		}
		return found, desc
	}
	if f, _ := has("work"); f {
		t.Fatal("no async tool, no ext_result")
	}
	src := strings.Replace(toolSrc("slow_report", "read", `"work" "review"`, ``), "timeout 5", "timeout 60", 1)
	v.svc.Add(ctx, src)
	v.svc.Enable(ctx, "alpha", "slow_report")
	if f, d := has("work"); !f || d != "ext_slow_report" {
		t.Fatalf("work: found=%v describing=%q", f, d)
	}
	if f, _ := has("review"); !f {
		t.Fatal("a read async tool is visible to review runs, so is ext_result")
	}
	if f, _ := has("revise"); f {
		t.Fatal("the async tool is not visible to revise runs, so ext_result is not offered")
	}
	ds, _ := v.svc.Declarations(ctx, 1, "work")
	for _, d := range ds {
		if d.Name == ResultTool && (len(d.InputSchema) == 0 || strings.Contains(string(d.InputSchema), "kube")) {
			t.Fatalf("result declaration: %+v", d)
		}
	}
}
