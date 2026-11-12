// Package sessionhost is the long-lived service that owns the PTYs of primary Claude sessions
// (ADR 0011 section 3). It runs apart from the controller so a controller restart or deploy does
// not kill a session. There is no terminal multiplexer: the host keeps a bounded raw ring, an
// emulated screen for lossless reattach snapshots, and any number of clients on a unix socket.
package sessionhost

import (
	"bytes"
	"fmt"
	"io"
	"strconv"
	"sync"

	"github.com/charmbracelet/x/vt"
)

// Screen is the host's model of what a session's terminal currently shows. It is an interface so
// the emulator (charmbracelet/x/vt is v0.1.0 with an unreleased dependency) can be replaced.
type Screen interface {
	// Write feeds the app's output, in order.
	Write(p []byte)
	// Resize changes the screen size; it is ordered with Write.
	Resize(cols, rows int)
	// Snapshot returns bytes that, written to a fresh terminal of the same size, reproduce the
	// current screen, cursor and tracked terminal modes.
	Snapshot() []byte
	// Close stops the screen's goroutines.
	Close()
}

// trackedModes are the DEC private modes a snapshot re-asserts. The emulator does not expose them
// (or does not apply them to a client), and a reattached client terminal would otherwise keep the
// previous attach's values: pasted text unbracketed, dead mouse, wrong cursor keys.
var trackedModes = []int{1, 7, 1000, 1002, 1003, 1004, 1005, 1006, 1015, 1016, 2004}

// modeTracker follows `CSI ? Pm h/l` and the kitty keyboard stack through the output stream, which
// may split a sequence across reads.
type modeTracker struct {
	on    map[int]bool
	kitty []int
	st    int // 0 text, 1 after ESC, 2 in CSI
	buf   []byte
}

func newModeTracker() *modeTracker {
	return &modeTracker{on: map[int]bool{7: true}}
}

func (m *modeTracker) feed(p []byte) {
	for _, c := range p {
		switch m.st {
		case 0:
			if c == 0x1b {
				m.st = 1
			}
		case 1:
			switch {
			case c == '[':
				m.st, m.buf = 2, m.buf[:0]
			case c == 'c': // RIS
				m.reset()
				m.st = 0
			case c == 0x1b:
			default:
				m.st = 0
			}
		case 2:
			if c >= 0x40 && c <= 0x7e {
				m.csi(m.buf, c)
				m.st = 0
			} else if c == 0x1b {
				m.st = 1
			} else if len(m.buf) < 64 {
				m.buf = append(m.buf, c)
			} else {
				m.st = 0
			}
		}
	}
}

func (m *modeTracker) reset() {
	m.on, m.kitty = map[int]bool{7: true}, nil
}

func params(b []byte) []int {
	var out []int
	for _, f := range bytes.FieldsFunc(b, func(r rune) bool { return r == ';' || r == ':' }) {
		n, _ := strconv.Atoi(string(f))
		out = append(out, n)
	}
	return out
}

func (m *modeTracker) csi(b []byte, final byte) {
	if len(b) == 0 {
		return
	}
	switch {
	case b[0] == '?' && (final == 'h' || final == 'l'):
		for _, n := range params(b[1:]) {
			m.on[n] = final == 'h'
		}
	case b[0] == '>' && final == 'u': // kitty: push flags
		fl := params(b[1:])
		v := 0
		if len(fl) > 0 {
			v = fl[0]
		}
		if len(m.kitty) < 16 {
			m.kitty = append(m.kitty, v)
		}
	case b[0] == '<' && final == 'u': // kitty: pop n
		n := 1
		if p := params(b[1:]); len(p) > 0 && p[0] > 0 {
			n = p[0]
		}
		m.kitty = m.kitty[:max(len(m.kitty)-n, 0)]
	case b[0] == '!' && final == 'p': // DECSTR
		m.reset()
	}
}

// prologue re-asserts every tracked mode explicitly (set or reset) so the client ends up in the
// app's state whatever it had before.
func (m *modeTracker) prologue() []byte {
	var b bytes.Buffer
	for _, n := range trackedModes {
		c := 'l'
		if m.on[n] {
			c = 'h'
		}
		fmt.Fprintf(&b, "\x1b[?%d%c", n, c)
	}
	b.WriteString("\x1b[<99u") // empty the client's kitty stack, then push the app's
	for _, f := range m.kitty {
		fmt.Fprintf(&b, "\x1b[>%du", f)
	}
	return b.Bytes()
}

// vtScreen is the Screen backed by vt.SafeEmulator. Output is queued to its own goroutine so a
// runaway producer never stalls the PTY reader or the clients (the emulator manages about 1 MB/s).
type vtScreen struct {
	emu   *vt.SafeEmulator
	reply func([]byte)

	qmu     sync.Mutex
	qcond   *sync.Cond
	queue   []qitem
	queued  int // bytes in queue
	closed  bool
	dropped int64

	mmu   sync.Mutex
	modes *modeTracker

	pend sync.WaitGroup // items enqueued and not yet applied
	done chan struct{}
}

