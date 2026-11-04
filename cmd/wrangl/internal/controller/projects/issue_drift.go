package projects

import (
	"context"
	"fmt"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/issuestore"
)

// IssueDrift compares every synced issue of the project's repos with GitHub and lists the ones
// that differ: a missed webhook shows as missing_local or stale, a deletion as missing_remote.
// With repair, differing rows are re-saved from GitHub (only-newer-wins applies, so a
// same-timestamp content difference is reported but left alone) and rows GitHub no longer has
// are removed.
func (s *Service) IssueDrift(ctx context.Context, slug string, repair bool) ([]issuestore.Drift, error) {
	if s.GH == nil {
		return nil, ErrNoGitHub
	}
	p, err := s.Project(ctx, slug)
	if err != nil {
		return nil, err
	}
	repos, err := s.DB.RQ.ListRepos(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	out := []issuestore.Drift{}
	for _, r := range repos {
		remote, err := s.GH.Issues(ctx, ghRepo(r.Repo), "all")
		if err != nil {
			return out, fmt.Errorf("%s/%s: %w", r.Repo.Owner, r.Repo.Name, err)
		}
		local, err := issuestore.ListRepo(ctx, s.DB, r.Repo.ID)
		if err != nil {
			return out, err
		}
		found := issuestore.Compare(r.Repo.Owner, r.Repo.Name, local, remote)
		if repair {
			for _, i := range remote {
				if _, _, err := issuestore.Save(ctx, s.DB, r.Repo.ID, i, s.Now()); err != nil {
					return out, err
				}
			}
			for _, l := range local {
				for _, d := range found {
					if d.Kind == issuestore.DriftMissingRemote && d.Ref == fmt.Sprintf("%s/%s#%d", l.Owner, l.Repo, l.Number) {
						if _, err := issuestore.Delete(ctx, s.DB, r.Repo.ID, l.Number); err != nil {
							return out, err
						}
					}
				}
			}
		}
		out = append(out, found...)
	}
	return out, nil
}
