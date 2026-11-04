package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/natsd"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/errs"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

// The web terminal: the owner watches (and, after taking control, types into) the PTYs on
// a runner. Output replays from the PTY stream; input and resize reach the runner as plain
// NATS publishes, and only while this connection holds control.

const (
	termReadLimit = 64 << 10
	termWriteWait = 10 * time.Second
	termPing      = 25 * time.Second // below Cloudflare's 100s idle cutoff
)

// TermHub tracks open terminal connections: it fans resizes out to every viewer of a
// session and tells the reaper which runners someone is attached to.
type TermHub struct {
	mu    sync.Mutex
	conns map[*termConn]struct{}
}

type termConn struct {
	runner int64
	id     string
	send   func(v any)
}

func (h *TermHub) add(c *termConn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.conns == nil {
		h.conns = map[*termConn]struct{}{}
	}
	h.conns[c] = struct{}{}
}

func (h *TermHub) remove(c *termConn) { h.mu.Lock(); delete(h.conns, c); h.mu.Unlock() }

// Attached reports whether anyone has a terminal open on runner r.
func (h *TermHub) Attached(r int64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.conns {
		if c.runner == r {
			return true
		}
	}
	return false
}

func (h *TermHub) broadcast(except *termConn, r int64, id string, v any) {
	h.mu.Lock()
	var to []*termConn
	for c := range h.conns {
		if c != except && c.runner == r && c.id == id {
			to = append(to, c)
		}
	}
	h.mu.Unlock()
	for _, c := range to {
		c.send(v)
	}
}

// runnerReq sends a request to runner r's cmd subject and decodes the JSON reply.
func runnerReq(ctx context.Context, d Deps, r int64, cmd string, body []byte, out any) error {
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	msg, err := d.Fleet.NC.RequestWithContext(rctx, natsd.RunnerCmdSubject(r, cmd), body)
	if err != nil {
		return fmt.Errorf("%w: runner did not answer: %v", errs.Conflict, err)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(msg.Data, out)
}

func registerTerm(mux *http.ServeMux, d Deps) {
	hub := d.Term
	if hub == nil {
		hub = &TermHub{}
	}
	runner := func(w http.ResponseWriter, r *http.Request) (dbgen.Runner, bool) {
		row, err := d.Fleet.DB.RQ.GetRunnerByName(r.Context(), r.PathValue("name"))
		if err != nil {
			writeErr(w, notFound(err, "runner "+r.PathValue("name")))
			return row, false
		}
		return row, true
	}

	mux.HandleFunc("GET /api/v1/runners/{name}/terms", func(w http.ResponseWriter, r *http.Request) {
		row, ok := runner(w, r)
		if !ok {
			return
		}
		infos := []proto.TermInfo{}
		if err := runnerReq(r.Context(), d, row.ID, "term.list", nil, &infos); err != nil {
			writeErr(w, err)
			return
		}
		type session struct {
			ID    string `json:"id"`
			Kind  string `json:"kind"`
			Title string `json:"title"`
			Live  bool   `json:"live"`
			Cols  uint16 `json:"cols"`
			Rows  uint16 `json:"rows"`
		}
		out := make([]session, 0, len(infos))
		for _, ti := range infos {
			s := session{ID: ti.ID, Kind: ti.Kind, Title: ti.Title, Live: true, Cols: ti.Cols, Rows: ti.Rows}
			if n, err := strconv.ParseInt(strings.TrimPrefix(ti.ID, "run-"), 10, 64); err == nil && ti.Kind == "run" {
				if run, err := d.Fleet.DB.RQ.GetRun(r.Context(), n); err == nil {
					if t, err := d.Fleet.DB.RQ.GetTask(r.Context(), run.TaskID); err == nil {
						s.Title = fmt.Sprintf("run #%d %s", n, t.Title)
					}
				}
			}
			out = append(out, s)
		}
		writeJSON(w, http.StatusOK, map[string]any{"sessions": out})
	})

	mux.HandleFunc("POST /api/v1/runners/{name}/shell", func(w http.ResponseWriter, r *http.Request) {
		row, ok := runner(w, r)
		if !ok {
			return
		}
		var rep proto.TermOpenReply
		if err := runnerReq(r.Context(), d, row.ID, "shell.open", nil, &rep); err != nil {
			writeErr(w, err)
			return
		}
		if rep.Error != "" {
			writeErr(w, fmt.Errorf("%w: %s", errs.Conflict, rep.Error))
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"id": rep.ID})
	})

	mux.HandleFunc("DELETE /api/v1/runners/{name}/term/{id}", func(w http.ResponseWriter, r *http.Request) {
		row, ok := runner(w, r)
		if !ok {
			return
		}
		id := r.PathValue("id")
		if !natsd.TermIDOK(id) {
			writeErr(w, fmt.Errorf("%w: bad terminal id", errs.Invalid))
			return
		}
		var rep proto.TermOpenReply
		if err := runnerReq(r.Context(), d, row.ID, "term.close."+id, nil, &rep); err != nil {
			writeErr(w, err)
			return
		}
		if rep.Error != "" {
			writeErr(w, fmt.Errorf("%w: %s", errs.Conflict, rep.Error))
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"closed": id})
	})

	mux.HandleFunc("GET /api/v1/runners/{name}/term/{id}", func(w http.ResponseWriter, r *http.Request) {
		row, ok := runner(w, r)
		if !ok {
			return
		}
		id := r.PathValue("id")
		if !natsd.TermIDOK(id) {
			writeErr(w, fmt.Errorf("%w: bad terminal id", errs.Invalid))
			return
		}
		serveTerm(w, r, d, hub, row.ID, id)
	})
}

