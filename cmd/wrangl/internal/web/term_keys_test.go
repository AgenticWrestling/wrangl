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
