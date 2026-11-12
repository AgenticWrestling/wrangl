// Package projecttools serves the project CRUD tools of the owner's remote MCP endpoint
// (project_get, project_create, project_edit, project_archive, project_restore,
// project_delete). They call the same projects.Service the REST API and CLI use, so the
// rules (slug format, template names, active-run refusals) are identical.
package projecttools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/projects"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/runs"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/errs"
)

type Service struct {
	DB       *db.DB
	Projects *projects.Service
}

// Register installs the tools on the runs service.
func (s *Service) Register(r *runs.Service) {
	r.ToolHandlers["project_get"] = s.Get
	r.ToolHandlers["project_create"] = s.Create
	r.ToolHandlers["project_edit"] = s.Edit
	r.ToolHandlers["project_archive"] = s.Archive
	r.ToolHandlers["project_restore"] = s.Restore
	r.ToolHandlers["project_delete"] = s.Delete
}

// args are the arguments of every project tool; pointers distinguish "not given" from "".
type args struct {
	Slug           string            `json:"slug"`
	Confirm        string            `json:"confirm"`
	Name           *string           `json:"name"`
	Description    *string           `json:"description"`
	Template       *string           `json:"template"`
	PlanTemplate   *string           `json:"plan_template"`
	AutoMerge      *bool             `json:"auto_merge"`
	AutoQueue      *bool             `json:"auto_queue"`
	AutoResolve    *bool             `json:"auto_resolve"`
	Priority       *int64            `json:"priority"`
	NotesRepo      *string           `json:"notes_repo"`
	Agent          *string           `json:"agent"`
	CitationAudit  *bool             `json:"citation_audit"`
	AllowTemplates *[]string         `json:"allow_templates"`
	Need           map[string]string `json:"need"`
}

func (a args) params() projects.ProjectParams {
	return projects.ProjectParams{Name: a.Name, Description: a.Description, Template: a.Template,
		PlanTemplate: a.PlanTemplate, AutoMerge: a.AutoMerge, AutoQueue: a.AutoQueue, AutoResolve: a.AutoResolve,
		Priority: a.Priority, NotesRepo: a.NotesRepo, Agent: a.Agent, CitationAudit: a.CitationAudit, AllowTemplates: a.AllowTemplates, Need: a.Need}
}

func parse(raw json.RawMessage) (args, error) {
	var a args
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &a); err != nil {
			return a, fmt.Errorf("%w: arguments: %v", errs.Invalid, err)
		}
	}
	if a.Slug == "" {
		return a, fmt.Errorf("%w: slug is required", errs.Invalid)
	}
	return a, nil
}

func (s *Service) tmpl(ctx context.Context, id int64, ok bool) string {
	if !ok {
		return ""
	}
	if t, err := s.DB.RQ.GetTemplate(ctx, id); err == nil {
		return t.Name
	}
	return ""
}

// View is the full description of a project, as project_get returns it.
func (s *Service) View(ctx context.Context, p dbgen.Project) map[string]any {
	m := map[string]any{"slug": p.Slug, "name": p.Name, "description": p.Description, "priority": p.Priority,
		"auto_merge": p.AutoMerge == 1, "auto_queue": p.AutoQueue == 1, "archived": p.ArchivedAt.Valid,
		"template":      s.tmpl(ctx, p.DefaultTemplateID.Int64, p.DefaultTemplateID.Valid),
		"plan_template": s.tmpl(ctx, p.PlanTemplateID.Int64, p.PlanTemplateID.Valid)}
	if ar, err := s.DB.RQ.GetProjectAutoResolve(ctx, p.ID); err == nil {
		m["auto_resolve"] = ar == 1
	}
	if ca, err := s.DB.RQ.GetProjectCitationAudit(ctx, p.ID); err == nil {
		m["citation_audit"] = ca == 1
	}
	if ag := s.Projects.DefaultAgent(ctx, p.ID); ag != "" {
		m["agent"] = ag
	}
	if id, err := s.DB.RQ.GetNotesRepoID(ctx, p.ID); err == nil {
		if rp, err := s.DB.RQ.GetRepo(ctx, id); err == nil {
			m["notes_repo"] = rp.Owner + "/" + rp.Name
		}
	}
	m["allow_templates"] = s.Projects.AllowedTemplates(ctx, p.ID)
	repos := []string{}
	if rows, err := s.DB.RQ.ListRepos(ctx, p.ID); err == nil {
		for _, x := range rows {
			repos = append(repos, x.Repo.Owner+"/"+x.Repo.Name)
		}
	}
	m["repos"] = repos
	return m
}

func (s *Service) Get(ctx context.Context, _ runs.Scope, raw json.RawMessage) (any, error) {
	a, err := parse(raw)
	if err != nil {
		return nil, err
	}
	p, err := s.Projects.Project(ctx, a.Slug)
	if err != nil {
		return nil, err
	}
	return s.View(ctx, p), nil
}

func (s *Service) Create(ctx context.Context, _ runs.Scope, raw json.RawMessage) (any, error) {
	a, err := parse(raw)
	if err != nil {
		return nil, err
	}
	p, err := s.Projects.Create(ctx, a.Slug, a.params())
	if err != nil {
		return nil, err
	}
	return s.View(ctx, p), nil
}

func (s *Service) Edit(ctx context.Context, _ runs.Scope, raw json.RawMessage) (any, error) {
	a, err := parse(raw)
	if err != nil {
		return nil, err
	}
	p, err := s.Projects.Update(ctx, a.Slug, a.params())
	if err != nil {
		return nil, err
	}
	return s.View(ctx, p), nil
}

func (s *Service) Archive(ctx context.Context, _ runs.Scope, raw json.RawMessage) (any, error) {
	a, err := parse(raw)
	if err != nil {
		return nil, err
	}
	p, cancelled, err := s.Projects.Archive(ctx, a.Slug)
	if err != nil {
		return nil, err // a project with active runs is refused; the message lists them
	}
	if cancelled == nil {
		cancelled = []int64{}
	}
	return map[string]any{"project": s.View(ctx, p), "cancelled_runs": cancelled}, nil
}

func (s *Service) Restore(ctx context.Context, _ runs.Scope, raw json.RawMessage) (any, error) {
	a, err := parse(raw)
	if err != nil {
		return nil, err
	}
	p, err := s.Projects.Restore(ctx, a.Slug)
	if err != nil {
		return nil, err
	}
	return s.View(ctx, p), nil
}

// Delete is irreversible, so the caller must repeat the slug in confirm.
func (s *Service) Delete(ctx context.Context, _ runs.Scope, raw json.RawMessage) (any, error) {
	a, err := parse(raw)
	if err != nil {
		return nil, err
	}
	if a.Confirm != a.Slug {
		return nil, fmt.Errorf("%w: deleting %s removes the project and all its tasks for good; pass confirm=%q to proceed (project_archive is reversible)", errs.Invalid, a.Slug, a.Slug)
	}
	if err := s.Projects.Delete(ctx, a.Slug); err != nil {
		return nil, err
	}
	return map[string]any{"deleted": a.Slug}, nil
}
