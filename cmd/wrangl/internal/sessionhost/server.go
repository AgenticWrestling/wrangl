package sessionhost

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// DefaultGroup owns the socket so the controller and the owner's CLI can both connect.
const DefaultGroup = "wrangl-sessions"

// Server serves a Host on a unix socket.
type Server struct {
	Host   *Host
	Path   string
	Group  string // socket group (default DefaultGroup); a missing group is logged, not fatal
	Log    *slog.Logger
	Listen func() (net.Listener, error) // tests may override
}

// Serve listens until ctx ends. The socket is mode 0660, group Group.
func (s *Server) Serve(ctx context.Context) error {
	if s.Log == nil {
		s.Log = slog.Default()
	}
	if s.Group == "" {
		s.Group = DefaultGroup
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o755); err != nil {
		return err
	}
	if c, err := net.DialTimeout("unix", s.Path, time.Second); err == nil {
		c.Close()
		return fmt.Errorf("a session host is already serving %s", s.Path)
	}
	os.Remove(s.Path)
	l, err := net.Listen("unix", s.Path)
	if err != nil {
		return err
	}
	defer os.Remove(s.Path)
	if g, err := user.LookupGroup(s.Group); err != nil {
		s.Log.Warn("socket group missing; the socket stays with the service's own group", "group", s.Group)
	} else if gid, _ := strconv.Atoi(g.Gid); true {
		if err := os.Chown(s.Path, -1, gid); err != nil {
			s.Log.Warn("chown socket", "err", err)
		}
	}
	if err := os.Chmod(s.Path, 0o660); err != nil {
		l.Close()
		return err
	}
	go func() { <-ctx.Done(); l.Close() }()
	var wg sync.WaitGroup
	for {
		c, err := l.Accept()
		if err != nil {
			wg.Wait()
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		wg.Add(1)
		go func() { defer wg.Done(); s.conn(ctx, c) }()
	}
}

func (s *Server) conn(ctx context.Context, c net.Conn) {
	defer c.Close()
	br := bufio.NewReaderSize(c, 64<<10)
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	line, err := br.ReadBytes('\n')
	if err != nil || len(line) > 64<<10 {
		return
	}
	c.SetReadDeadline(time.Time{})
	var req Request
	reply := func(r Response) { b, _ := json.Marshal(r); c.Write(append(b, '\n')) }
	if err := json.Unmarshal(line, &req); err != nil {
		reply(Response{Error: "bad request: " + err.Error()})
		return
	}
	h := s.Host
	switch req.Op {
	case OpList:
		reply(Response{OK: true, Sessions: h.List()})
	case OpStart:
		i, existing, err := h.Start(req.Slug, req.Dir, req.Name)
		if err != nil {
			reply(Response{Error: err.Error()})
			return
		}
		reply(Response{OK: true, Session: &i, Existing: existing})
	case OpStop:
		if err := h.Stop(req.Slug); err != nil {
			reply(Response{Error: err.Error()})
			return
		}
		reply(Response{OK: true})
	case OpRestart:
		i, err := h.Restart(req.Slug)
		if err != nil {
			reply(Response{Error: err.Error()})
			return
		}
		reply(Response{OK: true, Session: &i})
	case OpAttach:
		sess := h.get(req.Slug)
		if sess == nil {
			reply(Response{Error: errNoSession.Error()})
			return
		}
		cl, err := sess.attach(req.Control, req.Cols, req.Rows, req.Replay)
		if err != nil {
			reply(Response{Error: err.Error()})
			return
		}
		defer sess.detach(cl)
		i := sess.snapshotInfo()
		reply(Response{OK: true, Session: &i})
		done := make(chan struct{})
		go func() { // input and resize from the client; EOF means it detached
			defer close(done)
			for {
				typ, p, err := ReadFrame(br)
				if err != nil {
					return
				}
				if !req.Control {
					continue
				}
				switch typ {
				case FrameInput:
					sess.f.Write(p)
				case FrameResize:
					var sz struct{ Cols, Rows int }
					if json.Unmarshal(p, &sz) == nil {
						sess.resize(sz.Cols, sz.Rows)
					}
				}
			}
		}()
		for {
			select {
			case fr, ok := <-cl.out:
				if !ok {
					return
				}
				if WriteFrame(c, fr.typ, fr.p) != nil {
					return
				}
			case <-done:
				return
			case <-ctx.Done():
				return
			}
		}
	default:
		reply(Response{Error: "unknown op " + strconv.Quote(req.Op)})
	}
}