type qitem struct {
	data       []byte
	cols, rows int // resize when data == nil
}

// maxQueued bounds the emulator backlog. Beyond it the oldest output is dropped from the *screen
// model* only (clients still get every byte); the next full repaint by the app heals it.
var maxQueued = 8 << 20

// NewVTScreen builds a Screen. reply receives the emulator's answers to the app's terminal queries
// (device attributes, cursor position); the caller must forward them to the PTY or the app waits.
func NewVTScreen(cols, rows int, reply func([]byte)) Screen {
	s := &vtScreen{emu: vt.NewSafeEmulator(cols, rows), reply: reply, modes: newModeTracker(), done: make(chan struct{})}
	s.qcond = sync.NewCond(&s.qmu)
	go s.drain()
	go s.run()
	return s
}

// drain copies the emulator's input side (its answers) out; io.Pipe is synchronous, so without a
// reader the first query response would block the emulator forever.
func (s *vtScreen) drain() {
	buf := make([]byte, 4096)
	for {
		n, err := s.emu.Read(buf)
		if n > 0 && s.reply != nil {
			s.reply(append([]byte(nil), buf[:n]...))
		}
		if err != nil {
			return
		}
	}
}

func (s *vtScreen) run() {
	defer close(s.done)
	for {
		s.qmu.Lock()
		for len(s.queue) == 0 && !s.closed {
			s.qcond.Wait()
		}
		if len(s.queue) == 0 && s.closed {
			s.qmu.Unlock()
			return
		}
		it := s.queue[0]
		s.queue = s.queue[1:]
		s.queued -= len(it.data)
		s.qmu.Unlock()
		if it.data != nil {
			s.mmu.Lock()
			s.modes.feed(it.data)
			s.mmu.Unlock()
			s.emu.Write(it.data)
		} else {
			s.emu.Resize(it.cols, it.rows)
		}
		s.pend.Done()
	}
}

func (s *vtScreen) push(it qitem) {
	s.qmu.Lock()
	defer s.qmu.Unlock()
	if s.closed {
		return
	}
	for it.data != nil && s.queued+len(it.data) > maxQueued {
		i := 0
		for i < len(s.queue) && s.queue[i].data == nil { // keep resizes: they are ordered state
			i++
		}
		if i == len(s.queue) {
			break
		}
		s.queued -= len(s.queue[i].data)
		s.queue = append(s.queue[:i], s.queue[i+1:]...)
		s.dropped++
		s.pend.Done()
	}
	s.queue = append(s.queue, it)
	s.queued += len(it.data)
	s.pend.Add(1)
	s.qcond.Signal()
}

func (s *vtScreen) Write(p []byte) {
	if len(p) > 0 {
		s.push(qitem{data: append([]byte(nil), p...)})
	}
}

func (s *vtScreen) Resize(cols, rows int) { s.push(qitem{cols: cols, rows: rows}) }

// Snapshot waits for everything written so far to be applied, then renders it with the recipe from
// the ADR: alt screen first, clear, the render with CRLF line ends, the cursor, the modes.
func (s *vtScreen) Snapshot() []byte {
	s.pend.Wait()
	var b bytes.Buffer
	b.WriteString("\x1b[0m")
	if s.emu.IsAltScreen() {
		b.WriteString("\x1b[?1049h")
	}
	b.WriteString("\x1b[H\x1b[2J")
	b.WriteString(string(bytes.ReplaceAll([]byte(s.emu.Render()), []byte("\n"), []byte("\r\n"))))
	b.WriteString("\x1b[0m")
	s.mmu.Lock()
	b.Write(s.modes.prologue())
	s.mmu.Unlock()
	pos := s.emu.CursorPosition()
	fmt.Fprintf(&b, "\x1b[%d;%dH", pos.Y+1, pos.X+1)
	if s.emu.CursorHidden() {
		b.WriteString("\x1b[?25l")
	} else {
		b.WriteString("\x1b[?25h")
	}
	return b.Bytes()
}

func (s *vtScreen) Close() {
	s.qmu.Lock()
	s.closed = true
	n := len(s.queue)
	s.queue = nil
	s.qcond.Broadcast()
	s.qmu.Unlock()
	for i := 0; i < n; i++ {
		s.pend.Done()
	}
	<-s.done
	closeEmu(s.emu)
}

func (s *vtScreen) qlenDropped() int64 {
	s.qmu.Lock()
	defer s.qmu.Unlock()
	return s.dropped
}

// closeEmu ends the emulator's input pipe so a goroutine blocked in Read returns EOF. It does not
// call Emulator.Close, which sets an unsynchronised flag that Read also reads (a race in v0.1.0).
func closeEmu(e *vt.SafeEmulator) {
	if pw, ok := e.InputPipe().(*io.PipeWriter); ok {
		pw.CloseWithError(io.EOF)
	}
}
