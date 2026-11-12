package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/agentguard"
)

func guardEnv() agentguard.Env {
	return agentguard.Env{DefaultBranch: func(string) string { return "main" }, CurrentBranch: func(string) string { return "feature" }}
}

func guard(t *testing.T, payload string) (stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	if err := RunAgentGuard(strings.NewReader(payload), &out, &errb, guardEnv()); err != nil {
		t.Fatal(err)
	}
	return out.String(), errb.String()
}

func TestAgentGuardHookWireFormat(t *testing.T) {
	out, _ := guard(t, `{"hook_event_name":"PreToolUse","tool_name":"Agent","tool_input":{"subagent_type":"general-purpose","prompt":"x"},"cwd":"/w"}`)
	var raw map[string]map[string]string
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		t.Fatalf("not JSON: %q", out)
	}
	h := raw["hookSpecificOutput"]
	if h["hookEventName"] != "PreToolUse" || h["permissionDecision"] != "deny" || !strings.Contains(h["permissionDecisionReason"], "wrangl delegate") {
		t.Fatalf("%v", h)
	}
	// Allowed: nothing on stdout (so the normal permission rules decide).
	for _, p := range []string{
		`{"tool_name":"Agent","tool_input":{"subagent_type":"Explore"}}`,
		`{"tool_name":"Bash","tool_input":{"command":"git status"}}`,
		`{"tool_name":"Read","tool_input":{"file_path":"/x"}}`,
	} {
		if out, errs := guard(t, p); out != "" || errs != "" {
			t.Errorf("%s: stdout %q stderr %q", p, out, errs)
		}
	}
	if out, _ := guard(t, `{"tool_name":"Bash","tool_input":{"command":"claude -p hi"}}`); !strings.Contains(out, `"deny"`) {
		t.Errorf("bash claude: %q", out)
	}
	// A payload that is not JSON is allowed, with a note.
	if out, errs := guard(t, `not json`); out != "" || !strings.Contains(errs, "allowing") {
		t.Errorf("malformed: %q %q", out, errs)
	}
	if out, _ := guard(t, ``); out != "" {
		t.Errorf("empty: %q", out)
	}
}

func TestAgentGuardCommandIsWired(t *testing.T) {
	root := NewRoot()
	c, _, err := root.Find([]string{"hook", "agent-guard"})
	if err != nil || c.Name() != "agent-guard" {
		t.Fatalf("%v %v", c, err)
	}
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetIn(strings.NewReader(`{"tool_name":"Workflow","tool_input":{}}`))
	root.SetArgs([]string{"hook", "agent-guard"})
	if err := root.Execute(); err != nil || !strings.Contains(out.String(), "wrangl delegate") {
		t.Fatalf("%v %q", err, out.String())
	}
}
