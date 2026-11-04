package api

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/dbtime"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/natsd"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/errs"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

// sinceParam accepts a duration ("15m") or an RFC 3339 time; returns RFC 3339 UTC or "".
func sinceParam(v string) (string, error) {
	if v == "" {
		return "", nil
	}
	if d, err := time.ParseDuration(v); err == nil {
		return dbtime.Format(time.Now().Add(-d)), nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return dbtime.Format(t), nil
	}
	return "", fmt.Errorf("%w: since %q: want a duration like 15m or an RFC 3339 time", errs.Invalid, v)
}

// streamLogs writes NDJSON log lines; with follow=1 it keeps streaming until the run ends.
func streamLogs(w http.ResponseWriter, r *http.Request, d Deps, runID int64) {
	q := r.URL.Query()
	var re *regexp.Regexp
	if g := q.Get("grep"); g != "" {
		var err error
		if re, err = regexp.Compile(g); err != nil {
			writeErr(w, fmt.Errorf("%w: grep: %v", errs.Invalid, err))
			return
		}
	}
	since, err := sinceParam(q.Get("since"))
	if err != nil {
		writeErr(w, err)
		return
	}
	if _, err := d.Fleet.DB.RQ.GetRun(r.Context(), runID); err != nil {
		writeErr(w, noRows(err, fmt.Sprintf("run %d", runID)))
		return
	}
	follow := q.Get("follow") == "1"
	w.Header().Set("Content-Type", "application/x-ndjson")
	fl, _ := w.(http.Flusher)
	enc := json.NewEncoder(w)
	var after int64
	for {
		rows, err := d.Fleet.DB.RQ.RunLogs(r.Context(), dbgen.RunLogsParams{RunID: runID, AfterID: after, Stream: q.Get("stream"), Since: since, Lim: 1000})
		if err != nil {
			return
		}
		for _, l := range rows {
			after = l.ID
			if re != nil && !re.MatchString(l.Message) {
				continue
			}
			enc.Encode(proto.LogView{ID: l.ID, RunID: l.RunID, TS: l.Ts, Stream: l.Stream, Message: l.Message})
		}
		if fl != nil {
			fl.Flush()
		}
		if len(rows) == 1000 {
			continue
		}
		if !follow {
			return
		}
		run, err := d.Fleet.DB.RQ.GetRun(r.Context(), runID)
		if err != nil || proto.Terminal(run.State) {
			// One last pass for lines ingested just before the final state.
			if more, _ := d.Fleet.DB.RQ.RunLogs(r.Context(), dbgen.RunLogsParams{RunID: runID, AfterID: after, Stream: q.Get("stream"), Since: since, Lim: 1000}); len(more) == 0 {
				return
			}
			continue
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// streamPTY replays the run's terminal from JetStream and follows it until the run ends.
func streamPTY(w http.ResponseWriter, r *http.Request, d Deps, runID int64) {
	ctx := r.Context()
	run, err := d.Fleet.DB.RQ.GetRun(ctx, runID)
	if err != nil {
		writeErr(w, noRows(err, fmt.Sprintf("run %d", runID)))
		return
	}
	if !run.RunnerID.Valid {
		writeErr(w, fmt.Errorf("%w: run %d has not started", errs.Conflict, runID))
		return
	}
	occ := jetstream.OrderedConsumerConfig{
		FilterSubjects: []string{natsd.RunPTYSubject(run.RunnerID.Int64, runID)},
		DeliverPolicy:  jetstream.DeliverAllPolicy,
	}
	sse := r.URL.Query().Get("format") == "sse"
	if last, err := strconv.ParseUint(r.Header.Get("Last-Event-ID"), 10, 64); sse && err == nil && last > 0 {
		occ.DeliverPolicy, occ.OptStartSeq = jetstream.DeliverByStartSequencePolicy, last+1 // resume
	}
	// ?tail=N (KB) replays only the last N KB of output (the task card; phones); full=1 or no tail
	// replays everything. A resuming reconnect (Last-Event-ID) already knows where it is.
	truncated := false
	if kb, _ := strconv.Atoi(r.URL.Query().Get("tail")); kb > 0 && occ.DeliverPolicy == jetstream.DeliverAllPolicy && r.URL.Query().Get("full") != "1" {
		if seq := ptyTailStart(ctx, d, occ.FilterSubjects[0], kb<<10); seq > 1 {
			occ.DeliverPolicy, occ.OptStartSeq = jetstream.DeliverByStartSequencePolicy, seq
			truncated = true
		}
	}
	cons, err := d.JS.OrderedConsumer(ctx, "WR_PTY", occ)
	if err != nil {
		writeErr(w, err)
		return
	}
	if sse {
		streamPTYSSE(w, r, d, runID, cons, truncated)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	bw := bufio.NewWriter(w)
	for {
		msg, err := cons.Next(jetstream.FetchMaxWait(time.Second))
		if err == nil {
			bw.Write(msg.Data())
			continue
		}
		bw.Flush()
		if fl != nil {
			fl.Flush()
		}
		if ctx.Err() != nil {
			return
		}
		if cur, err := d.Fleet.DB.RQ.GetRun(ctx, runID); err != nil || proto.Terminal(cur.State) {
			return // caught up and the run is over
		}
	}
}

// ptyTailStart returns the stream sequence from which about maxBytes of the subject's most recent
// output remain, or 0 to replay everything. It walks back from the subject's last message; other
// subjects interleave in the stream, so the walk is bounded.
func ptyTailStart(ctx context.Context, d Deps, subject string, maxBytes int) uint64 {
	st, err := d.JS.Stream(ctx, "WR_PTY")
	if err != nil {
		return 0
	}
	last, err := st.GetLastMsgForSubject(ctx, subject)
	if err != nil {
		return 0
	}
	total, start := 0, last.Sequence
	for seq, n := last.Sequence, 0; seq > 0 && n < 4000; seq, n = seq-1, n+1 {
		m, err := st.GetMsg(ctx, seq)
		if err != nil {
			if errors.Is(err, jetstream.ErrMsgNotFound) {
				continue // deleted or expired gap
			}
			return 0
		}
		if m.Subject != subject {
			continue
		}
		total += len(m.Data)
		start = seq
		if total >= maxBytes {
			return start
		}
	}
	return 0 // fewer than maxBytes (or the walk gave up): replay all
}

// streamPTYSSE sends terminal output as SSE: each event's id is the stream sequence (so a
// reconnect resumes with Last-Event-ID) and data is base64; "end" when the run is over.
func streamPTYSSE(w http.ResponseWriter, r *http.Request, d Deps, runID int64, cons jetstream.Consumer, truncated bool) {
	ctx := r.Context()
	fl := sseStart(w)
	if truncated { // the client offers "load full history"
		fmt.Fprint(w, "event: tail\ndata: {}\n\n")
		if fl != nil {
			fl.Flush()
		}
	}
	idle := time.Now()
	for {
		msg, err := cons.Next(jetstream.FetchMaxWait(time.Second))
		if err == nil {
			meta, _ := msg.Metadata()
			var seq uint64
			if meta != nil {
				seq = meta.Sequence.Stream
			}
			fmt.Fprintf(w, "id: %d\nevent: pty\ndata: %s\n\n", seq, base64.StdEncoding.EncodeToString(msg.Data()))
			idle = time.Now()
			if fl != nil {
				fl.Flush()
			}
			continue
		}
		if ctx.Err() != nil {
			return
		}
		if cur, err := d.Fleet.DB.RQ.GetRun(ctx, runID); err != nil || proto.Terminal(cur.State) {
			fmt.Fprint(w, "event: end\ndata: {}\n\n")
			if fl != nil {
				fl.Flush()
			}
			return
		}
		if time.Since(idle) > KeepAlive {
			fmt.Fprint(w, ": ping\n\n")
			idle = time.Now()
			if fl != nil {
				fl.Flush()
			}
		}
	}
}
