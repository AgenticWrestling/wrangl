package sessionhost

import (
	"bytes"
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/charmbracelet/x/vt"
)

func TestRing(t *testing.T) {
	r := NewRing(8)
	r.Write([]byte("abc"))
	r.Write([]byte("defgh"))
	r.Write([]byte("ij"))
	if got := string(r.Bytes()); got != "cdefghij" {
		t.Fatalf("%q", got)
	}
	r.Write([]byte("0123456789ABCDEF"))
	if got := string(r.Bytes()); got != "89ABCDEF" {
		t.Fatalf("%q", got)
	}
}

func TestModeTrackerSplitSequencesAndKitty(t *testing.T) {
	m := newModeTracker()
	for _, p := range []string{"\x1b", "[?20", "04h", "x\x1b[?1000;1006h", "\x1b[>1u\x1b[>5u\x1b[<u", "\x1b[?1000l"} {
		m.feed([]byte(p))
	}
	if !m.on[2004] || !m.on[1006] || m.on[1000] || !m.on[7] {
		t.Fatalf("%v", m.on)
	}
	if len(m.kitty) != 1 || m.kitty[0] != 1 {
		t.Fatalf("kitty %v", m.kitty)
	}
	p := string(m.prologue())
	for _, w := range []string{"\x1b[?2004h", "\x1b[?1006h", "\x1b[?1000l", "\x1b[?1h"[:0] + "\x1b[?1l", "\x1b[<99u\x1b[>1u"} {
		if !strings.Contains(p, w) {
			t.Errorf("prologue lacks %q: %q", w, p)
		}
	}
	m.feed([]byte("\x1bc")) // RIS resets
	if m.on[2004] || len(m.kitty) != 0 {
		t.Errorf("RIS did not reset: %v %v", m.on, m.kitty)
	}
}

// ---- soak: a scripted long session, snapshot compared with the source cell by cell ----

type script struct {
	b    bytes.Buffer
	rnd  *rand.Rand
	step int
}

func (s *script) p(f string, a ...any) { fmt.Fprintf(&s.b, f, a...) }

// churn is what a Claude session looks like: styled prose, tool output scrolling past, a status
// line rewritten in place, spinner frames.
func (s *script) churn(lines int) {
	words := []string{"read", "edit", "ls -la", "cargo build", "ok", "PASS", "FAIL", "warning:", "src/main.rs", "日本語", "漢字かな", "😀", "é", "→", "━━━", "│"}
	for i := 0; i < lines; i++ {
		s.step++
		var l strings.Builder
		for w := 0; w < 3+s.rnd.Intn(12); w++ {
			l.WriteString(words[s.rnd.Intn(len(words))])
			l.WriteByte(' ')
		}
		switch s.rnd.Intn(5) {
		case 0:
			s.p("\x1b[1;32m%s\x1b[0m\r\n", l.String())
		case 1:
			s.p("\x1b[38;5;%dm\x1b[48;5;%dm%s\x1b[0m\r\n", s.rnd.Intn(256), s.rnd.Intn(256), l.String())
		case 2:
			s.p("\x1b[38;2;%d;%d;%dm%s\x1b[0m\r\n", s.rnd.Intn(256), s.rnd.Intn(256), s.rnd.Intn(256), l.String())
		case 3:
			s.p("\x1b[3;4m%s\x1b[0m\r\n", l.String())
		default:
			s.p("%s\r\n", l.String())
		}
		if i%7 == 0 { // status line: save cursor, rewrite the bottom row, restore
			s.p("\x1b7\x1b[999;1H\x1b[2K\x1b[7m status %d \x1b[0m\x1b8", s.step)
		}
	}
}

func (s *script) wide() {
	s.p("\x1b[2J\x1b[H")
	for _, l := range []string{"ＡＢＣ fullwidth", "日本語のテキスト、折り返しを確認します。日本語のテキスト、折り返しを確認します。日本語のテキスト、折り返しを確認します。", "👨‍👩‍👧 family 🏳️‍🌈 flag", "combining: áéö ñ", "tab\there\tand\ttabs", "┌──┬──┐\r\n│ab│cd│\r\n└──┴──┘"} {
		s.p("%s\r\n", l)
	}
}

func chunks(rnd *rand.Rand, b []byte) [][]byte { // random splits, including mid-rune and mid-escape
	var out [][]byte
	for len(b) > 0 {
		n := 1 + rnd.Intn(300)
		if n > len(b) {
			n = len(b)
		}
		out = append(out, b[:n])
		b = b[n:]
	}
	return out
}

// compare checks two emulators agree on every cell (content, width, style, link), the cursor and
// its visibility, and the alt-screen flag.
func compare(t *testing.T, label string, a, b *vt.SafeEmulator) {
	t.Helper()
	if a.Width() != b.Width() || a.Height() != b.Height() {
		t.Fatalf("%s: size %dx%d vs %dx%d", label, a.Width(), a.Height(), b.Width(), b.Height())
	}
	bad := 0
	for y := 0; y < a.Height(); y++ {
		for x := 0; x < a.Width(); x++ {
			ca, cb := a.CellAt(x, y), b.CellAt(x, y)
			if (ca == nil) != (cb == nil) {
				bad++
				continue
			}
			if ca == nil {
				continue
			}
			if ca.Content != cb.Content || ca.Width != cb.Width || ca.Style != cb.Style || ca.Link != cb.Link {
				if bad < 5 {
					t.Errorf("%s: cell (%d,%d): %+v vs %+v", label, x, y, *ca, *cb)
				}
				bad++
			}
		}
	}
	if bad > 0 {
		t.Fatalf("%s: %d cells differ", label, bad)
	}
	if a.CursorPosition() != b.CursorPosition() || a.CursorHidden() != b.CursorHidden() || a.IsAltScreen() != b.IsAltScreen() {
		t.Fatalf("%s: cursor %v hidden=%v alt=%v vs %v hidden=%v alt=%v", label, a.CursorPosition(), a.CursorHidden(), a.IsAltScreen(), b.CursorPosition(), b.CursorHidden(), b.IsAltScreen())
	}
}

