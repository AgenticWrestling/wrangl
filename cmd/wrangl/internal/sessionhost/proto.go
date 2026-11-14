package sessionhost

import (
	"encoding/binary"
	"fmt"
	"io"
	"time"
)

// Socket protocol. A connection starts with one JSON Request line and gets one JSON Response line.
// For op "attach" the connection then switches to binary frames in both directions until either
// side closes it; closing detaches and never touches the session.

// Ops.
const (
	OpList    = "list"
	OpStart   = "start"
	OpStop    = "stop"
	OpRestart = "restart"
	OpAttach  = "attach"
)

// Request is the first line a client sends.
type Request struct {
	Op   string `json:"op"`
	Slug string `json:"slug,omitempty"`
	Dir  string `json:"dir,omitempty"`  // start: the worktree to run in
	Name string `json:"name,omitempty"` // start: the Remote Control name (default: the slug)
	// start: when CloneURL is set the host prepares dir first (clone on the branch, or fast-forward
	// a clean checkout) as the host's own user. Token is used for that one git call, from the
	// environment of the git process, and is never stored.
	CloneURL string `json:"clone_url,omitempty"`
	Branch   string `json:"branch,omitempty"`
	Token    string `json:"token,omitempty"`
	// start: more checkouts to prepare beside Dir (a project with several repos). A failure to prepare
	// one is not fatal: the session starts with what is there.
	Extra []Checkout `json:"extra,omitempty"`
	// start: continue the previous conversation (`claude -c`) even though this host has no record of
	// the session, e.g. after the host itself restarted.
	Resume bool `json:"resume,omitempty"`
	// attach:
	Control bool   `json:"control,omitempty"` // may type and resize (watch-only otherwise)
	Cols    int    `json:"cols,omitempty"`    // size to apply when controlling
	Rows    int    `json:"rows,omitempty"`
	Replay  string `json:"replay,omitempty"` // "snapshot" (default) or "ring" (raw scrollback)
}

// Info describes one session.
type Info struct {
	Slug     string    `json:"slug"`
	Name     string    `json:"name"`
	Dir      string    `json:"dir"`
	PID      int       `json:"pid,omitempty"`
	State    string    `json:"state"` // running | exited
	ExitCode int       `json:"exit_code,omitempty"`
	Started  time.Time `json:"started"`
	Cols     int       `json:"cols"`
	Rows     int       `json:"rows"`
	Clients  int       `json:"clients"`
	Resumed  bool      `json:"resumed,omitempty"` // started with -c
}

// Response is the reply line.
type Response struct {
	OK       bool   `json:"ok"`
	Error    string `json:"error,omitempty"`
	Sessions []Info `json:"sessions,omitempty"`
	Session  *Info  `json:"session,omitempty"`
	Existing bool   `json:"existing,omitempty"` // start: it was already running
}

// Frame types.
const (
	FrameOutput = 'o' // server to client: terminal bytes (snapshot first, then live output)
	FrameInput  = 'i' // client to server: keystrokes (ignored unless controlling)
	FrameResize = 'r' // client to server: JSON {"cols":N,"rows":N} (ignored unless controlling)
	FrameSize   = 'z' // server to client: JSON {"cols":N,"rows":N} after any resize
	FrameEnd    = 'e' // server to client: JSON {"exit_code":N}; the session's process ended
)

const maxFrame = 1 << 20

// WriteFrame writes one frame.
func WriteFrame(w io.Writer, typ byte, p []byte) error {
	var h [5]byte
	h[0] = typ
	binary.BigEndian.PutUint32(h[1:], uint32(len(p)))
	_, err := w.Write(append(h[:], p...))
	return err
}

// ReadFrame reads one frame.
func ReadFrame(r io.Reader) (byte, []byte, error) {
	var h [5]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(h[1:])
	if n > maxFrame {
		return 0, nil, fmt.Errorf("frame of %d bytes", n)
	}
	p := make([]byte, n)
	if _, err := io.ReadFull(r, p); err != nil {
		return 0, nil, err
	}
	return h[0], p, nil
}

// Checkout is one more repo for the host to clone or fast-forward before a session starts.
type Checkout struct {
	Dir      string `json:"dir"`
	CloneURL string `json:"clone_url"`
	Branch   string `json:"branch,omitempty"`
	Token    string `json:"token,omitempty"`
}
