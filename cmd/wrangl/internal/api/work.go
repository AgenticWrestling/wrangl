package api

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/natsd"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/projects"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/errs"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/stack"
)

func noRows(err error, what string) error {
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %s", errs.NotFound, what)
	}
	return err
}

func registerWork(mux *http.ServeMux, d Deps) {
	db := d.Fleet.DB
	tmplName := func(ctx context.Context, id sql.NullInt64) string {
		if !id.Valid {
			return ""
		}
		t, err := db.RQ.GetTemplate(ctx, id.Int64)
		if err != nil {
			return ""
		}
		return t.Name
	}
	projectView := func(ctx context.Context, p dbgen.Project) proto.ProjectView {
		return proto.ProjectView{Slug: p.Slug, Name: p.Name, Description: p.Description,
			Template: tmplName(ctx, p.DefaultTemplateID), PlanTemplate: tmplName(ctx, p.PlanTemplateID),
			AutoMerge: p.AutoMerge == 1, AutoQueue: p.AutoQueue == 1, Priority: p.Priority}
	}
	params := func(r proto.ProjectRequest) projects.ProjectParams {
		return projects.ProjectParams{Name: r.Name, Description: r.Description, Template: r.Template,
			PlanTemplate: r.PlanTemplate, AutoMerge: r.AutoMerge, AutoQueue: r.AutoQueue, Priority: r.Priority}
	}

	// --- projects ---
	mux.HandleFunc("GET /api/v1/projects", func(w http.ResponseWriter, r *http.Request) {
		ps, err := db.RQ.ListProjects(r.Context())
		if err != nil {
			writeErr(w, err)
			return
		}
		out := []proto.ProjectView{}
		for _, p := range ps {
			out = append(out, projectView(r.Context(), p))
		}
		writeJSON(w, http.StatusOK, out)
	})
	mux.HandleFunc("POST /api/v1/projects", func(w http.ResponseWriter, r *http.Request) {
		var req proto.ProjectRequest
		if err := decode(r, &req); err != nil {
			writeErr(w, err)
			return
		}
		p, err := d.Projects.Create(r.Context(), req.Slug, params(req))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, projectView(r.Context(), p))
	})
	mux.HandleFunc("GET /api/v1/projects/{slug}", func(w http.ResponseWriter, r *http.Request) {
		p, err := d.Projects.Project(r.Context(), r.PathValue("slug"))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, projectView(r.Context(), p))
	})
	mux.HandleFunc("PATCH /api/v1/projects/{slug}", func(w http.ResponseWriter, r *http.Request) {
		var req proto.ProjectRequest
		if err := decode(r, &req); err != nil {
			writeErr(w, err)
			return
		}
		p, err := d.Projects.Update(r.Context(), r.PathValue("slug"), params(req))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, projectView(r.Context(), p))
	})
	mux.HandleFunc("DELETE /api/v1/projects/{slug}", func(w http.ResponseWriter, r *http.Request) {
		if err := d.Projects.Delete(r.Context(), r.PathValue("slug")); err != nil {
			writeErr(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	// --- repos ---
	mux.HandleFunc("GET /api/v1/repos", func(w http.ResponseWriter, r *http.Request) {
		var pid int64
		if slug := r.URL.Query().Get("project"); slug != "" {
			p, err := d.Projects.Project(r.Context(), slug)
			if err != nil {
				writeErr(w, err)
				return
			}
			pid = p.ID
		}
		rows, err := db.RQ.ListRepos(r.Context(), pid)
		if err != nil {
			writeErr(w, err)
			return
		}
		out := []proto.RepoView{}
		for _, x := range rows {
			out = append(out, proto.RepoView{Project: x.ProjectSlug, Repo: x.Repo.Owner + "/" + x.Repo.Name,
				DefaultBranch: x.Repo.DefaultBranch, CloneURL: x.Repo.CloneUrl})
		}
		writeJSON(w, http.StatusOK, out)
	})
	mux.HandleFunc("POST /api/v1/projects/{slug}/repos", func(w http.ResponseWriter, r *http.Request) {
		var req proto.LinkRepoRequest
		if err := decode(r, &req); err != nil {
			writeErr(w, err)
			return
		}
		x, err := d.Projects.LinkRepo(r.Context(), r.PathValue("slug"), req.Repo, req.Branch)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, proto.RepoView{Project: r.PathValue("slug"), Repo: x.Owner + "/" + x.Name,
			DefaultBranch: x.DefaultBranch, CloneURL: x.CloneUrl})
	})
	mux.HandleFunc("DELETE /api/v1/projects/{slug}/repos/{owner}/{name}", func(w http.ResponseWriter, r *http.Request) {
		if err := d.Projects.UnlinkRepo(r.Context(), r.PathValue("slug"), r.PathValue("owner")+"/"+r.PathValue("name")); err != nil {
			writeErr(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	// --- issues ---
	mux.HandleFunc("POST /api/v1/projects/{slug}/issues/sync", func(w http.ResponseWriter, r *http.Request) {
		n, err := d.Projects.SyncIssues(r.Context(), r.PathValue("slug"))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]int{"synced": n})
	})
	mux.HandleFunc("GET /api/v1/issues", func(w http.ResponseWriter, r *http.Request) {
		var pid int64
		if slug := r.URL.Query().Get("project"); slug != "" {
			p, err := d.Projects.Project(r.Context(), slug)
			if err != nil {
				writeErr(w, err)
				return
			}
			pid = p.ID
		}
		state := r.URL.Query().Get("state")
		if state == "" {
			state = "open"
		}
		rows, err := db.RQ.ListIssues(r.Context(), dbgen.ListIssuesParams{ProjectID: pid, State: state})
		if err != nil {
			writeErr(w, err)
			return
		}
		out := []proto.IssueView{}
		for _, x := range rows {
			out = append(out, proto.IssueView{Ref: fmt.Sprintf("%s/%s#%d", x.RepoOwner, x.RepoName, x.Issue.GhNumber),
				Title: x.Issue.Title, State: x.Issue.State, URL: x.Issue.Url, Task: x.TaskSqid})
		}
		writeJSON(w, http.StatusOK, out)
	})
	issueRef := func(r *http.Request) string {
		return r.PathValue("owner") + "/" + r.PathValue("name") + "#" + r.PathValue("number")
	}
	mux.HandleFunc("GET /api/v1/projects/{slug}/issues/{owner}/{name}/{number}", func(w http.ResponseWriter, r *http.Request) {
		i, err := d.Projects.Issue(r.Context(), r.PathValue("slug"), issueRef(r))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, proto.IssueView{Ref: issueRef(r), Title: i.Title, State: i.State, URL: i.Url, Body: i.Body})
	})
	mux.HandleFunc("POST /api/v1/projects/{slug}/issues/{owner}/{name}/{number}/task", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Parent   string `json:"parent,omitempty"`
			Template string `json:"template,omitempty"`
		}
		if err := decode(r, &req); err != nil {
			writeErr(w, err)
			return
		}
		t, err := d.Projects.IssueToTask(r.Context(), r.PathValue("slug"), issueRef(r), req.Parent, req.Template)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, proto.TaskRef{Sqid: t.Sqid.String, Title: t.Title, State: t.State})
	})

	// --- tasks ---
	activeLocks := func(ctx context.Context) (map[int64]string, error) {
		rs, err := db.RQ.ListRuns(ctx, dbgen.ListRunsParams{State: "active", Lim: 1000})
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
	treeRows := func(ctx context.Context, rows []dbgen.ProjectTreeRow, all bool) ([]proto.TaskNode, error) {
		locks, err := activeLocks(ctx)
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
	mux.HandleFunc("GET /api/v1/projects/{slug}/tree", func(w http.ResponseWriter, r *http.Request) {
		p, err := d.Projects.Project(r.Context(), r.PathValue("slug"))
		if err != nil {
			writeErr(w, err)
			return
		}
		rows, err := db.RQ.ProjectTree(r.Context(), p.ID)
		if err != nil {
			writeErr(w, err)
			return
		}
		out, err := treeRows(r.Context(), rows, r.URL.Query().Get("all") == "1")
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, out)
	})
	mux.HandleFunc("GET /api/v1/tasks/{sqid}/tree", func(w http.ResponseWriter, r *http.Request) {
		t, err := d.Stack.Task(r.Context(), db.RQ, r.PathValue("sqid"))
		if err != nil {
			writeErr(w, err)
			return
		}
		rows, err := db.RQ.ProjectTree(r.Context(), t.ProjectID)
		if err != nil {
			writeErr(w, err)
			return
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
		out, err := treeRows(r.Context(), sub, r.URL.Query().Get("all") == "1")
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, out)
	})
	mux.HandleFunc("GET /api/v1/tasks", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		var parent int64
		if s := q.Get("parent"); s != "" {
			t, err := d.Stack.Task(r.Context(), db.RQ, s)
			if err != nil {
				writeErr(w, err)
				return
			}
			parent = t.ID
		}
		rows, err := db.RQ.ListTasks(r.Context(), dbgen.ListTasksParams{Project: q.Get("project"), State: q.Get("state"), ParentID: parent})
		if err != nil {
			writeErr(w, err)
			return
		}
		out := []proto.TaskRef{}
		for _, x := range rows {
			out = append(out, proto.TaskRef{Sqid: x.Task.Sqid.String, Title: x.Task.Title, State: x.Task.State})
		}
		writeJSON(w, http.StatusOK, out)
	})
	mux.HandleFunc("POST /api/v1/tasks", func(w http.ResponseWriter, r *http.Request) {
		var req proto.TaskCreateRequest
		if err := decode(r, &req); err != nil {
			writeErr(w, err)
			return
		}
		t, err := d.Stack.Create(r.Context(), stack.CreateParams{Project: req.Project, Parent: req.Parent, Title: req.Title,
			Prompt: req.Prompt, Pos: stack.Position{First: req.First, After: req.After, Before: req.Before},
			Repo: req.Repo, Template: req.Template, Issue: req.Issue, Needs: req.Needs, Type: req.Type, TargetDate: req.TargetDate, Actor: stack.User})
		if err != nil {
			writeErr(w, err)
			return
		}
		// auto_queue projects queue new top-level tasks.
		if p, err := d.Projects.Project(r.Context(), req.Project); err == nil && p.AutoQueue == 1 && req.Parent == "" {
			if _, err := d.Stack.Queue(r.Context(), t.Sqid.String); err != nil {
				d.Fleet.Log.Warn("auto-queue", "task", t.Sqid.String, "err", err)
			}
		}
		writeJSON(w, http.StatusCreated, proto.TaskRef{Sqid: t.Sqid.String, Title: t.Title, State: t.State})
	})
	mux.HandleFunc("GET /api/v1/tasks/{sqid}", func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		t, err := d.Stack.Task(ctx, db.RQ, r.PathValue("sqid"))
		if err != nil {
			writeErr(w, err)
			return
		}
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
		writeJSON(w, http.StatusOK, det)
	})
	mux.HandleFunc("PATCH /api/v1/tasks/{sqid}", func(w http.ResponseWriter, r *http.Request) {
		var req proto.TaskPatchRequest
		if err := decode(r, &req); err != nil {
			writeErr(w, err)
			return
		}
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
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, proto.TaskRef{Sqid: t.Sqid.String, Title: t.Title, State: t.State})
	})
	mux.HandleFunc("POST /api/v1/tasks/{sqid}/move", func(w http.ResponseWriter, r *http.Request) {
		var req proto.TaskMoveRequest
		if err := decode(r, &req); err != nil {
			writeErr(w, err)
			return
		}
		parent := req.Parent
		if req.Root {
			empty := ""
			parent = &empty
		}
		t, err := d.Stack.Move(r.Context(), r.PathValue("sqid"), parent, stack.Position{First: req.First, After: req.After, Before: req.Before}, stack.User)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, proto.TaskRef{Sqid: t.Sqid.String, Title: t.Title, State: t.State})
	})
	mux.HandleFunc("POST /api/v1/tasks/{sqid}/needs/{pre}", func(w http.ResponseWriter, r *http.Request) {
		if err := d.Stack.NeedsAdd(r.Context(), r.PathValue("sqid"), r.PathValue("pre"), stack.User); err != nil {
			writeErr(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("DELETE /api/v1/tasks/{sqid}/needs/{pre}", func(w http.ResponseWriter, r *http.Request) {
		if err := d.Stack.NeedsRm(r.Context(), r.PathValue("sqid"), r.PathValue("pre"), stack.User); err != nil {
			writeErr(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/tasks/{sqid}/plan", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Guidance string `json:"guidance,omitempty"`
		}
		if r.ContentLength > 0 {
			if err := decode(r, &req); err != nil {
				writeErr(w, err)
				return
			}
		}
		run, err := d.Plans.Queue(r.Context(), r.PathValue("sqid"), req.Guidance)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"run_id": run.ID})
	})
	mux.HandleFunc("POST /api/v1/tasks/{sqid}/split", func(w http.ResponseWriter, r *http.Request) {
		var req proto.TaskSplitRequest
		if err := decode(r, &req); err != nil {
			writeErr(w, err)
			return
		}
		var nodes []stack.SplitNode
		if req.Outline != "" {
			var err error
			if nodes, err = stack.ParseOutline(req.Outline); err != nil {
				writeErr(w, err)
				return
			}
		} else {
			nodes = convertSplit(req.Children)
		}
		created, err := d.Stack.Split(r.Context(), r.PathValue("sqid"), nodes, req.Chain, req.CopyPrompt, stack.User)
		if err != nil {
			writeErr(w, err)
			return
		}
		out := []proto.TaskRef{}
		for _, t := range created {
			out = append(out, proto.TaskRef{Sqid: t.Sqid.String, Title: t.Title, State: t.State})
		}
		writeJSON(w, http.StatusCreated, out)
	})
	mux.HandleFunc("POST /api/v1/tasks/{sqid}/queue", func(w http.ResponseWriter, r *http.Request) {
		run, err := d.Plans.QueueWork(r.Context(), r.PathValue("sqid"))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"run_id": run.ID, "branch": run.Branch})
	})
	for _, op := range []string{"complete", "reopen", "cancel"} {
		mux.HandleFunc("POST /api/v1/tasks/{sqid}/"+op, func(w http.ResponseWriter, r *http.Request) {
			var req proto.MessageRequest
			if r.ContentLength > 0 {
				if err := decode(r, &req); err != nil {
					writeErr(w, err)
					return
				}
			}
			var err error
			switch op {
			case "complete":
				err = d.Stack.Complete(r.Context(), r.PathValue("sqid"), req.Message, stack.User)
			case "reopen":
				err = d.Stack.Reopen(r.Context(), r.PathValue("sqid"), stack.User)
			case "cancel":
				err = d.Stack.Cancel(r.Context(), r.PathValue("sqid"), req.Message, stack.User)
			}
			if err != nil {
				writeErr(w, err)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		})
	}
	mux.HandleFunc("DELETE /api/v1/tasks/{sqid}", func(w http.ResponseWriter, r *http.Request) {
		if err := d.Stack.Delete(r.Context(), r.PathValue("sqid"), stack.User); err != nil {
			writeErr(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	// --- questions ---
	mux.HandleFunc("GET /api/v1/questions", func(w http.ResponseWriter, r *http.Request) {
		qs, err := d.Runs.Questions(r.Context(), r.URL.Query().Get("all") == "1")
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, qs)
	})
	qid := func(r *http.Request) (int64, error) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			return 0, fmt.Errorf("%w: question id %q", errs.Invalid, r.PathValue("id"))
		}
		return id, nil
	}
	mux.HandleFunc("GET /api/v1/questions/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := qid(r)
		if err != nil {
			writeErr(w, err)
			return
		}
		q, err := d.Runs.Question(r.Context(), id)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, q)
	})
	mux.HandleFunc("POST /api/v1/questions/{id}/answer", func(w http.ResponseWriter, r *http.Request) {
		id, err := qid(r)
		if err != nil {
			writeErr(w, err)
			return
		}
		var req proto.AnswerRequest
		if err := decode(r, &req); err != nil {
			writeErr(w, err)
			return
		}
		if err := d.Runs.Answer(r.Context(), id, req.Value, req.Text); err != nil {
			writeErr(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /api/v1/runs/{id}/hooks", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			writeErr(w, fmt.Errorf("%w: run id", errs.Invalid))
			return
		}
		evs, err := db.RQ.RunHookEvents(r.Context(), id)
		if err != nil {
			writeErr(w, err)
			return
		}
		out := []map[string]any{}
		for _, e := range evs {
			out = append(out, map[string]any{"seq": e.Seq, "ts": e.Ts, "event": e.Name, "payload": json.RawMessage(e.PayloadJson)})
		}
		writeJSON(w, http.StatusOK, out)
	})

	// --- pull requests ---
	prView := func(x dbgen.ListPRsRow) proto.PRView {
		v := proto.PRView{ID: x.PullRequest.ID, Number: x.PullRequest.GhNumber, URL: x.PullRequest.Url, Repo: x.Repo, Task: x.TaskSqid,
			Title: x.TaskTitle, Branch: x.PullRequest.HeadBranch, State: x.PullRequest.State, Risk: x.PullRequest.Risk.String,
			Decision: x.PullRequest.Decision.String, Checks: x.PullRequest.Checks, Review: x.PullRequest.ReviewSummaryMd}
		if g := x.PullRequest.GatesJson; g != "" && g != "{}" {
			v.Gates = json.RawMessage(g)
		}
		return v
	}
	mux.HandleFunc("GET /api/v1/prs", func(w http.ResponseWriter, r *http.Request) {
		state := r.URL.Query().Get("state")
		if state == "" {
			state = "open"
		}
		rows, err := db.RQ.ListPRs(r.Context(), state)
		if err != nil {
			writeErr(w, err)
			return
		}
		out := []proto.PRView{}
		for _, x := range rows {
			out = append(out, prView(x))
		}
		writeJSON(w, http.StatusOK, out)
	})
	mux.HandleFunc("GET /api/v1/prs/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		rows, err := db.RQ.ListPRs(r.Context(), "all")
		if err != nil {
			writeErr(w, err)
			return
		}
		for _, x := range rows {
			if x.PullRequest.ID == id {
				writeJSON(w, http.StatusOK, prView(x))
				return
			}
		}
		writeErr(w, fmt.Errorf("%w: PR %d", errs.NotFound, id))
	})
	for _, op := range []string{"approve", "request-changes", "close"} {
		mux.HandleFunc("POST /api/v1/prs/{id}/"+op, func(w http.ResponseWriter, r *http.Request) {
			id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
			if err != nil {
				writeErr(w, fmt.Errorf("%w: PR id", errs.Invalid))
				return
			}
			var req proto.MessageRequest
			if r.ContentLength > 0 {
				if err := decode(r, &req); err != nil {
					writeErr(w, err)
					return
				}
			}
			switch op {
			case "approve":
				err = d.PRs.Approve(r.Context(), id)
			case "request-changes":
				err = d.PRs.RequestChanges(r.Context(), id, req.Message)
			case "close":
				err = d.PRs.Close(r.Context(), id)
			}
			if err != nil {
				writeErr(w, err)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		})
	}

	// --- runs ---
	runView := func(x dbgen.ListRunsRow) proto.RunView {
		return proto.RunView{ID: x.Run.ID, Task: x.TaskSqid, TaskTitle: x.TaskTitle, Project: x.ProjectSlug, Kind: x.Run.Kind,
			State: x.Run.State, Runner: x.RunnerName, Current: x.CurrentSqid, Branch: x.Run.Branch, Session: x.Run.SessionName,
			Status: x.Run.StatusText, Pushed: x.Run.Pushed == 1, BaseSHA: x.Run.BaseSha, HeadSHA: x.Run.HeadSha,
			Error: x.Run.Error, QueuedAt: x.Run.QueuedAt, StartedAt: x.Run.StartedAt.String, FinishedAt: x.Run.FinishedAt.String}
	}
	runID := func(r *http.Request) (int64, error) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			return 0, fmt.Errorf("%w: run id %q", errs.Invalid, r.PathValue("id"))
		}
		return id, nil
	}
	mux.HandleFunc("GET /api/v1/runs", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		lim, _ := strconv.ParseInt(q.Get("limit"), 10, 64)
		if lim <= 0 || lim > 500 {
			lim = 50
		}
		rows, err := db.RQ.ListRuns(r.Context(), dbgen.ListRunsParams{State: q.Get("state"), Runner: q.Get("runner"), Project: q.Get("project"), Lim: lim})
		if err != nil {
			writeErr(w, err)
			return
		}
		out := []proto.RunView{}
		for _, x := range rows {
			out = append(out, runView(x))
		}
		writeJSON(w, http.StatusOK, out)
	})
	mux.HandleFunc("GET /api/v1/runs/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := runID(r)
		if err != nil {
			writeErr(w, err)
			return
		}
		rows, err := db.RQ.ListRuns(r.Context(), dbgen.ListRunsParams{Lim: 100000})
		if err != nil {
			writeErr(w, err)
			return
		}
		for _, x := range rows {
			if x.Run.ID == id {
				writeJSON(w, http.StatusOK, runView(x))
				return
			}
		}
		writeErr(w, fmt.Errorf("%w: run %d", errs.NotFound, id))
	})
	mux.HandleFunc("POST /api/v1/runs/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		id, err := runID(r)
		if err != nil {
			writeErr(w, err)
			return
		}
		if err := d.Runs.Cancel(r.Context(), id); err != nil {
			writeErr(w, err)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	})
	for kind, path := range map[string]string{"diff": "diff", "tree": "files", "file": "file"} {
		mux.HandleFunc("GET /api/v1/runs/{id}/"+path, func(w http.ResponseWriter, r *http.Request) {
			id, err := runID(r)
			if err != nil {
				writeErr(w, err)
				return
			}
			out, err := d.Runs.Inspect(r.Context(), id, kind, r.URL.Query().Get("path"), r.URL.Query().Get("stat") == "1")
			if err != nil {
				writeErr(w, err)
				return
			}
			writeJSON(w, http.StatusOK, out)
		})
	}
	mux.HandleFunc("GET /api/v1/runs/{id}/pty", func(w http.ResponseWriter, r *http.Request) {
		id, err := runID(r)
		if err != nil {
			writeErr(w, err)
			return
		}
		streamPTY(w, r, d, id)
	})
	mux.HandleFunc("GET /api/v1/runs/{id}/logs", func(w http.ResponseWriter, r *http.Request) {
		id, err := runID(r)
		if err != nil {
			writeErr(w, err)
			return
		}
		streamLogs(w, r, d, id)
	})
	mux.HandleFunc("GET /api/v1/logs/search", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if strings.TrimSpace(q.Get("q")) == "" {
			writeErr(w, fmt.Errorf("%w: q is required", errs.Invalid))
			return
		}
		lim, _ := strconv.ParseInt(q.Get("limit"), 10, 64)
		if lim <= 0 || lim > 1000 {
			lim = 100
		}
		since, err := sinceParam(q.Get("since"))
		if err != nil {
			writeErr(w, err)
			return
		}
		rows, err := db.RQ.SearchLogsFiltered(r.Context(), dbgen.SearchLogsFilteredParams{Query: q.Get("q"), Project: q.Get("project"), Runner: q.Get("runner"), Since: since, Lim: lim})
		if err != nil {
			if strings.Contains(err.Error(), "fts5") || strings.Contains(err.Error(), "syntax") {
				err = fmt.Errorf("%w: search query: %v", errs.Invalid, err)
			}
			writeErr(w, err)
			return
		}
		out := []proto.LogView{}
		for _, x := range rows {
			out = append(out, proto.LogView{ID: x.ID, RunID: x.RunID, TS: x.Ts, Stream: x.Stream, Message: x.Message,
				Project: x.ProjectSlug, Runner: x.RunnerName, Task: x.TaskSqid})
		}
		writeJSON(w, http.StatusOK, out)
	})
}

