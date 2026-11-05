package runner

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

func TestStopMessageAndDetail(t *testing.T) {
	p := json.RawMessage(`{"hook_event_name":"Stop","last_assistant_message":"Done with **steps 1-5**.\n- next: card"}`)
	if got := stopMessage("Stop", p); !strings.Contains(got, "**steps 1-5**") {
		t.Fatalf("stopMessage: %q", got)
	}
	if got := stopMessage("PostToolUse", p); got != "" {
		t.Fatalf("other events carry no stop message: %q", got)
	}
	s := &session{}
	s.noteLastMessage("  ")
	if s.lastMessage() != "" {
		t.Fatal("blank message kept")
	}
	s.noteLastMessage("hello")
	if d := stoppedDetail(s.lastMessage(), "raw"); !strings.HasPrefix(d, "**The agent's last message:**") || strings.Contains(d, "```") {
		t.Fatalf("detail should be the agent's Markdown, not a code fence: %q", d)
	}
	if d := stoppedDetail("", "raw tail"); !strings.Contains(d, "```\nraw tail\n```") {
		t.Fatalf("fallback detail: %q", d)
	}
}

func TestNudgeAndReplyPrompts(t *testing.T) {
	n := &proto.StackNext{Sqid: "abc123", Title: "Do it"}
	if p := nudgePrompt(n); !strings.Contains(p, "ask_question") || !strings.Contains(p, "task_complete") {
		t.Fatalf("nudge: %q", p)
	}
	if p := ownerReplyPrompt(n, " use slice order B \n"); !strings.Contains(p, "use slice order B") || !strings.Contains(p, "abc123") {
		t.Fatalf("reply: %q", p)
	}
}
