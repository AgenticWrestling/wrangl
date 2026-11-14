package scheduler

import (
	"context"
	"sort"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/gh"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/projects"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

// MaxReadRepos is how many other repos one run gets to read (ADR 0012 section 6).
const MaxReadRepos = 8

// readRepos lists the project's other linked repos for a run to check out read-only, each with its own
// contents:read token. A project that set read_repos to none gets none; a repo whose read token cannot be
// minted is left out (the run is not held back for it). Without a GitHub client (tests, local clones) the
// repos come without tokens. Nothing changes for a project with a single repo: there are no other repos.
func (s *Scheduler) readRepos(ctx context.Context, r dbgen.QueuedRunsForAssignmentRow, own dbgen.Repo) []proto.ReadRepo {
	d := s.Fleet.DB
	task, err := d.RQ.GetTask(ctx, r.TaskID)
	if err != nil {
		return nil
	}
	proj, err := d.RQ.GetProject(ctx, task.ProjectID)
	if err != nil || proj.ReadRepos == "none" {
		return nil
	}
	rows, err := d.RQ.ListRepos(ctx, proj.ID)
	if err != nil {
		return nil
	}
	var others []dbgen.Repo
	for _, x := range rows {
		if x.Repo.ID != own.ID {
			others = append(others, x.Repo)
		}
	}
	sort.Slice(others, func(i, j int) bool {
		return others[i].Owner+"/"+others[i].Name < others[j].Owner+"/"+others[j].Name
	})
	if len(others) > MaxReadRepos {
		s.Log.Info("read repos cut", "run", r.ID, "linked", len(others), "max", MaxReadRepos)
		others = others[:MaxReadRepos]
	}
	taken := map[string]bool{}
	var out []proto.ReadRepo
	for _, o := range others {
		var tok string
		if s.GH != nil {
			rt, ok := s.GH.(gh.ReadTokenAPI)
			if !ok {
				continue // never fall back to a write token for a read
			}
			t, _, err := rt.RepoReadToken(ctx, projects.GHRepo(o))
			if err != nil {
				s.Log.Warn("no read token for a read-only checkout", "run", r.ID, "repo", o.Owner+"/"+o.Name, "err", err)
				continue
			}
			tok = t
		}
		name := o.Name
		if taken[name] {
			name = o.Owner + "-" + o.Name
		}
		taken[name] = true
		out = append(out, proto.ReadRepo{Repo: o.Owner + "/" + o.Name, Name: name, CloneURL: o.CloneUrl, Branch: o.DefaultBranch, Note: o.Note, Token: tok})
	}
	return out
}
