package web

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/api"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/transcript"
)

func TestLiveMode(t *testing.T) {
	for _, tc := range []struct {
		name    string
		running bool
		n       int
		want    string
	}{
		{"running, no transcript", true, 0, livePTY},
		{"running, entries", true, 5, liveTranscript},
		{"finished, entries", false, 5, liveStored},
		{"finished, none", false, 0, liveNone},
	} {
		if got := liveMode(tc.running, tc.n); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

func fixtureEntries(t *testing.T) []transcript.Entry {
	t.Helper()
	b, err := os.ReadFile("../transcript/testdata/session.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	var n transcript.Normaliser
	var es []transcript.Entry
	for _, l := range bytes.Split(b, []byte("\n")) {
		es = append(es, n.Parse(l)...)
	}
	return es
}

func TestRenderEntries(t *testing.T) {
	out := string(renderEntries(fixtureEntries(t)))
	for _, want := range []string{"<strong>tests</strong>", "<pre><code", "Bash: go test ./...", `class="txchip"`, "✓", "✗"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %s", want, out)
		}
	}
	if strings.Contains(out, "<script>") || strings.Contains(out, "**tests**") {
		t.Errorf("unsafe or unrendered markdown: %s", out)
	}
	// Bash's call and result are one chip: the output sits inside it, once.
	if strings.Count(out, "0.01s") != 1 || strings.Count(out, "Bash: go test") != 1 {
		t.Errorf("tool call and result not merged: %s", out)
	}
	if strings.Contains(out, "API_TOKEN=supersecret") {
		t.Error("secret leaked")
	}
}

// The card's panel for the four cases.
func TestCardLiveOutput(t *testing.T) {
	h, err := New(api.Deps{})
	if err != nil {
		t.Fatal(err)
	}
	card := func(mode string, withEntries bool) string {
		d := map[string]any{
			"Task": dbgen.Task{Title: "t", State: "in_progress"}, "Project": "p", "LiveMode": mode,
		}
		if mode != liveNone {
			d["LiveRun"] = int64(7)
		}
		if withEntries {
			d["TxHTML"] = renderEntries(fixtureEntries(t))
			d["TxFirst"], d["TxLast"] = 1, 8
		}
		out, err := h.render("main-task", d)
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}
	run := card(liveTranscript, true)
	if !strings.Contains(run, `class="livetx"`) || !strings.Contains(run, "transcript-live?run=7&after=8") || strings.Contains(run, "liveterm") {
		t.Errorf("running with transcript: %s", run)
	}
	pty := card(livePTY, false)
	if !strings.Contains(pty, `class="liveterm"`) || !strings.Contains(pty, "No transcript") || strings.Contains(pty, "livetx") {
		t.Errorf("running without transcript: %s", pty)
	}
	done := card(liveStored, true)
	if !strings.Contains(done, `class="livetx"`) || strings.Contains(done, "transcript-live") || strings.Contains(done, "liveterm") || !strings.Contains(done, "finished") {
		t.Errorf("finished with entries must not open a live connection: %s", done)
	}
	none := card(liveNone, false)
	if strings.Contains(none, "livetx") || strings.Contains(none, "liveterm") {
		t.Errorf("finished without entries: %s", none)
	}
}