func convertSplit(in []proto.SplitNode) []stack.SplitNode {
	out := make([]stack.SplitNode, len(in))
	for i, n := range in {
		out[i] = stack.SplitNode{Title: n.Title, Prompt: n.Prompt, Type: n.Type, Children: convertSplit(n.Children)}
	}
	return out
}

// sinceParam accepts a duration ("15m") or an RFC 3339 time; returns RFC 3339 UTC or "".
func sinceParam(v string) (string, error) {
	if v == "" {
		return "", nil
	}
	if d, err := time.ParseDuration(v); err == nil {
		return time.Now().Add(-d).UTC().Format("2006-01-02T15:04:05.000Z"), nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t.UTC().Format("2006-01-02T15:04:05.000Z"), nil
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
	cons, err := d.JS.OrderedConsumer(ctx, "WR_PTY", occ)
	if err != nil {
		writeErr(w, err)
		return
	}
	if sse {
		streamPTYSSE(w, r, d, runID, cons)
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

// streamPTYSSE sends terminal output as SSE: each event's id is the stream sequence (so a
// reconnect resumes with Last-Event-ID) and data is base64; "end" when the run is over.
func streamPTYSSE(w http.ResponseWriter, r *http.Request, d Deps, runID int64, cons jetstream.Consumer) {
	ctx := r.Context()
	fl := sseStart(w)
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
