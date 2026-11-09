package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/issuestore"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/projects"
	dbpkg "github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/errs"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

func templateName(ctx context.Context, d Deps, id sql.NullInt64) string {
	if !id.Valid {
		return ""
	}
	t, err := d.Fleet.DB.RQ.GetTemplate(ctx, id.Int64)
	if err != nil {
		return ""
	}
	return t.Name
}

func projectView(ctx context.Context, d Deps, p dbgen.Project) proto.ProjectView {
	db := d.Fleet.DB
	notes := ""
	if id, err := db.RQ.GetNotesRepoID(ctx, p.ID); err == nil {
		if rp, err := db.RQ.GetRepo(ctx, id); err == nil {
			notes = rp.Owner + "/" + rp.Name
		}
	}
	ca, _ := db.RQ.GetProjectCitationAudit(ctx, p.ID)
	ar, _ := db.RQ.GetProjectAutoResolve(ctx, p.ID)
	return proto.ProjectView{AutoResolve: ar == 1, CitationAudit: ca == 1, Agent: d.Projects.DefaultAgent(ctx, p.ID), AllowTemplates: d.Projects.AllowedTemplates(ctx, p.ID), Need: needView(ctx, d, p.ID, 0), NotesRepo: notes, Slug: p.Slug, Name: p.Name, Description: p.Description,
		Template: templateName(ctx, d, p.DefaultTemplateID), PlanTemplate: templateName(ctx, d, p.PlanTemplateID),
		AutoMerge: p.AutoMerge == 1, AutoQueue: p.AutoQueue == 1, Priority: p.Priority,
		Archived: p.ArchivedAt.Valid, ArchivedAt: p.ArchivedAt.String}
}

func projectParams(r proto.ProjectRequest) projects.ProjectParams {
	return projects.ProjectParams{Name: r.Name, Description: r.Description, Template: r.Template,
		PlanTemplate: r.PlanTemplate, AutoMerge: r.AutoMerge, AutoQueue: r.AutoQueue, AutoResolve: r.AutoResolve, Priority: r.Priority, NotesRepo: r.NotesRepo, Agent: r.Agent, CitationAudit: r.CitationAudit, AllowTemplates: r.AllowTemplates, Need: r.Need}
}

// projectIDFromQuery resolves the optional ?project=slug filter (0 = all projects).
func projectIDFromQuery(r *http.Request, d Deps) (int64, error) {
	slug := r.URL.Query().Get("project")
	if slug == "" {
		return 0, nil
	}
	p, err := d.Projects.Project(r.Context(), slug)
	return p.ID, err
}

