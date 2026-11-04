package web

// Template data for Logs and the run pages (log, diff, files).

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/errs"
)

func (h *Handler) viewLogs(ctx context.Context, view string, r *http.Request, d map[string]any) error {
	rows, err := h.logRows(ctx, reqValue(r, "kind"))
	if err != nil {
		return err
	}
	d["Rows"], d["Kind"], d["Title"] = rows, reqValue(r, "kind"), "Logs"
	return nil
}

func (h *Handler) viewRun(ctx context.Context, view string, r *http.Request, d map[string]any) error {
	db := h.D.Fleet.DB
	id, _ := strconv.ParseInt(reqValue(r, "id"), 10, 64)
	run, err := db.RQ.GetRun(ctx, id)
	if err != nil {
		return fmt.Errorf("%w: run %d", errs.NotFound, id)
	}
	d["Run"], d["Title"] = run, fmt.Sprintf("Run %d", id)
	if t, err := db.RQ.GetTask(ctx, run.TaskID); err == nil {
		d["Task"] = t
	}
	if run.RunnerID.Valid {
		if rn, err := db.RQ.GetRunner(ctx, run.RunnerID.Int64); err == nil {
			d["Runner"] = rn.Name
		}
	}
	if pr, err := db.RQ.OpenPRForTask(ctx, run.TaskID); err == nil {
		d["PR"] = pr
	}
	switch view {
	case "run":
		q := r.URL.Query()
		logs, _ := db.RQ.RunLogs(ctx, dbgen.RunLogsParams{RunID: id, Stream: q.Get("stream"), Lim: 2000})
		if g := strings.ToLower(q.Get("q")); g != "" {
			var f []dbgen.LogLine
			for _, l := range logs {
				if strings.Contains(strings.ToLower(l.Message), g) {
					f = append(f, l)
				}
			}
			logs = f
		}
		if len(logs) > 300 {
			logs = logs[len(logs)-300:]
		}
		d["Logs"], d["Q"], d["Stream"] = logs, q.Get("q"), q.Get("stream")
		d["Hooks"], _ = db.RQ.RunHookEvents(ctx, id)
		if h.D.Deliverables != nil {
			rows, _ := db.RQ.ListDeliverablesForRun(ctx, sql.NullInt64{Int64: id, Valid: true})
			d["Deliverables"] = h.deliverableViews(ctx, rows)
		}
	case "diff":
		out, err := h.D.Runs.Inspect(ctx, id, "diff", "", false)
		d["Err"] = errString(err)
		d["Files"] = parseDiff(out.Data)
		d["Truncated"] = out.Truncated
	case "files":
		path := r.URL.Query().Get("path")
		d["Path"] = path
		kind := "tree"
		if path != "" && !strings.HasSuffix(path, "/") {
			kind = "file"
		}
		out, err := h.D.Runs.Inspect(ctx, id, kind, path, false)
		if err != nil && kind == "file" { // maybe a directory without the slash
			if o2, e2 := h.D.Runs.Inspect(ctx, id, "tree", path, false); e2 == nil {
				out, err, kind, path = o2, nil, "tree", path+"/"
				d["Path"] = path
			}
		}
		d["Err"], d["Kind"] = errString(err), kind
		if kind == "tree" {
			var entries []string
			for _, e := range strings.Split(strings.TrimSpace(out.Data), "\n") {
				if e != "" {
					entries = append(entries, e)
				}
			}
			d["Entries"] = entries
		} else {
			d["IsMD"] = strings.HasSuffix(strings.ToLower(path), ".md") && r.URL.Query().Get("raw") != "1"
			d["Content"] = out.Data
			d["Highlighted"] = highlight(path, out.Data)
		}
		d["Up"] = parentDir(path)
	}
	return nil
}