type termMsg struct {
	T    string `json:"t"`
	Cols uint16 `json:"cols,omitempty"`
	Rows uint16 `json:"rows,omitempty"`
	On   bool   `json:"on,omitempty"`
	Live *bool  `json:"live,omitempty"`
	Msg  string `json:"msg,omitempty"`
}

func serveTerm(w http.ResponseWriter, r *http.Request, d Deps, hub *TermHub, rid int64, id string) {
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return // Accept already replied
	}
	defer c.CloseNow()
	c.SetReadLimit(termReadLimit)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	sendJSON := func(v any) {
		b, _ := json.Marshal(v)
		wctx, wc := context.WithTimeout(ctx, termWriteWait)
		defer wc()
		if err := c.Write(wctx, websocket.MessageText, b); err != nil {
			cancel()
		}
	}

	// Current size, if the session is live; completed runs replay at the default size.
	cols, rows, live := uint16(200), uint16(50), false
	var infos []proto.TermInfo
	if runnerReq(ctx, d, rid, "term.list", nil, &infos) == nil {
		for _, ti := range infos {
			if ti.ID == id {
				cols, rows, live = ti.Cols, ti.Rows, true
			}
		}
	}
	sendJSON(termMsg{T: "hello", Cols: cols, Rows: rows, Live: &live})

	tc := &termConn{runner: rid, id: id, send: func(v any) { sendJSON(v) }}
	hub.add(tc)
	defer hub.remove(tc)

	// Output: replay from the start of the session's stream, then follow it.
	cons, err := d.JS.OrderedConsumer(ctx, "WR_PTY", jetstream.OrderedConsumerConfig{
		FilterSubjects: []string{natsd.TermPTYSubject(rid, id)},
		DeliverPolicy:  jetstream.DeliverAllPolicy,
	})
	if err != nil {
		sendJSON(termMsg{T: "error", Msg: err.Error()})
		return
	}
	go func() {
		defer cancel()
		replaying := true
		for ctx.Err() == nil {
			msg, err := cons.Next(jetstream.FetchMaxWait(time.Second))
			if err != nil {
				if replaying { // caught up with an empty tail
					replaying = false
					sendJSON(termMsg{T: "live"})
				}
				// Caught up. A run's terminal ends with the run.
				if n, perr := strconv.ParseInt(strings.TrimPrefix(id, "run-"), 10, 64); perr == nil && strings.HasPrefix(id, "run-") {
					if cur, err := d.Fleet.DB.RQ.GetRun(ctx, n); err != nil || proto.Terminal(cur.State) {
						sendJSON(termMsg{T: "end"})
						return
					}
				}
				continue
			}
			if msg.Headers().Get(proto.TermEndHeader) != "" {
				sendJSON(termMsg{T: "end"})
				return
			}
			wctx, wc := context.WithTimeout(ctx, termWriteWait)
			err = c.Write(wctx, websocket.MessageBinary, msg.Data())
			wc()
			if err != nil {
				return
			}
			if replaying {
				if md, merr := msg.Metadata(); merr != nil || md.NumPending == 0 {
					replaying = false
					sendJSON(termMsg{T: "live"})
				}
			}
		}
	}()
	go func() { // keep idle proxies from dropping the connection
		t := time.NewTicker(termPing)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				pctx, pc := context.WithTimeout(ctx, termWriteWait)
				err := c.Ping(pctx)
				pc()
				if err != nil {
					cancel()
					return
				}
			}
		}
	}()

	control := false
	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		if typ == websocket.MessageBinary {
			if control {
				d.Fleet.NC.Publish(natsd.RunnerCmdSubject(rid, "term.in."+id), data)
			}
			continue
		}
		var m termMsg
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		switch m.T {
		case "control":
			if m.On && !live {
				sendJSON(termMsg{T: "error", Msg: "this terminal has ended; it is view-only"})
				continue
			}
			control = m.On
			sendJSON(termMsg{T: "control", On: control})
		case "size":
			if !control {
				continue
			}
			b, _ := json.Marshal(proto.TermSize{Cols: m.Cols, Rows: m.Rows})
			d.Fleet.NC.Publish(natsd.RunnerCmdSubject(rid, "term.size."+id), b)
			hub.broadcast(tc, rid, id, termMsg{T: "size", Cols: m.Cols, Rows: m.Rows})
		}
	}
}
