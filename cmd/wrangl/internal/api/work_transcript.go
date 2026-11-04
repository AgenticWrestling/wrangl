package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/natsd"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/errs"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/transcript"
)

// Transcript tail paging bounds.
const (
	transcriptDefaultLimit = 100
	transcriptMaxLimit     = 500
)

// TranscriptPage is the JSON tail: the newest entries before a cursor, oldest first.
type TranscriptPage struct {
	Entries []transcript.Entry `json:"entries"`
	// HasMore: older entries exist (page again with before=<first seq>).
	HasMore bool `json:"has_more"`
}

// streamTranscript serves GET /api/v1/runs/{id}/transcript. Entries come from the
// run's JetStream subject (nothing new is stored). format=sse streams them with id=seq
// and resumes after Last-Event-ID; otherwise a JSON tail (?limit=N&before=seq).
func streamTranscript(w http.ResponseWriter, r *http.Request, d Deps, runID int64) {
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
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 {
		limit = transcriptDefaultLimit
	}
	limit = min(limit, transcriptMaxLimit)
	subj := natsd.RunTranscriptSubject(run.RunnerID.Int64, runID)

	if q.Get("format") != "sse" {
		before, _ := strconv.Atoi(q.Get("before"))
		page, err := transcriptTail(ctx, d.JS, subj, before, limit)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, page)
		return
	}

	// SSE: resume after Last-Event-ID; a fresh connection replays the last `limit`
	// entries only when limit is given, else everything.
	after, _ := strconv.Atoi(r.Header.Get("Last-Event-ID"))
	if after == 0 {
		after, _ = strconv.Atoi(q.Get("after"))
	}
	if after == 0 && q.Get("limit") != "" {
		page, err := transcriptTail(ctx, d.JS, subj, 0, limit)
		if err != nil {
			writeErr(w, err)
			return
		}
		if len(page.Entries) > 0 {
			after = page.Entries[0].Seq - 1
		}
	}
	cons, err := d.JS.OrderedConsumer(ctx, "WR_EVT", jetstream.OrderedConsumerConfig{
		FilterSubjects: []string{subj}, DeliverPolicy: jetstream.DeliverAllPolicy,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	fl := sseStart(w)
	idle := time.Now()
	flush := func() {
		if fl != nil {
			fl.Flush()
		}
	}
	for {
		msg, err := cons.Next(jetstream.FetchMaxWait(time.Second))
		if err == nil {
			var e transcript.Entry
			if json.Unmarshal(msg.Data(), &e) != nil || e.Seq <= after {
				continue
			}
			fmt.Fprintf(w, "id: %d\nevent: entry\ndata: %s\n\n", e.Seq, msg.Data())
			idle = time.Now()
			flush()
			continue
		}
		if ctx.Err() != nil {
			return
		}
		if cur, err := d.Fleet.DB.RQ.GetRun(ctx, runID); err != nil || proto.Terminal(cur.State) {
			fmt.Fprint(w, "event: end\ndata: {}\n\n")
			flush()
			return
		}
		if time.Since(idle) > KeepAlive {
			fmt.Fprint(w, ": ping\n\n")
			idle = time.Now()
			flush()
		}
	}
}

// TranscriptTail is the newest `limit` entries of a run's transcript with seq < before (0: no
// bound), oldest first; used by the web card for stored entries.
func TranscriptTail(ctx context.Context, js jetstream.JetStream, runnerID, runID int64, before, limit int) (TranscriptPage, error) {
	return transcriptTail(ctx, js, natsd.RunTranscriptSubject(runnerID, runID), before, limit)
}

// transcriptTail returns the newest `limit` entries with seq < before (0: no bound).
func transcriptTail(ctx context.Context, js jetstream.JetStream, subj string, before, limit int) (TranscriptPage, error) {
	page := TranscriptPage{Entries: []transcript.Entry{}}
	cons, err := js.OrderedConsumer(ctx, "WR_EVT", jetstream.OrderedConsumerConfig{
		FilterSubjects: []string{subj}, DeliverPolicy: jetstream.DeliverAllPolicy,
	})
	if err != nil {
		return page, err
	}
	var ring []transcript.Entry
	for {
		msg, err := cons.Next(jetstream.FetchMaxWait(time.Second))
		if err != nil {
			break // nothing (more) to read
		}
		var e transcript.Entry
		if json.Unmarshal(msg.Data(), &e) == nil && (before <= 0 || e.Seq < before) {
			ring = append(ring, e)
			if len(ring) > 4*limit { // keep memory bounded
				ring = append([]transcript.Entry(nil), ring[len(ring)-limit:]...)
			}
		}
		if meta, err := msg.Metadata(); err == nil && meta.NumPending == 0 {
			break
		}
	}
	if len(ring) > limit {
		ring = ring[len(ring)-limit:]
	}
	if len(ring) > 0 {
		page.Entries = ring
		page.HasMore = ring[0].Seq > 1
	}
	return page, nil
}
