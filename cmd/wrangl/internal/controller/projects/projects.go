// Package projects manages projects, their linked GitHub repos and synced issues.
package projects

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/gh"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/issuestore"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/errs"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/placement"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/stack"
)

// Service manages projects, repos and issues.
type Service struct {
	DB    *db.DB
	GH    gh.API // nil when no GitHub App is configured
	Stack *stack.Service
	Now   func() time.Time
}

// ErrNoGitHub means no GitHub App is configured on this controller.
var ErrNoGitHub = fmt.Errorf("%w: GitHub is not configured (github.author_app in controller.yaml)", errs.Invalid)

var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

// ProjectParams create or update a project. Nil pointers leave fields unchanged on update.
type ProjectParams struct {
	Name, Description      *string
	Template, PlanTemplate *string // template names; "" clears
	AutoMerge, AutoQueue   *bool
	AutoResolve            *bool // auto-merge queues Smart Merge for conflicted PRs
	Priority               *int64
	ReadRepos              *string           // all | none: read-only checkouts of the other linked repos in runs
	DefaultRepo            *string           // owner/name of a linked repo tasks resolve to when none is set; "" clears
	NotesRepo              *string           // owner/name of a linked repo that holds research reports; "" clears
	Agent                  *string           // default agent definition by name; "" clears
	CitationAudit          *bool             // audit research PRs' citations before merge
	Need                   map[string]string // resource needs by field (vcpu, ram, disk); empty value clears
	Require, Unrequire     []string          // capability requirements to add (arch=arm64) / remove (by key)
	AllowTemplates         *[]string         // other templates this project's runs may use (the host); empty clears
}

func (s *Service) templateID(ctx context.Context, name string) (sql.NullInt64, error) {
	if name == "" {
		return sql.NullInt64{}, nil
	}
	t, err := s.DB.RQ.GetTemplateByName(ctx, name)
	if errors.Is(err, sql.ErrNoRows) {
		return sql.NullInt64{}, fmt.Errorf("%w: template %q", errs.NotFound, name)
	}
	return sql.NullInt64{Int64: t.ID, Valid: true}, err
}

// Project returns a project by slug.
func (s *Service) Project(ctx context.Context, slug string) (dbgen.Project, error) {
	p, err := s.DB.RQ.GetProjectBySlug(ctx, slug)
	if errors.Is(err, sql.ErrNoRows) {
		return p, fmt.Errorf("%w: project %q", errs.NotFound, slug)
	}
	return p, err
}

// activeProject is Project for a mutation: an archived project is read-only until restored.
func (s *Service) activeProject(ctx context.Context, slug string) (dbgen.Project, error) {
	p, err := s.Project(ctx, slug)
	if err != nil {
		return p, err
	}
	return p, stack.ActiveProject(ctx, s.DB.RQ, p.ID)
}

// Create adds a project.
func (s *Service) Create(ctx context.Context, slug string, p ProjectParams) (dbgen.Project, error) {
	if !slugRe.MatchString(slug) {
		return dbgen.Project{}, fmt.Errorf("%w: project slug %q: lowercase letters, digits and '-'", errs.Invalid, slug)
	}
	name := slug
	if p.Name != nil && *p.Name != "" {
		name = *p.Name
	}
	desc := ""
	if p.Description != nil {
		desc = *p.Description
	}
	var tmpl sql.NullInt64
	if p.Template != nil {
		var err error
		if tmpl, err = s.templateID(ctx, *p.Template); err != nil {
			return dbgen.Project{}, err
		}
	}
	created, err := s.DB.Q.CreateProject(ctx, dbgen.CreateProjectParams{Slug: slug, Name: name, Description: desc, DefaultTemplateID: tmpl})
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return created, fmt.Errorf("%w: project %q exists", errs.Conflict, slug)
		}
		return created, err
	}
	if p.CitationAudit != nil || p.Agent != nil || p.NotesRepo != nil || p.DefaultRepo != nil || p.ReadRepos != nil || p.PlanTemplate != nil || p.AutoMerge != nil || p.AutoQueue != nil || p.AutoResolve != nil || p.Priority != nil || p.AllowTemplates != nil || len(p.Need) > 0 || len(p.Require) > 0 || len(p.Unrequire) > 0 {
		p.Template = nil // already set
		return s.Update(ctx, slug, p)
	}
	return created, nil
}

