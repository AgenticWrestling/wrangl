package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/transcript"
)

type collector struct {
	mu sync.Mutex
	es []transcript.Entry
}

func (c *collector) add(e transcript.Entry) { c.mu.Lock(); c.es = append(c.es, e); c.mu.Unlock() }
func (c *collector) get() []transcript.Entry {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]transcript.Entry(nil), c.es...)
}
func (c *collector) waitFor(t *testing.T, n int) []transcript.Entry {
	t.Helper()
	for i := 0; i < 400; i++ {
		if es := c.get(); len(es) >= n {
			return es
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d entries; have %+v", n, c.get())
	return nil
}

func TestTailerAppendPartialTruncate(t *testing.T) {
	old := TranscriptPoll
	TranscriptPoll = 10 * time.Millisecond
	defer func() { TranscriptPoll = old }()

	fx, err := os.ReadFile("../transcript/testdata/session.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.SplitAfter(string(fx), "\n")
	path := filepath.Join(t.TempDir(), "s.jsonl") // does not exist yet
	var col collector
	tl := &tailer{path: func() string { return path }, norm: &transcript.Normaliser{},
		publish: col.add, offsets: map[string]int64{}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { tl.run(ctx); close(done) }()

	time.Sleep(30 * time.Millisecond) // file still missing: nothing, no crash
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	// First 3 fixture lines: unknown, user text, assistant thinking+text => 3 entries.
	f.WriteString(strings.Join(lines[:3], ""))
	col.waitFor(t, 3)

	// A partial line is held back until completed.
	f.WriteString(`{"type":"assistant","timestamp":"2026-01-01T10:00:02Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","na`)
	time.Sleep(60 * time.Millisecond)
	if n := len(col.get()); n != 3 {
		t.Fatalf("partial line published: %d entries", n)
	}
	f.WriteString(`me":"Bash","input":{"command":"go test ./..."}}]}}` + "\n")
	es := col.waitFor(t, 4)
	if es[3].Kind != transcript.KindToolUse || es[3].Summary != "Bash: go test ./..." {
		t.Fatalf("entry 4 = %+v", es[3])
	}

	// Truncate and rewrite: tailing restarts from the top; seq keeps increasing.
	f.Truncate(0)
	f.Seek(0, 0)
	f.WriteString(`{"type":"user","message":{"content":"after truncate"}}` + "\n")
	es = col.waitFor(t, 5)
	if es[4].Text != "after truncate" {
		t.Fatalf("entry 5 = %+v", es[4])
	}

	cancel()
	<-done
	for i, e := range col.get() {
		if e.Seq != i+1 {
			t.Errorf("entry %d seq = %d", i, e.Seq)
		}
	}
}

func TestTailerFinalDrain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	os.WriteFile(path, []byte(`{"type":"user","message":{"content":"late"}}`+"\n"), 0o600)
	var col collector
	tl := &tailer{path: func() string { return path }, norm: &transcript.Normaliser{},
		publish: col.add, offsets: map[string]int64{}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // run already over: the drain still publishes what is on disk
	tl.run(ctx)
	if len(col.get()) != 1 {
		t.Fatalf("got %+v", col.get())
	}
	// A second tailer sharing the offsets (resumed turn) does not repeat it.
	tl.run(ctx)
	if len(col.get()) != 1 {
		t.Fatalf("repeated: %+v", col.get())
	}
}

func TestClaudeTranscriptPath(t *testing.T) {
	got := claudeTranscriptPath("/home/a", "/home/a/work/12", "abc")
	if want := "/home/a/.claude/projects/-home-a-work-12/abc.jsonl"; got != want {
		t.Errorf("got %q want %q", got, want)
	}
	if claudeTranscriptPath("/h", "/w", "") != "" {
		t.Error("no session id should give no path")
	}
}
