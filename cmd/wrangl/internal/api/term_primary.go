package api

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"time"

	"github.com/coder/websocket"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/sessionhost"
)

// PrimaryPrefix marks a primary session's terminal id on the host runner: "primary-<slug>".
const PrimaryPrefix = "primary-"

var primarySlugRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

func primarySlugOK(s string) bool { return primarySlugRE.MatchString(s) }

// servePrimaryTerm is serveTerm for a primary session: the same websocket protocol (hello, live,
// binary output, size, end, control), but the PTY is the session host's, reached over its socket.
// The proxy attaches as a controlling client and gates input and resize itself: the browser is
// watch-only until it sends {t:"control",on:true}.
func servePrimaryTerm(w http.ResponseWriter, r *http.Request, d Deps, hub *TermHub, rid int64, id, slug string) {
	att, err := d.Sessions.Client().Attach(sessionhost.Request{Slug: slug, Control: true})
	if err != nil {
		http.Error(w, "primary session: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer att.Detach()
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer c.CloseNow()
	c.SetReadLimit(termReadLimit)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go func() { <-ctx.Done(); att.Detach() }()

	sendJSON := func(v any) {
		b, _ := json.Marshal(v)
		wctx, wc := context.WithTimeout(ctx, termWriteWait)
		defer wc()
		if err := c.Write(wctx, websocket.MessageText, b); err != nil {
			cancel()
		}
	}
	live := true
	sendJSON(termMsg{T: "hello", Cols: uint16(att.Info.Cols), Rows: uint16(att.Info.Rows), Live: &live})
	tc := &termConn{runner: rid, id: id, send: func(v any) { sendJSON(v) }}
	hub.add(tc)
	defer hub.remove(tc)

	go func() { // session host -> browser
		defer cancel()
		sawSize := false
		for {
			typ, p, err := att.Next()
			if err != nil {
				sendJSON(termMsg{T: "end"})
				return
			}
			switch typ {
			case sessionhost.FrameOutput:
				wctx, wc := context.WithTimeout(ctx, termWriteWait)
				err := c.Write(wctx, websocket.MessageBinary, p)
				wc()
				if err != nil {
					return
				}
			case sessionhost.FrameSize:
				var sz struct{ Cols, Rows uint16 }
				if json.Unmarshal(p, &sz) == nil {
					if !sawSize { // the snapshot has been sent: the replay has caught up
						sawSize = true
						sendJSON(termMsg{T: "live"})
					}
					sendJSON(termMsg{T: "size", Cols: sz.Cols, Rows: sz.Rows})
				}
			case sessionhost.FrameEnd:
				sendJSON(termMsg{T: "end"})
				return
			}
		}
	}()
	go func() {
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
				att.Input(data)
			}
			continue
		}
		var m termMsg
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		switch m.T {
		case "control":
			control = m.On
			sendJSON(termMsg{T: "control", On: control})
		case "size":
			if control && m.Cols > 0 && m.Rows > 0 {
				att.Resize(int(m.Cols), int(m.Rows))
			}
		}
	}
}