func b2i(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// Update changes the given fields.
func (s *Service) Update(ctx context.Context, slug string, p ProjectParams) (dbgen.Project, error) {
	cur, err := s.activeProject(ctx, slug)
	if err != nil {
		return cur, err
	}
	up := dbgen.UpdateProjectParams{
		Name: cur.Name, Description: cur.Description, DefaultTemplateID: cur.DefaultTemplateID,
		PlanTemplateID: cur.PlanTemplateID, AutoMerge: cur.AutoMerge, AutoQueue: cur.AutoQueue,
		Priority: cur.Priority, ID: cur.ID,
	}
	if p.Name != nil {
		up.Name = *p.Name
	}
	if p.Description != nil {
		up.Description = *p.Description
	}
	if p.Template != nil {
		if up.DefaultTemplateID, err = s.templateID(ctx, *p.Template); err != nil {
			return cur, err
		}
	}
	if p.PlanTemplate != nil {
		if up.PlanTemplateID, err = s.templateID(ctx, *p.PlanTemplate); err != nil {
			return cur, err
		}
	}
	if p.AutoMerge != nil {
		up.AutoMerge = b2i(*p.AutoMerge)
	}
	if p.AutoQueue != nil {
		up.AutoQueue = b2i(*p.AutoQueue)
	}
	if p.Priority != nil {
		up.Priority = *p.Priority
	}
	out, err := s.DB.Q.UpdateProject(ctx, up)
	if err == nil && p.AutoResolve != nil {
		err = s.DB.Q.SetProjectAutoResolve(ctx, b2i(*p.AutoResolve), cur.ID)
	}
	if err == nil && p.CitationAudit != nil {
		err = s.DB.Q.SetProjectCitationAudit(ctx, b2i(*p.CitationAudit), cur.ID)
	}
	if err == nil && p.Agent != nil {
		err = s.SetDefaultAgent(ctx, slug, *p.Agent)
	}
	if err == nil && p.NotesRepo != nil {
		err = s.SetNotesRepo(ctx, slug, *p.NotesRepo)
	}
	if err == nil && p.ReadRepos != nil {
		if *p.ReadRepos != "all" && *p.ReadRepos != "none" {
			return cur, fmt.Errorf("%w: read_repos is all or none", errs.Invalid)
		}
		err = s.DB.Q.SetProjectReadRepos(ctx, dbgen.SetProjectReadReposParams{ReadRepos: *p.ReadRepos, ID: cur.ID})
	}
	if err == nil && p.DefaultRepo != nil {
		err = s.SetDefaultRepo(ctx, slug, *p.DefaultRepo)
	}
	if err == nil && len(p.Need) > 0 {
		_, err = (&placement.Service{DB: s.DB}).SetProject(ctx, cur.ID, p.Need)
	}
	if err == nil && (len(p.Require) > 0 || len(p.Unrequire) > 0) {
		err = (&placement.Service{DB: s.DB}).EditRequirements(ctx, placement.OwnerProject, cur.ID, p.Require, p.Unrequire)
	}
	if err == nil && p.AllowTemplates != nil {
		err = s.SetAllowedTemplates(ctx, slug, *p.AllowTemplates)
	}
	return out, err
}

// SetAllowedTemplates replaces the templates (besides each run's own) the project's runs may use.
func (s *Service) SetAllowedTemplates(ctx context.Context, slug string, names []string) error {
	p, err := s.activeProject(ctx, slug)
	if err != nil {
		return err
	}
	ids := []int64{}
	for _, name := range names {
		if name == "" {
			continue
		}
		t, err := s.DB.RQ.GetTemplateByName(ctx, name)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: template %q", errs.NotFound, name)
		} else if err != nil {
			return err
		}
		ids = append(ids, t.ID)
	}
	if err := s.DB.Q.ClearAllowedTemplates(ctx, p.ID); err != nil {
		return err
	}
	for _, id := range ids {
		if err := s.DB.Q.AddAllowedTemplate(ctx, dbgen.AddAllowedTemplateParams{ProjectID: p.ID, TemplateID: id}); err != nil {
			return err
		}
	}
	return nil
}

// AllowedTemplates names the templates the project may use besides a run's own.
func (s *Service) AllowedTemplates(ctx context.Context, projectID int64) []string {
	rows, _ := s.DB.RQ.ListAllowedTemplates(ctx, projectID)
	out := []string{}
	for _, r := range rows {
		out = append(out, r.Name)
	}
	return out
}

// SetDefaultAgent sets (or with "" clears) the project's default agent definition.
func (s *Service) SetDefaultAgent(ctx context.Context, slug, name string) error {
	p, err := s.Project(ctx, slug)
	if err != nil {
		return err
	}
	var id sql.NullInt64
	if name != "" {
		a, err := s.DB.RQ.GetAgentByName(ctx, name)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: agent %q (see `wrangl agent list`)", errs.NotFound, name)
		} else if err != nil {
			return err
		}
		id = sql.NullInt64{Int64: a.ID, Valid: true}
	}
	return s.DB.Q.SetProjectDefaultAgent(ctx, dbgen.SetProjectDefaultAgentParams{AgentID: id, ID: p.ID})
}

