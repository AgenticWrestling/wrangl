package sessionhost

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
)

// Spec says what to run.
type Spec struct{ Slug, Dir, Name string }

// session is one process on one PTY, with its ring, its screen and its attached clients.
type session struct {
	info Info
	f    *os.File
	cmd  *exec.Cmd
	ring *Ring

	mu      sync.Mutex // orders PTY output into ring, screen and clients, and attach against it
	screen  Screen
	clients map[*client]struct{}
	cols    int
	rows    int
	done    chan struct{} // closed when the process has been reaped and the end frame sent
	reply   chan []byte
}

// outFrames is how many output frames (PTY reads, at most 32 KiB each, typically 4 KiB) a client may
// have queued before it counts as too slow and is dropped.
var outFrames = 4096

// client is one attached connection's outgoing side.
type client struct {
	out     chan frame
	control bool
	closed  bool
}

type frame struct {
	typ byte
	p   []byte
}

func startSession(sp Spec, argv []string, env []string, resumed bool, cols, rows, ringBytes int, newScreen func(cols, rows int, reply func([]byte)) Screen) (*session, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = sp.Dir
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	if err != nil {
		return nil, fmt.Errorf("start %s: %w", argv[0], err)
	}
	s := &session{
		f: f, cmd: cmd, ring: NewRing(ringBytes), clients: map[*client]struct{}{},
		cols: cols, rows: rows, done: make(chan struct{}), reply: make(chan []byte, 64),
		info: Info{Slug: sp.Slug, Name: sp.Name, Dir: sp.Dir, PID: cmd.Process.Pid, State: "running", Started: time.Now(), Cols: cols, Rows: rows, Resumed: resumed},
	}
	s.screen = newScreen(cols, rows, func(b []byte) {
		select { // never block the emulator: a child that stops reading input must not freeze it
		case s.reply <- b:
		default:
		}
	})
	go func() { // the emulator's answers to the app's queries go back to the PTY
		for {
			select {
			case b := <-s.reply:
				f.Write(b)
			case <-s.done:
				return
			}
		}
	}()
	go s.pump()
	return s, nil
}

// pump moves PTY output to the ring, the screen and every client until the process ends.
func (s *session) pump() {
	buf := make([]byte, 32<<10)
	for {
		n, err := s.f.Read(buf)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			s.mu.Lock()
			s.ring.Write(chunk)
			s.screen.Write(chunk)
			s.broadcastLocked(frame{FrameOutput, chunk})
			s.mu.Unlock()
		}
		if err != nil {
			break
		}
	}
	werr := s.cmd.Wait()
	code := 0
	if werr != nil {
		var ee *exec.ExitError
		if errors.As(werr, &ee) {
			code = ee.ExitCode()
		} else {
			code = -1
		}
	}
	s.mu.Lock()
	s.info.State, s.info.ExitCode = "exited", code
	end, _ := json.Marshal(map[string]int{"exit_code": code})
	s.broadcastLocked(frame{FrameEnd, end})
	for c := range s.clients {
		s.dropLocked(c)
	}
	s.mu.Unlock()
	s.f.Close()
	close(s.done)
	s.screen.Close()
}

// broadcastLocked queues a frame for every client. A client that cannot keep up is dropped (it can
// reattach and get a fresh snapshot) rather than stalling the session or the other clients.
func (s *session) broadcastLocked(fr frame) {
	for c := range s.clients {
		select {
		case c.out <- fr:
		default:
			s.dropLocked(c)
		}
	}
}

func (s *session) dropLocked(c *client) {
	if c.closed {
		return
	}
	c.closed = true
	delete(s.clients, c)
	close(c.out)
}

// attach registers a client. Under the output lock it takes the replay and adds the client, so the
// live frames that follow continue exactly where the replay ends.
func (s *session) attach(control bool, cols, rows int, replay string) (*client, error) {
	c := &client{out: make(chan frame, outFrames), control: control}
	s.mu.Lock()
	if control && cols > 0 && rows > 0 {
		if err := s.resizeLocked(cols, rows); err != nil {
			s.mu.Unlock()
			return nil, err
		}
	}
	var data []byte
	if replay == "ring" {
		data = s.ring.Bytes()
	} else {
		data = s.screen.Snapshot()
	}
	for len(data) > 0 {
		n := min(len(data), 32<<10)
		c.out <- frame{FrameOutput, data[:n]}
		data = data[n:]
	}
	sz, _ := json.Marshal(map[string]int{"cols": s.cols, "rows": s.rows})
	c.out <- frame{FrameSize, sz}
	running := s.info.State == "running"
	if running {
		s.clients[c] = struct{}{}
	} else {
		end, _ := json.Marshal(map[string]int{"exit_code": s.info.ExitCode})
		c.out <- frame{FrameEnd, end}
		c.closed = true
		close(c.out)
	}
	s.mu.Unlock()
	if running {
		s.winch() // the app repaints over the snapshot, whatever the size did
	}
	return c, nil
}

func (s *session) detach(c *client) {
	s.mu.Lock()
	s.dropLocked(c)
	s.mu.Unlock()
}

func (s *session) winch() { syscall.Kill(-s.info.PID, syscall.SIGWINCH) }

func (s *session) resizeLocked(cols, rows int) error {
	if cols < 1 || rows < 1 || cols > 1000 || rows > 1000 {
		return fmt.Errorf("size %dx%d out of range", cols, rows)
	}
	if cols == s.cols && rows == s.rows {
		return nil
	}
	if err := pty.Setsize(s.f, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)}); err != nil {
		return err
	}
	s.screen.Resize(cols, rows)
	s.cols, s.rows = cols, rows
	sz, _ := json.Marshal(map[string]int{"cols": cols, "rows": rows})
	s.broadcastLocked(frame{FrameSize, sz})
	return nil
}

// resize applies a controlling client's size; the latest one wins.
func (s *session) resize(cols, rows int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resizeLocked(cols, rows)
}

func (s *session) snapshotInfo() Info {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.info
	i.Cols, i.Rows, i.Clients = s.cols, s.rows, len(s.clients)
	return i
}

// stop ends the process: SIGHUP to its group, SIGKILL after grace.
func (s *session) stop(grace time.Duration) {
	select {
	case <-s.done:
		return
	default:
	}
	syscall.Kill(-s.info.PID, syscall.SIGHUP)
	select {
	case <-s.done:
	case <-time.After(grace):
		syscall.Kill(-s.info.PID, syscall.SIGKILL)
		<-s.done
	}
}
