package web

// The expanded card's "Live output": the agent's transcript rendered as Markdown, with the old
// PTY view kept as the fallback when a run has no transcript.

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/api"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/natsd"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/transcript"
)

// Live-output modes of the card.
const (
	liveTranscript = "transcript" // running, with transcript entries: rendered Markdown, followed live
	livePTY        = "pty"        // running, no transcript: today's read-only terminal, with a note
	liveStored     = "stored"     // finished, with entries: rendered once, no live connection
	liveNone       = ""           // finished, nothing recorded: no panel (the deliverable shows on the card)
)

// liveMode picks the panel from whether the run is running and how many entries it published.
func liveMode(running bool, entries int) string {
	switch {
	case running && entries > 0:
		return liveTranscript
	case running:
		return livePTY
	case entries > 0:
		return liveStored
	}
	return liveNone
}

const cardEntries = 60 // entries shown when a card opens; "load earlier" pages back

// livePanel fills d with what the card's live-output panel needs for run id (running or the
// task's latest finished run).
func (h *Handler) livePanel(ctx context.Context, d map[string]any, runID, runnerID int64, running bool) {
	var page api.TranscriptPage
	if h.D.JS != nil && runnerID > 0 {
		page, _ = api.TranscriptTail(ctx, h.D.JS, runnerID, runID, 0, cardEntries)
	}
	mode := liveMode(running, len(page.Entries))
	d["LiveMode"], d["LiveRun"] = mode, runID
	if mode == liveNone {
		delete(d, "LiveRun")
		return
	}
	if mode == livePTY {
		return
	}
	d["TxHTML"] = renderEntries(page.Entries)
	d["TxHasMore"] = page.HasMore
	d["TxFirst"], d["TxLast"] = page.Entries[0].Seq, page.Entries[len(page.Entries)-1].Seq
}

// renderEntries renders entries as HTML; a tool call and its result share one chip.
func renderEntries(es []transcript.Entry) template.HTML {
	results := map[int]transcript.Entry{} // tool_use seq -> its result
	pending := map[string][]int{}         // tool -> unanswered tool_use seqs (FIFO)
	paired := map[int]bool{}              // tool_result seqs folded into a chip
	for _, e := range es {
		switch e.Kind {
		case transcript.KindToolUse:
			pending[e.Tool] = append(pending[e.Tool], e.Seq)
		case transcript.KindToolResult:
			if q := pending[e.Tool]; len(q) > 0 {
				results[q[0]], paired[e.Seq] = e, true
				pending[e.Tool] = q[1:]
			}
		}
	}
	var b strings.Builder
	for _, e := range es {
		if paired[e.Seq] {
			continue
		}
		r, ok := results[e.Seq]
		b.WriteString(string(renderEntry(e, &r, ok)))
	}
	return template.HTML(b.String())
}

func renderEntry(e transcript.Entry, res *transcript.Entry, haveRes bool) template.HTML {
	esc := template.HTMLEscapeString
	switch e.Kind {
	case transcript.KindText:
		cls := "txmsg " + e.Role
		return template.HTML(fmt.Sprintf(`<div class="%s prose" id="tx-%d">%s</div>`, esc(cls), e.Seq, markdown(e.Text)))
	case transcript.KindThinking:
		return template.HTML(fmt.Sprintf(`<details class="txthink" id="tx-%d"><summary>thinking</summary><div class="prose">%s</div></details>`, e.Seq, markdown(e.Text)))
	case transcript.KindToolUse:
		st, out := "…", "(running)"
		if haveRes {
			st, out = chipStatus(*res), res.Text
		}
		return template.HTML(fmt.Sprintf(`<details class="txchip" id="tx-%d"><summary>%s <span class="txst" id="txs-%d">%s</span></summary><pre class="txout" id="txo-%d">%s</pre></details>`,
			e.Seq, esc(e.Summary), e.Seq, st, e.Seq, esc(out)))
	case transcript.KindToolResult: // a result whose call is outside the window
		return template.HTML(fmt.Sprintf(`<details class="txchip" id="tx-%d"><summary>%s <span class="txst">%s</span></summary><pre class="txout">%s</pre></details>`,
			e.Seq, esc(e.Summary), chipStatus(e), esc(e.Text)))
	}
	return ""
}

