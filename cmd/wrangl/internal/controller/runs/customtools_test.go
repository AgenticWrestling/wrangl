package runs_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/runs"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/tools"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/errs"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/toolkdl"
)

const wipeKDL = `tool "wipe" {
  description "Wipe an environment"
  effect "destructive"
  run-kinds "work"
  input { param "env" type="string" { enum "staging" "prod" } }
  exec { argv "wipe" "{{input.env}}" }
}`

const statusKDL = `tool "status" {
  description "Show status"
  effect "read"
  run-kinds "work" "review"
  exec { argv "status" }
}`

// ExecTool routes ext_ names to the engine with the scope the controller derived for the run,
// and a destructive call goes through the real approval question and its answer.
func TestExecToolCustomTools(t *testing.T) {
	m := newMaint(t)
	ctx := context.Background()
	runID := m.startRun(time.Minute)
	ran := 0
	svc := &tools.Service{DB: m.d}
	m.rs.Custom = &tools.Engine{Svc: svc, Approver: m.rs, Open: m.rs.QuestionOpen, Now: m.rs.Now, Executors: map[toolkdl.Kind]tools.Executor{
		toolkdl.KindExec: tools.ExecutorFunc(func(ctx context.Context, c *tools.Call) ([]byte, error) {
			ran++
			return []byte("done " + strings.Join(c.Def.Exec.Argv, " ")), nil
		})}}
	m.rs.ApprovalHandlers = map[string]func(context.Context, json.RawMessage) error{tools.ApprovalOp: m.rs.Custom.Approved}
	for _, src := range []string{wipeKDL, statusKDL} {
		if _, err := svc.Add(ctx, src); err != nil {
			t.Fatal(err)
		}
		name := "wipe"
		if src == statusKDL {
			name = "status"
		}
		if err := svc.Enable(ctx, "p", name); err != nil {
			t.Fatal(err)
		}
	}
	sc, err := m.rs.ScopeForRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}

	out, err := m.rs.ExecTool(ctx, sc, "ext_status", nil)
	if err != nil || out.(map[string]any)["output"] != "done status" {
		t.Fatalf("read tool: %v %v", out, err)
	}
	// stack tool names are untouched, and an unknown ext_ name is a scope error
	if _, err := m.rs.ExecTool(ctx, sc, "ext_nope", nil); !errors.Is(err, errs.Forbidden) {
		t.Fatalf("unknown: %v", err)
	}
	if _, err := m.rs.ExecTool(ctx, runs.UserScope(), "ext_status", nil); !errors.Is(err, errs.Forbidden) {
		t.Fatalf("owner scope: %v", err)
	}

	// destructive: pending, a real approval question exists, answering approve lets the repeat run
	out, err = m.rs.ExecTool(ctx, sc, "ext_wipe", json.RawMessage(`{"env":"staging"}`))
	pend, _ := out.(map[string]any)
	if err != nil || pend["pending_approval"] != true || ran != 1 {
		t.Fatalf("destructive first call: %v %v ran=%d", out, err, ran)
	}
	qid := pend["question_id"].(int64)
	if n := m.count(`SELECT COUNT(*) FROM question WHERE id=? AND kind='approval' AND state='open'`, qid); n != 1 {
		t.Fatalf("approval question missing")
	}
	if err := m.rs.Answer(ctx, qid, "approve", ""); err != nil {
		t.Fatal(err)
	}
	out, err = m.rs.ExecTool(ctx, sc, "ext_wipe", json.RawMessage(`{"env":"staging"}`))
	if err != nil || out.(map[string]any)["output"] != "done wipe {{input.env}}" || ran != 2 {
		t.Fatalf("approved call: %v %v ran=%d", out, err, ran)
	}

	// a denied one never runs
	out, _ = m.rs.ExecTool(ctx, sc, "ext_wipe", json.RawMessage(`{"env":"prod"}`))
	if err := m.rs.Answer(ctx, out.(map[string]any)["question_id"].(int64), "deny", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := m.rs.ExecTool(ctx, sc, "ext_wipe", json.RawMessage(`{"env":"prod"}`)); !errors.Is(err, errs.Forbidden) || ran != 2 {
		t.Fatalf("denied: %v ran=%d", err, ran)
	}

	// one audit row per run call (the owner scope has no run to audit against)
	if n := m.count(`SELECT COUNT(*) FROM tool_call WHERE run_id=?`, runID); n != 6 {
		t.Fatalf("audit rows: %d", n)
	}
}

func TestExecToolCustomWithoutEngine(t *testing.T) {
	m := newMaint(t)
	if _, err := m.rs.ExecTool(context.Background(), runs.Scope{RunID: 1, Kind: "work", ProjectID: 1}, "ext_x", nil); !errors.Is(err, errs.Forbidden) {
		t.Fatalf("%v", err)
	}
}
