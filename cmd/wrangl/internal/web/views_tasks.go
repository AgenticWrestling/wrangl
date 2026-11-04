package web

// Template data for the Tasks list and a task page.

import (
	"context"
	"database/sql"
	"net/http"
	"strconv"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbtypes"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

func (h *Handler) viewTasks(ctx context.Context, view string, r *http.Request, d map[string]any) error {
	db := h.D.Fleet.DB
	projects, err := db.RQ.ListProjects(ctx, "active")
	if err != nil {
		return err
	}
	d["Projects"] = projects
	d["Recent"], d["Rest"] = recentProjects(projects, 4)
	d["Templates"], _ = db.RQ.ListTemplates(ctx)
	slug := r.URL.Query().Get("project")
	if slug == "" && len(projects) > 0 {
		slug = projects[0].Slug
	}
	d["Project"], d["Title"] = slug, "Tasks"
	d["ArchivedProjects"], _ = db.RQ.ListProjects(ctx, "archived")
	if slug != "" { // a direct link to an archived project still works, with its actions disabled
		if cur, err := db.RQ.GetProjectBySlug(ctx, slug); err == nil {
			d["Archived"] = cur.ArchivedAt.Valid
		}
	}
	d["Types"] = dbtypes.TaskTypes
	d["All"] = r.URL.Query().Get("all") == "1"
	if slug != "" {
		secs, err := h.tree(ctx, slug, d["All"].(bool))
		if err != nil {
			return err
		}
		d["Roots"], d["Progress"], d["Queue"] = secs.Backlog, secs.Progress, secs.Queue
		d["Now"], d["NowOnly"] = secs.Now, r.URL.Query().Get("now") == "1"
	}
	return nil
}

func (h *Handler) viewTask(ctx context.Context, view string, r *http.Request, d map[string]any) error {
	db := h.D.Fleet.DB
	sqid := reqValue(r, "sqid")
	t, err := h.D.Stack.Task(ctx, db.RQ, sqid)
	if err != nil {
		return err
	}
	p, _ := db.RQ.GetProject(ctx, t.ProjectID)
	d["Task"], d["Project"], d["Title"] = t, p.Slug, t.Title
	d["Archived"] = p.ArchivedAt.Valid
	d["Attention"] = h.attention(ctx, p.Slug)[sqid] && t.State != "done" && t.State != "cancelled"
	if secs, err := h.tree(ctx, p.Slug, false); err == nil {
		if n := secs.Index[sqid]; n != nil {
			d["TNode"] = n
		}
		kk := map[string]string{}
		for sq, n := range secs.Index {
			kk[sq] = n.Kind
		}
		d["KidKind"] = kk
	}
	d["Types"] = dbtypes.TaskTypes
	if h.D.Deliverables != nil {
		rows, _ := db.RQ.ListDeliverablesForTask(ctx, t.ID)
		dv := h.deliverableViews(ctx, rows)
		d["Deliverables"] = dv
	}
	if t.ParentID.Valid {
		if pt, err := db.RQ.GetTask(ctx, t.ParentID.Int64); err == nil {
			d["Parent"] = pt
		}
	}
	kids, _ := db.RQ.ListChildren(ctx, sql.NullInt64{Int64: t.ID, Valid: true})
	d["Kids"] = kids
	if cp, err := db.RQ.LatestTaskCheckpoint(ctx, t.ID); err == nil {
		d["Checkpoint"] = checkpointLinks(cp)
	}
	// Queue is offered on every live task; if nothing is runnable the refusal says why (e.g.
	// subtasks still in review).
	d["CanQueue"] = false
	if n, ok := d["TNode"].(*node); ok {
		d["CanQueue"] = n.Queueable > 0
	}
	if h.D.Agents != nil {
		d["Thread"], d["Agents"] = h.threadView(ctx, t.ID)
	}
	// Live output: the in-progress run working on this task (or its subtasks), else the task's
	// latest finished run. With transcript entries it is rendered Markdown; otherwise a running
	// run falls back to its PTY (keyed by the run, not the runner) and a finished one shows nothing.
	if runs, err := db.RQ.ListRuns(ctx, dbgen.ListRunsParams{State: "active", Lim: 1000}); err == nil {
		for _, x := range runs {
			if (x.TaskSqid == sqid || x.CurrentSqid == sqid) && x.Run.RunnerID.Valid && x.Run.State != "queued" {
				h.livePanel(ctx, d, x.Run.ID, x.Run.RunnerID.Int64, true)
				break
			}
		}
	}
	if _, live := d["LiveMode"]; !live {
		if rs, err := db.RQ.RunsForTask(ctx, t.ID); err == nil {
			for _, x := range rs {
				if !proto.Terminal(x.State) {
					continue
				}
				if row, err := db.RQ.GetRun(ctx, x.ID); err == nil && row.RunnerID.Valid {
					h.livePanel(ctx, d, x.ID, row.RunnerID.Int64, false)
					if d["LiveMode"] != liveNone {
						break
					}
				}
			}
		}
	}
	if _, ok := d["LiveMode"]; !ok {
		d["LiveMode"] = liveNone
	}
	d["Pre"], _ = db.RQ.ListPrecursors(ctx, t.ID)
	d["Dep"], _ = db.RQ.ListDependents(ctx, t.ID)
	d["Runs"], _ = db.RQ.RunsForTask(ctx, t.ID)
	d["Events"], _ = db.RQ.TaskEvents(ctx, sqid)
	if qid := r.URL.Query().Get("split"); qid != "" { // "Edit" on a plan proposal
		if id, err := strconv.ParseInt(qid, 10, 64); err == nil {
			if q, err := h.D.Runs.Question(ctx, id); err == nil {
				d["SplitOutline"] = outlineFromPayload(q.Payload)
			}
		}
	}
	return nil
}