// reattach plays the snapshot into a fresh emulator the way a client terminal would, and also feeds
// it to a mode tracker to check the modes come back.
func reattach(t *testing.T, scr *vtScreen, label string) {
	t.Helper()
	snap := scr.Snapshot()
	fresh := vt.NewSafeEmulator(scr.emu.Width(), scr.emu.Height())
	go func() { // a real terminal answers its own queries; here just keep the pipe drained
		b := make([]byte, 1024)
		for {
			if _, err := fresh.Read(b); err != nil {
				return
			}
		}
	}()
	defer closeEmu(fresh)
	fresh.Write(snap)
	compare(t, label, scr.emu, fresh)
	mt := newModeTracker()
	mt.feed(snap)
	scr.mmu.Lock()
	want := scr.modes
	scr.mmu.Unlock()
	for _, n := range trackedModes {
		if mt.on[n] != want.on[n] {
			t.Errorf("%s: mode %d = %v after snapshot, want %v", label, n, mt.on[n], want.on[n])
		}
	}
	if fmt.Sprint(mt.kitty) != fmt.Sprint(want.kitty) {
		t.Errorf("%s: kitty %v vs %v", label, mt.kitty, want.kitty)
	}
}

func TestSoakSnapshotMatchesSource(t *testing.T) {
	rnd := rand.New(rand.NewSource(7))
	scr := NewVTScreen(100, 30, func([]byte) {}).(*vtScreen)
	defer scr.Close()
	feed := func(b []byte) {
		for _, c := range chunks(rnd, b) {
			scr.Write(c)
		}
	}
	s := &script{rnd: rnd}

	// 1. Start-up: modes on, a long churn of tool output with a status line, then reattach.
	s.p("\x1b[?2004h\x1b[?1004h\x1b[>1u\x1b[?25l")
	s.churn(400)
	s.p("\x1b[?25h")
	feed(s.b.Bytes())
	s.b.Reset()
	reattach(t, scr, "after churn")

	// 2. Wide characters, emoji, combining marks, box drawing, tabs.
	s.wide()
	feed(s.b.Bytes())
	s.b.Reset()
	reattach(t, scr, "after wide chars")

	// 3. Resize mid-stream, in the middle of output, twice, then more output.
	s.churn(60)
	feed(s.b.Bytes())
	s.b.Reset()
	scr.Resize(80, 24)
	s.churn(80)
	feed(s.b.Bytes())
	s.b.Reset()
	reattach(t, scr, "after shrink")
	scr.Resize(120, 40)
	s.churn(80)
	s.wide()
	feed(s.b.Bytes())
	s.b.Reset()
	reattach(t, scr, "after grow")

	// 4. Alt screen (a full-screen tool), mouse and bracketed paste, then leave it again.
	s.p("\x1b[?1049h\x1b[?1000h\x1b[?1006h\x1b[2J\x1b[H")
	for y := 1; y <= 38; y++ {
		s.p("\x1b[%d;1H\x1b[44;97m%-60s\x1b[0m", y, fmt.Sprintf(" alt row %d 日本語 😀", y))
	}
	s.p("\x1b[10;10H\x1b[?25l")
	feed(s.b.Bytes())
	s.b.Reset()
	reattach(t, scr, "in alt screen")
	s.p("\x1b[?1049l\x1b[?1000l\x1b[?25h")
	s.churn(40)
	feed(s.b.Bytes())
	s.b.Reset()
	reattach(t, scr, "back from alt screen")

	// 5. Scroll region and reverse index, the way an editor scrolls.
	s.p("\x1b[5;20r\x1b[20;1H")
	for i := 0; i < 50; i++ {
		s.p("scroll region line %d\r\n", i)
	}
	s.p("\x1b[5;1H\x1bM\x1bM\x1b[r\x1b[H")
	feed(s.b.Bytes())
	s.b.Reset()
	reattach(t, scr, "after scroll region")

	if scr.qlenDropped() != 0 {
		t.Errorf("emulator queue dropped %d chunks", scr.qlenDropped())
	}
}

func TestSnapshotOfEmptyAndBulkDoesNotStall(t *testing.T) {
	scr := NewVTScreen(80, 24, nil).(*vtScreen)
	defer scr.Close()
	if len(scr.Snapshot()) == 0 {
		t.Fatal("empty snapshot")
	}
	// A runaway producer: Write must return immediately however slow the emulator is.
	old := maxQueued
	maxQueued = 300 << 10
	defer func() { maxQueued = old }()
	big := bytes.Repeat([]byte("0123456789\r\n"), 8000) // 96 KB
	for i := 0; i < 40; i++ {
		scr.Write(big)
	}
	scr.qmu.Lock()
	dropped := scr.dropped
	scr.qmu.Unlock()
	if dropped == 0 {
		t.Error("nothing dropped: the backlog is unbounded")
	}
	scr.Snapshot() // still answers once the backlog drains
}
