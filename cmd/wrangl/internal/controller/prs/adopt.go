package prs

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strings"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
)

var (
	titleTag  = regexp.MustCompile(`\[([A-Za-z0-9]{3,})\]`)
	runBranch = regexp.MustCompile(`^wrangl/([A-Za-z0-9]+)-(\d+)$`)
)

// Adopt looks for open PRs in a linked repository that an agent opened itself (with gh)
// instead of leaving it to wrangl: the head is a run branch (wrangl/<sqid>-<n>), or the
// title carries a task's [sqid]. Each is linked to its task and run like a wrangl-opened PR
// and evaluated, which raises the pr_review Inbox item. PRs from forks, PRs against a base
// other than the repo's default branch, and heads wrangl already tracks are left alone.
// It returns the adopted pull_request ids and is idempotent.
func (s *Service) Adopt(ctx context.Context, repoID int64) ([]int64, error) {
	repo, ghr, err := s.repo(ctx, repoID)
	if err != nil {
		return nil, err
	}
	api := s.gh(ghr)
	if api == nil {
		return nil, nil
	}
	open, err := api.ListOpenPRs(ctx, ghr)
	if err != nil {
		return nil, err
	}
	var adopted []int64
	for _, p := range open {
		if p.HeadRepo != "" && !strings.EqualFold(p.HeadRepo, repo.Owner+"/"+repo.Name) {
			continue // a fork
		}
		if p.Head == "" || p.Base != repo.DefaultBranch {
			continue
		}
		if _, err := s.DB.RQ.OpenPRForBranch(ctx, dbgen.OpenPRForBranchParams{RepoID: repo.ID, HeadBranch: p.Head}); err == nil {
			continue // already tracked for this head
		} else if !errors.Is(err, sql.ErrNoRows) {
			return adopted, err
		}
		if _, err := s.DB.RQ.PRByNumber(ctx, dbgen.PRByNumberParams{Owner: repo.Owner, Name: repo.Name, GhNumber: p.Number}); err == nil {
			continue // known by number (merged or closed earlier)
		} else if !errors.Is(err, sql.ErrNoRows) {
			return adopted, err
		}
		run, ok, err := s.runFor(ctx, repo, p.Title, p.Head)
		if err != nil {
			return adopted, err
		}
		if !ok {
			continue
		}
		pr, err := s.DB.Q.CreatePR(ctx, dbgen.CreatePRParams{TaskID: run.TaskID, RepoID: repo.ID, RunID: run.ID, GhNumber: p.Number,
			Url: p.URL, HeadBranch: p.Head, HeadSha: p.HeadSHA})
		if err != nil {
			return adopted, err
		}
		s.Log.Info("adopted agent-opened PR", "pr", p.URL, "run", run.ID, "task", run.TaskID)
		adopted = append(adopted, pr.ID)
		if err := s.Evaluate(ctx, pr.ID); err != nil {
			s.Log.Warn("evaluate adopted PR", "pr", p.URL, "err", err)
		}
	}
	return adopted, nil
}

// runFor finds the work run a PR belongs to: the run whose branch is the head, else the
// latest run of the task named by a [sqid] tag in the title. The task must be in the repo's project.
func (s *Service) runFor(ctx context.Context, repo dbgen.Repo, title, head string) (dbgen.Run, bool, error) {
	if m := runBranch.FindStringSubmatch(head); m != nil {
		var id int64
		err := s.DB.R.QueryRowContext(ctx, `SELECT r.id FROM run r JOIN task t ON t.id = r.task_id
			WHERE r.branch = ? AND r.kind IN ('work','revise') AND t.project_id = ? ORDER BY r.id DESC LIMIT 1`, head, repo.ProjectID).Scan(&id)
		if err == nil {
			r, err := s.DB.RQ.GetRun(ctx, id)
			return r, err == nil, err
		} else if !errors.Is(err, sql.ErrNoRows) {
			return dbgen.Run{}, false, err
		}
	}
	for _, m := range titleTag.FindAllStringSubmatch(title, -1) {
		t, err := s.DB.RQ.GetTaskBySqid(ctx, sql.NullString{String: m[1], Valid: true})
		if errors.Is(err, sql.ErrNoRows) {
			continue
		} else if err != nil {
			return dbgen.Run{}, false, err
		}
		if t.ProjectID != repo.ProjectID {
			continue
		}
		var id int64
		err = s.DB.R.QueryRowContext(ctx, `SELECT id FROM run WHERE kind IN ('work','revise') AND (id = ? OR task_id = ?
			OR task_id IN (SELECT parent_id FROM task WHERE id = ?)) ORDER BY (id = ?) DESC, id DESC LIMIT 1`,
			t.CompletedRunID.Int64, t.ID, t.ID, t.CompletedRunID.Int64).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		} else if err != nil {
			return dbgen.Run{}, false, err
		}
		r, err := s.DB.RQ.GetRun(ctx, id)
		return r, err == nil, err
	}
	return dbgen.Run{}, false, nil
}

// AdoptAll runs Adopt on every linked repository (the poll).
func (s *Service) AdoptAll(ctx context.Context) {
	rows, err := s.DB.RQ.ListRepos(ctx, 0)
	if err != nil {
		s.Log.Error("adopt PRs", "err", err)
		return
	}
	for _, r := range rows {
		if _, err := s.Adopt(ctx, r.Repo.ID); err != nil {
			s.Log.Warn("adopt PRs", "repo", r.Repo.Owner+"/"+r.Repo.Name, "err", err)
		}
	}
}
