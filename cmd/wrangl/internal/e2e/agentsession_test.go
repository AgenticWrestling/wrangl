package e2e

import (
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

// execAgent behaves like `codex exec --json`: a new session prints its thread id first;
// `resume <id>` continues one. It also reports a different id through a SessionStart
// hook, which must lose to the id the agent printed.
const execAgent = `#!/bin/sh
tool() { curl -s --unix-socket "$WRANGL_SOCK" -d "$2" "http://x/tool/$1"; }
if [ "$1" = resume ]; then echo "resuming session $2"; prompt="$3"
else
  echo '{"type":"thread.started","thread_id":"th-run'"$WRANGL_RUN_ID"'"}'
  curl -s --unix-socket "$WRANGL_SOCK" -d '{"event":"SessionStart","payload":{"session_id":"from-hook"}}' http://x/hook >/dev/null
  prompt="$2"
fi
grep -q '"x": 1' "$HOME/.fake/settings.json" && grep -q '"y": 2' "$HOME/.fake/settings.json" && grep -q '"keep": true' "$HOME/.fake/settings.json" && echo "merged ok"
printf '%s' "$prompt" | grep -o 'This is a documentation task\|call the task_complete tool' || true
sqid=$(printf '%s' "$prompt" | head -1 | sed -n 's/^You are working on task \([A-Za-z0-9]*\):.*/\1/p')
printf '%s\n' "$sqid" >> NOTES.md
tool task_complete "{\"sqid\":\"$sqid\",\"summary\":\"done\"}" >/dev/null
`

func TestAgentSessionResume(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs linux")
	}
	h := newAgentHarness(t, execAgent, `name: execy
arch: %s
min: {vcpu: 1, ram: 256MiB, disk: 1GiB}
mise_toml: ""
agent:
  start:  [%[2]q, "new", "{{.Prompt}}"]
  resume: [%[2]q, "resume", "{{.AgentSessionID}}", "{{.Prompt}}"]
  files:
    - {path: "{{.Home}}/.fake/settings.json", content: '{"a": {"x": 1}, "keep": true}'}
    - {path: "{{.Home}}/.fake/settings.json", merge: json, content: '{"a": {"y": 2}}'}
`, nil)
	defer h.stop()
	parent := h.mk(proto.TaskCreateRequest{Title: "two steps", Type: "docs"})
	for _, title := range []string{"one", "two"} {
		h.mk(proto.TaskCreateRequest{Title: title, Parent: parent})
	}
	runID, _, err := h.api.QueueTask(h.ctx, parent)
	if err != nil {
		t.Fatal(err)
	}
	v := h.waitRun(runID, "succeeded", "failed", "blocked")
	text := h.runLog(runID)
	if v.State != "succeeded" {
		t.Fatalf("run: %+v\n%s", v, text)
	}
	want := "resuming session th-run" + strconv.FormatInt(runID, 10)
	if !strings.Contains(text, want) || strings.Contains(text, "from-hook") || strings.Contains(text, "instead of resuming") {
		t.Fatalf("want %q (the id the agent printed, not the hook's):\n%s", want, text)
	}
	if strings.Count(text, "merged ok") != 2 {
		t.Fatalf("merged settings file not seen by both turns:\n%s", text)
	}
	if !strings.Contains(text, "call the task_complete tool") {
		t.Fatalf("the prompt must tell the agent to call task_complete:\n%s", text)
	}
	if !strings.Contains(text, "This is a documentation task") {
		t.Fatalf("task-type guidance missing from the prompt:\n%s", text)
	}
}