func chipStatus(res transcript.Entry) string {
	if strings.HasSuffix(res.Summary, " failed") {
		return "✗"
	}
	return "✓"
}

// transcriptMore serves GET /ui/transcript?run=ID&before=SEQ: older entries for "load earlier".
func (h *Handler) transcriptMore(w http.ResponseWriter, r *http.Request) {
	run, runner, ok := h.runRunner(w, r)
	if !ok {
		return
	}
	before, _ := strconv.Atoi(r.URL.Query().Get("before"))
	page, err := api.TranscriptTail(r.Context(), h.D.JS, runner, run, before, cardEntries)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	more := ""
	if page.HasMore {
		more = fmt.Sprintf(`<a class="txearlier" href="#" data-before="%d">load earlier</a>`, page.Entries[0].Seq)
	}
	fmt.Fprint(w, more, string(renderEntries(page.Entries)))
}

func (h *Handler) runRunner(w http.ResponseWriter, r *http.Request) (run, runner int64, ok bool) {
	run, err := strconv.ParseInt(r.URL.Query().Get("run"), 10, 64)
	if err != nil || h.D.JS == nil {
		http.Error(w, "bad run", http.StatusBadRequest)
		return 0, 0, false
	}
	row, err := h.D.Fleet.DB.RQ.GetRun(r.Context(), run)
	if err != nil || !row.RunnerID.Valid {
		http.Error(w, "no such run", http.StatusNotFound)
		return 0, 0, false
	}
	return run, row.RunnerID.Int64, true
}

// transcriptLive serves GET /ui/transcript-live?run=ID&after=SEQ as a Datastar stream: each new
// entry is appended to #txl-ID; a tool result patches its chip's status and output in place, so
// an expanded chip stays open.
func (h *Handler) transcriptLive(w http.ResponseWriter, r *http.Request) {
	run, runner, ok := h.runRunner(w, r)
	if !ok {
		return
	}
	after, _ := strconv.Atoi(r.URL.Query().Get("after"))
	ctx := r.Context()
	cons, err := h.D.JS.OrderedConsumer(ctx, "WR_EVT", jetstream.OrderedConsumerConfig{
		FilterSubjects: []string{natsd.RunTranscriptSubject(runner, run)}, DeliverPolicy: jetstream.DeliverAllPolicy,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	fl, _ := w.(http.Flusher)
	pending := map[string][]int{}
	idle := time.Now()
	list := fmt.Sprintf("#txl-%d", run)
	for {
		msg, err := cons.Next(jetstream.FetchMaxWait(time.Second))
		if err == nil {
			var e transcript.Entry
			if json.Unmarshal(msg.Data(), &e) != nil {
				continue
			}
			// Track pairing for every entry, but only send those past `after`.
			switch e.Kind {
			case transcript.KindToolUse:
				pending[e.Tool] = append(pending[e.Tool], e.Seq)
			case transcript.KindToolResult:
				if q := pending[e.Tool]; len(q) > 0 {
					use := q[0]
					pending[e.Tool] = q[1:]
					if e.Seq > after {
						mergeFragments(w, fmt.Sprintf(`<span class="txst" id="txs-%d">%s</span>`, use, chipStatus(e)))
						mergeFragments(w, fmt.Sprintf(`<pre class="txout" id="txo-%d">%s</pre>`, use, template.HTMLEscapeString(e.Text)))
						flush(w, fl)
					}
					continue
				}
			}
			if e.Seq > after {
				idle = time.Now()
				fmt.Fprintf(w, "event: datastar-patch-elements\ndata: selector %s\ndata: mode append\n", list)
				for _, line := range strings.Split(string(renderEntry(e, nil, false)), "\n") {
					fmt.Fprintf(w, "data: elements %s\n", line)
				}
				fmt.Fprint(w, "\n")
				flush(w, fl)
			}
			continue
		}
		if ctx.Err() != nil {
			return
		}
		if row, err := h.D.Fleet.DB.RQ.GetRun(ctx, run); err != nil || proto.Terminal(row.State) {
			return
		}
		if time.Since(idle) > api.KeepAlive {
			fmt.Fprint(w, ": ping\n\n")
			flush(w, fl)
			idle = time.Now()
		}
	}
}

func flush(w http.ResponseWriter, fl http.Flusher) {
	if fl != nil {
		fl.Flush()
	}
}
