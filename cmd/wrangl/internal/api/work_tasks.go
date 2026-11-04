package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/stack"
)

func taskRef(t dbgen.Task) proto.TaskRef {
	return proto.TaskRef{Sqid: t.Sqid.String, Title: t.Title, State: t.State}
}

// activeLocks maps each task a run is working on (or queued for) to the runner that holds it.
func activeLocks(ctx context.Context, d Deps) (map[int64]string, error) {
	rs, err := d.Fleet.DB.RQ.ListRuns(ctx, dbgen.ListRunsParams{State: "active", Lim: 1000})
	if err != nil {
		return nil, err
	}
	m := map[int64]string{}
	for _, r := range rs {
		name := r.RunnerName
		if name == "" {
			name = "queued"
		}
		m[r.Run.TaskID] = name
	}
	return m, nil
}

// treeRows renders project-tree rows as task nodes, inheriting the runner lock down each subtree.
func treeRows(ctx context.Context, d Deps, rows []dbgen.ProjectTreeRow, all bool) ([]proto.TaskNode, error) {
	db := d.Fleet.DB
	locks, err := activeLocks(ctx, d)
	if err != nil {
		return nil, err
	}
	sqidByID := map[int64]string{}
	type frame struct {
		depth int64
		lock  string
	}
	var stackF []frame
	out := []proto.TaskNode{}
	for _, t := range rows {
		sqidByID[t.ID] = t.Sqid
		for len(stackF) > 0 && stackF[len(stackF)-1].depth >= t.Depth {
			stackF = stackF[:len(stackF)-1]
		}
		lock := ""
		if len(stackF) > 0 {
			lock = stackF[len(stackF)-1].lock
		}
		if l, ok := locks[t.ID]; ok {
			lock = l
		}
		stackF = append(stackF, frame{t.Depth, lock})
		if !all && (t.State == "done" || t.State == "cancelled") {
			continue
		}
		n := proto.TaskNode{Sqid: t.Sqid, Parent: sqidByID[t.ParentID.Int64], Title: t.Title, State: t.State,
			Type: string(t.TaskType), Target: t.TargetDate.String, Depth: t.Depth, Unmet: t.Unmet, Children: t.Children, LockedBy: lock}
		if t.TaskType.Planning() {
			if full, err := db.RQ.GetTask(ctx, t.ID); err == nil {
				n.Done, n.Total, n.Slip, _ = d.Stack.Rollup(ctx, full)
			}
		}
		out = append(out, n)
	}
	return out, nil
}

// taskDetail is the body of GET /tasks/{sqid}.
func taskDetail(ctx context.Context, d Deps, t dbgen.Task) proto.TaskDetail {
	db := d.Fleet.DB
	p, _ := db.RQ.GetProject(ctx, t.ProjectID)
	det := proto.TaskDetail{Sqid: t.Sqid.String, Project: p.Slug, Title: t.Title, Prompt: t.Prompt, State: t.State,
		Type: string(t.TaskType), TargetDate: t.TargetDate.String, CreatedBy: t.CreatedBy, Summary: t.CompletionSummary,
		Children: []proto.TaskRef{}, Precursors: []proto.TaskRef{}, Dependents: []proto.TaskRef{}, Runs: []proto.TaskRunView{}, Events: []proto.TaskEventView{}}
	if t.ParentID.Valid {
		if pt, err := db.RQ.GetTask(ctx, t.ParentID.Int64); err == nil {
			det.Parent = pt.Sqid.String
		}
	}
	kids, _ := db.RQ.ListChildren(ctx, sql.NullInt64{Int64: t.ID, Valid: true})
	for _, k := range kids {
		det.Children = append(det.Children, proto.TaskRef{Sqid: k.Sqid.String, Title: k.Title, State: k.State})
	}
	pre, _ := db.RQ.ListPrecursors(ctx, t.ID)
	for _, k := range pre {
		det.Precursors = append(det.Precursors, proto.TaskRef{Sqid: k.Sqid, Title: k.Title, State: k.State})
	}
	dep, _ := db.RQ.ListDependents(ctx, t.ID)
	for _, k := range dep {
		det.Dependents = append(det.Dependents, proto.TaskRef{Sqid: k.Sqid, Title: k.Title, State: k.State})
	}
	rs, _ := db.RQ.RunsForTask(ctx, t.ID)
	for _, x := range rs {
		det.Runs = append(det.Runs, proto.TaskRunView{ID: x.ID, State: x.State, Runner: x.RunnerName, Branch: x.Branch,
			Pushed: x.Pushed == 1, HeadSHA: x.HeadSha, QueuedAt: x.QueuedAt})
	}
	evs, _ := db.RQ.TaskEvents(ctx, t.Sqid.String)
	for _, e := range evs {
		det.Events = append(det.Events, proto.TaskEventView{TS: e.Ts, Actor: e.Actor, Op: e.Op, Diff: json.RawMessage(e.DiffJson)})
	}
	return det
}