// DefaultAgent names the project's default agent ("" = none).
func (s *Service) DefaultAgent(ctx context.Context, projectID int64) string {
	id, err := s.DB.RQ.GetProjectDefaultAgent(ctx, projectID)
	if err != nil || !id.Valid {
		return ""
	}
	a, _ := s.DB.RQ.GetAgent(ctx, id.Int64)
	return a.Name
}

// SetNotesRepo gives one of the project's linked repos the "notes" role ("" clears it).
// Research tasks then run in that repo and write their reports under research/.
func (s *Service) SetNotesRepo(ctx context.Context, slug, ref string) error {
	p, err := s.activeProject(ctx, slug)
	if err != nil {
		return err
	}
	if ref == "" {
		return s.DB.Q.ClearNotesRepo(ctx, p.ID)
	}
	owner, name, err := splitRepo(ref)
	if err != nil {
		return err
	}
	r, err := s.DB.RQ.GetProjectRepo(ctx, dbgen.GetProjectRepoParams{ProjectID: p.ID, Owner: owner, Name: name})
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: link %s to %s first", errs.Invalid, ref, slug)
	} else if err != nil {
		return err
	}
	return s.DB.Q.SetNotesRepo(ctx, dbgen.SetNotesRepoParams{ProjectID: p.ID, RepoID: r.ID})
}

// SetDefaultRepo makes one of the project's linked repos the one tasks resolve to when neither they nor
// an ancestor name a repo ("" clears it; ADR 0012 section 3).
func (s *Service) SetDefaultRepo(ctx context.Context, slug, ref string) error {
	p, err := s.activeProject(ctx, slug)
	if err != nil {
		return err
	}
	var id sql.NullInt64
	if ref != "" {
		owner, name, err := splitRepo(ref)
		if err != nil {
			return err
		}
		r, err := s.DB.RQ.GetProjectRepo(ctx, dbgen.GetProjectRepoParams{ProjectID: p.ID, Owner: owner, Name: name})
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: link %s to %s first", errs.Invalid, ref, slug)
		} else if err != nil {
			return err
		}
		id = sql.NullInt64{Int64: r.ID, Valid: true}
	}
	return s.DB.Q.SetProjectDefaultRepo(ctx, dbgen.SetProjectDefaultRepoParams{DefaultRepoID: id, ID: p.ID})
}

// Archive hides a project and makes it inert. It returns *db.ActiveRunsError when runs are
// active, and otherwise the ids of the queued runs it cancelled. Archiving twice is a conflict.
func (s *Service) Archive(ctx context.Context, slug string) (dbgen.Project, []int64, error) {
	p, err := s.Project(ctx, slug)
	if err != nil {
		return p, nil, err
	}
	if p.ArchivedAt.Valid {
		return p, nil, fmt.Errorf("%w: project %s is already archived", errs.Conflict, slug)
	}
	p, cancelled, err := s.DB.ArchiveProject(ctx, p.ID)
	if errors.Is(err, sql.ErrNoRows) {
		err = fmt.Errorf("%w: project %s is already archived", errs.Conflict, slug)
	}
	return p, cancelled, err
}

// Restore brings an archived project back.
func (s *Service) Restore(ctx context.Context, slug string) (dbgen.Project, error) {
	p, err := s.Project(ctx, slug)
	if err != nil {
		return p, err
	}
	if !p.ArchivedAt.Valid {
		return p, fmt.Errorf("%w: project %s is not archived", errs.Conflict, slug)
	}
	return s.DB.Q.RestoreProject(ctx, p.ID)
}

// Delete removes a project (and its tasks) unless a run is active on it.
func (s *Service) Delete(ctx context.Context, slug string) error {
	p, err := s.Project(ctx, slug)
	if err != nil {
		return err
	}
	active, err := s.DB.RQ.ListRuns(ctx, dbgen.ListRunsParams{State: "active", Project: slug, Lim: 1})
	if err != nil {
		return err
	}
	if len(active) > 0 {
		return fmt.Errorf("%w: project %s has an active run (cancel it first)", errs.Conflict, slug)
	}
	_, err = s.DB.Q.DeleteProject(ctx, p.ID)
	return err
}

func splitRepo(ref string) (string, string, error) {
	owner, name, ok := strings.Cut(ref, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return "", "", fmt.Errorf("%w: repo %q: want owner/name", errs.Invalid, ref)
	}
	return owner, strings.TrimSuffix(name, ".git"), nil
}

