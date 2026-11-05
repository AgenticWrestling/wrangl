package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/errs"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/toolkdl"
)

func toolSrc(name, effect, runKinds, secret string) string {
	return fmt.Sprintf(`tool %q {
  description "Tool %s"
  effect %q
  run-kinds %s
  input { param "env" type="string" { enum "staging" "prod" } }
  exec {
    argv "kubectl" "get" "{{input.env}}"
    %s
    timeout 5
    max-output-kb 1
  }
}`, name, name, effect, runKinds, secret)
}

type fakeApprover struct {
	n      int
	prompt string
	open   bool
}

func (a *fakeApprover) RequestApproval(ctx context.Context, runID int64, prompt, detail string, payload any) (int64, error) {
	a.n++
	a.prompt = prompt + detail
	a.open = true
	return int64(100 + a.n), nil
}

type env struct {
	e    *Engine
	svc  *Service
	ap   *fakeApprover
	runs []*Call
	out  []byte
	err  error
}

func newEnv(t *testing.T) *env {
	t.Helper()
	svc := newSvc(t)
	withStore(svc)
	var logs bytes.Buffer
	svc.Log = slog.New(slog.NewTextHandler(&logs, nil))
	v := &env{svc: svc, ap: &fakeApprover{}}
	v.e = &Engine{Svc: svc, Approver: v.ap, Now: time.Now, Log: svc.Log,
		Open: func(ctx context.Context, id int64) (bool, error) { return v.ap.open, nil },
		Executors: map[toolkdl.Kind]Executor{toolkdl.KindExec: ExecutorFunc(func(ctx context.Context, c *Call) ([]byte, error) {
			v.runs = append(v.runs, c)
			return v.out, v.err
		})}}
	for _, src := range []string{
		toolSrc("deploy_status", "read", `"work" "revise" "review"`, `secret "KUBECONFIG" name="kube.prod" as="file"`),
		toolSrc("restart", "write", `"work"`, ``),
		toolSrc("wipe", "destructive", `"work" "revise"`, ``),
		toolSrc("work_only", "read", `"work"`, ``),
	} {
		if _, err := svc.Add(context.Background(), src); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range []string{"deploy_status", "restart", "wipe", "work_only"} {
		svc.Enable(context.Background(), "alpha", n)
	}
	for _, q := range []string{
		`INSERT INTO task (project_id, rank, title, sqid) VALUES (1, 'V', 'ta', 'AAAAAA')`,
		`INSERT INTO run (id, task_id) VALUES (7, 1), (8, 1)`,
	} {
		if _, err := svc.DB.W.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	svc.SetSecret(context.Background(), "alpha", "kube.prod", strings.NewReader("KUBE-SECRET-VALUE"))
	return v
}

func (v *env) call(kind, name, args string) (any, error) {
	return v.e.Call(context.Background(), Caller{RunID: 7, Kind: kind, ProjectID: 1}, Prefix+name, json.RawMessage(args))
}

func (v *env) audit(t *testing.T) []string {
	t.Helper()
	rows, err := v.svc.DB.RQ.ListToolCallsForRun(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range rows {
		out = append(out, r.Tool+":"+r.Status)
	}
	return out
}

func TestScopeAndEffectPolicy(t *testing.T) {
	v := newEnv(t)
	v.out = []byte("ok")

	// a run of the right kind in the right project may call it
	if out, err := v.call("work", "deploy_status", `{"env":"prod"}`); err != nil || out.(map[string]any)["output"] != "ok" {
		t.Fatalf("work: %v %v", out, err)
	}
	// review runs: read tools whose run-kinds include review only
	if _, err := v.call("review", "deploy_status", `{"env":"prod"}`); err != nil {
		t.Fatalf("review read tool: %v", err)
	}
	for _, c := range []struct{ kind, tool string }{
		{"review", "restart"}, {"plan", "restart"}, {"review", "wipe"}, {"plan", "work_only"}, // write/destructive, or kind not listed
		{"revise", "work_only"}, {"resolve", "deploy_status"}, {"user", "deploy_status"},
	} {
		if _, err := v.call(c.kind, c.tool, `{"env":"prod"}`); !errors.Is(err, errs.Forbidden) {
			t.Errorf("%s run calling %s: %v", c.kind, c.tool, err)
		}
	}
	if n := len(v.runs); n != 2 {
		t.Fatalf("executor ran %d times, want 2", n)
	}

	// caller without a run or project (the owner's remote MCP), a project that did not enable it
	if _, err := v.e.Call(context.Background(), Caller{Kind: "user"}, Prefix+"deploy_status", nil); !errors.Is(err, errs.Forbidden) {
		t.Fatalf("no run: %v", err)
	}
	v.svc.DB.W.Exec(`INSERT INTO project (slug, name) VALUES ('gamma','Gamma')`)
	if _, err := v.e.Call(context.Background(), Caller{RunID: 7, Kind: "work", ProjectID: 3}, Prefix+"deploy_status", json.RawMessage(`{"env":"prod"}`)); !errors.Is(err, errs.Forbidden) {
		t.Fatalf("other project: %v", err)
	}
	v.svc.Disable(context.Background(), "alpha", "work_only")
	if _, err := v.call("work", "work_only", `{"env":"prod"}`); !errors.Is(err, errs.Forbidden) {
		t.Fatalf("disabled: %v", err)
	}
	if _, err := v.call("work", "no_such_tool", `{}`); !errors.Is(err, errs.Forbidden) {
		t.Fatalf("unknown: %v", err)
	}
	// the project always comes from the scope: arguments cannot name one
	if _, err := v.call("work", "deploy_status", `{"env":"prod","project":"beta"}`); !errors.Is(err, errs.Invalid) {
		t.Fatalf("project argument: %v", err)
	}
}

func TestInputValidatedBeforeExecution(t *testing.T) {
	v := newEnv(t)
	for _, in := range []string{`{}`, `{"env":"dev"}`, `{"env":"-n"}`, `{"env":"prod","x":1}`, `[]`} {
		if _, err := v.call("work", "deploy_status", in); !errors.Is(err, errs.Invalid) {
			t.Errorf("%s: %v", in, err)
		}
	}
	if len(v.runs) != 0 {
		t.Fatal("executor ran on invalid input")
	}
	v.out = []byte("x")
	v.call("work", "deploy_status", `{"env":"staging"}`)
	if c := v.runs[0]; c.Input.Args["env"] != "staging" || c.Project != "alpha" || c.RunID != 7 {
		t.Fatalf("call: %+v", c)
	}
}

func TestResultCapScrubAndAudit(t *testing.T) {
	v := newEnv(t)
	v.out = []byte("kubeconfig: KUBE-SECRET-VALUE\n" + strings.Repeat("x", 3000) + "é")
	out, err := v.call("work", "deploy_status", `{"env":"prod"}`)
	if err != nil {
		t.Fatal(err)
	}
	m := out.(map[string]any)
	text := m["output"].(string)
	if strings.Contains(text, "KUBE-SECRET-VALUE") || !strings.Contains(text, Redacted) {
		t.Fatalf("not scrubbed: %.80q", text)
	}
	if m["truncated"] != true || !strings.Contains(text, "truncated:") || len(text) > 1024+200 {
		t.Fatalf("not capped: len=%d truncated=%v", len(text), m["truncated"])
	}

	// errors are scrubbed too, and a missing secret is named without any value
	v.out, v.err = nil, errors.New("auth failed with KUBE-SECRET-VALUE")
	if _, err := v.call("work", "deploy_status", `{"env":"prod"}`); err == nil || strings.Contains(err.Error(), "KUBE-SECRET-VALUE") {
		t.Fatalf("error not scrubbed: %v", err)
	}
	v.err = nil
	if _, err := v.e.Call(context.Background(), Caller{RunID: 7, Kind: "work", ProjectID: 1}, Prefix+"deploy_status", json.RawMessage(`{"env":"prod"}`)); err != nil {
		t.Fatal(err)
	}
	v.svc.GetSecret = func(string) ([]byte, error) { return nil, errors.New("gone") }
	if _, err := v.call("work", "deploy_status", `{"env":"prod"}`); err == nil || !strings.Contains(err.Error(), "kube.prod") {
		t.Fatalf("missing secret: %v", err)
	}

	got := v.audit(t)
	want := []string{"ext_deploy_status:ok", "ext_deploy_status:error", "ext_deploy_status:ok", "ext_deploy_status:error"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("audit rows: %v", got)
	}
	rows, _ := v.svc.DB.RQ.ListToolCallsForRun(context.Background(), 7)
	if rows[0].Bytes != int64(len(text)) || rows[0].RunID != 7 {
		t.Fatalf("audit row: %+v", rows[0])
	}
}

func TestExecutorMissingAndTimeout(t *testing.T) {
	v := newEnv(t)
	delete(v.e.Executors, toolkdl.KindExec)
	if _, err := v.call("work", "restart", `{"env":"prod"}`); err == nil || !strings.Contains(err.Error(), "cannot run") {
		t.Fatalf("no executor: %v", err)
	}
	v.e.Executors[toolkdl.KindExec] = ExecutorFunc(func(ctx context.Context, c *Call) ([]byte, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("executor context has no deadline")
		}
		return []byte("fine"), nil
	})
	if _, err := v.call("work", "restart", `{"env":"prod"}`); err != nil {
		t.Fatal(err)
	}
}

func TestDestructiveNeedsApproval(t *testing.T) {
	v := newEnv(t)
	v.out = []byte("wiped")
	args := `{"env":"staging"}`

	out, err := v.call("work", "wipe", args)
	m, _ := out.(map[string]any)
	if err != nil || m["pending_approval"] != true || m["question_id"] != int64(101) || len(v.runs) != 0 {
		t.Fatalf("first call: %v %v runs=%d", out, err, len(v.runs))
	}
	if !strings.Contains(v.ap.prompt, "ext_wipe") || !strings.Contains(v.ap.prompt, "staging") {
		t.Fatalf("approval prompt: %q", v.ap.prompt)
	}
	// asking again while the owner thinks does not create another question
	out, _ = v.call("work", "wipe", `{ "env" : "staging" }`)
	if out.(map[string]any)["question_id"] != int64(101) || v.ap.n != 1 {
		t.Fatalf("repeat: %v asked=%d", out, v.ap.n)
	}
	// polling the same pending question does not add audit rows: one for the request
	if rows := v.audit(t); len(rows) != 1 || rows[0] != "ext_wipe:pending_approval" {
		t.Fatalf("audit while polling: %v", rows)
	}
	// different arguments are a different call
	if out, _ := v.call("work", "wipe", `{"env":"prod"}`); out.(map[string]any)["question_id"] != int64(102) {
		t.Fatalf("other args: %v", out)
	}
	// the owner approves the first one: the repeated call runs, once
	var stagingKey string
	v.e.mu.Lock()
	for k, q := range v.e.pending {
		if q == 101 {
			stagingKey = k
		}
	}
	v.e.mu.Unlock()
	b, _ := json.Marshal(approvalPayload{Op: ApprovalOp, RunID: 7, Tool: "ext_wipe", Key: stagingKey})
	if err := v.e.Approved(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	v.ap.open = false
	out, err = v.call("work", "wipe", args)
	if err != nil || out.(map[string]any)["output"] != "wiped" || len(v.runs) != 1 {
		t.Fatalf("approved call: %v %v", out, err)
	}
	// the approval was single-use
	if out, _ := v.call("work", "wipe", args); out.(map[string]any)["pending_approval"] != true || v.ap.n != 3 {
		t.Fatalf("second use: %v asked=%d", out, v.ap.n)
	}
	// another run cannot use run 7's approval
	if _, err := v.e.Call(context.Background(), Caller{RunID: 8, Kind: "work", ProjectID: 1}, Prefix+"wipe", json.RawMessage(args)); err != nil {
		t.Fatal(err)
	}
	if len(v.runs) != 1 {
		t.Fatalf("approval leaked to another run")
	}
}

func TestDestructiveDenied(t *testing.T) {
	v := newEnv(t)
	v.call("work", "wipe", `{"env":"staging"}`)
	v.ap.open = false // answered, but not approved
	if _, err := v.call("work", "wipe", `{"env":"staging"}`); !errors.Is(err, errs.Forbidden) || !strings.Contains(err.Error(), "declined") {
		t.Fatalf("denied: %v", err)
	}
	if len(v.runs) != 0 {
		t.Fatal("ran without approval")
	}
	got := strings.Join(v.audit(t), ",")
	if got != "ext_wipe:pending_approval,ext_wipe:denied" {
		t.Fatalf("audit: %s", got)
	}
}

func TestApprovalExpires(t *testing.T) {
	v := newEnv(t)
	now := time.Now()
	v.e.Now = func() time.Time { return now }
	v.call("work", "wipe", `{"env":"staging"}`)
	var key string
	for k := range v.e.pending {
		key = k
	}
	b, _ := json.Marshal(approvalPayload{Op: ApprovalOp, RunID: 7, Tool: "ext_wipe", Key: key})
	v.e.Approved(context.Background(), b)
	v.ap.open = false
	now = now.Add(2 * time.Hour)
	if out, _ := v.call("work", "wipe", `{"env":"staging"}`); out.(map[string]any)["pending_approval"] != true {
		t.Fatalf("stale approval accepted: %v", out)
	}
}

func TestDeclarationsAreRunnerSafe(t *testing.T) {
	v := newEnv(t)
	ctx := context.Background()
	names := func(kind string) string {
		ds, err := v.svc.Declarations(ctx, 1, kind)
		if err != nil {
			t.Fatal(err)
		}
		var n []string
		for _, d := range ds {
			n = append(n, d.Name)
			b, _ := json.Marshal(d)
			for _, leak := range []string{"kubectl", "kube.prod", "KUBECONFIG", "argv", "{{", "KUBE-SECRET-VALUE"} {
				if strings.Contains(string(b), leak) {
					t.Errorf("%s declaration leaks %q: %s", d.Name, leak, b)
				}
			}
			if !strings.HasPrefix(d.Name, Prefix) || len(d.InputSchema) == 0 || len(d.RunKinds) == 0 {
				t.Errorf("declaration: %+v", d)
			}
		}
		return strings.Join(n, ",")
	}
	if got := names("work"); got != "ext_deploy_status,ext_restart,ext_wipe,ext_work_only" {
		t.Fatalf("work: %s", got)
	}
	if got := names("revise"); got != "ext_deploy_status,ext_wipe" {
		t.Fatalf("revise: %s", got)
	}
	if got := names("review"); got != "ext_deploy_status" {
		t.Fatalf("review: %s", got)
	}
	if got := names("plan"); got != "" {
		t.Fatalf("plan: %s", got)
	}
	if ds, _ := v.svc.Declarations(ctx, 2, "work"); len(ds) != 0 {
		t.Fatalf("other project sees tools: %v", ds)
	}
}

func TestTruncateUTF8(t *testing.T) {
	s, tr := Truncate([]byte("aé"), 2) // cutting inside é must not emit a broken rune
	if !tr || !strings.HasPrefix(s, "a\n[truncated") {
		t.Fatalf("%q", s)
	}
	if s, tr := Truncate([]byte("abc"), 3); tr || s != "abc" {
		t.Fatalf("%q %v", s, tr)
	}
}
