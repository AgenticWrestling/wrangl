package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/errs"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

func runView(x dbgen.ListRunsRow) proto.RunView {
	return proto.RunView{ID: x.Run.ID, Task: x.TaskSqid, TaskTitle: x.TaskTitle, Project: x.ProjectSlug, Kind: x.Run.Kind,
		State: x.Run.State, Runner: x.RunnerName, Current: x.CurrentSqid, Branch: x.Run.Branch, Session: x.Run.SessionName,
		Status: x.Run.StatusText, Pushed: x.Run.Pushed == 1, BaseSHA: x.Run.BaseSha, HeadSHA: x.Run.HeadSha,
		BlockedSince: x.Run.BlockedSince.String, Error: x.Run.Error, QueuedAt: x.Run.QueuedAt, StartedAt: x.Run.StartedAt.String, FinishedAt: x.Run.FinishedAt.String}
}

func registerRuns(mux *http.ServeMux, d Deps) {
	db := d.Fleet.DB
	mux.HandleFunc("GET /api/v1/runs/{id}/hooks", getJSON(func(r *http.Request) ([]map[string]any, error) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%w: run id", errs.Invalid)
		}
		evs, err := db.RQ.RunHookEvents(r.Context(), id)
		if err != nil {
			return nil, err
		}
		out := []map[string]any{}
		for _, e := range evs {
			out = append(out, map[string]any{"seq": e.Seq, "ts": e.Ts, "event": e.Name, "payload": json.RawMessage(e.PayloadJson)})
		}
		return out, nil
	}))
	mux.HandleFunc("GET /api/v1/runs", getJSON(func(r *http.Request) ([]proto.RunView, error) {
		q := r.URL.Query()
		lim, _ := strconv.ParseInt(q.Get("limit"), 10, 64)
		if lim <= 0 || lim > 500 {
			lim = 50
		}
		rows, err := db.RQ.ListRuns(r.Context(), dbgen.ListRunsParams{State: q.Get("state"), Runner: q.Get("runner"), Project: q.Get("project"), Lim: lim})
		if err != nil {
			return nil, err
		}
		out := []proto.RunView{}
		for _, x := range rows {
			out = append(out, runView(x))
		}
		return out, nil
	}))
	mux.HandleFunc("GET /api/v1/runs/{id}", getJSON(func(r *http.Request) (proto.RunView, error) {
		id, err := pathID(r, "id", "run id")
		if err != nil {
			return proto.RunView{}, err
		}
		rows, err := db.RQ.ListRuns(r.Context(), dbgen.ListRunsParams{Lim: 100000})
		if err != nil {
			return proto.RunView{}, err
		}
		for _, x := range rows {
			if x.Run.ID == id {
				v := runView(x)
				v.Warning, _ = db.RQ.GetRunWarning(r.Context(), id)
				return v, nil
			}
		}
		return proto.RunView{}, fmt.Errorf("%w: run %d", errs.NotFound, id)
	}))
	mux.HandleFunc("POST /api/v1/runs/{id}/cancel", ack(http.StatusAccepted, func(r *http.Request) error {
		id, err := pathID(r, "id", "run id")
		if err != nil {
			return err
		}
		return d.Runs.Cancel(r.Context(), id)
	}))
	for kind, path := range map[string]string{"diff": "diff", "tree": "files", "file": "file"} {
		mux.HandleFunc("GET /api/v1/runs/{id}/"+path, getJSON(func(r *http.Request) (proto.InspectReply, error) {
			id, err := pathID(r, "id", "run id")
			if err != nil {
				return proto.InspectReply{}, err
			}
			return d.Runs.Inspect(r.Context(), id, kind, r.URL.Query().Get("path"), r.URL.Query().Get("stat") == "1")
		}))
	}
	mux.HandleFunc("GET /api/v1/runs/{id}/diffdata", getJSON(func(r *http.Request) (proto.DiffView, error) {
		id, err := pathID(r, "id", "run id")
		if err != nil {
			return proto.DiffView{}, err
		}
		if d.Diffs == nil {
			return proto.DiffView{}, fmt.Errorf("%w: diff data", errs.NotFound)
		}
		return d.Diffs.ForRun(r.Context(), id, r.URL.Query().Get("commit"))
	}))
	mux.HandleFunc("GET /api/v1/runs/{id}/pty", func(w http.ResponseWriter, r *http.Request) {
		id, err := pathID(r, "id", "run id")
		if err != nil {
			writeErr(w, err)
			return
		}
		streamPTY(w, r, d, id)
	})
	mux.HandleFunc("GET /api/v1/runs/{id}/transcript", func(w http.ResponseWriter, r *http.Request) {
		id, err := pathID(r, "id", "run id")
		if err != nil {
			writeErr(w, err)
			return
		}
		streamTranscript(w, r, d, id)
	})
	mux.HandleFunc("GET /api/v1/runs/{id}/logs", func(w http.ResponseWriter, r *http.Request) {
		id, err := pathID(r, "id", "run id")
		if err != nil {
			writeErr(w, err)
			return
		}
		streamLogs(w, r, d, id)
	})
	mux.HandleFunc("GET /api/v1/logs/search", getJSON(func(r *http.Request) ([]proto.LogView, error) {
		q := r.URL.Query()
		if strings.TrimSpace(q.Get("q")) == "" {
			return nil, fmt.Errorf("%w: q is required", errs.Invalid)
		}
		lim, _ := strconv.ParseInt(q.Get("limit"), 10, 64)
		if lim <= 0 || lim > 1000 {
			lim = 100
		}
		since, err := sinceParam(q.Get("since"))
		if err != nil {
			return nil, err
		}
		rows, err := db.RQ.SearchLogsFiltered(r.Context(), dbgen.SearchLogsFilteredParams{Query: q.Get("q"), Project: q.Get("project"), Runner: q.Get("runner"), Since: since, Lim: lim})
		if err != nil {
			if strings.Contains(err.Error(), "fts5") || strings.Contains(err.Error(), "syntax") {
				err = fmt.Errorf("%w: search query: %v", errs.Invalid, err)
			}
			return nil, err
		}
		out := []proto.LogView{}
		for _, x := range rows {
			out = append(out, proto.LogView{ID: x.ID, RunID: x.RunID, TS: x.Ts, Stream: x.Stream, Message: x.Message,
				Project: x.ProjectSlug, Runner: x.RunnerName, Task: x.TaskSqid})
		}
		return out, nil
	}))
}