// LinkRepo links a GitHub repo (which the author App must be installed on) to a project.
func (s *Service) LinkRepo(ctx context.Context, slug, ref, branch string) (dbgen.Repo, error) {
	if s.GH == nil {
		return dbgen.Repo{}, ErrNoGitHub
	}
	p, err := s.activeProject(ctx, slug)
	if err != nil {
		return dbgen.Repo{}, err
	}
	owner, name, err := splitRepo(ref)
	if err != nil {
		return dbgen.Repo{}, err
	}
	r, err := s.GH.Repo(ctx, owner, name)
	var noApp *gh.NoAppError
	if errors.Is(err, gh.ErrNotInstalled) || errors.As(err, &noApp) {
		// %w twice keeps the typed error for the web app, which links to the fix
		return dbgen.Repo{}, fmt.Errorf("%w: %w", errs.Invalid, err)
	} else if err != nil {
		return dbgen.Repo{}, err
	}
	if branch == "" {
		branch = r.DefaultBranch
	}
	out, err := s.DB.Q.CreateRepo(ctx, dbgen.CreateRepoParams{
		ProjectID: p.ID, Owner: owner, Name: name, DefaultBranch: branch,
		GhInstallationID: sql.NullInt64{Int64: r.InstallationID, Valid: true}, CloneUrl: r.CloneURL,
	})
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return out, fmt.Errorf("%w: %s is already linked to %s", errs.Conflict, ref, slug)
	}
	return out, err
}

// SetRepoNote sets the one-line note on one of the project's linked repos ("" clears it).
func (s *Service) SetRepoNote(ctx context.Context, slug, ref, note string) error {
	p, err := s.activeProject(ctx, slug)
	if err != nil {
		return err
	}
	owner, name, err := splitRepo(ref)
	if err != nil {
		return err
	}
	r, err := s.DB.RQ.GetProjectRepo(ctx, dbgen.GetProjectRepoParams{ProjectID: p.ID, Owner: owner, Name: name})
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: link %s to %s first", errs.Invalid, ref, slug)
	} else if err != nil {
		return err
	}
	note = strings.TrimSpace(strings.ReplaceAll(note, "\n", " "))
	if len(note) > 200 {
		return fmt.Errorf("%w: a repo note is one line, up to 200 characters", errs.Invalid)
	}
	return s.DB.Q.SetRepoNote(ctx, dbgen.SetRepoNoteParams{Note: note, ID: r.ID})
}

// UnlinkRepo removes a repo link (tasks pointing at it keep their history).
func (s *Service) UnlinkRepo(ctx context.Context, slug, ref string) error {
	p, err := s.activeProject(ctx, slug)
	if err != nil {
		return err
	}
	owner, name, err := splitRepo(ref)
	if err != nil {
		return err
	}
	r, err := s.DB.RQ.GetProjectRepo(ctx, dbgen.GetProjectRepoParams{ProjectID: p.ID, Owner: owner, Name: name})
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %s is not linked to %s", errs.NotFound, ref, slug)
	} else if err != nil {
		return err
	}
	if _, err := s.DB.Q.DeleteRepo(ctx, r.ID); err != nil {
		if strings.Contains(err.Error(), "FOREIGN KEY") {
			return fmt.Errorf("%w: tasks or runs still use %s", errs.Conflict, ref)
		}
		return err
	}
	return nil
}

// ghRepo rebuilds the gh.Repo for a stored repo.
func ghRepo(r dbgen.Repo) gh.Repo {
	return gh.Repo{Owner: r.Owner, Name: r.Name, DefaultBranch: r.DefaultBranch, CloneURL: r.CloneUrl, InstallationID: r.GhInstallationID.Int64}
}

// GHRepo is exported for the scheduler (token minting at assignment).
func GHRepo(r dbgen.Repo) gh.Repo { return ghRepo(r) }

// SyncIssues pulls open and closed issues for every repo of a project.
func (s *Service) SyncIssues(ctx context.Context, slug string) (int, error) {
	if s.GH == nil {
		return 0, ErrNoGitHub
	}
	p, err := s.activeProject(ctx, slug)
	if err != nil {
		return 0, err
	}
	repos, err := s.DB.RQ.ListRepos(ctx, p.ID)
	if err != nil {
		return 0, err
	}
	n := 0
	now := s.Now()
	for _, r := range repos {
		issues, err := s.GH.Issues(ctx, ghRepo(r.Repo), "all")
		if err != nil {
			return n, fmt.Errorf("%s/%s: %w", r.Repo.Owner, r.Repo.Name, err)
		}
		for _, i := range issues {
			// Only-newer-wins, so a sync never overwrites a fresher webhook or live read.
			if _, _, err := issuestore.Save(ctx, s.DB, r.Repo.ID, i, now); err != nil {
				return n, err
			}
			n++
		}
	}
	return n, nil
}

