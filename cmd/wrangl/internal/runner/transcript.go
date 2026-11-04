package runner

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/natsd"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/transcript"
)

// TranscriptPoll is how often the tailer checks the transcript for new lines.
var TranscriptPoll = 250 * time.Millisecond

// maxTranscriptLine bounds one buffered line; longer ones are dropped.
const maxTranscriptLine = 8 << 20

// tailer follows a growing JSONL transcript and hands each normalised entry to
// publish. It copes with a file that does not exist yet, a partial last line
// (held back until its newline arrives) and truncation (reading restarts from
// the top). The path is re-resolved on every tick so a hook can supply it late.
type tailer struct {
	path    func() string // "" until known
	norm    *transcript.Normaliser
	publish func(transcript.Entry)
	offsets map[string]int64 // resume offsets per file
}

// run polls until ctx ends, then drains once more so nothing written just
// before the run ended is lost.
func (t *tailer) run(ctx context.Context) {
	tick := time.NewTicker(TranscriptPoll)
	defer tick.Stop()
	for {
		done := ctx.Err() != nil
		if p := t.path(); p != "" {
			t.poll(p)
		}
		if done {
			return
		}
		select {
		case <-ctx.Done():
		case <-tick.C:
		}
	}
}

func (t *tailer) poll(path string) {
	f, err := os.Open(path)
	if err != nil {
		return // not created yet
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return
	}
	off := t.offsets[path]
	if st.Size() < off { // truncated or replaced: start over
		off = 0
	}
	if st.Size() == off {
		t.offsets[path] = off
		return
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return
	}
	buf, err := io.ReadAll(io.LimitReader(f, st.Size()-off))
	if err != nil && len(buf) == 0 {
		return
	}
	// Only complete lines are consumed: the offset stops before a partial last
	// line, which is re-read once its newline has arrived.
	for {
		i := indexByte(buf, '\n')
		if i < 0 {
			break
		}
		for _, e := range t.norm.Parse(buf[:i]) {
			t.publish(e)
		}
		off += int64(i + 1)
		buf = buf[i+1:]
	}
	if len(buf) > maxTranscriptLine { // runaway line without a newline: skip it
		off += int64(len(buf))
	}
	t.offsets[path] = off
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}

var nonPathChar = regexp.MustCompile(`[^A-Za-z0-9]`)

// claudeTranscriptPath derives Claude Code's transcript location:
// <home>/.claude/projects/<cwd with non-alphanumerics as "-">/<session>.jsonl.
func claudeTranscriptPath(home, cwd, sessionID string) string {
	if sessionID == "" {
		return ""
	}
	return filepath.Join(home, ".claude", "projects", nonPathChar.ReplaceAllString(cwd, "-"), sessionID+".jsonl")
}

// noteTranscriptPath records the transcript_path a hook reported (the first wins).
func (s *session) noteTranscriptPath(p string) {
	if p == "" {
		return
	}
	s.mu.Lock()
	if s.tsPath == "" {
		s.tsPath = p
	}
	s.mu.Unlock()
}

// transcriptPath is the hook-reported path, else one derived from the session id.
func (s *session) transcriptPath() string {
	s.mu.Lock()
	p, id := s.tsPath, s.a.SessionUUID
	if id == "" {
		id = s.agentSession
	}
	s.mu.Unlock()
	if p != "" {
		return p
	}
	return claudeTranscriptPath(s.id.home, s.dir, id)
}

// startTranscriptTail tails the run's transcript until ctx ends; the returned
// channel closes once the final drain is published. Without a bus it does nothing.
func (s *session) startTranscriptTail(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	if s.w.JS == nil {
		close(done)
		return done
	}
	subj := natsd.RunTranscriptSubject(s.w.Cfg.RunnerID, s.a.RunID)
	s.mu.Lock()
	if s.tsOffsets == nil {
		s.tsOffsets = map[string]int64{}
	}
	s.mu.Unlock()
	t := &tailer{
		path: s.transcriptPath, norm: &s.tsNorm, offsets: s.tsOffsets,
		publish: func(e transcript.Entry) {
			b, _ := json.Marshal(e)
			if _, err := s.w.JS.PublishAsync(subj, b); err != nil {
				s.w.Log.Warn("transcript publish", "err", err)
			}
		},
	}
	go func() { defer close(done); t.run(ctx) }()
	return done
}
