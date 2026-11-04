package prs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/gh"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
)

var (
	branchSqid = regexp.MustCompile(`^wrangl/([A-Za-z0-9]+)-\d+$`)
	titleSqid  = regexp.MustCompile(`^\s*\[([A-Za-z0-9]+)\]`)
)

// taggedSqid is the task sqid a PR names by its head branch (wrangl/<sqid>-N) or title ([sqid]).
func taggedSqid(p gh.OpenPR) []string {
	var out []string
	if m := branchSqid.FindStringSubmatch(p.HeadBranch); m != nil {
		out = append(out, m[1])
	}
	if m := titleSqid.FindStringSubmatch(p.Title); m != nil {
		out = append(out, m[1])
	}
	return out
}

// AdoptAll adopts agent-opened PRs in every linked repository.
func (s *Service) AdoptAll(ctx context.Context) {
	repos, err := s.DB.RQ.ListRepos(ctx, 0)
	if err != nil {
		s.Log.Error("adopt PRs: list repos", "err", err)
		return
	}
	for _, r := range repos {
		if err := s.AdoptRepo(ctx, r.Repo.ID); err != nil {
			s.Log.Warn("adopt PRs", "repo", r.Repo.Owner+"/"+r.Repo.Name, "err", err)
		}
	}
}

// AdoptRepo links the repository's open PRs that name a known task of its project (by head
// branch or title) and that wrangl has no row for, then runs the normal evaluation, which
// raises the pr_review item. Idempotent; PRs from forks are never adopted.
func (s *Service) AdoptRepo(ctx context.Context, repoID int64) error {
	repo, ghr, err := s.repo(ctx, repoID)
	if err != nil {
		return err
	}
	api := s.gh(ghr)
	if api == nil {
		return nil
	}
	open, err := api.OpenPRs(ctx, ghr)
	if err != nil {
		return err
	}
	for _, p := range open {
		if p.Fork || p.State != "open" || p.Merged {
			continue
		}
		if _, err := s.DB.RQ.PRByNumber(ctx, dbgen.PRByNumberParams{Owner: repo.Owner, Name: repo.Name, GhNumber: p.Number}); err == nil {
			continue
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		for _, sq := range taggedSqid(p) {
			id, ok, err := s.adopt(ctx, repo, p, sq)
			if err != nil {
				s.Log.Warn("adopt PR", "pr", p.URL, "err", err)
				break
			}
			if ok {
				s.Evaluate(ctx, id)
				break
			}
		}
	}
	return nil
}

func (s *Service) adopt(ctx context.Context, repo dbgen.Repo, p gh.OpenPR, sqid string) (int64, bool, error) {
	t, err := s.DB.RQ.GetTaskBySqid(ctx, nstr(sqid))
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	} else if err != nil {
		return 0, false, err
	}
	if t.ProjectID != repo.ProjectID {
		return 0, false, nil
	}
	if p, perr := s.DB.RQ.GetProject(ctx, t.ProjectID); perr == nil && p.ArchivedAt.Valid {
		// Inert while archived; nothing is recorded, so the next AdoptAll after a restore adopts it.
		return 0, false, nil
	}
	// The run that did the work: the newest on the task or on an ancestor that owns it.
	var runID, rootID int64
	err = s.DB.R.QueryRowContext(ctx, `
WITH RECURSIVE up (id, parent_id) AS (
    SELECT id, parent_id FROM task WHERE id = ?1
    UNION ALL
    SELECT t.id, t.parent_id FROM task t JOIN up ON t.id = up.parent_id
)
SELECT r.id, r.task_id FROM run r
WHERE r.kind IN ('work', 'revise') AND r.task_id IN (SELECT id FROM up)
ORDER BY r.id DESC LIMIT 1`, t.ID).Scan(&runID, &rootID)
	if errors.Is(err, sql.ErrNoRows) {
		s.Log.Info("PR names a task that never ran; not adopted", "pr", p.URL, "task", sqid)
		return 0, false, nil
	} else if err != nil {
		return 0, false, err
	}
	pr, err := s.DB.Q.CreatePR(ctx, dbgen.CreatePRParams{TaskID: rootID, RepoID: repo.ID, RunID: runID, GhNumber: p.Number,
		Url: p.URL, HeadBranch: p.HeadBranch, HeadSha: p.HeadSHA})
	if err != nil {
		return 0, false, err
	}
	s.Log.Info("adopted agent-opened PR", "pr", p.URL, "task", sqid, "run", runID)
	s.Bus.Publish("pr", fmt.Sprint(pr.ID))
	return pr.ID, true, nil
}