// IssueRef is owner/name#number.
func parseIssueRef(ref string) (owner, name string, num int64, err error) {
	repo, n, ok := strings.Cut(ref, "#")
	if ok {
		owner, name, err = splitRepo(repo)
		if err == nil {
			num, err = strconv.ParseInt(n, 10, 64)
		}
	}
	if !ok || err != nil || num <= 0 {
		return "", "", 0, fmt.Errorf("%w: issue %q: want owner/name#number", errs.Invalid, ref)
	}
	return owner, name, num, nil
}

// Issue finds a synced issue in a project.
func (s *Service) Issue(ctx context.Context, slug, ref string) (dbgen.Issue, error) {
	p, err := s.Project(ctx, slug)
	if err != nil {
		return dbgen.Issue{}, err
	}
	owner, name, num, err := parseIssueRef(ref)
	if err != nil {
		return dbgen.Issue{}, err
	}
	i, err := s.DB.RQ.GetIssueByNumber(ctx, dbgen.GetIssueByNumberParams{ProjectID: p.ID, Owner: owner, Name: name, Number: num})
	if errors.Is(err, sql.ErrNoRows) {
		return i, fmt.Errorf("%w: issue %s in %s (run wrangl issue sync %s)", errs.NotFound, ref, slug, slug)
	}
	return i, err
}

// IssueToTask creates a task from an issue, linked to it and to its repo.
func (s *Service) IssueToTask(ctx context.Context, slug, ref, parent, template string) (dbgen.Task, error) {
	if _, err := s.activeProject(ctx, slug); err != nil {
		return dbgen.Task{}, err
	}
	i, err := s.Issue(ctx, slug, ref)
	if err != nil {
		return dbgen.Task{}, err
	}
	owner, name, num, _ := parseIssueRef(ref)
	prompt := fmt.Sprintf("Resolve GitHub issue %s/%s#%d (%s).\n\n%s", owner, name, num, i.Url, strings.TrimSpace(i.Body))
	return s.Stack.Create(ctx, stack.CreateParams{
		Project: slug, Parent: parent, Title: fmt.Sprintf("#%d %s", num, i.Title), Prompt: prompt,
		Repo: owner + "/" + name, Template: template, Issue: ref, Actor: stack.User,
	})
}

// Runners lists every runner that has an active session (run) for the project, with those sessions
// and the other projects each runner is serving at the same time. Runners are not owned by projects
// and there can be many, so nothing here assumes one.
func (s *Service) Runners(ctx context.Context, slug string) ([]proto.ProjectRunner, error) {
	p, err := s.Project(ctx, slug)
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.R.QueryContext(ctx, `
SELECT ru.name, ru.state, COALESCE(tp.name, ''), r.id, COALESCE(t.sqid, ''), t.title, r.state,
       (SELECT group_concat(DISTINCT p2.slug) FROM run r2 JOIN task t2 ON t2.id = r2.task_id JOIN project p2 ON p2.id = t2.project_id
         WHERE r2.runner_id = ru.id AND p2.id <> p.id AND r2.state IN ('assigned','preparing','running','needs_input','blocked','pushing'))
FROM run r JOIN task t ON t.id = r.task_id JOIN project p ON p.id = t.project_id
JOIN runner ru ON ru.id = r.runner_id LEFT JOIN runner_template tp ON tp.id = ru.template_id
WHERE p.id = ? AND r.state IN ('assigned','preparing','running','needs_input','blocked','pushing')
ORDER BY ru.name, r.id`, p.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []proto.ProjectRunner{}
	for rows.Next() {
		var name, rstate, tmpl, sqid, title, runState string
		var runID int64
		var also sql.NullString
		if err := rows.Scan(&name, &rstate, &tmpl, &runID, &sqid, &title, &runState, &also); err != nil {
			return nil, err
		}
		if n := len(out); n == 0 || out[n-1].Name != name {
			pr := proto.ProjectRunner{Name: name, State: rstate, Template: tmpl, Sessions: []proto.ProjectSession{}}
			if also.Valid && also.String != "" {
				pr.AlsoServing = strings.Split(also.String, ",")
			}
			out = append(out, pr)
		}
		last := &out[len(out)-1]
		last.Sessions = append(last.Sessions, proto.ProjectSession{Run: runID, Task: sqid, Title: title, State: runState})
	}
	return out, rows.Err()
}
