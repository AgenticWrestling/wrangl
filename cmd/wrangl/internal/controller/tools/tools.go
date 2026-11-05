// Package tools is the system-wide custom tool library and its per-project enablement
// (docs/wrangl/adr/0003). Definitions are validated by toolkdl before they are stored.
package tools

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/errs"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/toolkdl"
)

// Service owns the tool tables.
type Service struct {
	DB *db.DB
	// PutSecret and GetSecret are the controller's secret store (see secrets.go); nil disables
	// secret management and makes every secret read as missing.
	PutSecret func(name string, r io.Reader) error
	GetSecret func(name string) ([]byte, error)
	Log       *slog.Logger
}

// Tool is a stored library entry.
type Tool struct {
	dbgen.Tool
	Projects []string // slugs the tool is enabled on
}

func (s *Service) parse(kdl string) (*toolkdl.Definition, string, error) {
	d, err := toolkdl.Parse([]byte(kdl))
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", errs.Invalid, err)
	}
	decl, _ := json.Marshal(d.Declaration())
	return d, string(decl), nil
}

func (s *Service) load(ctx context.Context, t dbgen.Tool) (Tool, error) {
	ps, err := s.DB.RQ.ListToolProjects(ctx, t.ID)
	return Tool{Tool: t, Projects: ps}, err
}

// Get returns a tool by name.
func (s *Service) Get(ctx context.Context, name string) (Tool, error) {
	t, err := s.DB.RQ.GetToolByName(ctx, name)
	if errors.Is(err, sql.ErrNoRows) {
		return Tool{}, fmt.Errorf("%w: tool %q", errs.NotFound, name)
	}
	if err != nil {
		return Tool{}, err
	}
	return s.load(ctx, t)
}

// List returns the whole library.
func (s *Service) List(ctx context.Context) ([]Tool, error) {
	ts, err := s.DB.RQ.ListTools(ctx)
	if err != nil {
		return nil, err
	}
	return s.loadAll(ctx, ts)
}

func (s *Service) loadAll(ctx context.Context, ts []dbgen.Tool) ([]Tool, error) {
	out := []Tool{}
	for _, t := range ts {
		l, err := s.load(ctx, t)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, nil
}

// Add validates kdl and stores it as a new library tool.
func (s *Service) Add(ctx context.Context, kdl string) (Tool, error) {
	d, decl, err := s.parse(kdl)
	if err != nil {
		return Tool{}, err
	}
	t, err := s.DB.Q.InsertTool(ctx, dbgen.InsertToolParams{Name: d.Name, Kind: string(d.Kind), Effect: string(d.Effect),
		Description: d.Description, Kdl: kdl, DeclJson: decl})
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return Tool{}, fmt.Errorf("%w: tool %q exists (use edit)", errs.Conflict, d.Name)
	}
	if err != nil {
		return Tool{}, err
	}
	return s.load(ctx, t)
}

// Update replaces the definition of an existing tool. The name cannot change.
func (s *Service) Update(ctx context.Context, name, kdl string) (Tool, error) {
	if _, err := s.Get(ctx, name); err != nil {
		return Tool{}, err
	}
	d, decl, err := s.parse(kdl)
	if err != nil {
		return Tool{}, err
	}
	if d.Name != name {
		return Tool{}, fmt.Errorf("%w: the definition is named %q, not %q (a tool cannot be renamed; add a new one)", errs.Invalid, d.Name, name)
	}
	t, err := s.DB.Q.UpdateTool(ctx, dbgen.UpdateToolParams{Kind: string(d.Kind), Effect: string(d.Effect),
		Description: d.Description, Kdl: kdl, DeclJson: decl, Name: name})
	if err != nil {
		return Tool{}, err
	}
	return s.load(ctx, t)
}

// Remove deletes a tool. While it is enabled on projects that needs force.
func (s *Service) Remove(ctx context.Context, name string, force bool) error {
	t, err := s.Get(ctx, name)
	if err != nil {
		return err
	}
	if len(t.Projects) > 0 && !force {
		return fmt.Errorf("%w: tool %q is enabled on %s (disable it first or use --force)", errs.Conflict, name, strings.Join(t.Projects, ", "))
	}
	_, err = s.DB.Q.DeleteTool(ctx, name)
	return err
}

func (s *Service) project(ctx context.Context, slug string) (dbgen.Project, error) {
	p, err := s.DB.RQ.GetProjectBySlug(ctx, slug)
	if errors.Is(err, sql.ErrNoRows) {
		return p, fmt.Errorf("%w: project %q", errs.NotFound, slug)
	}
	return p, err
}

// Enable turns a library tool on for a project (idempotent).
func (s *Service) Enable(ctx context.Context, slug, name string) error {
	p, err := s.project(ctx, slug)
	if err != nil {
		return err
	}
	t, err := s.Get(ctx, name)
	if err != nil {
		return err
	}
	return s.DB.Q.EnableProjectTool(ctx, dbgen.EnableProjectToolParams{ProjectID: p.ID, ToolID: t.ID})
}

// Disable turns a tool off for a project (idempotent).
func (s *Service) Disable(ctx context.Context, slug, name string) error {
	p, err := s.project(ctx, slug)
	if err != nil {
		return err
	}
	t, err := s.Get(ctx, name)
	if err != nil {
		return err
	}
	_, err = s.DB.Q.DisableProjectTool(ctx, dbgen.DisableProjectToolParams{ProjectID: p.ID, ToolID: t.ID})
	return err
}

// ForProject lists the tools enabled for a project.
func (s *Service) ForProject(ctx context.Context, slug string) ([]Tool, error) {
	p, err := s.project(ctx, slug)
	if err != nil {
		return nil, err
	}
	ts, err := s.DB.RQ.ListProjectTools(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	return s.loadAll(ctx, ts)
}

// Enabled reports whether name is enabled for the project.
func (s *Service) Enabled(ctx context.Context, slug, name string) (Tool, error) {
	t, err := s.Get(ctx, name)
	if err != nil {
		return Tool{}, err
	}
	for _, p := range t.Projects {
		if p == slug {
			return t, nil
		}
	}
	if _, err := s.project(ctx, slug); err != nil {
		return Tool{}, err
	}
	return Tool{}, fmt.Errorf("%w: tool %q is not enabled for project %q (wrangl tool enable %s --project %s)", errs.Conflict, name, slug, name, slug)
}
