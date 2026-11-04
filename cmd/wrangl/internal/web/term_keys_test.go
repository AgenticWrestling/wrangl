package web

import (
	"os/exec"
	"strings"
	"testing"
)

// The key bar must follow the app's DECCKM mode: ESC O A in application cursor mode, ESC [ A otherwise.
func TestTermKeysApplicationCursor(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	script := `const k = require('./static/term-keys.js');
const out = [k.csi('A', 0, 1, false), k.csi('A', 0, 1, true), k.csi('F', 0, 1, true),
  k.csi('~', 5, 1, true), k.csi('A', 0, 5, true)];
console.log(JSON.stringify(out));`
	cmd := exec.Command(node, "-e", script)
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	want := `["\u001b[A","\u001bOA","\u001bOF","\u001b[5~","\u001b[1;5A"]`
	if got := strings.TrimSpace(string(b)); got != want {
		t.Fatalf("got %s want %s", got, want)
	}
}

// While replayed history is written into wterm, the replies it generates to old queries
// (DA, cursor position) must not reach the PTY; after the live marker they must.
func TestTermReplayRepliesSuppressed(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	script := `const k = require('./static/term-keys.js');
const sent = [];
const gate = k.replyGate();
const onData = gate.onData((d) => sent.push(d));
// fake terminal: every write containing a DA query replies synchronously, like wterm
const write = (b) => { if (b.includes('\x1b[c')) onData('\x1b[?62c'); };
gate.write(write, 'old output\x1b[c');   // replay
onData('ls\r');                            // user typing during replay is not a reply
gate.live();
gate.write(write, 'new\x1b[c');           // live query is answered
console.log(JSON.stringify(sent));`
	b, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	want := `["ls\r","\u001b[?62c"]`
	if got := strings.TrimSpace(string(b)); got != want {
		t.Fatalf("got %s want %s", got, want)
	}
}
