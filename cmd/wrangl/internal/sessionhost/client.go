package sessionhost

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"time"
)

// Client talks to a session host's socket.
type Client struct{ Path string }

func (c Client) dial(req Request) (net.Conn, *bufio.Reader, Response, error) {
	conn, err := net.DialTimeout("unix", c.Path, 3*time.Second)
	if err != nil {
		return nil, nil, Response{}, err
	}
	b, _ := json.Marshal(req)
	if _, err := conn.Write(append(b, '\n')); err != nil {
		conn.Close()
		return nil, nil, Response{}, err
	}
	br := bufio.NewReader(conn)
	line, err := br.ReadBytes('\n')
	if err != nil {
		conn.Close()
		return nil, nil, Response{}, err
	}
	var r Response
	if err := json.Unmarshal(line, &r); err != nil {
		conn.Close()
		return nil, nil, Response{}, err
	}
	if !r.OK {
		conn.Close()
		return nil, nil, r, fmt.Errorf("%s", r.Error)
	}
	return conn, br, r, nil
}

// Do runs a one-shot op (list, start, stop, restart).
func (c Client) Do(req Request) (Response, error) {
	conn, _, r, err := c.dial(req)
	if err == nil {
		conn.Close()
	}
	return r, err
}

// Attached is a live attach connection.
type Attached struct {
	conn net.Conn
	br   *bufio.Reader
	Info Info
}

// Attach opens an attach connection. Read frames with Next; send with Input and Resize.
func (c Client) Attach(req Request) (*Attached, error) {
	req.Op = OpAttach
	conn, br, r, err := c.dial(req)
	if err != nil {
		return nil, err
	}
	return &Attached{conn: conn, br: br, Info: *r.Session}, nil
}

// Next returns the next server frame.
func (a *Attached) Next() (byte, []byte, error) { return ReadFrame(a.br) }

// Input sends keystrokes.
func (a *Attached) Input(p []byte) error { return WriteFrame(a.conn, FrameInput, p) }

// Resize asks for a new size (controlling clients only).
func (a *Attached) Resize(cols, rows int) error {
	b, _ := json.Marshal(map[string]int{"cols": cols, "rows": rows})
	return WriteFrame(a.conn, FrameResize, b)
}

// Detach closes the connection; the session keeps running.
func (a *Attached) Detach() error { return a.conn.Close() }