func convertSplit(in []proto.SplitNode) []stack.SplitNode {
	out := make([]stack.SplitNode, len(in))
	for i, n := range in {
		out[i] = stack.SplitNode{Title: n.Title, Prompt: n.Prompt, Type: n.Type, Children: convertSplit(n.Children)}
	}
	return out
}

func registerTasks(mux *http.ServeMux, d Deps) {
	db := d.Fleet.DB
	mux.HandleFunc("GET /api/v1/projects/{slug}/tree", getJSON(func(r *http.Request) ([]proto.TaskNode, error) {
		p, err := d.Projects.Project(r.Context(), r.PathValue("slug"))
		if err != nil {
			return nil, err
		}
		rows, err := db.RQ.ProjectTree(r.Context(), p.ID)
		if err != nil {
			return nil, err
		}
		return treeRows(r.Context(), d, rows, r.URL.Query().Get("all") == "1")
	}))
	mux.HandleFunc("GET /api/v1/tasks/{sqid}/tree", getJSON(func(r *http.Request) ([]proto.TaskNode, error) {
		t, err := d.Stack.Task(r.Context(), db.RQ, r.PathValue("sqid"))
		if err != nil {
			return nil, err
		}
		rows, err := db.RQ.ProjectTree(r.Context(), t.ProjectID)
		if err != nil {
			return nil, err
		}
		// Keep the subtree rooted at t (contiguous in depth-first order).
		var sub []dbgen.ProjectTreeRow
		var rootDepth int64 = -1
		for _, x := range rows {
			if rootDepth < 0 {
				if x.ID == t.ID {
					rootDepth = x.Depth
					sub = append(sub, x)
				}
				continue
			}
			if x.Depth <= rootDepth {
				break
			}
			sub = append(sub, x)
		}
		return treeRows(r.Context(), d, sub, r.URL.Query().Get("all") == "1")
	}))
	mux.HandleFunc("GET /api/v1/tasks", getJSON(func(r *http.Request) ([]proto.TaskRef, error) {
		q := r.URL.Query()
		var parent int64
		if s := q.Get("parent"); s != "" {
			t, err := d.Stack.Task(r.Context(), db.RQ, s)
			if err != nil {
				return nil, err
			}
			parent = t.ID
		}
		rows, err := db.RQ.ListTasks(r.Context(), dbgen.ListTasksParams{Project: q.Get("project"), State: q.Get("state"), ParentID: parent})
		if err != nil {
			return nil, err
		}
		out := []proto.TaskRef{}
		for _, x := range rows {
			out = append(out, taskRef(x.Task))
		}
		return out, nil
	}))
	mux.HandleFunc("POST /api/v1/tasks", handle(http.StatusCreated, func(r *http.Request, req proto.TaskCreateRequest) (proto.TaskRef, error) {
		t, err := d.Stack.Create(r.Context(), stack.CreateParams{Project: req.Project, Parent: req.Parent, Title: req.Title,
			Prompt: req.Prompt, Pos: stack.Position{First: req.First, After: req.After, Before: req.Before},
			Repo: req.Repo, Template: req.Template, Issue: req.Issue, Needs: req.Needs, Type: req.Type, TargetDate: req.TargetDate, Actor: stack.User})
		if err != nil {
			return proto.TaskRef{}, err
		}
		// auto_queue projects queue new top-level tasks.
		if p, err := d.Projects.Project(r.Context(), req.Project); err == nil && p.AutoQueue == 1 && req.Parent == "" {
			if _, err := d.Stack.Queue(r.Context(), t.Sqid.String); err != nil {
				d.Fleet.Log.Warn("auto-queue", "task", t.Sqid.String, "err", err)
			}
		}
		return taskRef(t), nil
	}))
	mux.HandleFunc("GET /api/v1/tasks/{sqid}", getJSON(func(r *http.Request) (proto.TaskDetail, error) {
		t, err := d.Stack.Task(r.Context(), db.RQ, r.PathValue("sqid"))
		if err != nil {
			return proto.TaskDetail{}, err
		}
		return taskDetail(r.Context(), d, t), nil
	}))
	mux.HandleFunc("PATCH /api/v1/tasks/{sqid}", handle(http.StatusOK, func(r *http.Request, req proto.TaskPatchRequest) (proto.TaskRef, error) {
		var t dbgen.Task
		var err error
		if req.Type != nil {
			t, err = d.Stack.SetType(r.Context(), r.PathValue("sqid"), *req.Type, stack.User)
		}
		if err == nil && req.TargetDate != nil {
			t, err = d.Stack.SetTargetDate(r.Context(), r.PathValue("sqid"), *req.TargetDate, stack.User)
		}
		if err == nil && (req.Title != nil || req.Prompt != nil || (req.Type == nil && req.TargetDate == nil)) {
			t, err = d.Stack.Edit(r.Context(), r.PathValue("sqid"), req.Title, req.Prompt, stack.User)
		}
		if err != nil {
			return proto.TaskRef{}, err
		}
		return taskRef(t), nil
	}))
	mux.HandleFunc("POST /api/v1/tasks/{sqid}/move", handle(http.StatusOK, func(r *http.Request, req proto.TaskMoveRequest) (proto.TaskRef, error) {
		parent := req.Parent
		if req.Root {
			empty := ""
			parent = &empty
		}
		t, err := d.Stack.Move(r.Context(), r.PathValue("sqid"), parent, stack.Position{First: req.First, After: req.After, Before: req.Before}, stack.User)
		if err != nil {
			return proto.TaskRef{}, err
		}
		return taskRef(t), nil
	}))
	// Reorder the Queue: ids are queued run ids in the wanted order. Only sets queue_rank on the
	// runs; tasks are not reparented.
	type queueOrderRequest struct {
		Runs []int64 `json:"runs"`
	}
	mux.HandleFunc("POST /api/v1/runs/queue-order", handle(http.StatusNoContent, func(r *http.Request, req queueOrderRequest) (none, error) {
		for i, id := range req.Runs {
			if _, err := db.Q.SetRunQueueRank(r.Context(), dbgen.SetRunQueueRankParams{QueueRank: sql.NullFloat64{Float64: float64(i + 1), Valid: true}, ID: id}); err != nil {
				return none{}, err
			}
		}
		return none{}, nil
	}))
	mux.HandleFunc("POST /api/v1/tasks/{sqid}/needs/{pre}", ack(http.StatusNoContent, func(r *http.Request) error {
		return d.Stack.NeedsAdd(r.Context(), r.PathValue("sqid"), r.PathValue("pre"), stack.User)
	}))
	mux.HandleFunc("DELETE /api/v1/tasks/{sqid}/needs/{pre}", ack(http.StatusNoContent, func(r *http.Request) error {
		return d.Stack.NeedsRm(r.Context(), r.PathValue("sqid"), r.PathValue("pre"), stack.User)
	}))
	type planRequest struct {
		Guidance string `json:"guidance,omitempty"`
	}
	mux.HandleFunc("POST /api/v1/tasks/{sqid}/plan", handleOpt(http.StatusCreated, func(r *http.Request, req planRequest) (map[string]any, error) {
		run, err := d.Plans.Queue(r.Context(), r.PathValue("sqid"), req.Guidance)
		if err != nil {
			return nil, err
		}
		return map[string]any{"run_id": run.ID}, nil
	}))
	mux.HandleFunc("POST /api/v1/tasks/{sqid}/split", handle(http.StatusCreated, func(r *http.Request, req proto.TaskSplitRequest) ([]proto.TaskRef, error) {
		var nodes []stack.SplitNode
		if req.Outline != "" {
			var err error
			if nodes, err = stack.ParseOutline(req.Outline); err != nil {
				return nil, err
			}
		} else {
			nodes = convertSplit(req.Children)
		}
		created, err := d.Stack.Split(r.Context(), r.PathValue("sqid"), nodes, req.Chain, req.CopyPrompt, stack.User)
		if err != nil {
			return nil, err
		}
		out := []proto.TaskRef{}
		for _, t := range created {
			out = append(out, taskRef(t))
		}
		return out, nil
	}))
	mux.HandleFunc("POST /api/v1/tasks/{sqid}/dequeue", getJSON(func(r *http.Request) (map[string]any, error) {
		n, err := d.Runs.Dequeue(r.Context(), r.PathValue("sqid"))
		if err != nil {
			return nil, err
		}
		return map[string]any{"dequeued": n}, nil
	}))
	mux.HandleFunc("POST /api/v1/tasks/{sqid}/queue", handleOpt(http.StatusCreated, func(r *http.Request, req proto.QueueRequest) (map[string]any, error) {
		run, err := d.Plans.QueueWorkWith(r.Context(), r.PathValue("sqid"), req.Agent)
		if err != nil {
			return nil, err
		}
		return map[string]any{"run_id": run.ID, "branch": run.Branch}, nil
	}))
	for _, op := range []string{"complete", "reopen", "cancel"} {
		mux.HandleFunc("POST /api/v1/tasks/{sqid}/"+op, handleOpt(http.StatusNoContent, func(r *http.Request, req proto.MessageRequest) (none, error) {
			sqid := r.PathValue("sqid")
			var err error
			switch op {
			case "complete":
				err = d.Stack.Complete(r.Context(), sqid, req.Message, stack.User)
			case "reopen":
				err = d.Stack.Reopen(r.Context(), sqid, stack.User)
			case "cancel":
				err = d.Stack.Cancel(r.Context(), sqid, req.Message, stack.User)
			}
			return none{}, err
		}))
	}
	mux.HandleFunc("DELETE /api/v1/tasks/{sqid}", ack(http.StatusNoContent, func(r *http.Request) error {
		return d.Stack.Delete(r.Context(), r.PathValue("sqid"), stack.User)
	}))
}
