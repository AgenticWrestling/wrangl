package scheduler

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/errs"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

// NoteNeedsRepo puts a queued run whose task resolves to no repo in the Inbox (ADR 0012 section 4).
// One open item per run; it does nothing when one is already open.
func (s *Scheduler) NoteNeedsRepo(ctx context.Context, runID int64) error {
	d := s.Fleet.DB
	run, err := d.RQ.GetRun(ctx, runID)
	if err != nil {
		return err
	}
	if run.State != "queued" || run.RepoID.Valid {
		return nil
	}
	var open int
	if err := d.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM question WHERE kind = 'needs_repo' AND state = 'open' AND run_id = ?`, runID).Scan(&open); err != nil || open > 0 {
		return err
	}
	t, err := d.RQ.GetTask(ctx, run.TaskID)
	if err != nil {
		return err
	}
	repos, err := d.RQ.ListRepos(ctx, t.ProjectID)
	if err != nil {
		return err
	}
	var opts []proto.QuestionOption
	for _, r := range repos {
		name := r.Repo.Owner + "/" + r.Repo.Name
		opts = append(opts, proto.QuestionOption{Label: "Use " + name, Value: "repo:" + name, Description: "Set " + name + " on this task; the run is scheduled normally."})
		if t.ParentID.Valid {
			opts = append(opts, proto.QuestionOption{Label: "Use " + name + " for the whole subtree", Value: "repo_tree:" + name,
				Description: "Set " + name + " on the topmost ancestor that has no repo, so everything under it inherits it."})
		}
	}
	opts = append(opts,
		proto.QuestionOption{Label: "Link another repo", Value: "link_repo", Description: "Open the project's repositories to link one; this item stays until the run has a repo."},
		proto.QuestionOption{Label: "Cancel the task's run", Value: "cancel_run"})
	title := t.Title
	q, err := d.Q.CreateQuestion(ctx, dbgen.CreateQuestionParams{TaskID: run.TaskID, RunID: sql.NullInt64{Int64: runID, Valid: true},
		Kind: "needs_repo", Prompt: "Which repo should “" + title + "” work in?",
		DetailMd: "No repo is set on this task or an ancestor, and the project has no default repo, so the run is waiting.", PayloadJson: `{"reason":"needs_repo"}`})
	if err != nil {
		return err
	}
	for i, o := range opts {
		d.Q.CreateQuestionOption(ctx, dbgen.CreateQuestionOptionParams{QuestionID: q.ID, Position: int64(i), Label: o.Label, Value: o.Value, Description: o.Description})
	}
	s.Fleet.Bus.Question(q.ID)
	return nil
}

// resolveNeedsRepo closes open items whose run has a repo now or is no longer queued.
func (s *Scheduler) resolveNeedsRepo(ctx context.Context) {
	d := s.Fleet.DB
	rows, err := d.RQ.ListOpenNeedsRepo(ctx)
	if err != nil {
		return
	}
	for _, r := range rows {
		if r.RunState.String == "queued" && !r.RunRepoID.Valid {
			continue
		}
		if n, err := d.Q.ResolveQuestion(ctx, dbgen.ResolveQuestionParams{Text: "resolved: the run has a repo", ID: r.ID}); err == nil && n > 0 {
			s.Fleet.Bus.Question(r.ID)
		}
	}
}

// AnswerNeedsRepo handles an answered needs_repo item. repo:OWNER/NAME sets the repo on the task and
// repo_tree:OWNER/NAME on the topmost ancestor without one; either way the queued run takes it so the
// next tick schedules it. link_repo changes nothing (the web opens the project's repositories).
func (s *Scheduler) AnswerNeedsRepo(ctx context.Context, q dbgen.Question, value, _ string) error {
	d := s.Fleet.DB
	switch {
	case value == "link_repo":
		return nil
	case value == "cancel_run":
		if s.CancelRun == nil || !q.RunID.Valid {
			return nil
		}
		return s.CancelRun(ctx, q.RunID.Int64)
	case strings.HasPrefix(value, "repo:"), strings.HasPrefix(value, "repo_tree:"):
		ref := value[strings.Index(value, ":")+1:]
		owner, name, ok := strings.Cut(ref, "/")
		if !ok {
			return fmt.Errorf("%w: repo %q: want owner/name", errs.Invalid, ref)
		}
		t, err := d.RQ.GetTask(ctx, q.TaskID)
		if err != nil {
			return err
		}
		repo, err := d.RQ.GetProjectRepo(ctx, dbgen.GetProjectRepoParams{ProjectID: t.ProjectID, Owner: owner, Name: name})
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %s is not linked to the project", errs.Invalid, ref)
		} else if err != nil {
			return err
		}
		target := t.ID
		if strings.HasPrefix(value, "repo_tree:") {
			chain, err := d.RQ.AncestorChain(ctx, t.ID) // node first
			if err != nil {
				return err
			}
			for _, a := range chain {
				if a.RepoID.Valid {
					break
				}
				target = a.ID
			}
		}
		id := sql.NullInt64{Int64: repo.ID, Valid: true}
		if err := d.Q.SetTaskRepo(ctx, dbgen.SetTaskRepoParams{RepoID: id, ID: target}); err != nil {
			return err
		}
		if q.RunID.Valid {
			if _, err := d.Q.SetRunRepo(ctx, dbgen.SetRunRepoParams{RepoID: repo.ID, ID: q.RunID.Int64}); err != nil {
				return err
			}
			s.Fleet.Bus.Run(q.RunID.Int64)
		}
		return nil
	}
	return fmt.Errorf("%w: unknown answer %q", errs.Invalid, value)
}