func registerProjects(mux *http.ServeMux, d Deps) {
	db := d.Fleet.DB
	mux.HandleFunc("GET /api/v1/projects", getJSON(func(r *http.Request) ([]proto.ProjectView, error) {
		filter := r.URL.Query().Get("archived") // "", "active", "archived" or "all"
		switch filter {
		case "", "active":
			filter = "active"
		case "archived", "all":
		default:
			return nil, fmt.Errorf("%w: archived must be active, archived or all", errs.Invalid)
		}
		ps, err := db.RQ.ListProjects(r.Context(), filter)
		if err != nil {
			return nil, err
		}
		out := []proto.ProjectView{}
		for _, p := range ps {
			out = append(out, projectView(r.Context(), d, p))
		}
		return out, nil
	}))
	mux.HandleFunc("POST /api/v1/projects", handle(http.StatusCreated, func(r *http.Request, req proto.ProjectRequest) (proto.ProjectView, error) {
		p, err := d.Projects.Create(r.Context(), req.Slug, projectParams(req))
		if err != nil {
			return proto.ProjectView{}, err
		}
		return projectView(r.Context(), d, p), nil
	}))
	mux.HandleFunc("GET /api/v1/projects/{slug}", getJSON(func(r *http.Request) (proto.ProjectView, error) {
		p, err := d.Projects.Project(r.Context(), r.PathValue("slug"))
		if err != nil {
			return proto.ProjectView{}, err
		}
		return projectView(r.Context(), d, p), nil
	}))
	mux.HandleFunc("GET /api/v1/projects/{slug}/runners", getJSON(func(r *http.Request) ([]proto.ProjectRunner, error) {
		return d.Projects.Runners(r.Context(), r.PathValue("slug"))
	}))
	mux.HandleFunc("PATCH /api/v1/projects/{slug}", handle(http.StatusOK, func(r *http.Request, req proto.ProjectRequest) (proto.ProjectView, error) {
		p, err := d.Projects.Update(r.Context(), r.PathValue("slug"), projectParams(req))
		if err != nil {
			return proto.ProjectView{}, err
		}
		return projectView(r.Context(), d, p), nil
	}))
	mux.HandleFunc("POST /api/v1/projects/{slug}/archive", func(w http.ResponseWriter, r *http.Request) {
		p, cancelled, err := d.Projects.Archive(r.Context(), r.PathValue("slug"))
		var are *dbpkg.ActiveRunsError
		if errors.As(err, &are) { // the refusal lists the blocking runs, so it is not a plain error body
			ref := proto.ProjectArchiveRefusal{Error: are.Error(), Runs: []proto.ProjectArchiveRunView{}}
			for _, run := range are.Runs {
				ref.Runs = append(ref.Runs, proto.ProjectArchiveRunView{ID: run.ID, State: run.State, Task: run.TaskSqid, Title: run.TaskTitle})
			}
			writeJSON(w, http.StatusConflict, ref)
			return
		}
		if err != nil {
			writeErr(w, err)
			return
		}
		if cancelled == nil {
			cancelled = []int64{}
		}
		writeJSON(w, http.StatusOK, proto.ProjectArchiveResult{Project: projectView(r.Context(), d, p), CancelledRuns: cancelled})
	})
	mux.HandleFunc("POST /api/v1/projects/{slug}/restore", getJSON(func(r *http.Request) (proto.ProjectView, error) {
		p, err := d.Projects.Restore(r.Context(), r.PathValue("slug"))
		if err != nil {
			return proto.ProjectView{}, err
		}
		return projectView(r.Context(), d, p), nil
	}))
	mux.HandleFunc("DELETE /api/v1/projects/{slug}", ack(http.StatusNoContent, func(r *http.Request) error {
		return d.Projects.Delete(r.Context(), r.PathValue("slug"))
	}))

	// --- repos ---
	mux.HandleFunc("GET /api/v1/repos", getJSON(func(r *http.Request) ([]proto.RepoView, error) {
		pid, err := projectIDFromQuery(r, d)
		if err != nil {
			return nil, err
		}
		rows, err := db.RQ.ListRepos(r.Context(), pid)
		if err != nil {
			return nil, err
		}
		out := []proto.RepoView{}
		for _, x := range rows {
			out = append(out, proto.RepoView{Project: x.ProjectSlug, Repo: x.Repo.Owner + "/" + x.Repo.Name,
				DefaultBranch: x.Repo.DefaultBranch, CloneURL: x.Repo.CloneUrl})
		}
		return out, nil
	}))
	mux.HandleFunc("POST /api/v1/projects/{slug}/repos", handle(http.StatusCreated, func(r *http.Request, req proto.LinkRepoRequest) (proto.RepoView, error) {
		x, err := d.Projects.LinkRepo(r.Context(), r.PathValue("slug"), req.Repo, req.Branch)
		if err != nil {
			return proto.RepoView{}, err
		}
		return proto.RepoView{Project: r.PathValue("slug"), Repo: x.Owner + "/" + x.Name,
			DefaultBranch: x.DefaultBranch, CloneURL: x.CloneUrl}, nil
	}))
	mux.HandleFunc("DELETE /api/v1/projects/{slug}/repos/{owner}/{name}", ack(http.StatusNoContent, func(r *http.Request) error {
		return d.Projects.UnlinkRepo(r.Context(), r.PathValue("slug"), r.PathValue("owner")+"/"+r.PathValue("name"))
	}))

	// --- issues ---
	mux.HandleFunc("POST /api/v1/projects/{slug}/issues/sync", getJSON(func(r *http.Request) (map[string]int, error) {
		n, err := d.Projects.SyncIssues(r.Context(), r.PathValue("slug"))
		if err != nil {
			return nil, err
		}
		return map[string]int{"synced": n}, nil
	}))
	mux.HandleFunc("GET /api/v1/projects/{slug}/issues/drift", getJSON(func(r *http.Request) ([]issuestore.Drift, error) {
		return d.Projects.IssueDrift(r.Context(), r.PathValue("slug"), r.URL.Query().Get("repair") == "1")
	}))
	mux.HandleFunc("GET /api/v1/issues", getJSON(func(r *http.Request) ([]proto.IssueView, error) {
		pid, err := projectIDFromQuery(r, d)
		if err != nil {
			return nil, err
		}
		state := r.URL.Query().Get("state")
		if state == "" {
			state = "open"
		}
		rows, err := db.RQ.ListIssues(r.Context(), dbgen.ListIssuesParams{ProjectID: pid, State: state})
		if err != nil {
			return nil, err
		}
		out := []proto.IssueView{}
		for _, x := range rows {
			out = append(out, proto.IssueView{Ref: fmt.Sprintf("%s/%s#%d", x.RepoOwner, x.RepoName, x.Issue.GhNumber),
				Title: x.Issue.Title, State: x.Issue.State, URL: x.Issue.Url, Task: x.TaskSqid})
		}
		return out, nil
	}))
	issueRef := func(r *http.Request) string {
		return r.PathValue("owner") + "/" + r.PathValue("name") + "#" + r.PathValue("number")
	}
	mux.HandleFunc("GET /api/v1/projects/{slug}/issues/{owner}/{name}/{number}", getJSON(func(r *http.Request) (proto.IssueView, error) {
		i, err := d.Projects.Issue(r.Context(), r.PathValue("slug"), issueRef(r))
		if err != nil {
			return proto.IssueView{}, err
		}
		return proto.IssueView{Ref: issueRef(r), Title: i.Title, State: i.State, URL: i.Url, Body: i.Body}, nil
	}))
	type issueTaskRequest struct {
		Parent   string `json:"parent,omitempty"`
		Template string `json:"template,omitempty"`
	}
	mux.HandleFunc("POST /api/v1/projects/{slug}/issues/{owner}/{name}/{number}/task", handle(http.StatusCreated, func(r *http.Request, req issueTaskRequest) (proto.TaskRef, error) {
		t, err := d.Projects.IssueToTask(r.Context(), r.PathValue("slug"), issueRef(r), req.Parent, req.Template)
		if err != nil {
			return proto.TaskRef{}, err
		}
		return taskRef(t), nil
	}))
}
