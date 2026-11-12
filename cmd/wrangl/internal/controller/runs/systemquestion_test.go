package runs_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// One blocker must be one Inbox card: when the runner raises the "stopped twice" system
// question, the agent's own still-open questions for the run are folded into it.
func TestStoppedTwiceFoldsOpenAgentQuestions(t *testing.T) {
	m := newMaint(t)
	ctx := context.Background()
	run := m.startRun(time.Minute)
	ask := func(tool, prompt string) {
		t.Helper()
		args, _ := json.Marshal(map[string]any{"prompt": prompt})
		sc, err := m.rs.ScopeForRun(ctx, run)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := m.rs.ExecTool(ctx, sc, tool, args); err != nil {
			t.Fatal(err)
		}
	}
	ask("ask_question", "Which database should I use?")
	ask("ask_question", "Should I keep the old API?")
	ask("_system_question", "The agent stopped twice without completing it. What now?")

	if n := m.count(`SELECT COUNT(*) FROM question WHERE run_id=? AND state='open'`, run); n != 1 {
		t.Fatalf("want 1 open card for the run, got %d", n)
	}
	if n := m.count(`SELECT COUNT(*) FROM question WHERE run_id=? AND kind='agent' AND state='cancelled'`, run); n != 2 {
		t.Fatalf("agent questions should be cancelled, got %d", n)
	}
	var detail string
	m.d.R.QueryRow(`SELECT detail_md FROM question WHERE run_id=? AND kind='system'`, run).Scan(&detail)
	for _, want := range []string{"Which database should I use?", "Should I keep the old API?"} {
		if !strings.Contains(detail, want) {
			t.Errorf("system question body lost %q:\n%s", want, detail)
		}
	}
}
